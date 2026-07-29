package htc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// DevMenuLogFanoutPortName is a synthetic port name - not a real target
// service - that `nxdbg serve` publishes so every devmenu command in a
// session can share one connection to the target's log, instead of each one
// dialing (and dropping) its own.
//
// iywys@$LogManager's real on-device implementation only ever accepts one
// client connection for its whole lifetime, confirmed empirically: once that
// client goes away, nothing on the target side goes back to listening for a
// new one until the target reboots. Each devmenu invocation used to dial
// LogManager fresh and drop the connection when it was done, which meant the
// target's one accept slot was already spent by the second devmenu command
// run against a live serve session, and every one after that hung until a
// `nxdbg shell <serial> reboot`. This fanout dials LogManager exactly once,
// for the life of `nxdbg serve`, and republishes the decoded devmenu output
// on a local port any number of short-lived CLI processes can freely connect
// to and disconnect from, since only the fanout's own local listener sees
// those, never the target.
const DevMenuLogFanoutPortName = "nxdbg@$devmenu-log"

// LogFanout owns the one connection to the target's devmenu log and
// republishes it to any number of local subscribers.
type LogFanout struct {
	// Log receives non-fatal trouble, in particular Run ending. nil discards.
	Log func(string)

	mu   sync.Mutex
	subs map[chan string]struct{}
}

func NewLogFanout() *LogFanout {
	return &LogFanout{subs: map[chan string]struct{}{}}
}

func (f *LogFanout) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log(fmt.Sprintf(format, args...))
	}
}

// Run dials the target's devmenu log, retrying while the target hasn't
// published it yet (normal during boot), and forwards every line to every
// subscriber until the connection ends or ctx is done. Every subscriber is
// closed on the way out, so a caller blocked reading finds out immediately
// rather than idling until its own timeout with no way to tell why.
//
// It does not retry once a connection has been established and then broken:
// that means the target's one accept slot is gone until reboot, and
// reconnecting would just hang the same way the old per-command dial did.
// The returned error says so, once, instead of every later devmenu command
// failing silently with its own timeout.
func (f *LogFanout) Run(ctx context.Context, serial string) error {
	err := f.run(ctx, serial)
	if err != nil {
		f.logf("devmenu log fanout stopped: %v", err)
	}
	f.closeAll()
	return err
}

func (f *LogFanout) run(ctx context.Context, serial string) error {
	var lines <-chan string
	var closeLog func()
	for {
		l, c, err := dialDirectDevMenuLog(ctx, serial)
		if err == nil {
			lines, closeLog = l, c
			break
		}
		if _, ok := err.(*PortNotPublishedError); !ok {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	defer closeLog()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return errors.New("the target log closed; its single accept " +
					"slot is spent until the next target reboot")
			}
			f.broadcast(line)
		}
	}
}

func (f *LogFanout) broadcast(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- line:
		default:
			// A subscriber that isn't keeping up drops lines rather than
			// stalling every other subscriber behind it.
		}
	}
}

// closeAll closes every current subscriber channel, so a reader blocked on
// one learns the fanout is done rather than idling until its own timeout.
func (f *LogFanout) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		close(ch)
	}
	f.subs = map[chan string]struct{}{}
}

func (f *LogFanout) subscribe() (chan string, func()) {
	ch := make(chan string, 256)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs, ch)
		f.mu.Unlock()
	}
}

// ListenAndServe binds a local port and streams every subscriber a live tail
// of devmenu output, one line at a time, plain text - the same shape
// readLogLines already expects, so openDevMenuLog's reader needs no changes
// to consume it.
func (f *LogFanout) ListenAndServe(host string) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, fmt.Errorf("devmenu log fanout: %w", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serveConn(conn)
		}
	}()
	return ln, nil
}

func (f *LogFanout) serveConn(conn net.Conn) {
	defer conn.Close()
	ch, cancel := f.subscribe()
	defer cancel()
	// A dead client is only noticed on write, so watch for it hanging up
	// rather than leaking this goroutine per disconnected reader.
	gone := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		conn.Read(buf)
		close(gone)
	}()
	for {
		select {
		case <-gone:
			return
		case line, ok := <-ch:
			if !ok {
				// Run ended and closed every subscriber; nothing more is
				// coming, so there is nothing left to do but hang up.
				return
			}
			if _, err := conn.Write([]byte(line + "\n")); err != nil {
				return
			}
		}
	}
}
