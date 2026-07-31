// Package videodecode turns the target's raw H.264 stream into displayable
// frames.
//
// The target never sends an IDR or parameter sets (see NXVideoConfig in
// internal/htc), so decoding it takes the same workaround `nxdbg video
// record` already points a user at from the command line: force the decoder
// to emit frames anyway rather than waiting for a reference picture that
// never arrives. This package does that by shelling out to ffmpeg - a real
// H.264 decoder is not something worth re-implementing, and ffmpeg already
// has the exact flag for this stream's specific shape.
package videodecode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrNoFrameYet reports that decoding has started but hasn't produced
// anything yet. A caller polling on an interval treats this the same as any
// other transient miss and just tries again.
var ErrNoFrameYet = errors.New("videodecode: no frame decoded yet")

// binary is the decoder executable. A var so a test can point it at
// something that doesn't exist without touching PATH.
var binary = "ffmpeg"

// startupGrace is how long Start waits to see whether the process is still
// alive before handing it back. Long enough to catch "bad arguments" or
// "missing shared library" failures, short enough that a real devkit
// session doesn't notice the wait - the process just sits blocked on stdin
// during this window, which is the normal, correct state before any data
// has been written to it.
const startupGrace = 400 * time.Millisecond

// Decoder turns a fed Annex B H.264 stream into raw RGB24 frames, top row
// first, matching what internal/remoteview.FrameFunc expects.
type Decoder struct {
	width, height int
	frameSize     int

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan error

	// done is closed the moment readFrames stops, which is what happens as
	// soon as the process exits or its stdout goes bad for any reason. This
	// is the signal Session watches to know a decoder needs replacing,
	// rather than guessing on a timer.
	done chan struct{}

	mu      sync.Mutex
	latest  []byte
	frameNo uint64
	err     error
	sawIDR  bool

	stderr *syncBuffer
}

// syncBuffer is a bytes.Buffer safe for one goroutine writing (the running
// ffmpeg process, via exec's internal copy) concurrently with another
// reading (a caller inspecting decode errors while it's still running).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Start launches the decoder sized for width x height and writes
// parameterSets, if any, ahead of anything sent to Write afterwards.
func Start(ctx context.Context, width, height int, parameterSets []byte) (*Decoder, error) {
	if _, err := exec.LookPath(binary); err != nil {
		return nil, fmt.Errorf("videodecode: %s not found on PATH: %w", binary, err)
	}

	// No -fflags nobuffer: it minimizes I/O buffering for the continuous
	// P-slice stream, but on real hardware it also stops ffmpeg from
	// flushing a single isolated frame with nothing queued behind it yet
	// (confirmed: identical input decodes fine without it, decodes to
	// nothing - not an error, just silently zero output - with it), which
	// is exactly the shape of a freshly seeded reference frame arriving
	// before any live payload has followed it.
	cmd := exec.CommandContext(ctx, binary,
		"-hide_banner",
		"-loglevel", "warning",
		"-flags", "low_delay",
		"-probesize", "32",
		"-analyzeduration", "0",
		"-f", "h264",
		"-flags2", "+showall",
		"-i", "pipe:0",
		"-f", "rawvideo",
		"-pix_fmt", "rgb24",
		"-fps_mode", "passthrough",
		"pipe:1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("videodecode: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("videodecode: stdout pipe: %w", err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("videodecode: start %s: %w", binary, err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("videodecode: %s exited immediately: %s", binary, msg)
		}
		return nil, fmt.Errorf("videodecode: %s exited immediately: %w", binary, err)
	case <-time.After(startupGrace):
	}

	d := &Decoder{
		width: width, height: height, frameSize: width * height * 3,
		cmd: cmd, stdin: stdin, exited: exited, done: make(chan struct{}),
		stderr: stderr,
	}
	if len(parameterSets) > 0 {
		if _, err := stdin.Write(parameterSets); err != nil {
			cmd.Process.Kill()
			return nil, fmt.Errorf("videodecode: write parameter sets: %w", err)
		}
	}
	go d.readFrames(stdout)
	return d, nil
}

// Write feeds one Annex B payload - one access unit off the wire - into the
// decoder.
func (d *Decoder) Write(payload []byte) error {
	if containsIDR(payload) {
		d.mu.Lock()
		d.sawIDR = true
		d.mu.Unlock()
	}
	_, err := d.stdin.Write(payload)
	return err
}

// SawIDR reports whether this decoder has ever been fed a real IDR slice
// (NAL type 5) since it started. The stream this package decodes has never
// been observed to send one - live hardware testing against both a static
// screen and real, actively changing gameplay came back with zero IDR NALs
// either way, see docs - but detecting it costs one cheap scan per payload,
// and means the pipeline (Session's resync backstop, in particular)
// benefits automatically if that ever turns out not to be universally true,
// rather than silently continuing to treat a real reference frame the same
// as having none.
func (d *Decoder) SawIDR() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sawIDR
}

// containsIDR reports whether an Annex B buffer contains a NAL unit with
// type 5 (IDR slice). The NAL header byte is never escaped (only the RBSP
// bytes after it are, see internal/htc's bitWriter.escaped), so reading it
// straight off a start code is safe.
func containsIDR(payload []byte) bool {
	for i := 0; i+4 < len(payload); i++ {
		if payload[i] == 0 && payload[i+1] == 0 && payload[i+2] == 0 && payload[i+3] == 1 {
			if payload[i+4]&0x1f == 5 {
				return true
			}
		}
	}
	return false
}

// readFrames pulls fixed-size raw frames off r until it errors, which is
// what happens when the decoder process exits for any reason.
func (d *Decoder) readFrames(r io.Reader) {
	br := bufio.NewReaderSize(r, d.frameSize)
	buf := make([]byte, d.frameSize)
	for {
		if _, err := io.ReadFull(br, buf); err != nil {
			d.mu.Lock()
			d.err = fmt.Errorf("videodecode: decoder stopped: %w", err)
			d.mu.Unlock()
			close(d.done)
			return
		}
		frame := make([]byte, d.frameSize)
		copy(frame, buf)
		d.mu.Lock()
		d.latest = frame
		d.frameNo++
		d.mu.Unlock()
	}
}

// Done reports when this decoder has stopped producing frames, which is the
// signal that it needs replacing.
func (d *Decoder) Done() <-chan struct{} {
	return d.done
}

// Stderr returns whatever this decoder's ffmpeg process has written to
// stderr so far, for diagnosing a decode that runs but produces nothing.
func (d *Decoder) Stderr() string {
	return d.stderr.String()
}

// Frame returns the most recently decoded frame. Its shape matches
// internal/remoteview.FrameFunc: a caller polling on an interval gets
// ErrNoFrameYet until decoding has produced at least one real frame, which
// that caller's own retry/backoff already tolerates.
func (d *Decoder) Frame(context.Context) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	if d.latest == nil {
		return nil, ErrNoFrameYet
	}
	return d.latest, nil
}

// Close stops feeding the decoder and waits for it to exit.
func (d *Decoder) Close() error {
	d.stdin.Close()
	select {
	case err := <-d.exited:
		return err
	case <-time.After(2 * time.Second):
		d.cmd.Process.Kill()
		return errors.New("videodecode: decoder did not exit after stdin closed, killed")
	}
}

// resyncInterval bounds how long one decoder instance runs before Session
// replaces it, on top of the immediate failure-triggered replace (see
// Decoder.Done), which is what actually carries most of the load in normal
// operation. There is no real reference frame anywhere in this stream (see
// the package doc) even during real gameplay - a real capture of actual play
// was checked NAL type by NAL type and still carried zero IDR frames - so a
// fresh decoder starts flat gray and has to build a picture up entirely from
// the intra macroblocks arriving in ordinary P-slices. Checked against a
// real 30-second gameplay capture (constant on-screen motion, not idle
// DevMenu): a decoder is still mostly gray at 2 seconds in, recognizable by
// around 15, and stays that way with no further corruption through the full
// 30 - regions that keep changing every frame (rustling grass, particle
// effects) stay visibly noisy the whole time since there's no correct
// reference for their motion vectors to track against, but nothing gets
// worse over time the way it did before the parameter sets matched (see
// NXVideoConfig). So this exists as a backstop for a decoder that's wedged
// in some way readFrames' own error handling doesn't catch, not a routine
// quality refresh - it needs to stay out of the way long enough for a fresh
// decoder to actually reach a good picture before tearing it down again.
var resyncInterval = 45 * time.Second

// startDecoder is Start, indirected so a test can substitute a fake
// constructor instead of spawning a real process.
var startDecoder = Start

// Session keeps a decoder running across the periodic restarts
// resyncInterval requires, so a caller sees one continuous picture source
// rather than having to manage decoder lifetimes itself.
type Session struct {
	ctx           context.Context
	width, height int
	paramFunc     func(context.Context) []byte

	mu  sync.Mutex
	cur *Decoder

	// seedCh holds the next decoder's seed once prefetchNext has it ready.
	// Capacity 1: at most one prefetch is ever in flight, kicked off right
	// after the decoder it fetched for goes live.
	seedCh chan []byte

	stop   chan struct{}
	events chan string
}

// StartSession launches the first decoder and begins the resync loop.
// paramFunc is called fresh every time a decoder (re)starts - the initial
// start and every resync - so a caller can seed each one with a genuinely
// current reference (see EncodeIDRFrame) instead of the same fixed bytes
// for the whole life of the session.
func StartSession(ctx context.Context, width, height int, paramFunc func(context.Context) []byte) (*Session, error) {
	dec, err := startDecoder(ctx, width, height, paramFunc(ctx))
	if err != nil {
		return nil, err
	}
	s := &Session{
		ctx: ctx, width: width, height: height, paramFunc: paramFunc,
		cur: dec, stop: make(chan struct{}), events: make(chan string, 8),
		seedCh: make(chan []byte, 1),
	}
	go s.prefetchNext()
	go s.resyncLoop()
	return s, nil
}

// prefetchNext generates the seed for the decoder after this one ahead of
// time, off the resync path. paramFunc (a screenshot capture plus an ffmpeg
// encode) takes on the order of a second - run on replace's critical path,
// that alone was eating most of resyncInterval, leaving the fresh decoder
// only a sliver of its life to actually decode and flush a frame before the
// next backstop tore it down again. Preparing it in the background instead
// means replace almost always finds a seed already waiting.
func (s *Session) prefetchNext() {
	seed := s.paramFunc(s.ctx)
	select {
	case s.seedCh <- seed:
	case <-s.stop:
	case <-s.ctx.Done():
	}
}

// StaticParams wraps a fixed parameter-set buffer as a paramFunc, for a
// caller that has no fresher reference to offer on every restart.
func StaticParams(b []byte) func(context.Context) []byte {
	return func(context.Context) []byte { return b }
}

// Events reports why Session replaces its decoder, as they happen - a
// caller that wants visibility into the resync pipeline (why it happened,
// how often) reads from here rather than the package doing its own logging.
// Sends are non-blocking, so a reader that falls behind or never reads just
// misses events instead of stalling the resync loop.
func (s *Session) Events() <-chan string {
	return s.events
}

func (s *Session) notify(msg string) {
	select {
	case s.events <- msg:
	default:
	}
}

func (s *Session) resyncLoop() {
	backstop := time.NewTicker(resyncInterval)
	defer backstop.Stop()
	for {
		s.mu.Lock()
		cur := s.cur
		s.mu.Unlock()
		select {
		case <-s.ctx.Done():
			return
		case <-s.stop:
			return
		case <-cur.Done():
			s.notify("decoder exited, replacing")
			s.replace()
		case <-backstop.C:
			// This used to skip the resync once SawIDR went true, on the
			// theory that a real IDR gives cur an actual reference and
			// nothing needs fixing. Live testing proved that backwards: the
			// target has never been observed sending a real IDR (see
			// SawIDR's own doc - exhaustive pcap and source analysis, zero
			// hits), so the one time this flag did flip true it was almost
			// certainly a coincidental byte match, not a real keyframe -
			// and skipping the resync on the strength of it left a decoder
			// permanently un-refreshed, converged once and then just
			// drifting forever with nothing to pull it back. The backstop
			// always firing is what actually keeps the picture healthy.
			s.notify("backstop resync")
			s.replace()
		}
	}
}

// replace swaps in a fresh decoder in place of the current one. Called both
// when the current decoder has actually failed and, rarely, off the
// backstop ticker.
func (s *Session) replace() {
	select {
	case <-s.stop:
		return
	case <-s.ctx.Done():
		return
	default:
	}
	// The common case: prefetchNext already has a seed waiting, so this
	// costs nothing. A decoder that failed unusually fast (before its
	// prefetch finished) falls back to generating one right here instead.
	var seed []byte
	select {
	case seed = <-s.seedCh:
	default:
		seed = s.paramFunc(s.ctx)
	}
	next, err := startDecoder(s.ctx, s.width, s.height, seed)
	if err != nil {
		// Keep the current one running rather than losing the picture
		// over a transient failure to relaunch.
		return
	}
	s.mu.Lock()
	old := s.cur
	s.cur = next
	s.mu.Unlock()
	old.Close()
	go s.prefetchNext()
}

// Write feeds one payload into whichever decoder is currently active.
func (s *Session) Write(payload []byte) error {
	s.mu.Lock()
	dec := s.cur
	s.mu.Unlock()
	wasSeen := dec.SawIDR()
	err := dec.Write(payload)
	if !wasSeen && dec.SawIDR() {
		s.notify("real IDR frame seen on the wire for the first time")
	}
	return err
}

// Frame returns the active decoder's most recent frame. Its shape matches
// internal/remoteview.FrameFunc.
func (s *Session) Frame(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	dec := s.cur
	s.mu.Unlock()
	return dec.Frame(ctx)
}

// Stderr returns the active decoder's ffmpeg stderr so far.
func (s *Session) Stderr() string {
	s.mu.Lock()
	dec := s.cur
	s.mu.Unlock()
	return dec.Stderr()
}

// Close stops the resync loop and the active decoder.
func (s *Session) Close() error {
	close(s.stop)
	s.mu.Lock()
	dec := s.cur
	s.mu.Unlock()
	return dec.Close()
}
