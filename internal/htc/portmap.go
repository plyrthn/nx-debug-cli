package htc

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// nxdbg serve runs a small XML control server on loopback whose only job is
// to publish where every target's HTCS ports have been forwarded to on the
// host. Write "<RequestSystemPortMapping />\n" and it answers with a
// newline-delimited stream of <HtcsInfo> documents - one immediately, then
// another every time the mapping changes. It's a subscription, not a
// request/response, so a client that reads one document and hangs up sees a
// snapshot while a client that stays connected tracks the target live.

// DefaultControlPort is the HTCS control port `nxdbg serve` publishes on.
const DefaultControlPort = 20184

// ControlPortEnv overrides port discovery entirely.
const ControlPortEnv = "NXDBG_HTCS_CONTROL_PORT"

// ControlPort returns the TCP port the HTCS control server is listening on:
// the env override if set, otherwise the documented default.
func ControlPort() int {
	if v := os.Getenv(ControlPortEnv); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return DefaultControlPort
}

// ControlAddr is ControlPort as a dialable loopback address. The control
// server binds loopback only, so the host is never anything else.
func ControlAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(ControlPort()))
}

// PortMapEntry is one forwarded HTCS port: a service on a target, reachable
// at a plain TCP address on the host.
type PortMapEntry struct {
	// Peer is the target's HTCS peer name, which is its serial number.
	Peer string
	// Port is the HTCS port name, e.g. "iywys@$gdb".
	Port string
	// Address is the host-side IP the daemon forwards from.
	Address string
	// TCPPort is the host-side TCP port.
	TCPPort int
}

// Addr is the entry as a dialable host:port.
func (e PortMapEntry) Addr() string {
	return net.JoinHostPort(e.Address, strconv.Itoa(e.TCPPort))
}

// Service resolves the entry against the service registry. Unrecognised
// ports report false; callers should show the raw port name rather than
// substitute a guess.
func (e PortMapEntry) Service() (Service, bool) {
	return ServiceForPort(e.Port)
}

// PortMapTarget is a target the control server currently knows about.
type PortMapTarget struct {
	Peer     string
	PeerType string
	ID       string
}

// Target is a devkit reachable over HTCS, as shown to a user. There is no
// handle: nothing numbers targets, so a serial is the only identifier there
// is.
type Target struct {
	Name                string
	UniqueIdentifier    string
	HardwareType        string
	CommunicationMethod string
}

// PortMapSnapshot is one <HtcsInfo> document: every target and every
// forwarded port at a single instant.
type PortMapSnapshot struct {
	Targets []PortMapTarget
	Entries []PortMapEntry
}

// Find returns the entry for a target's service. peer may be empty to accept
// any target, which is the common case when only one is connected.
func (s *PortMapSnapshot) Find(peer, portName string) (PortMapEntry, bool) {
	for _, e := range s.Entries {
		if e.Port != portName {
			continue
		}
		if peer == "" || e.Peer == peer {
			return e, true
		}
	}
	return PortMapEntry{}, false
}

// Ports lists the HTCS port names published for a target, in the order the
// daemon reported them.
func (s *PortMapSnapshot) Ports(peer string) []string {
	var out []string
	for _, e := range s.Entries {
		if peer == "" || e.Peer == peer {
			out = append(out, e.Port)
		}
	}
	return out
}

// htcsInfo mirrors the wire document. Kept unexported so the XML shape stays
// an implementation detail.
type htcsInfo struct {
	XMLName    xml.Name `xml:"HtcsInfo"`
	TargetList struct {
		Targets []struct {
			PeerType string `xml:"PeerType"`
			PeerName string `xml:"HtcsPeerName"`
			ID       string `xml:"Id"`
		} `xml:"Target"`
	} `xml:"TargetList"`
	PortMap struct {
		Items []struct {
			PeerName  string `xml:"HtcsPeerName"`
			PortName  string `xml:"HtcsPortName"`
			IPAddress string `xml:"IPAddress"`
			TCPPort   int    `xml:"TcpPortNumber"`
		} `xml:"PortMapItem"`
	} `xml:"PortMap"`
}

func parsePortMap(doc []byte) (*PortMapSnapshot, error) {
	var info htcsInfo
	if err := xml.Unmarshal(doc, &info); err != nil {
		return nil, fmt.Errorf("htc: parse port map: %w", err)
	}
	snap := &PortMapSnapshot{}
	for _, t := range info.TargetList.Targets {
		snap.Targets = append(snap.Targets, PortMapTarget{
			Peer:     t.PeerName,
			PeerType: t.PeerType,
			ID:       t.ID,
		})
	}
	for _, it := range info.PortMap.Items {
		snap.Entries = append(snap.Entries, PortMapEntry{
			Peer:    it.PeerName,
			Port:    it.PortName,
			Address: it.IPAddress,
			TCPPort: it.TCPPort,
		})
	}
	return snap, nil
}

// PortMapWatcher is a live subscription to the control server. Next blocks
// until the next snapshot arrives, so a caller can react to a target
// publishing or withdrawing a service.
type PortMapWatcher struct {
	conn net.Conn
	r    *bufio.Reader
}

// maxPortMapDoc bounds a single document. The real ones run a few kilobytes;
// this only exists so a wedged or wrong-protocol peer can't be read forever.
const maxPortMapDoc = 4 << 20

// WatchPortMap subscribes to the control server at addr. Pass ControlAddr()
// for the local daemon.
func WatchPortMap(ctx context.Context, addr string) (*PortMapWatcher, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("htc: dial HTCS control port %s: %w", addr, err)
	}
	// The request is a bare XML element terminated by a newline; the server
	// reads it a line at a time.
	if _, err := conn.Write([]byte("<RequestSystemPortMapping />\n")); err != nil {
		conn.Close()
		return nil, fmt.Errorf("htc: request port mapping: %w", err)
	}
	return &PortMapWatcher{conn: conn, r: bufio.NewReaderSize(conn, 64<<10)}, nil
}

// Next returns the next snapshot the server pushes.
func (w *PortMapWatcher) Next(ctx context.Context) (*PortMapSnapshot, error) {
	if deadline, ok := ctx.Deadline(); ok {
		w.conn.SetReadDeadline(deadline)
	} else {
		w.conn.SetReadDeadline(time.Time{})
	}
	// Cancelling the context unblocks the read by closing the connection;
	// there's no other way to interrupt one.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			w.conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()

	line, err := readLimitedLine(w.r, maxPortMapDoc)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("htc: read port map: %w", err)
	}
	return parsePortMap(line)
}

func (w *PortMapWatcher) Close() error {
	return w.conn.Close()
}

// readLimitedLine reads up to and including the next newline, refusing to
// buffer more than limit bytes.
func readLimitedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > limit {
			return nil, fmt.Errorf("document exceeds %d bytes", limit)
		}
		if !isPrefix {
			return out, nil
		}
	}
}

// settleWindow is how long PortMap waits for a follow-up snapshot before
// deciding the mapping has stopped changing.
const settleWindow = 400 * time.Millisecond

// settleLimit caps the whole settling loop, so a target whose mapping keeps
// changing yields the latest snapshot instead of blocking forever.
const settleLimit = 3 * time.Second

// PortMap fetches the current mapping and disconnects.
//
// The first document the server sends is not the whole picture: it pushes a
// partial mapping immediately on subscribe and then the complete one a
// moment later. Taking the first one makes services that are up look absent,
// so this keeps reading until the pushes stop.
func PortMap(ctx context.Context, addr string) (*PortMapSnapshot, error) {
	w, err := WatchPortMap(ctx, addr)
	if err != nil {
		// Nothing listening on the control port is by far the most common way
		// to get here, and the raw dial error says nothing about what to do
		// about it.
		return nil, &NoSessionError{Addr: addr, Err: err}
	}
	defer w.Close()

	snap, err := w.Next(ctx)
	if err != nil {
		return nil, err
	}
	// A target that's churning could push updates forever; take the latest
	// one seen by then rather than blocking.
	giveUp := time.Now().Add(settleLimit)
	for time.Now().Before(giveUp) {
		settleCtx, cancel := context.WithTimeout(ctx, settleWindow)
		next, err := w.Next(settleCtx)
		cancel()
		if err != nil {
			// Nothing more arrived in the window, so this is settled. A
			// real failure would have shown up on the first read.
			break
		}
		snap = next
	}
	return snap, nil
}

// ResolvePort returns the host-side address of a target's HTCS port. peer is
// the target's serial number, or empty to take whichever target publishes
// it. portName accepts either a registry key ("hid") or a full port name
// ("iywys@$hid").
func ResolvePort(ctx context.Context, peer, portName string) (PortMapEntry, error) {
	if svc, ok := LookupService(portName); ok {
		portName = svc.Port
	}
	snap, err := PortMap(ctx, ControlAddr())
	if err != nil {
		return PortMapEntry{}, err
	}
	e, ok := snap.Find(peer, portName)
	if !ok {
		return PortMapEntry{}, &PortNotPublishedError{Peer: peer, Port: portName, Published: snap.Ports(peer)}
	}
	return e, nil
}

// NoSessionError says nothing is serving the control port, so no target can be
// resolved at all.
//
// This is the difference between "the target isn't offering that service" and
// "there is no session to ask", and it is worth distinguishing because the
// fixes are unrelated: one is a target-side state, the other means starting
// `nxdbg serve`.
type NoSessionError struct {
	Addr string
	Err  error
}

func (e *NoSessionError) Error() string {
	return fmt.Sprintf("no session on %s.\n"+
		"  Start one with:  nxdbg serve", e.Addr)
}

func (e *NoSessionError) Unwrap() error { return e.Err }

// PortNotPublishedError says the target is reachable but isn't listening on
// the requested port. That's a normal state - a target only publishes the
// services its running software has opened - so it carries the list of what
// is published to make the situation obvious.
type PortNotPublishedError struct {
	Peer      string
	Port      string
	Published []string
}

func (e *PortNotPublishedError) Error() string {
	who := e.Peer
	if who == "" {
		who = "any target"
	}
	return fmt.Sprintf("htc: %s is not published by %s (%d ports up: %v)", e.Port, who, len(e.Published), e.Published)
}
