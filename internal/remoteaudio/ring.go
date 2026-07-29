package remoteaudio

import "sync"

// ring is the fixed-size buffer between the network stream and the sound
// device. The two run at independent rates, so it absorbs jitter in both
// directions: a late chunk means the device reads silence instead of
// stalling, and a burst drops the oldest audio instead of letting latency
// grow without limit.
//
// Everything it holds is already converted, so it is always four bytes to a
// frame, and reads and writes stay frame-aligned as long as the capacity is.
type ring struct {
	mu    sync.Mutex
	buf   []byte
	start int // where the next read comes from
	size  int // bytes currently held

	dropped int64
	starved int64
}

const frameBytes = 4

func newRing(capacity int) *ring {
	if capacity < frameBytes {
		capacity = frameBytes
	}
	capacity -= capacity % frameBytes
	return &ring{buf: make([]byte, capacity)}
}

// Write queues converted audio, overwriting the oldest bytes if there isn't
// room. It never blocks and never fails: stalling the stream reader to wait
// for the speaker would back pressure all the way up the gRPC stream.
func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(p)
	if n > len(r.buf) {
		// One chunk larger than the whole buffer. Only its tail can
		// survive, and whatever was already queued is stale by definition.
		r.dropped += int64(n-len(r.buf)) + int64(r.size)
		p = p[n-len(r.buf):]
		r.start, r.size = 0, 0
	}
	if over := r.size + len(p) - len(r.buf); over > 0 {
		r.start = (r.start + over) % len(r.buf)
		r.size -= over
		r.dropped += int64(over)
	}

	end := (r.start + r.size) % len(r.buf)
	c := copy(r.buf[end:], p)
	copy(r.buf, p[c:])
	r.size += len(p)
	return n, nil
}

// Read fills p, padding with silence when the buffer has run dry, and always
// reports a full read. A short read or an io.EOF would make the player treat
// the stream as finished and stop it for good, so an underrun has to look
// like quiet audio rather than like the end.
func (r *ring) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(p)
	if n > r.size {
		for i := r.size; i < n; i++ {
			p[i] = 0
		}
		r.starved += int64(n - r.size)
		n = r.size
	}
	if len(r.buf) > 0 {
		c := copy(p[:n], r.buf[r.start:])
		copy(p[c:n], r.buf)
		r.start = (r.start + n) % len(r.buf)
	}
	r.size -= n
	return len(p), nil
}

// stats reports bytes thrown away because the buffer was full and silence
// inserted because it was empty. The two tell a target streaming faster than
// the host plays apart from a stream that keeps arriving late.
func (r *ring) stats() (dropped, starved int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped, r.starved
}

func (r *ring) buffered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.size
}
