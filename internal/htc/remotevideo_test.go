package htc

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/plyrthn/nx-debug-cli/internal/htcs"
)

// buildV2Frame encodes one v2-layout video data frame: the layout is tried
// first during detection and is fully self-describing, so a fake server can
// hand back exactly one frame without needing to fake a follow-up header for
// the chain check.
func buildV2Frame(timestampUs int64, payload []byte) []byte {
	h := make([]byte, v2HeaderSize)
	binary.LittleEndian.PutUint32(h[0:], uint32(VideoDataFrame))
	binary.LittleEndian.PutUint64(h[4:], uint64(timestampUs))
	binary.LittleEndian.PutUint32(h[12:], 0) // LackFrameCount
	binary.LittleEndian.PutUint32(h[16:], uint32(len(payload)))
	return append(h, payload...)
}

// startFakeVideoServer models the host side of a real session: one listener
// held open for the test, handing out one frame per accepted connection and
// then closing it, same as nxdbg serve's own stable per-port listener queues
// the target's next connection behind a single stable address. Each accept
// gets the next timestamp in order, so a test can tell which connection a
// session actually ended up reading from.
func startFakeVideoServer(t *testing.T, timestamps ...int64) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for _, ts := range timestamps {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Write(buildV2Frame(ts, []byte{0xAA, 0xBB, 0xCC, 0xDD}))
			conn.Close()
		}
	}()
	return ln
}

func listenerAddr(t *testing.T, ln net.Listener) string {
	t.Helper()
	return ln.Addr().String()
}

// useFakeControlServer points ControlAddr() at a control server publishing
// whatever ports returns, and cleans up both when the test ends.
func useFakeControlServer(t *testing.T, ports func() []htcs.Port) *htcs.ControlServer {
	t.Helper()
	srv, err := htcs.ListenControl("127.0.0.1:0", "SERIAL", ports)
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	_, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split %s: %v", srv.Addr(), err)
	}
	t.Setenv(ControlPortEnv, portStr)
	return srv
}

func TestDialMediaSessionConnectsThroughTheWatcher(t *testing.T) {
	ln := startFakeVideoServer(t, 1000)
	addr := listenerAddr(t, ln)
	useFakeControlServer(t, func() []htcs.Port {
		return []htcs.Port{{Name: VideoPortName, Addr: addr}}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := DialMediaSession(ctx, "SERIAL", VideoPortName)
	if err != nil {
		t.Fatalf("DialMediaSession: %v", err)
	}
	defer sess.Close()

	f, err := sess.NextFrame()
	if err != nil {
		t.Fatalf("NextFrame: %v", err)
	}
	if f.TimestampUs != 1000 {
		t.Errorf("timestamp = %d, want 1000 (frame from the published server)", f.TimestampUs)
	}
}

// This is the regression test for the actual bug: the first resolve went
// through a one-shot port map lookup (PortMap) that deliberately settles for
// several hundred milliseconds before answering, even though the entry was
// already published and sitting there. A session watching the control port
// instead should get it back the instant the subscription's first push
// arrives.
func TestDialMediaSessionResolvesWithoutWaitingOutTheSettleWindow(t *testing.T) {
	ln := startFakeVideoServer(t, 1000)
	addr := listenerAddr(t, ln)
	useFakeControlServer(t, func() []htcs.Port {
		return []htcs.Port{{Name: VideoPortName, Addr: addr}}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	sess, err := DialMediaSession(ctx, "SERIAL", VideoPortName)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DialMediaSession: %v", err)
	}
	defer sess.Close()

	// PortMap's settle window is 400ms; the entry was already published
	// before the dial even started. Anything near that means resolving fell
	// back to the settle-and-wait path instead of the live subscription.
	if elapsed > 200*time.Millisecond {
		t.Errorf("resolve took %v for an already-published entry, want well under the 400ms settle window", elapsed)
	}
}

// The host side of a stream's address does not move when the target drops
// it (nxdbg serve holds one listener open per port for the session and
// queues the target's next connection behind it), so a reconnect should
// just redial the known address immediately rather than going back through
// the port map at all.
func TestMediaSessionRedialsTheSameAddressOnDrop(t *testing.T) {
	ln := startFakeVideoServer(t, 1000, 2000)
	addr := listenerAddr(t, ln)
	useFakeControlServer(t, func() []htcs.Port {
		return []htcs.Port{{Name: VideoPortName, Addr: addr}}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := DialMediaSession(ctx, "SERIAL", VideoPortName)
	if err != nil {
		t.Fatalf("DialMediaSession: %v", err)
	}
	defer sess.Close()

	if _, err := sess.NextFrame(); err != nil {
		t.Fatalf("first NextFrame: %v", err)
	}

	start := time.Now()
	f, err := sess.NextFrame()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("reconnecting NextFrame: %v", err)
	}
	if f.TimestampUs != 2000 {
		t.Errorf("timestamp = %d, want 2000 (frame from the second connection)", f.TimestampUs)
	}
	if sess.Reconnects != 1 {
		t.Errorf("Reconnects = %d, want 1", sess.Reconnects)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("reconnect took %v, want a plain redial of the known address, not a port map round trip", elapsed)
	}
}
