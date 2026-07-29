package htcs

import (
	"fmt"
	"io"
	"net"
	"sync"
)

// readAhead is how much a connected socket buffers from the host side before
// the target asks for it. It exists so readability can be answered without
// consuming: Go has no way to poll a net.Conn, so the only honest way to say
// "there is data" is to already be holding some.
const readAhead = 64 * 1024

// nonblockFlag is the target's O_NONBLOCK. It's the only fcntl flag the
// protocol allows.
const nonblockFlag = 4

// socket is one entry in the target's handle table. Exactly one of ln, conn
// or event is set; which one is decided by the call that created it, and a
// mismatch is reported rather than assumed away.
type socket struct {
	handle int32

	peerName string
	portName string

	ln    net.Listener
	conn  net.Conn
	event *eventfd

	mu       sync.Mutex
	cond     *sync.Cond
	rx       []byte
	rxErr    error
	rxEOF    bool
	nonblock bool
	closed   bool
}

func newSocket(handle int32) *socket {
	s := &socket{handle: handle}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// attach starts pumping the connection into the read-ahead buffer.
func (s *socket) attach(conn net.Conn) {
	s.conn = conn
	go s.pump()
}

func (s *socket) pump() {
	buf := make([]byte, 32*1024)
	for {
		s.mu.Lock()
		for len(s.rx) >= readAhead && !s.closed {
			s.cond.Wait()
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}

		n, err := s.conn.Read(buf)
		s.mu.Lock()
		if n > 0 {
			s.rx = append(s.rx, buf[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				s.rxEOF = true
			} else {
				s.rxErr = err
			}
			s.cond.Broadcast()
			s.mu.Unlock()
			return
		}
		s.cond.Broadcast()
		s.mu.Unlock()
	}
}

// read serves the target's recv. It blocks for at least one byte unless the
// socket is non-blocking, and reports EOF as (0, nil) - which the protocol
// carries as a success with an empty body, and is how the target learns the
// far end hung up.
func (s *socket) read(max int, waitAll bool) ([]byte, Errno) {
	if s.conn == nil {
		return nil, ENOTCONN
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		want := max
		if waitAll {
			// MSG_WAITALL: hold out for the full amount, but not past a
			// close, or the target would wait forever for bytes that are
			// never coming.
			if len(s.rx) < max && !s.rxEOF && s.rxErr == nil && !s.closed {
				if s.nonblock {
					return nil, EAGAIN
				}
				s.cond.Wait()
				continue
			}
		}
		if len(s.rx) > 0 {
			if want > len(s.rx) {
				want = len(s.rx)
			}
			out := make([]byte, want)
			copy(out, s.rx)
			s.rx = s.rx[want:]
			s.cond.Broadcast()
			return out, ENONE
		}
		if s.rxErr != nil {
			return nil, toErrno(s.rxErr)
		}
		if s.rxEOF || s.closed {
			return nil, ENONE
		}
		if s.nonblock {
			return nil, EAGAIN
		}
		s.cond.Wait()
	}
}

func (s *socket) write(p []byte) Errno {
	if s.conn == nil {
		return ENOTCONN
	}
	if _, err := s.conn.Write(p); err != nil {
		return toErrno(err)
	}
	return ENONE
}

// readable reports whether a recv would return without blocking. A closed or
// failed connection counts as readable: recv on it returns immediately, which
// is what select is being asked.
func (s *socket) readable() bool {
	if s.event != nil {
		return s.event.readable()
	}
	if s.ln != nil {
		return s.ln.(*pendingListener).ready()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rx) > 0 || s.rxEOF || s.rxErr != nil || s.closed
}

func (s *socket) setNonblock(on bool) {
	s.mu.Lock()
	s.nonblock = on
	s.mu.Unlock()
}

func (s *socket) flags() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nonblock {
		return nonblockFlag
	}
	return 0
}

// detachListener gives up this handle's claim on a listener without closing
// it, for the listeners the server keeps alive across the target's re-listen
// cycle.
func (s *socket) detachListener() {
	s.mu.Lock()
	s.ln = nil
	s.mu.Unlock()
}

func (s *socket) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.cond.Broadcast()

	if s.conn != nil {
		s.conn.Close()
	}
	if s.ln != nil {
		s.ln.Close()
	}
	if s.event != nil {
		s.event.close()
	}
}

// port reports the TCP port this handle sits on, which is what the target
// asks for with GetTcpPortNumber.
func (s *socket) port() (int, bool) {
	var addr net.Addr
	switch {
	case s.ln != nil:
		addr = s.ln.Addr()
	case s.conn != nil:
		addr = s.conn.LocalAddr()
	default:
		return 0, false
	}
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.Port, true
	}
	return 0, false
}

// pendingListener wraps a listener with a one-connection lookahead, so
// readability can be answered without stealing the connection from a
// concurrent accept.
type pendingListener struct {
	net.Listener

	mu       sync.Mutex
	waiting  []net.Conn
	incoming chan net.Conn
	errs     chan error
	once     sync.Once
}

func newPendingListener(ln net.Listener) *pendingListener {
	p := &pendingListener{
		Listener: ln,
		incoming: make(chan net.Conn, 8),
		errs:     make(chan error, 1),
	}
	go p.loop()
	return p
}

func (p *pendingListener) loop() {
	for {
		conn, err := p.Listener.Accept()
		if err != nil {
			p.once.Do(func() { p.errs <- err; close(p.incoming) })
			return
		}
		p.mu.Lock()
		p.waiting = append(p.waiting, conn)
		p.mu.Unlock()
		p.incoming <- conn
	}
}

func (p *pendingListener) Accept() (net.Conn, error) {
	conn, ok := <-p.incoming
	if !ok {
		select {
		case err := <-p.errs:
			return nil, err
		default:
			return nil, net.ErrClosed
		}
	}
	p.mu.Lock()
	for i, c := range p.waiting {
		if c == conn {
			p.waiting = append(p.waiting[:i], p.waiting[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	return conn, nil
}

func (p *pendingListener) ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.waiting) > 0
}

// eventfd is the target's wakeup primitive, implemented in memory. It is not
// a host eventfd and does not need to be: nothing outside this process ever
// sees the handle.
type eventfd struct {
	mu        sync.Mutex
	cond      *sync.Cond
	value     uint64
	semaphore bool
	nonblock  bool
	closed    bool
}

func newEventFd(initial uint64, semaphore, nonblock bool) *eventfd {
	e := &eventfd{value: initial, semaphore: semaphore, nonblock: nonblock}
	e.cond = sync.NewCond(&e.mu)
	return e
}

func (e *eventfd) read() (uint64, Errno) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.value == 0 && !e.closed {
		if e.nonblock {
			return 0, EAGAIN
		}
		e.cond.Wait()
	}
	if e.closed {
		return 0, EINTR
	}
	if e.semaphore {
		e.value--
		return 1, ENONE
	}
	v := e.value
	e.value = 0
	return v, ENONE
}

func (e *eventfd) write(v uint64) Errno {
	if v == ^uint64(0) {
		return EINVAL
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return EINTR
	}
	e.value += v
	e.cond.Broadcast()
	return ENONE
}

func (e *eventfd) readable() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.value > 0 || e.closed
}

func (e *eventfd) close() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.cond.Broadcast()
}

// table is the handle space. Handles are never reused while live, and a
// lookup for an unknown one fails rather than returning a zero socket.
type table struct {
	mu   sync.Mutex
	next int32
	m    map[int32]*socket
}

func newTable() *table { return &table{next: 1, m: map[int32]*socket{}} }

func (t *table) create() *socket {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.next
	t.next++
	s := newSocket(h)
	t.m[h] = s
	return s
}

func (t *table) get(handle int32) (*socket, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[handle]
	if !ok {
		return nil, fmt.Errorf("htcs: no socket for handle %d", handle)
	}
	return s, nil
}

func (t *table) remove(handle int32) (*socket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[handle]
	if ok {
		delete(t.m, handle)
	}
	return s, ok
}

func (t *table) all() []*socket {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*socket, 0, len(t.m))
	for _, s := range t.m {
		out = append(out, s)
	}
	return out
}
