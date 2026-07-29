package htcs

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"
)

// Port is a target service the host has published, and where to reach it.
type Port struct {
	Name string
	Addr string
}

// Server answers the target's socket calls over one htclow stream.
//
// Every handler that can block runs on its own goroutine: the target has many
// calls in flight at once, keyed by task id, and serving them in order would
// deadlock the moment it left a recv outstanding while doing anything else.
type Server struct {
	rw   io.ReadWriter
	tbl  *table
	send chan []byte

	// Listen binds to loopback by default. Nothing here should be reachable
	// from the network just because a devkit asked.
	Host string

	// OnPort is called when the target publishes a service. It runs on the
	// server's goroutine, so it should not block.
	OnPort func(Port)

	// Log receives anything worth seeing that isn't fatal. nil discards.
	Log func(string)

	// Trace, when set, sees every request and every reply. It's the only way
	// to find out where a service stops talking, since a target that doesn't
	// like an answer just goes quiet rather than saying so.
	Trace func(string)

	mu    sync.Mutex
	ports map[string]string
	// listeners are keyed by port name and outlive the handles that use
	// them, because the target re-listens constantly and the local address
	// has to stay put across that.
	listeners map[string]*pendingListener
	closed    bool
	err       error
	done      chan struct{}
}

// NewServer wires a server to a stream. Serve does the work.
func NewServer(rw io.ReadWriter) *Server {
	return &Server{
		rw:        rw,
		tbl:       newTable(),
		send:      make(chan []byte, 64),
		Host:      "127.0.0.1",
		ports:     map[string]string{},
		listeners: map[string]*pendingListener{},
		done:      make(chan struct{}),
	}
}

// Ports lists what the target has published so far, sorted by name.
func (s *Server) Ports() []Port {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Port, 0, len(s.ports))
	for name, addr := range s.ports {
		out = append(out, Port{Name: name, Addr: addr})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Addr resolves a published port name, or reports that the target hasn't
// published it. It never guesses at a near match: a service that isn't up is
// the usual reason something doesn't respond, and saying so is the point.
func (s *Server) Addr(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	addr, ok := s.ports[name]
	return addr, ok
}

// Done closes when Serve stops. Err says why.
func (s *Server) Done() <-chan struct{} { return s.done }

func (s *Server) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, args...))
	}
}

// Serve runs until the stream fails or Close is called.
func (s *Server) Serve() error {
	go s.sendLoop()
	err := s.receiveLoop()
	s.stop(err)
	return err
}

// Close shuts the server down and drops every socket it was holding.
func (s *Server) Close() error {
	s.stop(nil)
	return nil
}

func (s *Server) stop(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.err = err
	close(s.done)
	listeners := s.listeners
	s.listeners = map[string]*pendingListener{}
	s.mu.Unlock()

	for _, sock := range s.tbl.all() {
		sock.close()
	}
	// The listeners outlive individual handles, so they are the server's to
	// release rather than any socket's.
	for _, ln := range listeners {
		ln.Close()
	}
}

func (s *Server) sendLoop() {
	for {
		select {
		case <-s.done:
			return
		case pkt := <-s.send:
			if _, err := s.rw.Write(pkt); err != nil {
				s.stop(err)
				return
			}
		}
	}
}

func (s *Server) reply(p Packet) {
	if s.Trace != nil {
		s.Trace(fmt.Sprintf("-> %s [%s]", p, Errno(p.Param[0])))
	}
	select {
	case s.send <- p.Encode():
	case <-s.done:
	}
}

func (s *Server) receiveLoop() error {
	head := make([]byte, HeaderSize)
	for {
		if _, err := io.ReadFull(s.rw, head); err != nil {
			return err
		}
		pkt, bodySize, err := ParseHeader(head)
		if err != nil {
			return err
		}
		if bodySize > 0 {
			pkt.Body = make([]byte, bodySize)
			if _, err := io.ReadFull(s.rw, pkt.Body); err != nil {
				return err
			}
		}
		switch pkt.Category {
		case Request:
			s.dispatch(pkt)
		case Notification:
			// Only the Large transfer flows use notifications, and those
			// aren't wired up. Saying so beats silently dropping them.
			s.logf("unhandled notification: %s", pkt)
		default:
			return fmt.Errorf("htcs: target sent a %s, which only the host sends", pkt.Category)
		}
	}
}

// handler is one operation. Returning a Packet sends it; handlers that answer
// asynchronously send their own reply and return false.
type handler struct {
	// blocking says the handler may wait on the network, so it runs on its
	// own goroutine rather than stalling every other in-flight task.
	blocking bool
	// notify says the target waits for a bare acknowledgement before the
	// answer. It goes out from dispatch, not from the handler, so it can't
	// end up behind the work it is supposed to precede.
	notify bool
	run    func(*Server, Packet)
}

// handlers is the dispatch table. It's a map rather than a switch so that a
// type with no entry is caught by a test that walks Types(), instead of
// falling through to whatever the default branch happened to do.
var handlers = map[Type]handler{
	Socket:           {blocking: false, run: (*Server).handleSocket},
	Bind:             {blocking: false, run: (*Server).handleBind},
	Listen:           {blocking: false, run: (*Server).handleListen},
	Accept:           {blocking: true, run: (*Server).handleAccept},
	Send:             {blocking: true, run: (*Server).handleSend},
	Receive:          {blocking: true, run: (*Server).handleReceive},
	Shutdown:         {blocking: false, run: (*Server).handleShutdown},
	Close:            {blocking: false, run: (*Server).handleClose},
	Fcntl:            {blocking: false, run: (*Server).handleFcntl},
	GetTCPPortNumber: {blocking: false, run: (*Server).handleGetTCPPort},
	// Select is the one request the target waits to hear was picked up. The
	// daemon queues the notification before it starts selecting, and a target
	// that never gets one gives up on whatever it was waiting for.
	Select:       {blocking: true, notify: true, run: (*Server).handleSelect},
	EventFd:      {blocking: false, run: (*Server).handleEventFd},
	EventFdRead:  {blocking: true, run: (*Server).handleEventFdRead},
	EventFdWrite: {blocking: false, run: (*Server).handleEventFdWrite},
	Connect:      {blocking: false, run: (*Server).handleConnect},
	ReceiveLarge: {blocking: false, run: (*Server).handleLarge},
	SendLarge:    {blocking: false, run: (*Server).handleLarge},
}

func (s *Server) dispatch(p Packet) {
	if s.Trace != nil {
		s.Trace("<- " + p.String())
	}
	h, ok := handlers[p.Type]
	if !ok {
		s.logf("no handler for %s", p)
		s.reply(p.response(EINVAL))
		return
	}
	if h.notify {
		s.reply(p.notification())
	}
	if h.blocking {
		go h.run(s, p)
		return
	}
	h.run(s, p)
}

func (s *Server) handleSocket(p Packet) {
	sock := s.tbl.create()
	s.reply(p.response(ENONE, int64(sock.handle)))
}

func (s *Server) handleBind(p Packet) {
	sock, err := s.tbl.get(int32(p.Param[0]))
	if err != nil {
		s.reply(p.response(EINVAL))
		return
	}
	peer, port, err := ParseNames(p.Body)
	if err != nil {
		s.logf("bind: %v", err)
		s.reply(p.response(EINVAL))
		return
	}
	sock.peerName, sock.portName = peer, port
	s.reply(p.response(ENONE))
}

func (s *Server) handleListen(p Packet) {
	sock, err := s.tbl.get(int32(p.Param[0]))
	if err != nil {
		s.reply(p.response(EINVAL))
		return
	}
	if sock.portName == "" {
		s.logf("listen on handle %d with no bound port name", sock.handle)
		s.reply(p.response(EINVAL))
		return
	}
	// One listener per port name, kept for the life of the session.
	//
	// The target does not listen once and wait. It tears the whole service
	// down and re-listens roughly twice a second - measured at a 497ms cycle
	// on the video port, and the same pattern is visible on every service.
	// Binding a fresh ephemeral port each time would move the address out
	// from under any client twice a second, and a client that has to
	// re-resolve through the control port spends most of each window
	// reconnecting rather than reading. Holding the listener open makes the
	// address stable and lets the next connection be waiting the instant the
	// target accepts, which is what the daemon does and the reason its video
	// stream looks continuous.
	ln, err := s.listenerFor(sock.portName)
	if err != nil {
		s.logf("listen %s: %v", sock.portName, err)
		s.reply(p.response(toErrno(err)))
		return
	}
	sock.ln = ln

	addr := ln.Addr().String()
	s.mu.Lock()
	_, known := s.ports[sock.portName]
	s.ports[sock.portName] = addr
	s.mu.Unlock()
	// Only announce a service the first time. The re-listens are constant
	// and carry no news; reporting each one would bury everything else.
	if s.OnPort != nil && !known {
		s.OnPort(Port{Name: sock.portName, Addr: addr})
	}
	s.reply(p.response(ENONE))
}

// listenerFor returns the stable listener for a port name, creating it the
// first time.
func (s *Server) listenerFor(name string) (*pendingListener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ln, ok := s.listeners[name]; ok {
		return ln, nil
	}
	raw, err := net.Listen("tcp", net.JoinHostPort(s.Host, "0"))
	if err != nil {
		return nil, err
	}
	ln := newPendingListener(raw)
	s.listeners[name] = ln
	return ln, nil
}

func (s *Server) handleAccept(p Packet) {
	server, err := s.tbl.get(int32(p.Param[0]))
	if err != nil || server.ln == nil {
		s.reply(p.response(EINVAL))
		return
	}
	conn, err := server.ln.Accept()
	if err != nil {
		s.reply(p.response(toErrno(err)))
		return
	}
	sock := s.tbl.create()
	sock.peerName, sock.portName = server.peerName, server.portName
	sock.attach(conn)
	s.reply(p.response(ENONE, int64(sock.handle)))
}

func (s *Server) handleSend(p Packet) {
	sock, err := s.tbl.get(int32(p.Param[0]))
	if err != nil {
		s.reply(p.response(EINVAL))
		return
	}
	size := int(p.Param[1])
	if size < 0 || size > len(p.Body) {
		s.reply(p.response(EINVAL))
		return
	}
	s.reply(p.response(sock.write(p.Body[:size])))
}

func (s *Server) handleReceive(p Packet) {
	sock, err := s.tbl.get(int32(p.Param[0]))
	if err != nil {
		s.reply(p.response(EINVAL))
		return
	}
	size := int(p.Param[1])
	if size < 0 {
		s.reply(p.response(EINVAL))
		return
	}
	// MSG_WAITALL. MSG_PEEK isn't implemented; consuming data the target
	// asked only to look at would corrupt its stream, so it's refused.
	flags := p.Param[2]
	if flags&int64(msgPeek) != 0 {
		s.logf("MSG_PEEK is not implemented (handle %d)", sock.handle)
		s.reply(p.response(EINVAL))
		return
	}
	data, errno := sock.read(size, flags&int64(msgWaitAll) != 0)
	if errno != ENONE {
		s.reply(p.response(errno))
		return
	}
	out := p.response(ENONE)
	out.Body = data
	s.reply(out)
}

// Message flags, from the target's own space.
const (
	msgPeek    = 1
	msgWaitAll = 2
)

func (s *Server) handleShutdown(p Packet) {
	sock, err := s.tbl.get(int32(p.Param[0]))
	if err != nil {
		s.reply(p.response(EINVAL))
		return
	}
	// SHUT_RD=0, SHUT_WR=1, SHUT_RDWR=2. Go only exposes the halves on a
	// TCPConn, so anything else is closed whole.
	tcp, ok := sock.conn.(*net.TCPConn)
	if !ok {
		sock.close()
		s.reply(p.response(ENONE))
		return
	}
	var shutErr error
	switch p.Param[1] {
	case 0:
		shutErr = tcp.CloseRead()
	case 1:
		shutErr = tcp.CloseWrite()
	default:
		shutErr = tcp.Close()
	}
	s.reply(p.response(toErrno(shutErr)))
}

func (s *Server) handleClose(p Packet) {
	handle := int32(p.Param[0])
	sock, ok := s.tbl.remove(handle)
	if !ok {
		s.reply(p.response(EINVAL))
		return
	}
	// Closing a listening handle drops the handle, not the listener. The
	// target closes and re-listens twice a second as a matter of course, so
	// treating that as "the service went away" would un-publish and re-publish
	// the port continuously and hand every client a moving address. The
	// listener is owned by the port name and outlives any one handle; it goes
	// away with the server.
	if sock.ln != nil {
		sock.detachListener()
	}
	sock.close()
	s.reply(p.response(ENONE))
}

func (s *Server) handleFcntl(p Packet) {
	sock, err := s.tbl.get(int32(p.Param[0]))
	if err != nil {
		s.reply(p.response(EINVAL))
		return
	}
	// F_GETFL=3, F_SETFL=4, and O_NONBLOCK is the only flag the protocol
	// allows through.
	switch p.Param[1] {
	case 3:
		s.reply(p.response(ENONE, sock.flags()))
	case 4:
		if p.Param[2] != 0 && p.Param[2] != nonblockFlag {
			s.reply(p.response(EINVAL))
			return
		}
		sock.setNonblock(p.Param[2] == nonblockFlag)
		s.reply(p.response(ENONE))
	default:
		s.reply(p.response(EINVAL))
	}
}

func (s *Server) handleGetTCPPort(p Packet) {
	sock, err := s.tbl.get(int32(p.Param[0]))
	if err != nil {
		s.reply(p.response(EINVAL))
		return
	}
	port, ok := sock.port()
	if !ok {
		s.reply(p.response(ENOTCONN, -1))
		return
	}
	s.reply(p.response(ENONE, int64(port)))
}

// handleSelect answers readiness. Read readiness is real, since a connected
// socket buffers ahead and a listener holds accepted connections. Write
// readiness is reported as always ready, which is what a socket with room in
// its send buffer would say and is the answer for every case here; exception
// readiness is never reported, because nothing in this transport raises one.
func (s *Server) handleSelect(p Packet) {
	nRead, nWrite, nExcept := int(p.Param[0]), int(p.Param[1]), int(p.Param[2])
	total := nRead + nWrite + nExcept
	if nRead < 0 || nWrite < 0 || nExcept < 0 || len(p.Body) != 4*total {
		s.reply(p.response(EINVAL))
		return
	}
	handles := make([]int32, total)
	for i := range handles {
		handles[i] = int32(binary.LittleEndian.Uint32(p.Body[4*i:]))
	}
	readSet := handles[:nRead]
	writeSet := handles[nRead : nRead+nWrite]

	// Param3/Param4 are seconds and microseconds, both -1 for "no timeout".
	var deadline <-chan time.Time
	if p.Param[3] != -1 || p.Param[4] != -1 {
		d := time.Duration(p.Param[3])*time.Second + time.Duration(p.Param[4])*time.Microsecond
		t := time.NewTimer(d)
		defer t.Stop()
		deadline = t.C
	}

	for {
		var ready []int32
		for _, h := range readSet {
			if sock, err := s.tbl.get(h); err == nil && sock.readable() {
				ready = append(ready, h)
			}
		}
		if len(ready) > 0 || nWrite > 0 {
			out := p.response(ENONE, int64(len(ready)), int64(nWrite), 0)
			body := make([]byte, 4*(len(ready)+nWrite))
			for i, h := range ready {
				binary.LittleEndian.PutUint32(body[4*i:], uint32(h))
			}
			for i, h := range writeSet {
				binary.LittleEndian.PutUint32(body[4*(len(ready)+i):], uint32(h))
			}
			out.Body = body
			s.reply(out)
			return
		}
		select {
		case <-s.done:
			s.reply(p.response(EINTR))
			return
		case <-deadline:
			s.reply(p.response(ENONE, 0, 0, 0))
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (s *Server) handleEventFd(p Packet) {
	sock := s.tbl.create()
	flags := p.Param[1]
	sock.event = newEventFd(uint64(p.Param[0]), flags&1 != 0, flags&nonblockFlag != 0)
	s.reply(p.response(ENONE, int64(sock.handle)))
}

func (s *Server) handleEventFdRead(p Packet) {
	sock, err := s.tbl.get(int32(p.Param[0]))
	if err != nil || sock.event == nil {
		s.reply(p.response(EINVAL))
		return
	}
	v, errno := sock.event.read()
	s.reply(p.response(errno, int64(v)))
}

func (s *Server) handleEventFdWrite(p Packet) {
	sock, err := s.tbl.get(int32(p.Param[0]))
	if err != nil || sock.event == nil {
		s.reply(p.response(EINVAL))
		return
	}
	s.reply(p.response(sock.event.write(uint64(p.Param[1]))))
}

// handleConnect is the target asking to reach a service on the host. Nothing
// here publishes one, so the honest answer is that the address doesn't exist
// - inventing a destination would connect the target to something arbitrary.
func (s *Server) handleConnect(p Packet) {
	if peer, port, err := ParseNames(p.Body); err == nil {
		s.logf("target tried to connect out to %s/%s, which nothing here publishes", peer, port)
	}
	s.reply(p.response(EADDRNOTAVAIL))
}

// handleLarge refuses the bulk transfer path. It only comes into play above a
// 1 MB threshold, and every service seen so far stays well under it; failing
// loudly here means a service that does need it shows up as an error rather
// than as a stall.
func (s *Server) handleLarge(p Packet) {
	s.logf("%s is not implemented (handle %d, %d bytes)", p.Type, p.Param[0], p.Param[1])
	s.reply(p.response(EINVAL))
}
