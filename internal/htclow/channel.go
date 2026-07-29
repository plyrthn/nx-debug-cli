package htclow

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrChannelClosed is returned once a channel has been torn down, by either
// side.
var ErrChannelClosed = errors.New("htclow: channel closed")

// Buffer sizes the daemon opens the HTCS RPC channel with. They matter to the
// peer, not just to us: the receive size is the initial flow-control credit
// the target is told it may send into.
const (
	DefaultReceiveBuffer = 262144
	DefaultSendBuffer    = 2097152
)

// Stream is one multiplexed channel, presented as a byte stream. HTCS runs on
// exactly one of these.
type Stream struct {
	link *Link
	id   Channel

	mu   sync.Mutex
	cond *sync.Cond

	// Receive side. window is cumulative: it's the total number of bytes this
	// side has ever been willing to accept, so it only goes up, and the peer
	// compares its own running total against it. windowSent is the last value
	// actually put on the wire.
	rx         []byte
	rxTotal    uint64
	window     uint64
	windowSent uint64
	rxCap      int

	// Send side. credit is the peer's cumulative window, so at any moment
	// this side may send up to credit-txTotal more bytes.
	txTotal uint64
	credit  uint64

	closed bool
	err    error
}

func newStream(link *Link, id Channel, rxCap int) *Stream {
	s := &Stream{link: link, id: id, rxCap: rxCap, window: uint64(rxCap)}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Channel reports which mux channel this stream is.
func (s *Stream) Channel() Channel { return s.id }

// Read returns whatever has arrived, blocking until something has. Draining
// the buffer is what frees credit, so a stream nobody reads eventually stops
// the peer from sending - which is the point of flow control, not a bug.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	for len(s.rx) == 0 && !s.closed {
		s.cond.Wait()
	}
	if len(s.rx) == 0 {
		err := s.err
		s.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	n := copy(p, s.rx)
	s.rx = s.rx[n:]
	s.window += uint64(n)

	// Renew credit well before it runs out. Waiting until it's exhausted
	// would stall the peer for a full round trip every time.
	var renew []byte
	if s.window-s.windowSent >= uint64(s.rxCap/2) {
		pkt, err := MaxDataPacket(s.id, s.window)
		if err == nil {
			renew = pkt
			s.windowSent = s.window
		}
	}
	s.mu.Unlock()

	if renew != nil {
		if err := s.link.send(renew); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Write blocks until the peer has granted enough credit, splitting at the
// negotiated body size. A short write is only ever reported with an error.
func (s *Stream) Write(p []byte) (int, error) {
	sent := 0
	for sent < len(p) {
		s.mu.Lock()
		for s.credit <= s.txTotal && !s.closed {
			s.cond.Wait()
		}
		if s.closed {
			err := s.err
			s.mu.Unlock()
			if err == nil {
				err = ErrChannelClosed
			}
			return sent, err
		}
		n := len(p) - sent
		if room := int(s.credit - s.txTotal); n > room {
			n = room
		}
		if n > MuxDefaultBody {
			n = MuxDefaultBody
		}
		offset := uint32(s.txTotal)
		window := s.window
		s.txTotal += uint64(n)
		s.mu.Unlock()

		pkt, err := DataPacket(s.id, offset, window, p[sent:sent+n])
		if err != nil {
			return sent, err
		}
		if err := s.link.send(pkt); err != nil {
			return sent, err
		}
		sent += n
	}
	return sent, nil
}

// Close tears the stream down locally. The peer finds out via the link. It
// also forgets the channel on the link's dispatch table, which matters for
// one opened with OpenChannel: nothing else will look it up again, and a
// bulk channel's id needs to be free for the next transfer to reuse.
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cond.Broadcast()
	s.link.CloseChannel(s.id)
	return nil
}

// SendBulk pushes a whole bulk transfer's payload as a run of Data packets
// and returns once it is all on the wire. Bulk channels do not do flow
// control - the size is agreed on the request/response channel before this
// is ever called - so unlike Write there is no credit to wait for: every
// packet carries the transfer's total length as its share field, which is
// what the reference sets once up front (MakeBulkSendConfig's
// InitialCounterMaxData) rather than a running grant.
func (s *Stream) SendBulk(p []byte) error {
	total := uint64(len(p))
	sent := uint64(0)
	for sent < total {
		n := total - sent
		if n > MuxMaxBodySize {
			n = MuxMaxBodySize
		}
		pkt, err := DataPacket(s.id, uint32(sent), total, p[sent:sent+n])
		if err != nil {
			return err
		}
		if err := s.link.send(pkt); err != nil {
			return err
		}
		sent += n
	}
	return nil
}

// ReceiveBulk copies exactly n bytes of bulk data to w, blocking until they
// have all arrived. Unlike Read it never sends a credit renewal: the peer
// was told the total up front and sends the whole thing regardless of
// anything this side sends back, so a renewal packet here is traffic the
// wire protocol has no use for on a bulk channel.
func (s *Stream) ReceiveBulk(w io.Writer, n int64) error {
	remaining := n
	for remaining > 0 {
		s.mu.Lock()
		for len(s.rx) == 0 && !s.closed {
			s.cond.Wait()
		}
		if len(s.rx) == 0 {
			err := s.err
			s.mu.Unlock()
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		take := int64(len(s.rx))
		if take > remaining {
			take = remaining
		}
		chunk := make([]byte, take)
		copy(chunk, s.rx[:take])
		s.rx = s.rx[take:]
		s.mu.Unlock()

		if _, err := w.Write(chunk); err != nil {
			return err
		}
		remaining -= take
	}
	return nil
}

// deliver takes a received Data packet. The stream position has to line up
// exactly: the peer is tracking the same number, and a gap means bytes were
// lost, which is not something to paper over by carrying on.
func (s *Stream) deliver(h Header, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if uint32(s.rxTotal) != h.Word1 {
		return fmt.Errorf("htclow: channel %s expected stream offset %d, got %d", s.id, uint32(s.rxTotal), h.Word1)
	}
	if h.Share < s.credit {
		return fmt.Errorf("htclow: channel %s credit went backwards, %d after %d", s.id, h.Share, s.credit)
	}
	s.credit = h.Share
	s.rx = append(s.rx, body...)
	s.rxTotal += uint64(len(body))
	s.cond.Broadcast()
	return nil
}

// grant takes a received MaxData packet, which carries credit and nothing
// else.
func (s *Stream) grant(h Header) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.Share < s.credit {
		return fmt.Errorf("htclow: channel %s credit went backwards, %d after %d", s.id, h.Share, s.credit)
	}
	s.credit = h.Share
	s.cond.Broadcast()
	return nil
}

// fail closes the stream with a reason, so a blocked Read or Write reports
// why rather than hanging or returning a bare EOF.
func (s *Stream) fail(err error) {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.err = err
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}
