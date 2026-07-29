package htc

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// A real document captured off the control port, trimmed to a few ports.
const sampleDoc = `<HtcsInfo><TargetList><Target><PeerType>NX-NX</PeerType><HtcsPeerName>SERIAL0001</HtcsPeerName><Id>TargetId01</Id></Target></TargetList><PortMap>` +
	`<PortMapItem><HtcsPeerName>SERIAL0001</HtcsPeerName><HtcsPortName>iywys@$gdb</HtcsPortName><IPAddress>127.0.0.1</IPAddress><TcpPortNumber>54849</TcpPortNumber></PortMapItem>` +
	`<PortMapItem><HtcsPeerName>SERIAL0001</HtcsPeerName><HtcsPortName>@Log</HtcsPortName><IPAddress>127.0.0.1</IPAddress><TcpPortNumber>54854</TcpPortNumber></PortMapItem>` +
	`<PortMapItem><HtcsPeerName>SERIAL0002</HtcsPeerName><HtcsPortName>iywys@$hid</HtcsPortName><IPAddress>127.0.0.1</IPAddress><TcpPortNumber>54860</TcpPortNumber></PortMapItem>` +
	`</PortMap></HtcsInfo>`

func TestParsePortMap(t *testing.T) {
	snap, err := parsePortMap([]byte(sampleDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(snap.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(snap.Targets))
	}
	if snap.Targets[0].Peer != "SERIAL0001" || snap.Targets[0].PeerType != "NX-NX" {
		t.Errorf("target = %+v", snap.Targets[0])
	}
	if len(snap.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(snap.Entries))
	}
	if got := snap.Entries[0].Addr(); got != "127.0.0.1:54849" {
		t.Errorf("addr = %q, want 127.0.0.1:54849", got)
	}
}

func TestPortMapFind(t *testing.T) {
	snap, err := parsePortMap([]byte(sampleDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Scoped to the right peer.
	if e, ok := snap.Find("SERIAL0002", "iywys@$hid"); !ok || e.TCPPort != 54860 {
		t.Errorf("find hid on SERIAL0002 = %+v, %v", e, ok)
	}
	// The same port on the wrong peer must not match - two targets can
	// publish the same service and picking either would be wrong.
	if _, ok := snap.Find("SERIAL0001", "iywys@$hid"); ok {
		t.Error("hid matched on SERIAL0001, which doesn't publish it")
	}
	// An empty peer accepts any target.
	if _, ok := snap.Find("", "iywys@$hid"); !ok {
		t.Error("empty peer should match any target")
	}
	if _, ok := snap.Find("SERIAL0001", "iywys@$cs"); ok {
		t.Error("unpublished port reported as found")
	}
}

func TestPortMapPorts(t *testing.T) {
	snap, _ := parsePortMap([]byte(sampleDoc))
	got := strings.Join(snap.Ports("SERIAL0001"), ",")
	if got != "iywys@$gdb,@Log" {
		t.Errorf("ports = %q", got)
	}
	if len(snap.Ports("")) != 3 {
		t.Errorf("unfiltered ports = %d, want 3", len(snap.Ports("")))
	}
}

func TestParsePortMapGarbage(t *testing.T) {
	if _, err := parsePortMap([]byte("not xml at all")); err == nil {
		t.Error("expected an error on non-XML input")
	}
}

func TestControlPortEnvOverride(t *testing.T) {
	t.Setenv(ControlPortEnv, "31337")
	if got := ControlPort(); got != 31337 {
		t.Errorf("ControlPort = %d, want 31337", got)
	}
	if got := ControlAddr(); got != "127.0.0.1:31337" {
		t.Errorf("ControlAddr = %q", got)
	}

	// A junk override must not win; fall through to the default rather
	// than dialling port zero.
	t.Setenv(ControlPortEnv, "not-a-port")
	if got := ControlPort(); got <= 0 {
		t.Errorf("ControlPort = %d on a bad override, want a usable port", got)
	}
}

// fakeControlServer stands in for the control server: it waits for the
// request line then pushes each document it was given.
func fakeControlServer(t *testing.T, docs ...string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		if _, err := conn.Read(buf); err != nil {
			return
		}
		for _, doc := range docs {
			if _, err := conn.Write([]byte(doc + "\n")); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Hold the connection so the client sees a subscription, not EOF.
		time.Sleep(time.Second)
	}()
	return ln.Addr().String()
}

func TestPortMapOverTheWire(t *testing.T) {
	addr := fakeControlServer(t, sampleDoc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snap, err := PortMap(ctx, addr)
	if err != nil {
		t.Fatalf("PortMap: %v", err)
	}
	if len(snap.Entries) != 3 {
		t.Errorf("entries = %d, want 3", len(snap.Entries))
	}
}

// The daemon pushes a partial mapping the instant you subscribe and the real
// one a moment later. Stopping at the first document makes live services look
// absent, which is exactly the wrong answer for "is this channel up".
func TestPortMapWaitsForTheFullMapping(t *testing.T) {
	partial := `<HtcsInfo><TargetList><Target><PeerType>NX-NX</PeerType><HtcsPeerName>SERIAL0001</HtcsPeerName><Id>TargetId01</Id></Target></TargetList><PortMap>` +
		`<PortMapItem><HtcsPeerName>SERIAL0001</HtcsPeerName><HtcsPortName>@Log</HtcsPortName><IPAddress>127.0.0.1</IPAddress><TcpPortNumber>54854</TcpPortNumber></PortMapItem>` +
		`</PortMap></HtcsInfo>`
	addr := fakeControlServer(t, partial, sampleDoc)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	snap, err := PortMap(ctx, addr)
	if err != nil {
		t.Fatalf("PortMap: %v", err)
	}
	if len(snap.Entries) != 3 {
		t.Fatalf("entries = %d, want 3 (settled on the partial snapshot)", len(snap.Entries))
	}
	if _, ok := snap.Find("SERIAL0001", "iywys@$gdb"); !ok {
		t.Error("settled snapshot is missing a port that the full mapping has")
	}
}

func TestWatchPortMapSeesUpdates(t *testing.T) {
	second := strings.Replace(sampleDoc, "iywys@$gdb", "iywys@$cs", 1)
	addr := fakeControlServer(t, sampleDoc, second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w, err := WatchPortMap(ctx, addr)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer w.Close()

	first, err := w.Next(ctx)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, ok := first.Find("SERIAL0001", "iywys@$gdb"); !ok {
		t.Error("first snapshot missing gdb")
	}

	next, err := w.Next(ctx)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, ok := next.Find("SERIAL0001", "iywys@$cs"); !ok {
		t.Error("second snapshot missing cs")
	}
}

func TestWatchPortMapRespectsContext(t *testing.T) {
	addr := fakeControlServer(t, sampleDoc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := WatchPortMap(ctx, addr)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer w.Close()

	if _, err := w.Next(ctx); err != nil {
		t.Fatalf("first: %v", err)
	}

	// No further documents are coming; cancelling has to unblock the read.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := w.Next(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Next did not unblock on cancel")
	}
}

func TestPortNotPublishedErrorMessage(t *testing.T) {
	err := &PortNotPublishedError{Peer: "SERIAL0001", Port: "iywys@$hid", Published: []string{"iywys@$gdb"}}
	msg := err.Error()
	for _, want := range []string{"iywys@$hid", "SERIAL0001", "iywys@$gdb"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	// It has to survive errors.As so callers can tell "not listening" apart
	// from "couldn't reach the control port".
	var target *PortNotPublishedError
	if !errors.As(error(err), &target) {
		t.Error("errors.As failed")
	}
}

func TestReadLimitedLine(t *testing.T) {
	addr := fakeControlServer(t, strings.Repeat("x", 1024))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w, err := WatchPortMap(ctx, addr)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer w.Close()

	// Long lines have to reassemble across bufio's internal buffer rather
	// than being truncated, even though this one isn't valid XML.
	if _, err := w.Next(ctx); err == nil || !strings.Contains(err.Error(), "parse port map") {
		t.Errorf("err = %v, want a parse failure (proving the full line was read)", err)
	}
}

func TestPortMapWithNothingListening(t *testing.T) {
	// Port 1 on loopback is never served, and the dial fails immediately.
	_, err := PortMap(context.Background(), "127.0.0.1:1")
	var ns *NoSessionError
	if !errors.As(err, &ns) {
		t.Fatalf("PortMap returned %v (%T), want a *NoSessionError", err, err)
	}
	if !strings.Contains(ns.Error(), "nxdbg serve") {
		t.Errorf("message never says what to start: %s", ns.Error())
	}
}
