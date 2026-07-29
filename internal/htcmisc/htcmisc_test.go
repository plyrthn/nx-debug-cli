package htcmisc

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestHeaderRoundTrip(t *testing.T) {
	p := Packet{
		Version:  2,
		Category: Request,
		Type:     GetEnvironmentVariable,
		TaskID:   7,
		Param0:   -1,
		Param1:   2,
		Param2:   3,
		Param3:   4,
		Param4:   5,
		Body:     []byte("PATH"),
	}
	raw := p.Encode()
	if len(raw) != HeaderSize+len(p.Body) {
		t.Fatalf("encoded %d bytes, want %d", len(raw), HeaderSize+len(p.Body))
	}
	got, bodySize, err := ParseHeader(raw[:HeaderSize])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bodySize != len(p.Body) {
		t.Errorf("body size = %d, want %d", bodySize, len(p.Body))
	}
	got.Body = raw[HeaderSize:]
	if got.Version != p.Version || got.Category != p.Category || got.Type != p.Type ||
		got.TaskID != p.TaskID || got.Param0 != p.Param0 || got.Param4 != p.Param4 ||
		!bytes.Equal(got.Body, p.Body) {
		t.Errorf("round trip = %+v, want %+v", got, p)
	}
}

// The header layout is not ours to choose, so the offsets are worth pinning
// against the numbers rather than against Encode's own idea of them.
func TestHeaderFieldOffsets(t *testing.T) {
	raw := Packet{Version: 2, Category: Response, Type: SyncTime, TaskID: 0x11223344, Param0: 0x55}.Encode()
	cases := []struct {
		name   string
		offset int
		size   int
		want   uint64
	}{
		{"protocol", 0, 2, uint64(Protocol)},
		{"version", 2, 2, 2},
		{"category", 4, 2, uint64(Response)},
		{"type", 6, 2, uint64(SyncTime)},
		{"body size", 8, 8, 0},
		{"task id", 16, 4, 0x11223344},
		{"param0", 24, 8, 0x55},
	}
	for _, c := range cases {
		var got uint64
		switch c.size {
		case 2:
			got = uint64(binary.LittleEndian.Uint16(raw[c.offset:]))
		case 4:
			got = uint64(binary.LittleEndian.Uint32(raw[c.offset:]))
		case 8:
			got = binary.LittleEndian.Uint64(raw[c.offset:])
		}
		if got != c.want {
			t.Errorf("%s at %d = %d, want %d", c.name, c.offset, got, c.want)
		}
	}
}

func TestParseHeaderRejectsBadPackets(t *testing.T) {
	good := Packet{Type: SyncTime}.Encode()

	t.Run("short", func(t *testing.T) {
		if _, _, err := ParseHeader(good[:HeaderSize-1]); err == nil {
			t.Error("a short header was accepted")
		}
	})
	t.Run("wrong protocol", func(t *testing.T) {
		raw := append([]byte(nil), good...)
		binary.LittleEndian.PutUint16(raw[0:], uint16(Protocol+1))
		if _, _, err := ParseHeader(raw); err == nil {
			t.Error("another protocol's header was accepted")
		}
	})
	t.Run("future version", func(t *testing.T) {
		raw := append([]byte(nil), good...)
		binary.LittleEndian.PutUint16(raw[2:], uint16(MaxVersion+1))
		if _, _, err := ParseHeader(raw); err == nil {
			t.Error("a version this build cannot speak was accepted")
		}
	})
	t.Run("oversized body", func(t *testing.T) {
		raw := append([]byte(nil), good...)
		binary.LittleEndian.PutUint64(raw[8:], maxBody+1)
		if _, _, err := ParseHeader(raw); err == nil {
			t.Error("an oversized body was accepted")
		}
	})
}

// newTestServer runs a Server against one end of a pipe and returns a
// request/response round trip over the other.
func newTestServer(t *testing.T, configure func(*Server)) func(Packet) Packet {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	s := NewServer(server)
	if configure != nil {
		configure(s)
	}
	go s.Serve()

	return func(p Packet) Packet {
		t.Helper()
		p.Category = Request
		client.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := client.Write(p.Encode()); err != nil {
			t.Fatalf("write request: %v", err)
		}
		head := make([]byte, HeaderSize)
		if _, err := io.ReadFull(client, head); err != nil {
			t.Fatalf("read response: %v", err)
		}
		reply, bodySize, err := ParseHeader(head)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if bodySize > 0 {
			reply.Body = make([]byte, bodySize)
			if _, err := io.ReadFull(client, reply.Body); err != nil {
				t.Fatalf("read response body: %v", err)
			}
		}
		return reply
	}
}

func TestServerAnswersEnvironmentVariables(t *testing.T) {
	call := newTestServer(t, func(s *Server) {
		s.Env = func(name string) (string, bool) {
			if name == "NX_TEST" {
				return "value", true
			}
			return "", false
		}
	})

	r := call(Packet{Type: GetEnvironmentVariable, TaskID: 3, Body: []byte("NX_TEST")})
	if Result(r.Param0) != Success || string(r.Body) != "value" {
		t.Errorf("get = %s/%q, want Success/%q", Result(r.Param0), r.Body, "value")
	}
	if r.TaskID != 3 {
		t.Errorf("task id = %d, want 3, or the target cannot match the reply", r.TaskID)
	}

	r = call(Packet{Type: GetEnvironmentVariableLength, Body: []byte("NX_TEST")})
	n, ok := int64Body(r.Body)
	if Result(r.Param0) != Success || !ok || n != 5 {
		t.Errorf("length = %s/%d, want Success/5", Result(r.Param0), n)
	}

	// Not set is a real answer, not an error, and has to be distinguishable.
	r = call(Packet{Type: GetEnvironmentVariable, Body: []byte("NX_ABSENT")})
	if Result(r.Param0) != InvalidRequest {
		t.Errorf("absent variable = %s, want InvalidRequest", Result(r.Param0))
	}
}

func TestServerAnswersWorkingDirectory(t *testing.T) {
	call := newTestServer(t, func(s *Server) {
		s.WorkingDir = func() (string, error) { return "/host/dir", nil }
	})

	r := call(Packet{Type: GetWorkingDirectory})
	if Result(r.Param0) != Success || string(r.Body) != "/host/dir" {
		t.Errorf("dir = %s/%q, want Success/%q", Result(r.Param0), r.Body, "/host/dir")
	}

	r = call(Packet{Type: GetWorkingDirectorySize})
	n, ok := int64Body(r.Body)
	if !ok || n != int64(len("/host/dir")) {
		t.Errorf("size = %d, want %d", n, len("/host/dir"))
	}
}

func TestServerReportsTargetStatus(t *testing.T) {
	got := make(chan int64, 1)
	call := newTestServer(t, func(s *Server) {
		s.Status = func(status int64) { got <- status }
	})

	r := call(Packet{Type: SetTargetStatus, Param0: 9})
	if Result(r.Param0) != Success {
		t.Errorf("status = %s, want Success", Result(r.Param0))
	}
	select {
	case status := <-got:
		if status != 9 {
			t.Errorf("reported status = %d, want 9", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the status was never reported")
	}
}

// The two sync methods are answered completely differently, and answering the
// wrong one would leave the target believing a clock sync that never happened.
func TestServerSyncTime(t *testing.T) {
	call := newTestServer(t, nil)

	r := call(Packet{Type: SyncTime, Param0: SyncByResponse})
	if Result(r.Param0) != Success {
		t.Fatalf("sync by response = %s, want Success", Result(r.Param0))
	}
	if _, err := time.Parse("20060102150405", string(r.Body)); err != nil {
		t.Errorf("sync by response body %q is not the target's time format: %v", r.Body, err)
	}

	r = call(Packet{Type: SyncTime, Param0: SyncByDevMenuCommand})
	if Result(r.Param0) == Success {
		t.Error("sync by devmenu command reported success, but nothing set the clock")
	}

	r = call(Packet{Type: SyncTime, Param0: 99})
	if Result(r.Param0) != InvalidRequest {
		t.Errorf("unknown sync method = %s, want InvalidRequest", Result(r.Param0))
	}
}

// Running a command line on this machine on the target's say-so is refused,
// and the refusal has to be well-formed rather than silence.
func TestServerRefusesRunOnHost(t *testing.T) {
	call := newTestServer(t, nil)

	r := call(Packet{Type: RunOnHost, Body: []byte("whoami")})
	if r.Type != RunOnHost {
		t.Errorf("reply type = %s, want %s", r.Type, RunOnHost)
	}
	if r.Param0 == 0 {
		t.Error("exit code = 0, which tells the target the command succeeded")
	}
}

// An unhandled request gets a refusal rather than nothing, because a target
// waiting on a reply that never comes stalls the whole channel.
func TestServerRefusesUnknownTypes(t *testing.T) {
	call := newTestServer(t, nil)

	r := call(Packet{Type: Type(999), TaskID: 5})
	if Result(r.Param0) != InvalidRequest {
		t.Errorf("unknown type = %s, want InvalidRequest", Result(r.Param0))
	}
	if r.TaskID != 5 {
		t.Errorf("task id = %d, want 5", r.TaskID)
	}
}

// hostRequestTypes are the operations the *host* makes of the target, so the
// server is right not to have a handler for them. Everything else the target
// can ask must be handled, and adding a new type to the enum without deciding
// which side it belongs on should fail here rather than on hardware.
var hostRequestTypes = map[Type]bool{
	GetMaxProtocolVersion: true,
	SetProtocolVersion:    true,
	SetTargetName:         true,
}

func TestEveryRequestTypeIsAccountedFor(t *testing.T) {
	for typ := range typeNames {
		_, handled := handlers[typ]
		if handled == hostRequestTypes[typ] {
			if handled {
				t.Errorf("%s is listed as a host request but the server also handles it", typ)
			} else {
				t.Errorf("%s has no handler and is not listed as a host request", typ)
			}
		}
	}
}

func TestEveryTypeAndResultHasAName(t *testing.T) {
	for _, typ := range []Type{
		GetMaxProtocolVersion, SetProtocolVersion, GetEnvironmentVariable,
		GetEnvironmentVariableLength, SetTargetStatus, RunOnHost,
		GetWorkingDirectory, GetWorkingDirectorySize, SetTargetName, SyncTime,
	} {
		if _, ok := typeNames[typ]; !ok {
			t.Errorf("type %d has no name", int16(typ))
		}
	}
	for _, r := range []Result{Success, UnknownError, UnsupportedVer, InvalidRequest} {
		if _, ok := resultNames[r]; !ok {
			t.Errorf("result %d has no name", int64(r))
		}
	}
}

func TestClientSetTargetName(t *testing.T) {
	client, target := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		target.Close()
	})
	c := NewClient(client)

	done := make(chan Packet, 1)
	go func() {
		target.SetDeadline(time.Now().Add(2 * time.Second))
		head := make([]byte, HeaderSize)
		if _, err := io.ReadFull(target, head); err != nil {
			t.Errorf("read request: %v", err)
			close(done)
			return
		}
		p, bodySize, err := ParseHeader(head)
		if err != nil {
			t.Errorf("parse request: %v", err)
			close(done)
			return
		}
		p.Body = make([]byte, bodySize)
		io.ReadFull(target, p.Body)
		target.Write(p.respond(Success, nil).Encode())
		done <- p
	}()

	if err := c.SetTargetName("scratch"); err != nil {
		t.Fatalf("set target name: %v", err)
	}
	p, ok := <-done
	if !ok {
		t.Fatal("the target never saw a request")
	}
	if p.Category != Request || p.Type != SetTargetName || string(p.Body) != "scratch" {
		t.Errorf("request = %s, want a SetTargetName carrying %q", p, "scratch")
	}
}

func TestClientReportsATargetSideFailure(t *testing.T) {
	client, target := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		target.Close()
	})
	c := NewClient(client)

	go func() {
		target.SetDeadline(time.Now().Add(2 * time.Second))
		head := make([]byte, HeaderSize)
		if _, err := io.ReadFull(target, head); err != nil {
			return
		}
		p, bodySize, err := ParseHeader(head)
		if err != nil {
			return
		}
		// The whole request has to come off the pipe before anything is
		// written back: net.Pipe is unbuffered, so the client is still inside
		// its own Write until the body is read.
		io.ReadFull(target, make([]byte, bodySize))
		target.Write(p.respond(UnknownError, nil).Encode())
	}()

	if err := c.SetTargetName("scratch"); err == nil {
		t.Fatal("a refused rename was reported as success")
	}
}
