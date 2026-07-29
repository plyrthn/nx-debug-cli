package remoteaudio

import (
	"bytes"
	"sync"
	"testing"
)

func seq(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func TestRingRoundTrip(t *testing.T) {
	r := newRing(64)
	in := seq(16)
	if n, _ := r.Write(in); n != len(in) {
		t.Fatalf("Write returned %d, want %d", n, len(in))
	}
	if got := r.buffered(); got != 16 {
		t.Fatalf("buffered = %d, want 16", got)
	}

	out := make([]byte, 16)
	n, err := r.Read(out)
	if err != nil || n != 16 {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("got % x, want % x", out, in)
	}
	if got := r.buffered(); got != 0 {
		t.Errorf("buffered = %d after draining, want 0", got)
	}
}

// The write path has to wrap around the end of the backing array without
// reordering samples, which is where an off-by-one shows up as a click.
func TestRingWrapsInOrder(t *testing.T) {
	r := newRing(16)
	r.Write(seq(12))
	got := make([]byte, 8)
	r.Read(got)

	// Now the tail is 4 bytes in and this write has to straddle the end.
	next := []byte{100, 101, 102, 103, 104, 105, 106, 107}
	r.Write(next)

	out := make([]byte, 12)
	r.Read(out)
	want := append([]byte{9, 10, 11, 12}, next...)
	if !bytes.Equal(out, want) {
		t.Errorf("got % x, want % x", out, want)
	}
}

// Overrunning must cost the oldest audio, not the newest: a live view should
// stay current rather than fall further behind.
func TestRingOverrunDropsOldest(t *testing.T) {
	r := newRing(16)
	r.Write(seq(16))
	r.Write([]byte{90, 91, 92, 93})

	out := make([]byte, 16)
	r.Read(out)
	want := append(seq(16)[4:], 90, 91, 92, 93)
	if !bytes.Equal(out, want) {
		t.Errorf("got % x, want % x", out, want)
	}
	if dropped, _ := r.stats(); dropped != 4 {
		t.Errorf("dropped = %d, want 4", dropped)
	}
}

// A single chunk bigger than the whole buffer keeps its tail. Everything
// queued behind it is stale by definition and counts as dropped too.
func TestRingWriteLargerThanBuffer(t *testing.T) {
	r := newRing(8)
	r.Write(seq(4))
	r.Write(seq(20))

	out := make([]byte, 8)
	r.Read(out)
	if want := seq(20)[12:]; !bytes.Equal(out, want) {
		t.Errorf("got % x, want % x", out, want)
	}
	if dropped, _ := r.stats(); dropped != 16 {
		t.Errorf("dropped = %d, want 16 (12 from the oversized chunk, 4 already queued)", dropped)
	}
}

// An underrun has to look like quiet audio, never like the end of the
// stream - a short read or an EOF stops the player permanently.
func TestRingUnderrunPadsWithSilence(t *testing.T) {
	r := newRing(64)
	r.Write([]byte{1, 2, 3, 4})

	out := make([]byte, 12)
	for i := range out {
		out[i] = 0xff
	}
	n, err := r.Read(out)
	if err != nil {
		t.Fatalf("Read errored on an underrun: %v", err)
	}
	if n != len(out) {
		t.Fatalf("Read = %d, want a full %d - a short read stops playback", n, len(out))
	}
	want := append([]byte{1, 2, 3, 4}, make([]byte, 8)...)
	if !bytes.Equal(out, want) {
		t.Errorf("got % x, want % x", out, want)
	}
	if _, starved := r.stats(); starved != 8 {
		t.Errorf("starved = %d, want 8", starved)
	}
}

func TestRingReadFromEmpty(t *testing.T) {
	r := newRing(64)
	out := bytes.Repeat([]byte{0xff}, 16)
	n, err := r.Read(out)
	if err != nil || n != 16 {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if !bytes.Equal(out, make([]byte, 16)) {
		t.Errorf("got % x, want all zeroes", out)
	}
}

// Capacity is rounded down to whole frames so wrapping can never put the
// stereo pair out of step.
func TestRingCapacityIsFrameAligned(t *testing.T) {
	for _, c := range []int{-1, 0, 1, 3, 7, 4, 9, 4095} {
		r := newRing(c)
		if len(r.buf)%frameBytes != 0 {
			t.Errorf("newRing(%d) has a %d byte buffer, not a whole number of frames", c, len(r.buf))
		}
		if len(r.buf) == 0 {
			t.Errorf("newRing(%d) has a zero-length buffer", c)
		}
	}
}

// Writes come off the gRPC stream while reads come from the audio device's
// own thread, so this runs under -race in CI.
func TestRingConcurrentReadWrite(t *testing.T) {
	r := newRing(1024)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 500 {
			r.Write(seq(64))
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 128)
		for range 500 {
			if n, err := r.Read(buf); n != len(buf) || err != nil {
				t.Errorf("Read = %d, %v", n, err)
				return
			}
		}
	}()
	wg.Wait()
}
