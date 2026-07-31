package videodecode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"
)

func TestFrameReportsNoFrameYetBeforeAnyDecoded(t *testing.T) {
	d := &Decoder{width: 2, height: 1, frameSize: 6}
	if _, err := d.Frame(context.Background()); !errors.Is(err, ErrNoFrameYet) {
		t.Errorf("err = %v, want ErrNoFrameYet", err)
	}
}

// waitForFrameNo polls until readFrames (running on another goroutine) has
// recorded at least n frames, or fails the test if it never does.
func waitForFrameNo(t *testing.T, d *Decoder, n uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		got := d.frameNo
		d.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("frameNo never reached %d", n)
}

func TestReadFramesSlicesFixedSizeChunks(t *testing.T) {
	// Two 2x1 RGB24 frames back to back, six bytes each, delivered while
	// readFrames is still running - the real case, since the decoder
	// process stays alive between frames rather than exiting after each one.
	frame1 := []byte{1, 2, 3, 4, 5, 6}
	frame2 := []byte{9, 9, 9, 8, 8, 8}
	d := &Decoder{width: 2, height: 1, frameSize: 6, done: make(chan struct{})}

	pr, pw := io.Pipe()
	go d.readFrames(pr)

	if _, err := pw.Write(frame1); err != nil {
		t.Fatal(err)
	}
	waitForFrameNo(t, d, 1)
	if _, err := pw.Write(frame2); err != nil {
		t.Fatal(err)
	}
	waitForFrameNo(t, d, 2)
	pw.Close()

	got, err := d.Frame(context.Background())
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	if !bytes.Equal(got, frame2) {
		t.Errorf("Frame() = % x, want the second (latest) frame % x", got, frame2)
	}
}

func TestReadFramesReportsShortFinalChunkAsAnError(t *testing.T) {
	d := &Decoder{width: 2, height: 1, frameSize: 6, done: make(chan struct{})}
	// A complete frame followed by a partial one - the process died or
	// closed its stdout mid-frame.
	d.readFrames(bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 1, 2, 3}))

	got, err := d.Frame(context.Background())
	if err == nil {
		t.Fatal("Frame: want an error once the stream ends mid-frame")
	}
	if got != nil {
		t.Errorf("Frame() = % x, want nil once decoding has stopped", got)
	}

	// The last complete frame should still have been recorded before the
	// short read that ended the loop.
	d.mu.Lock()
	frameNo := d.frameNo
	d.mu.Unlock()
	if frameNo != 1 {
		t.Errorf("frameNo = %d, want 1 (the one complete frame before the short read)", frameNo)
	}
}

// A frame fetched before the next one arrives must keep its own content -
// readFrames reuses one internal read buffer across every frame, so storing
// that buffer directly instead of a copy would let a later frame silently
// overwrite one a caller is still holding.
func TestFrameContentSurvivesTheNextFrameArriving(t *testing.T) {
	frame1 := []byte{1, 2, 3, 4, 5, 6}
	frame2 := []byte{9, 9, 9, 8, 8, 8}
	d := &Decoder{width: 2, height: 1, frameSize: 6, done: make(chan struct{})}
	pr, pw := io.Pipe()
	go d.readFrames(pr)
	defer pw.Close()

	if _, err := pw.Write(frame1); err != nil {
		t.Fatal(err)
	}
	waitForFrameNo(t, d, 1)
	got, err := d.Frame(context.Background())
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	held := append([]byte(nil), got...)

	if _, err := pw.Write(frame2); err != nil {
		t.Fatal(err)
	}
	waitForFrameNo(t, d, 2)

	if !bytes.Equal(held, frame1) {
		t.Errorf("frame held from before frame 2 arrived = % x, want unchanged % x", held, frame1)
	}
	if !bytes.Equal(got, frame1) {
		t.Errorf("the same slice reference read after frame 2 arrived = % x, want it still % x, not aliased into the reused read buffer", got, frame1)
	}
}

func TestStartFailsFastWhenTheBinaryIsMissing(t *testing.T) {
	old := binary
	binary = "nxdbg-videodecode-does-not-exist"
	defer func() { binary = old }()

	start := time.Now()
	_, err := Start(context.Background(), 2, 1, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Start: want an error for a missing binary")
	}
	if elapsed > time.Second {
		t.Errorf("Start took %v to fail on a missing binary, want a fast LookPath failure", elapsed)
	}
}

// io.ReadFull returning io.EOF exactly on a frame boundary (no partial
// trailing bytes) has to report the error, not silently stop - a caller
// otherwise cannot distinguish "decoding is still fine" from "the process
// is gone".
func TestReadFramesReportsCleanEOFAsAnError(t *testing.T) {
	d := &Decoder{width: 2, height: 1, frameSize: 6, done: make(chan struct{})}
	d.readFrames(bytes.NewReader([]byte{1, 2, 3, 4, 5, 6}))

	d.mu.Lock()
	err := d.err
	d.mu.Unlock()
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want it to wrap io.EOF", err)
	}
}

func TestContainsIDRFindsATypeFiveNal(t *testing.T) {
	sps := []byte{0, 0, 0, 1, 0x67, 1, 2, 3}
	pps := []byte{0, 0, 0, 1, 0x68, 1, 2}
	nonIDR := []byte{0, 0, 0, 1, 0x01, 1, 2, 3}
	idr := []byte{0, 0, 0, 1, 0x05, 1, 2, 3}

	cases := []struct {
		name string
		buf  []byte
		want bool
	}{
		{"empty", nil, false},
		{"non-IDR slice alone", nonIDR, false},
		{"SPS+PPS+non-IDR, the normal shape this stream has always sent", concat(sps, pps, nonIDR), false},
		{"a real IDR slice", idr, true},
		{"SPS+PPS+IDR", concat(sps, pps, idr), true},
	}
	for _, c := range cases {
		if got := containsIDR(c.buf); got != c.want {
			t.Errorf("%s: containsIDR = %v, want %v", c.name, got, c.want)
		}
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestWriteRecordsSawIDR is the Decoder-level half of the regression proof:
// SawIDR must flip true only after an actual IDR-carrying Write, not before.
func TestWriteRecordsSawIDR(t *testing.T) {
	d := newFakeDecoder()
	if d.SawIDR() {
		t.Fatal("SawIDR = true before any Write")
	}
	if err := d.Write([]byte{0, 0, 0, 1, 0x01, 1, 2, 3}); err != nil {
		t.Fatalf("Write (non-IDR): %v", err)
	}
	if d.SawIDR() {
		t.Fatal("SawIDR = true after only a non-IDR slice")
	}
	if err := d.Write([]byte{0, 0, 0, 1, 0x05, 1, 2, 3}); err != nil {
		t.Fatalf("Write (IDR): %v", err)
	}
	if !d.SawIDR() {
		t.Fatal("SawIDR = false after writing a real IDR slice")
	}
}

// nopWriteCloser lets a fake Decoder accept Write/Close calls without a
// real ffmpeg process backing it.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// newFakeDecoder builds a Decoder with no real process behind it, so a test
// can control exactly when it "fails" by closing its done channel.
func newFakeDecoder() *Decoder {
	exited := make(chan error, 1)
	exited <- nil
	return &Decoder{
		width: 2, height: 2, frameSize: 12,
		stdin:  nopWriteCloser{},
		exited: exited,
		done:   make(chan struct{}),
	}
}

// TestSessionResyncsWhenTheCurrentDecoderFails is the real regression proof
// for the resync loop: a decoder dying should be replaced right away, not
// only when a timer happens to fire. resyncInterval is pushed far out so
// only the failure signal can explain a swap within the test's deadline.
func TestSessionResyncsWhenTheCurrentDecoderFails(t *testing.T) {
	oldStart := startDecoder
	defer func() { startDecoder = oldStart }()
	oldInterval := resyncInterval
	resyncInterval = time.Hour
	defer func() { resyncInterval = oldInterval }()

	first := newFakeDecoder()
	second := newFakeDecoder()
	calls := 0
	startDecoder = func(ctx context.Context, w, h int, ps []byte) (*Decoder, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sess, err := StartSession(ctx, 2, 2, StaticParams(nil))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	close(first.done)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sess.mu.Lock()
		cur := sess.cur
		sess.mu.Unlock()
		if cur == second {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Session never swapped to a fresh decoder after the current one failed")
}

// TestSessionBackstopFiresEvenAfterSeeingAnIDR is the regression proof that
// SawIDR no longer gates the backstop. It used to: the theory was that a
// decoder with a real reference has nothing left for the backstop to fix.
// Live testing proved that backwards - the target has never been observed
// sending a real IDR (see SawIDR's doc), so the one time this flag flipped
// true live it was almost certainly a coincidental byte match, and skipping
// the resync on the strength of it left a decoder converged once and then
// drifting forever with no further refresh. The backstop has to keep firing
// regardless.
func TestSessionBackstopFiresEvenAfterSeeingAnIDR(t *testing.T) {
	oldStart := startDecoder
	defer func() { startDecoder = oldStart }()
	oldInterval := resyncInterval
	resyncInterval = 30 * time.Millisecond
	defer func() { resyncInterval = oldInterval }()

	first := newFakeDecoder()
	second := newFakeDecoder()
	calls := 0
	startDecoder = func(ctx context.Context, w, h int, ps []byte) (*Decoder, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sess, err := StartSession(ctx, 2, 2, StaticParams(nil))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	if err := sess.Write([]byte{0, 0, 0, 1, 0x05, 1, 2, 3}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		sess.mu.Lock()
		cur := sess.cur
		sess.mu.Unlock()
		if cur == second {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Session never replaced a decoder that had seen a (spurious) IDR")
}

// TestSessionResyncsToAFreshDecoderPeriodically is a real integration test:
// it spawns actual ffmpeg processes (skipped if ffmpeg isn't on PATH) to
// confirm the backstop ticker genuinely replaces the running decoder on a
// tick rather than just scheduling something that never fires, for the
// (rare, healthy-but-never-failing) case Done() alone wouldn't catch.
func TestSessionResyncsToAFreshDecoderPeriodically(t *testing.T) {
	if _, err := exec.LookPath(binary); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	oldInterval := resyncInterval
	resyncInterval = 150 * time.Millisecond
	defer func() { resyncInterval = oldInterval }()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	sess, err := StartSession(ctx, 2, 2, StaticParams(nil))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	sess.mu.Lock()
	first := sess.cur
	sess.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess.mu.Lock()
		cur := sess.cur
		sess.mu.Unlock()
		if cur != first {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Session never swapped to a fresh decoder after the resync interval")
}
