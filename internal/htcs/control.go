package htcs

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"net"
	"strings"
	"sync"
)

// ControlPort is where the daemon publishes its port mapping, and therefore
// where everything that resolves a service by name looks. Serving it is what
// makes the existing clients work against this process without knowing
// anything has changed.
const ControlPort = 20184

// controlRequest is the one thing a subscriber sends. Anything else is
// ignored rather than guessed at.
const controlRequest = "<RequestSystemPortMapping"

// ControlServer publishes a port map over the daemon's control protocol: a
// subscriber connects, asks once, and is then pushed a document whenever the
// mapping changes.
type ControlServer struct {
	// Peer is the target's HTCS peer name, which is its serial. Clients
	// resolve by peer as well as port, so getting this right is what makes a
	// lookup match.
	Peer     string
	PeerType string
	ID       string

	// Ports supplies the current mapping. It's a function rather than a
	// snapshot so the server always publishes what is true now.
	Ports func() []Port

	// Log receives non-fatal trouble. nil discards.
	Log func(string)

	ln net.Listener

	mu      sync.Mutex
	clients map[chan struct{}]struct{}
	closed  bool
}

// ListenControl starts the control server on the given address. Pass an empty
// address for the daemon's own port on loopback.
func ListenControl(addr string, peer string, ports func() []Port) (*ControlServer, error) {
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", ControlPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("htcs: control port %s: %w", addr, err)
	}
	c := &ControlServer{
		Peer:     peer,
		PeerType: "NX",
		ID:       peer,
		Ports:    ports,
		ln:       ln,
		clients:  map[chan struct{}]struct{}{},
	}
	go c.accept()
	return c, nil
}

// Addr is where the server is listening.
func (c *ControlServer) Addr() string { return c.ln.Addr().String() }

func (c *ControlServer) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

// Changed tells every subscriber to re-read the mapping. Callers use it when
// a service comes up or goes away.
func (c *ControlServer) Changed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ch := range c.clients {
		// Non-blocking: a subscriber that hasn't drained its last wakeup will
		// pick up the current state anyway, since the snapshot is built at
		// send time rather than queued.
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (c *ControlServer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.ln.Close()
}

func (c *ControlServer) accept() {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}
		go c.serve(conn)
	}
}

func (c *ControlServer) serve(conn net.Conn) {
	defer conn.Close()

	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		return
	}
	if !strings.Contains(line, controlRequest) {
		c.logf("control port: unrecognised request %q", strings.TrimSpace(line))
		return
	}

	wake := make(chan struct{}, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.clients[wake] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.clients, wake)
		c.mu.Unlock()
	}()

	// The subscriber blocks until the first document arrives, so one goes out
	// immediately whether or not anything has been published yet.
	if err := c.push(conn); err != nil {
		return
	}
	// A closed connection is only noticed on write, so watch for the peer
	// hanging up rather than leaking a goroutine per dead subscriber.
	gone := make(chan struct{})
	go func() {
		r.ReadString('\n')
		close(gone)
	}()
	for {
		select {
		case <-gone:
			return
		case <-wake:
			if err := c.push(conn); err != nil {
				return
			}
		}
	}
}

func (c *ControlServer) push(conn net.Conn) error {
	doc, err := c.document()
	if err != nil {
		return err
	}
	_, err = conn.Write(append(doc, '\n'))
	return err
}

// htcsInfoDoc mirrors the document the daemon emits. The element names are
// contract: a client resolving a service matches on them exactly.
type htcsInfoDoc struct {
	XMLName    xml.Name `xml:"HtcsInfo"`
	TargetList struct {
		Targets []targetDoc `xml:"Target"`
	} `xml:"TargetList"`
	PortMap struct {
		Items []portMapItemDoc `xml:"PortMapItem"`
	} `xml:"PortMap"`
}

type targetDoc struct {
	PeerType string `xml:"PeerType"`
	PeerName string `xml:"HtcsPeerName"`
	ID       string `xml:"Id"`
}

type portMapItemDoc struct {
	PeerName  string `xml:"HtcsPeerName"`
	PortName  string `xml:"HtcsPortName"`
	IPAddress string `xml:"IPAddress"`
	TCPPort   int    `xml:"TcpPortNumber"`
}

// document builds one snapshot. It must be a single line: the protocol is
// newline-delimited, so an indented document would be read as several
// truncated ones.
func (c *ControlServer) document() ([]byte, error) {
	var doc htcsInfoDoc
	doc.TargetList.Targets = append(doc.TargetList.Targets, targetDoc{
		PeerType: c.PeerType,
		PeerName: c.Peer,
		ID:       c.ID,
	})
	for _, p := range c.Ports() {
		host, port, err := splitHostPort(p.Addr)
		if err != nil {
			c.logf("control port: skipping %s, %v", p.Name, err)
			continue
		}
		doc.PortMap.Items = append(doc.PortMap.Items, portMapItemDoc{
			PeerName:  c.Peer,
			PortName:  p.Name,
			IPAddress: host,
			TCPPort:   port,
		})
	}
	return xml.Marshal(doc)
}

func splitHostPort(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return "", 0, fmt.Errorf("port %q is not a number", portStr)
	}
	return host, port, nil
}
