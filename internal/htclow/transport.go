package htclow

import (
	"fmt"
	"io"
)

// Transport is the wire under the link. It is packet-oriented on the way out
// and byte-oriented on the way in, which is not an inconsistency: on USB the
// send side has to control transfer boundaries (the target reads a header and
// then reads exactly BodySize more, so a combined write overruns its first
// read), while the receive side may get a header and body together or apart
// and has to cope with either.
type Transport interface {
	// WritePacket sends one whole packet, framed however the transport needs.
	WritePacket(pkt []byte) error
	// Read returns whatever bytes have arrived, in order.
	Read(p []byte) (int, error)
	io.Closer
}

// reader turns a Transport's arbitrary reads into exact-length ones. Without
// it the framing depends on how the driver happened to split a transfer,
// which is not something to build a protocol on.
type reader struct {
	t    Transport
	buf  []byte
	off  int
	fill int
}

func newReader(t Transport, size int) *reader {
	return &reader{t: t, buf: make([]byte, size)}
}

// readFull returns exactly n bytes. The slice it returns aliases the internal
// buffer and stays valid only until the next call.
func (r *reader) readFull(n int) ([]byte, error) {
	if n > len(r.buf) {
		return nil, fmt.Errorf("htclow: %d bytes exceeds the %d byte read buffer", n, len(r.buf))
	}
	for r.fill-r.off < n {
		// Slide the remainder down rather than growing: packets are read one
		// at a time, so the buffer only ever needs to hold one.
		if r.off > 0 {
			copy(r.buf, r.buf[r.off:r.fill])
			r.fill -= r.off
			r.off = 0
		}
		if r.fill == len(r.buf) {
			return nil, fmt.Errorf("htclow: read buffer full at %d bytes with %d still wanted", r.fill, n)
		}
		got, err := r.t.Read(r.buf[r.fill:])
		if got > 0 {
			r.fill += got
			continue
		}
		if err == nil {
			err = io.ErrNoProgress
		}
		return nil, err
	}
	out := r.buf[r.off : r.off+n]
	r.off += n
	return out, nil
}
