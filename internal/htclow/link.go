package htclow

import (
	"errors"
	"fmt"
	"sync"
)

// TargetInfo is what the target says about itself during the connect
// exchange. It's the same JSON the daemon reports, obtained without it.
type TargetInfo struct {
	Spec   string `json:"Spec"`
	Conn   string `json:"Conn"`
	HW     string `json:"HW"`
	Name   string `json:"Name"`
	SN     string `json:"SN"`
	FW     string `json:"FW"`
	Prot   int    `json:"Prot"`
	Method string `json:"-"`
}

// Link is a live htclow session: the control handshake plus every mux channel
// running over one transport.
type Link struct {
	t    Transport
	r    *reader
	Info TargetInfo

	sendMu sync.Mutex

	mu      sync.Mutex
	streams map[Channel]*Stream
	closed  bool
	err     error

	done chan struct{}
}

// Dial brings a link up: connect, ready, then a stream per channel the target
// agreed to. It returns once the link is usable; the receive loop keeps
// running until Close or a wire error.
func Dial(t Transport) (*Link, error) {
	l := &Link{
		t:       t,
		r:       newReader(t, HeaderSize+MuxMaxBodySize),
		streams: map[Channel]*Stream{},
		done:    make(chan struct{}),
	}
	if err := l.handshake(); err != nil {
		return nil, err
	}
	go l.receiveLoop()
	return l, nil
}

func (l *Link) handshake() error {
	seq := uint32(1)
	sendCtrl := func(ct CtrlType, body []byte) error {
		pkt, err := CtrlPacket(ct, seq, body)
		if err != nil {
			return err
		}
		seq++
		return l.send(pkt)
	}

	if err := sendCtrl(ConnectFromHost, nil); err != nil {
		return fmt.Errorf("sending %s: %w", ConnectFromHost, err)
	}
	h, body, err := l.readPacket()
	if err != nil {
		return err
	}
	if !h.Ctrl() || CtrlType(h.Type) != ConnectFromTarget {
		return fmt.Errorf("expected %s, got %s", ConnectFromTarget, h.TypeName())
	}
	if err := parseTargetInfo(body, &l.Info); err != nil {
		return err
	}

	// The channel set is not open yet and carries no credit. Flow control for
	// a channel the target hasn't agreed to exists stalls the pipe, so
	// nothing goes out on the mux until ReadyFromTarget comes back.
	if err := sendCtrl(ReadyFromHost, ReadyFromHostBody(ServiceChannels)); err != nil {
		return fmt.Errorf("sending %s: %w", ReadyFromHost, err)
	}

	var agreed Ready
	for i := 0; ; i++ {
		if i == 8 {
			return fmt.Errorf("no %s after %d packets", ReadyFromTarget, i)
		}
		h, body, err = l.readPacket()
		if err != nil {
			return err
		}
		if h.Ctrl() && CtrlType(h.Type) == ReadyFromTarget {
			agreed, err = ParseReady(body)
			if err != nil {
				return err
			}
			break
		}
	}
	if len(agreed.Channels) == 0 {
		return errors.New("htclow: target agreed to no channels")
	}

	// Opening a channel is exactly one thing on the wire: telling the peer
	// how much this side can receive on it.
	for _, ch := range agreed.Channels {
		s := newStream(l, ch, DefaultReceiveBuffer)
		l.streams[ch] = s
		pkt, err := MaxDataPacket(ch, s.window)
		if err != nil {
			return err
		}
		if err := l.send(pkt); err != nil {
			return fmt.Errorf("opening channel %s: %w", ch, err)
		}
		s.windowSent = s.window
	}
	return nil
}

// Stream returns the channel's byte stream, or false if the target didn't
// agree to that channel. Callers must not assume a channel exists just
// because it's in ServiceChannels - the target's list is what counts.
func (l *Link) Stream(ch Channel) (*Stream, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.streams[ch]
	return s, ok
}

// OpenChannel raises a channel outside the initial handshake, for a bulk
// transfer whose id the peer already named in a request on an existing
// channel (HTCFS's ReadFileLarge/WriteFileLarge and HTCS's SendLarge/
// ReceiveLarge all work this way). Unlike the four service channels,
// opening one sends nothing: bulk channels skip the handshake by agreement,
// so the first traffic on the wire is the data itself, and the peer already
// expects it - it is the one that picked the id.
func (l *Link) OpenChannel(ch Channel, rxCap int) (*Stream, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrChannelClosed
	}
	if _, exists := l.streams[ch]; exists {
		return nil, fmt.Errorf("htclow: channel %s is already open", ch)
	}
	s := newStream(l, ch, rxCap)
	l.streams[ch] = s
	return s, nil
}

// CloseChannel forgets a channel opened with OpenChannel. A bulk channel's id
// is only good for one transfer, so once it is done there is nothing left to
// dispatch to - and holding onto the entry would let a stray late packet on
// a reused id resurrect a finished stream.
func (l *Link) CloseChannel(ch Channel) {
	l.mu.Lock()
	delete(l.streams, ch)
	l.mu.Unlock()
}

// Channels lists what the target agreed to, in the order it named them.
func (l *Link) Channels() []Channel {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Channel, 0, len(l.streams))
	for _, ch := range ServiceChannels {
		if _, ok := l.streams[ch]; ok {
			out = append(out, ch)
		}
	}
	return out
}

// Done closes when the link stops, for whatever reason. Err says why.
func (l *Link) Done() <-chan struct{} { return l.done }

// Err reports why the link stopped, or nil if it was closed deliberately.
func (l *Link) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Close tells the target the host is gone and stops the link. Skipping the
// disconnect is what makes the next attempt come back as DisconnectFromTarget
// instead of a fresh handshake.
func (l *Link) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()

	if pkt, err := CtrlPacket(DisconnectFromHost, 0, nil); err == nil {
		l.send(pkt)
	}
	l.stop(nil)
	return l.t.Close()
}

// send serialises every write onto the transport. Two goroutines interleaving
// a header and a body would corrupt the framing in a way the target reports
// only by dropping the link.
func (l *Link) send(pkt []byte) error {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	return l.t.WritePacket(pkt)
}

func (l *Link) readPacket() (Header, []byte, error) {
	head, err := l.r.readFull(HeaderSize)
	if err != nil {
		return Header{}, nil, err
	}
	h, err := ParseHeader(head)
	if err != nil {
		return Header{}, nil, err
	}
	limit := CtrlMaxBodySize
	if h.Mux() {
		limit = MuxMaxBodySize
	}
	if int(h.BodySize) > limit {
		return Header{}, nil, fmt.Errorf("htclow: %s claims a %d byte body, limit %d", h.TypeName(), h.BodySize, limit)
	}
	if h.BodySize == 0 {
		return h, nil, nil
	}
	body, err := l.r.readFull(int(h.BodySize))
	if err != nil {
		return Header{}, nil, err
	}
	return h, body, nil
}

func (l *Link) receiveLoop() {
	for {
		h, body, err := l.readPacket()
		if err != nil {
			l.stop(err)
			return
		}
		if err := l.dispatch(h, body); err != nil {
			l.stop(err)
			return
		}
		if l.isClosed() {
			return
		}
	}
}

func (l *Link) dispatch(h Header, body []byte) error {
	switch {
	case h.Ctrl():
		switch CtrlType(h.Type) {
		case DisconnectFromTarget:
			return errors.New("htclow: target disconnected")
		case SuspendFromTarget, ResumeFromTarget, InformationFromTarget:
			// Informational. Nothing here acts on target power state, and
			// answering a packet this code doesn't model would be worse than
			// ignoring it.
			return nil
		}
		return nil

	case h.Mux():
		ch := Channel{Module: h.ModuleID, ID: h.ChannelID}
		s, ok := l.Stream(ch)
		if !ok {
			// A channel the target opened that this side never agreed to.
			// Dropping the link over it would be worse than ignoring it.
			return nil
		}
		switch MuxType(h.Type) {
		case MuxData:
			return s.deliver(h, body)
		case MuxMaxData:
			return s.grant(h)
		case MuxError:
			s.fail(fmt.Errorf("htclow: target reported an error on channel %s", ch))
			return nil
		}
		return nil
	}
	return fmt.Errorf("htclow: unrecognised packet, signature %#08x", h.Signature)
}

func (l *Link) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *Link) stop(err error) {
	l.mu.Lock()
	if l.err == nil {
		l.err = err
	}
	streams := make([]*Stream, 0, len(l.streams))
	for _, s := range l.streams {
		streams = append(streams, s)
	}
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	l.mu.Unlock()

	if err == nil {
		err = ErrChannelClosed
	}
	for _, s := range streams {
		s.fail(err)
	}
}
