package htc_test

import (
	"context"
	"testing"
	"time"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
	"github.com/plyrthn/nx-debug-cli/internal/htcs"
)

// The point of serving the daemon's control protocol is that nothing else has
// to change: the real client has to resolve a service against our server
// exactly as it does against the daemon. Testing the two halves separately
// would miss a mismatch in the document, which is the part that actually
// carries the contract.
func TestPortMapClientReadsOurControlServer(t *testing.T) {
	ports := []htcs.Port{
		{Name: "iywys@$hid", Addr: "127.0.0.1:50894"},
		{Name: "iywys@$remoteVideo", Addr: "127.0.0.1:50908"},
	}
	srv, err := htcs.ListenControl("127.0.0.1:0", "SERIAL", func() []htcs.Port { return ports })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	snap, err := htc.PortMap(ctx, srv.Addr())
	if err != nil {
		t.Fatal(err)
	}

	if len(snap.Targets) != 1 || snap.Targets[0].Peer != "SERIAL" {
		t.Fatalf("targets = %+v, want the one peer", snap.Targets)
	}
	entry, ok := snap.Find("SERIAL", "iywys@$hid")
	if !ok {
		t.Fatal("client could not find iywys@$hid in our document")
	}
	if entry.Addr() != "127.0.0.1:50894" {
		t.Errorf("resolved to %s, want 127.0.0.1:50894", entry.Addr())
	}

	// A peer filter that doesn't match must not resolve, or a two-devkit
	// setup would silently hand out the wrong target's port.
	if _, ok := snap.Find("OTHER", "iywys@$hid"); ok {
		t.Error("resolved a port for the wrong peer")
	}
	// And a service nobody published must report as absent rather than
	// falling back to a near match.
	if _, ok := snap.Find("SERIAL", "iywys@$notPublished"); ok {
		t.Error("resolved a service that was never published")
	}
}

// A subscriber has to be pushed a fresh document when a service appears,
// since that's how a client notices a channel coming up mid-session.
func TestControlServerPushesOnChange(t *testing.T) {
	published := []htcs.Port{}
	srv, err := htcs.ListenControl("127.0.0.1:0", "SERIAL", func() []htcs.Port { return published })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	w, err := htc.WatchPortMap(ctx, srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	first, err := w.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 0 {
		t.Errorf("first snapshot had %d entries, want none", len(first.Entries))
	}

	published = append(published, htcs.Port{Name: "iywys@$hid", Addr: "127.0.0.1:50894"})
	srv.Changed()

	next, err := w.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next.Find("SERIAL", "iywys@$hid"); !ok {
		t.Error("the pushed snapshot did not carry the new service")
	}
}
