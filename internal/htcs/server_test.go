package htcs

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// Every operation the protocol defines needs a handler. A missing one shows
// up as an EINVAL the target can't act on, which reads as a service that
// simply doesn't work - so this is checked mechanically rather than by
// remembering to add an entry.
func TestEveryTypeHasAHandler(t *testing.T) {
	for _, ty := range Types() {
		if _, ok := handlers[ty]; !ok {
			t.Errorf("no handler for %s", ty)
		}
	}
	if len(handlers) != len(Types()) {
		t.Errorf("%d handlers for %d types", len(handlers), len(Types()))
	}
}

func TestPacketRoundTrip(t *testing.T) {
	p := Packet{
		Category: Request,
		Type:     Send,
		Version:  6,
		TaskID:   1234,
		Body:     []byte("payload"),
	}
	p.Param = [5]int64{1, 2, 3, 4, 5}

	buf := p.Encode()
	if len(buf) != HeaderSize+len(p.Body) {
		t.Fatalf("encoded %d bytes, want %d", len(buf), HeaderSize+len(p.Body))
	}
	got, bodySize, err := ParseHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if bodySize != int64(len(p.Body)) {
		t.Errorf("body size = %d, want %d", bodySize, len(p.Body))
	}
	if got.Category != Request || got.Type != Send || got.TaskID != 1234 {
		t.Errorf("header round trip = %+v", got)
	}
	if got.Param != p.Param {
		t.Errorf("params = %v, want %v", got.Param, p.Param)
	}
	if !bytes.Equal(buf[HeaderSize:], p.Body) {
		t.Error("body did not round trip")
	}
}

// A peer speaking a different protocol or a newer version has to be refused,
// not interpreted: every field after the header would mean something else.
func TestParseHeaderRejectsForeignPackets(t *testing.T) {
	good := Packet{Category: Request, Type: Socket, Version: 6}.Encode()

	wrongProto := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(wrongProto[offProtocol:], 9)
	if _, _, err := ParseHeader(wrongProto); err == nil {
		t.Error("a foreign protocol was accepted")
	}

	tooNew := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(tooNew[offVersion:], uint16(MaxVersion+1))
	if _, _, err := ParseHeader(tooNew); err == nil {
		t.Error("a future version was accepted")
	}

	if _, _, err := ParseHeader(good[:HeaderSize-1]); err == nil {
		t.Error("a short buffer parsed as a header")
	}
}

func TestParseNames(t *testing.T) {
	body := make([]byte, 2*NameFieldSize)
	copy(body, "SERIAL")
	copy(body[NameFieldSize:], "iywys@$hid")

	peer, port, err := ParseNames(body)
	if err != nil {
		t.Fatal(err)
	}
	if peer != "SERIAL" || port != "iywys@$hid" {
		t.Errorf("= %q/%q, want SERIAL/iywys@$hid", peer, port)
	}
}

// A short body must fail rather than yield empty names: binding the empty
// port name would publish a service under a name nothing can resolve.
func TestParseNamesRejectsShortBodies(t *testing.T) {
	for _, n := range []int{0, 1, 2*NameFieldSize - 1} {
		if _, _, err := ParseNames(make([]byte, n)); err == nil {
			t.Errorf("%d-byte body accepted", n)
		}
	}
}

func TestTypeNamesUnknownValue(t *testing.T) {
	if got := Type(999).String(); got != "type 999" {
		t.Errorf("= %q, want the number rather than a neighbour's name", got)
	}
	if got := Errno(4242).String(); got != "errno 4242" {
		t.Errorf("= %q, want the number", got)
	}
}

// testServer drives a Server over an in-memory pipe, standing in for the
// target: it writes requests and reads replies.
type testServer struct {
	t      *testing.T
	srv    *Server
	conn   net.Conn
	mu     sync.Mutex
	nextID int32
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	target, host := net.Pipe()
	srv := NewServer(host)
	go srv.Serve()
	t.Cleanup(func() {
		srv.Close()
		target.Close()
		host.Close()
	})
	return &testServer{t: t, srv: srv, conn: target, nextID: 1}
}

// call sends a request and waits for the response with the matching task id.
// Replies can arrive out of order, which is the whole reason task ids exist.
// Some requests are acknowledged with a notification before the answer, so a
// caller after the result has to look past those the way the target does.
func (ts *testServer) call(p Packet) Packet {
	ts.t.Helper()
	ts.mu.Lock()
	p.TaskID = ts.nextID
	ts.nextID++
	ts.mu.Unlock()
	p.Category = Request
	p.Version = MaxVersion

	ts.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := ts.conn.Write(p.Encode()); err != nil {
		ts.t.Fatalf("write %s: %v", p.Type, err)
	}
	for {
		got := ts.recv()
		if got.TaskID == p.TaskID && got.Category == Response {
			return got
		}
	}
}

// callExpectingNotification is call plus the acknowledgement that must come
// first. It exists so the ordering is asserted somewhere rather than merely
// tolerated: the target waits for this before the answer, and a server that
// stopped sending it would otherwise still pass every other test here.
func (ts *testServer) callExpectingNotification(p Packet) (Packet, Packet) {
	ts.t.Helper()
	ts.mu.Lock()
	p.TaskID = ts.nextID
	ts.nextID++
	ts.mu.Unlock()
	p.Category = Request
	p.Version = MaxVersion

	ts.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := ts.conn.Write(p.Encode()); err != nil {
		ts.t.Fatalf("write %s: %v", p.Type, err)
	}
	var note, resp Packet
	for resp.Category != Response {
		got := ts.recv()
		if got.TaskID != p.TaskID {
			continue
		}
		if got.Category == Notification {
			note = got
			continue
		}
		resp = got
	}
	return note, resp
}

func (ts *testServer) recv() Packet {
	ts.t.Helper()
	head := make([]byte, HeaderSize)
	ts.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(ts.conn, head); err != nil {
		ts.t.Fatalf("read header: %v", err)
	}
	p, bodySize, err := ParseHeader(head)
	if err != nil {
		ts.t.Fatal(err)
	}
	if bodySize > 0 {
		p.Body = make([]byte, bodySize)
		if _, err := io.ReadFull(ts.conn, p.Body); err != nil {
			ts.t.Fatalf("read body: %v", err)
		}
	}
	return p
}

func nameBody(peer, port string) []byte {
	b := make([]byte, 2*NameFieldSize)
	copy(b, peer)
	copy(b[NameFieldSize:], port)
	return b
}

// The whole publish-and-relay path, which is what the daemon does: the target
// creates a socket, binds a service name, listens, and the host turns that
// into a real listener something can connect to.
func TestPublishAndRelay(t *testing.T) {
	ts := newTestServer(t)

	sock := ts.call(Packet{Type: Socket})
	if Errno(sock.Param[0]) != ENONE {
		t.Fatalf("socket: %s", Errno(sock.Param[0]))
	}
	handle := sock.Param[1]

	bind := ts.call(Packet{Type: Bind, Param: [5]int64{handle}, Body: nameBody("SERIAL", "iywys@$hid")})
	if Errno(bind.Param[0]) != ENONE {
		t.Fatalf("bind: %s", Errno(bind.Param[0]))
	}

	listen := ts.call(Packet{Type: Listen, Param: [5]int64{handle, 4}})
	if Errno(listen.Param[0]) != ENONE {
		t.Fatalf("listen: %s", Errno(listen.Param[0]))
	}

	addr, ok := ts.srv.Addr("iywys@$hid")
	if !ok {
		t.Fatal("service was not published after listen")
	}

	// Something connects, the way a client would.
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	accept := ts.call(Packet{Type: Accept, Param: [5]int64{handle}})
	if Errno(accept.Param[0]) != ENONE {
		t.Fatalf("accept: %s", Errno(accept.Param[0]))
	}
	conn := accept.Param[1]
	if conn == handle {
		t.Error("accept returned the listening handle rather than a new one")
	}

	// Client to target.
	if _, err := client.Write([]byte("to-target")); err != nil {
		t.Fatal(err)
	}
	recv := ts.call(Packet{Type: Receive, Param: [5]int64{conn, 64, 0}})
	if Errno(recv.Param[0]) != ENONE {
		t.Fatalf("receive: %s", Errno(recv.Param[0]))
	}
	if string(recv.Body) != "to-target" {
		t.Errorf("received %q, want %q", recv.Body, "to-target")
	}

	// Target to client.
	send := ts.call(Packet{Type: Send, Param: [5]int64{conn, 7, 0}, Body: []byte("to-host")})
	if Errno(send.Param[0]) != ENONE {
		t.Fatalf("send: %s", Errno(send.Param[0]))
	}
	buf := make([]byte, 16)
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "to-host" {
		t.Errorf("client got %q, want %q", buf[:n], "to-host")
	}

	// Closing a listening handle must NOT withdraw the service, and the
	// address must not move. Real targets tear the listener down and rebuild
	// it about twice a second; treating that as the service going away
	// republishes a different port on every cycle and leaves any client
	// chasing an address that changes faster than it can resolve it.
	before, _ := ts.srv.Addr("iywys@$hid")
	ts.call(Packet{Type: Close, Param: [5]int64{handle}})
	after, ok := ts.srv.Addr("iywys@$hid")
	if !ok {
		t.Fatal("service was withdrawn when its listening handle closed")
	}
	if after != before {
		t.Errorf("address moved from %s to %s across a close", before, after)
	}

	// And re-listening under the same name has to land back on that address
	// rather than binding a fresh one.
	relisten := ts.call(Packet{Type: Socket})
	ts.call(Packet{Type: Bind, Param: [5]int64{relisten.Param[1]}, Body: nameBody("SERIAL", "iywys@$hid")})
	ts.call(Packet{Type: Listen, Param: [5]int64{relisten.Param[1], 4}})
	again, _ := ts.srv.Addr("iywys@$hid")
	if again != before {
		t.Errorf("re-listen moved the address from %s to %s", before, again)
	}
}

// The target waits to hear that a Select was picked up before it waits for
// the answer, so the acknowledgement is part of the contract rather than a
// courtesy.
func TestSelectIsAcknowledgedBeforeItAnswers(t *testing.T) {
	ts := newTestServer(t)
	conn, client := ts.openConnected("iywys@$cs")
	if _, err := client.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, uint32(conn))
	note, resp := ts.callExpectingNotification(Packet{Type: Select, Param: [5]int64{1, 0, 0, 5, 0}, Body: body})
	if note.Category != Notification || note.Type != Select {
		t.Errorf("no Select notification arrived before the response (got %s)", note)
	}
	if Errno(resp.Param[0]) != ENONE {
		t.Errorf("select: %s", Errno(resp.Param[0]))
	}
}

// A client hanging up has to reach the target as an orderly end of stream:
// success with an empty body, not an error.
func TestReceiveReportsEOFAsAnEmptySuccess(t *testing.T) {
	ts := newTestServer(t)
	conn, client := ts.openConnected("iywys@$cs")
	client.Close()

	recv := ts.call(Packet{Type: Receive, Param: [5]int64{conn, 64, 0}})
	if Errno(recv.Param[0]) != ENONE {
		t.Errorf("receive after close = %s, want ENONE", Errno(recv.Param[0]))
	}
	if len(recv.Body) != 0 {
		t.Errorf("receive after close returned %d bytes", len(recv.Body))
	}
}

// openConnected runs socket/bind/listen/accept and returns the connected
// handle plus the client end.
func (ts *testServer) openConnected(port string) (int64, net.Conn) {
	ts.t.Helper()
	sock := ts.call(Packet{Type: Socket})
	handle := sock.Param[1]
	ts.call(Packet{Type: Bind, Param: [5]int64{handle}, Body: nameBody("SERIAL", port)})
	ts.call(Packet{Type: Listen, Param: [5]int64{handle, 4}})

	addr, ok := ts.srv.Addr(port)
	if !ok {
		ts.t.Fatalf("%s was not published", port)
	}
	client, err := net.Dial("tcp", addr)
	if err != nil {
		ts.t.Fatal(err)
	}
	ts.t.Cleanup(func() { client.Close() })

	accept := ts.call(Packet{Type: Accept, Param: [5]int64{handle}})
	if Errno(accept.Param[0]) != ENONE {
		ts.t.Fatalf("accept: %s", Errno(accept.Param[0]))
	}
	return accept.Param[1], client
}

// A non-blocking recv with nothing waiting has to say EAGAIN rather than
// block, or the target's own event loop stalls.
func TestNonBlockingReceive(t *testing.T) {
	ts := newTestServer(t)
	conn, _ := ts.openConnected("iywys@$gdb")

	set := ts.call(Packet{Type: Fcntl, Param: [5]int64{conn, 4, nonblockFlag}})
	if Errno(set.Param[0]) != ENONE {
		t.Fatalf("fcntl set: %s", Errno(set.Param[0]))
	}
	get := ts.call(Packet{Type: Fcntl, Param: [5]int64{conn, 3}})
	if get.Param[1] != nonblockFlag {
		t.Errorf("fcntl get = %d, want %d", get.Param[1], nonblockFlag)
	}

	recv := ts.call(Packet{Type: Receive, Param: [5]int64{conn, 64, 0}})
	if Errno(recv.Param[0]) != EAGAIN {
		t.Errorf("non-blocking receive = %s, want EAGAIN", Errno(recv.Param[0]))
	}
}

func TestSelectReportsReadableSockets(t *testing.T) {
	ts := newTestServer(t)
	conn, client := ts.openConnected("iywys@$dmnt")
	if _, err := client.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, uint32(conn))
	sel := ts.call(Packet{Type: Select, Param: [5]int64{1, 0, 0, 5, 0}, Body: body})
	if Errno(sel.Param[0]) != ENONE {
		t.Fatalf("select: %s", Errno(sel.Param[0]))
	}
	if sel.Param[1] != 1 {
		t.Fatalf("select reported %d readable, want 1", sel.Param[1])
	}
	if got := int64(binary.LittleEndian.Uint32(sel.Body)); got != conn {
		t.Errorf("select named handle %d, want %d", got, conn)
	}
}

func TestEventFd(t *testing.T) {
	ts := newTestServer(t)

	ev := ts.call(Packet{Type: EventFd, Param: [5]int64{0, 0}})
	if Errno(ev.Param[0]) != ENONE {
		t.Fatalf("eventfd: %s", Errno(ev.Param[0]))
	}
	h := ev.Param[1]

	if w := ts.call(Packet{Type: EventFdWrite, Param: [5]int64{h, 7}}); Errno(w.Param[0]) != ENONE {
		t.Fatalf("eventfd write: %s", Errno(w.Param[0]))
	}
	r := ts.call(Packet{Type: EventFdRead, Param: [5]int64{h}})
	if Errno(r.Param[0]) != ENONE || r.Param[1] != 7 {
		t.Errorf("eventfd read = %s/%d, want ENONE/7", Errno(r.Param[0]), r.Param[1])
	}
}

// An unknown handle is a real error, not something to invent a socket for.
func TestUnknownHandleIsRejected(t *testing.T) {
	ts := newTestServer(t)
	for _, ty := range []Type{Receive, Send, Close, Fcntl, GetTCPPortNumber, Accept} {
		got := ts.call(Packet{Type: ty, Param: [5]int64{9999, 1, 0}})
		if Errno(got.Param[0]) == ENONE {
			t.Errorf("%s on an unknown handle succeeded", ty)
		}
	}
}

// Listen without a bound name must fail: it would otherwise publish a service
// under the empty name, which nothing can resolve and which silently shadows
// the next unnamed one.
func TestListenWithoutBindIsRejected(t *testing.T) {
	ts := newTestServer(t)
	sock := ts.call(Packet{Type: Socket})
	got := ts.call(Packet{Type: Listen, Param: [5]int64{sock.Param[1], 4}})
	if Errno(got.Param[0]) == ENONE {
		t.Error("listen succeeded with no port name bound")
	}
}

// The target reaching out to a host service is answered honestly. Inventing a
// destination would connect it to something arbitrary.
func TestConnectIsRefusedRatherThanGuessed(t *testing.T) {
	ts := newTestServer(t)
	sock := ts.call(Packet{Type: Socket})
	got := ts.call(Packet{
		Type:  Connect,
		Param: [5]int64{sock.Param[1]},
		Body:  nameBody("HOST", "iywys@$something"),
	})
	if Errno(got.Param[0]) != EADDRNOTAVAIL {
		t.Errorf("connect = %s, want EADDRNOTAVAIL", Errno(got.Param[0]))
	}
}

func TestGetTCPPortNumber(t *testing.T) {
	ts := newTestServer(t)
	conn, _ := ts.openConnected("iywys@$perfmon")
	got := ts.call(Packet{Type: GetTCPPortNumber, Param: [5]int64{conn}})
	if Errno(got.Param[0]) != ENONE {
		t.Fatalf("= %s", Errno(got.Param[0]))
	}
	if got.Param[1] <= 0 || got.Param[1] > 65535 {
		t.Errorf("port = %d, want a real TCP port", got.Param[1])
	}
}

func TestPortsAreSortedAndComplete(t *testing.T) {
	ts := newTestServer(t)
	for _, name := range []string{"iywys@$video", "iywys@$audio", "iywys@$hid"} {
		sock := ts.call(Packet{Type: Socket})
		h := sock.Param[1]
		ts.call(Packet{Type: Bind, Param: [5]int64{h}, Body: nameBody("SERIAL", name)})
		ts.call(Packet{Type: Listen, Param: [5]int64{h, 4}})
	}
	ports := ts.srv.Ports()
	if len(ports) != 3 {
		t.Fatalf("%d ports, want 3", len(ports))
	}
	for i := 1; i < len(ports); i++ {
		if ports[i-1].Name > ports[i].Name {
			t.Errorf("ports out of order: %s before %s", ports[i-1].Name, ports[i].Name)
		}
	}
}
