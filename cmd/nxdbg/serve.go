package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
	"github.com/plyrthn/nx-debug-cli/internal/htcfs"
	"github.com/plyrthn/nx-debug-cli/internal/htclow"
	"github.com/plyrthn/nx-debug-cli/internal/htcmisc"
	"github.com/plyrthn/nx-debug-cli/internal/htcs"
)

// htcsChannel is the mux channel HTCS runs on. The daemon opens module 4 and
// channel 0 within it, which is the 4:0:0 in the ready handshake.
var htcsChannel = htclow.Channel{Module: 4, ID: 0}

// cmdServe is the daemon-free session: bring the htclow link up over USB,
// then answer the target's socket calls ourselves. Every port the target
// publishes becomes a local TCP listener, which is the same thing the daemon
// does and the reason the existing service clients need no changes.
//
// The daemon must not be running. Two hosts driving one link would interleave
// packets on the same channels and neither would make sense of the result.
func cmdServe(rest []string) error {
	verbose, trace, readOnly := false, false, false
	root := ""
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "-v", "--verbose":
			verbose = true
		case "-t", "--trace":
			verbose, trace = true, true
		case "--read-only":
			readOnly = true
		case "--root":
			if i+1 >= len(rest) {
				return errors.New("--root needs a directory")
			}
			i++
			root = rest[i]
		}
	}

	link, err := openLink()
	if err != nil {
		return err
	}
	defer link.Close()

	fmt.Printf("%s %s\n", link.Info.HW, link.Info.FW)
	fmt.Printf("serial %s, %s, mux v%d\n", link.Info.SN, link.Info.Conn, link.Info.Prot)
	chans := make([]string, 0, 4)
	for _, ch := range link.Channels() {
		chans = append(chans, ch.String())
	}
	fmt.Printf("channels %s\n\n", strings.Join(chans, " "))

	stream, ok := link.Stream(htcsChannel)
	if !ok {
		return fmt.Errorf("target did not agree to channel %s, so htcs has nowhere to run", htcsChannel)
	}

	// Timestamps matter more than they look: most of what goes wrong in this
	// protocol is a call answered too late rather than answered wrongly, and
	// without a clock on each line an ordering problem reads like a logic one.
	stamp := func(prefix string) func(string) {
		start := time.Now()
		return func(msg string) {
			fmt.Printf("%8.3f %s%s\n", time.Since(start).Seconds(), prefix, msg)
		}
	}

	// HTCMISC is the target's control channel, and answering it is part of
	// being a host rather than an optional extra. The reference daemon brings
	// it up alongside HTCS on every connect, and a target whose HTCMISC goes
	// unanswered behaves like one that never finished attaching.
	if err := serveHtcmisc(link, stamp, verbose, trace); err != nil {
		fmt.Printf("  htcmisc: %v\n", err)
	}

	// HTCFS is the third service a target manager brings up on connect. The
	// target asks for the protocol version the moment the link is live - that
	// is the 64 bytes that used to be dropped on channel 1:0:0 - and a program
	// running on it reaches the host's filesystem through here.
	if err := serveHtcfs(link, stamp, root, readOnly, verbose, trace); err != nil {
		fmt.Printf("  htcfs: %v\n", err)
	}

	// Drain whatever is left. Nothing here consumes those channels, but an
	// unread channel is not free: its receive window fills, the credit stops
	// being renewed, and the sender on the far side blocks. That stall lands on
	// whatever target thread was writing, which is not necessarily the service
	// that owns the channel.
	for _, ch := range link.Channels() {
		if ch == htcsChannel || ch.Module == htcmisc.Module || ch.Module == htcfs.Module {
			continue
		}
		other, ok := link.Stream(ch)
		if !ok {
			continue
		}
		go drainChannel(ch, other, verbose)
	}

	srv := htcs.NewServer(stream)
	if verbose {
		srv.Log = stamp("  ")
	}
	if trace {
		srv.Trace = stamp("   ")
	}

	// The devmenu log fanout gets its own local listener before the control
	// port opens, so its address can be folded into the same port map from
	// the start rather than announced separately later.
	fanout := htc.NewLogFanout()
	fanout.Log = func(msg string) { fmt.Println("  devmenu log fanout: " + msg) }
	fanoutLn, fanoutErr := fanout.ListenAndServe("127.0.0.1")
	if fanoutErr != nil {
		fmt.Printf("  devmenu log fanout: %v\n", fanoutErr)
	} else {
		defer fanoutLn.Close()
	}
	ports := srv.Ports
	if fanoutLn != nil {
		fanoutAddr := fanoutLn.Addr().String()
		ports = func() []htcs.Port {
			return append(srv.Ports(), htcs.Port{Name: htc.DevMenuLogFanoutPortName, Addr: fanoutAddr})
		}
	}

	// Serve the daemon's own control port. Everything that resolves a service
	// by name reads its mapping from there, so publishing it is what lets the
	// existing clients work against this process unchanged.
	control, err := htcs.ListenControl("", link.Info.SN, ports)
	if err != nil {
		return fmt.Errorf("%w (is the daemon still running?)", err)
	}
	defer control.Close()

	if fanoutLn != nil {
		fanoutCtx, cancelFanout := context.WithCancel(context.Background())
		defer cancelFanout()
		go fanout.Run(fanoutCtx, link.Info.SN)
	}
	if verbose {
		control.Log = func(msg string) { fmt.Println("  " + msg) }
	}

	srv.OnPort = func(p htcs.Port) {
		fmt.Printf("✓ %-24s %s\n", displayPort(p.Name), p.Addr)
		control.Changed()
	}

	go srv.Serve()

	fmt.Printf("htcs control port on %s\n", control.Addr())
	fmt.Println("waiting for the target to publish its services (ctrl-c to stop)")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	// A settled port list is worth printing once: it's the thing that says
	// whether this is a usable session or just a link.
	settle := time.NewTimer(5 * time.Second)
	defer settle.Stop()

	for {
		select {
		case <-sig:
			fmt.Println("\nstopping")
			srv.Close()
			return nil
		case <-settle.C:
			printPortSummary(srv)
		case <-srv.Done():
			if err := srv.Err(); err != nil {
				return fmt.Errorf("htcs stopped: %w", err)
			}
			return nil
		case <-link.Done():
			if err := link.Err(); err != nil {
				return fmt.Errorf("link stopped: %w", err)
			}
			return nil
		}
	}
}

// serveHtcmisc brings up both halves of the target's control channel: the
// server that answers its requests, and the client that agrees a protocol
// version with it.
//
// Neither half is optional. The target asks the host for environment variables
// and the time and reports its own status; leaving those unanswered is what a
// half-attached host looks like from the target's side.
func serveHtcmisc(link *htclow.Link, stamp func(string) func(string), verbose, trace bool) error {
	serverCh := htclow.Channel{Module: htcmisc.Module, ID: htcmisc.ServerChannel}
	clientCh := htclow.Channel{Module: htcmisc.Module, ID: htcmisc.ClientChannel}

	sstream, ok := link.Stream(serverCh)
	if !ok {
		return fmt.Errorf("target did not agree to channel %s", serverCh)
	}
	misc := htcmisc.NewServer(sstream)
	if verbose {
		misc.Log = stamp("  misc ")
	}
	if trace {
		misc.Trace = stamp("   misc ")
	}
	misc.Status = func(status int64) {
		fmt.Printf("target status %d\n", status)
	}
	go misc.Serve()

	cstream, ok := link.Stream(clientCh)
	if !ok {
		return fmt.Errorf("target did not agree to channel %s", clientCh)
	}
	// The client channel carries only SetTargetName, so it stays open and
	// silent. It still has to be drained: a channel the target agreed to and
	// nobody reads fills its receive window and stalls the target thread
	// behind it.
	go drainChannel(clientCh, cstream, verbose)
	return nil
}

// openLink brings the USB link up, resetting and retrying once if the first
// attempt finds it half-open.
//
// That state is normal rather than exceptional: a previous session that ended
// without a clean disconnect - a crash, a ctrl-c at the wrong moment, the
// devkit rebooting out from under it - leaves the target believing a host is
// still attached, and the next handshake gets a disconnect instead of a fresh
// start. Resetting fixes it, and doing that here means nobody has to know to
// run `nxdbg usb reset` first.
func openLink() (*htclow.Link, error) {
	t, err := htclow.OpenUSB()
	if err != nil {
		return nil, err
	}
	link, err := htclow.Dial(t)
	if err == nil {
		return link, nil
	}
	first := err

	fmt.Printf("the link was left half-open (%v)\nresetting and retrying\n", first)
	t.Reset()
	t.Close()

	// The device re-enumerates after a reset, so reopening immediately can
	// find it still gone.
	time.Sleep(time.Second)

	t, err = htclow.OpenUSB()
	if err != nil {
		return nil, fmt.Errorf("%w (after resetting to recover from: %v)", err, first)
	}
	link, err = htclow.Dial(t)
	if err != nil {
		t.Reset()
		t.Close()
		return nil, fmt.Errorf("%w (still failing after a reset; try unplugging the devkit, "+
			"and check no other process has it: the daemon, another nxdbg serve)", err)
	}
	fmt.Println("recovered")
	return link, nil
}

// serveHtcfs answers the target's filesystem requests.
//
// root bounds what the target can reach; empty means the working directory,
// which is where the reference host lands when a target has no working
// directory configured. Nothing outside it is reachable, deliberately: the
// reference resolves paths straight onto the host filesystem, and a devkit
// with read and write access to the whole machine is more authority than this
// needs to hand out by default.
func serveHtcfs(link *htclow.Link, stamp func(string) func(string), root string, readOnly, verbose, trace bool) error {
	ch := htclow.Channel{Module: htcfs.Module, ID: htcfs.Channel}
	stream, ok := link.Stream(ch)
	if !ok {
		return fmt.Errorf("target did not agree to channel %s", ch)
	}
	srv := htcfs.NewServer(stream)
	srv.Bulk = linkBulkChannel{link}
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", abs)
		}
		srv.Root = abs
	}
	srv.ReadOnly = readOnly
	if verbose {
		srv.Log = stamp("  fs ")
	}
	if trace {
		srv.Trace = stamp("   fs ")
	}
	access := "read/write"
	if readOnly {
		access = "read-only"
	}
	fmt.Printf("target filesystem rooted at %s (%s)\n", srv.Root, access)
	go srv.Serve()
	return nil
}

// linkBulkChannel adapts a htclow.Link to htcfs.BulkChannel, so htcfs never
// has to import htclow just to open the second channel a large transfer
// needs - the channel id is a number either way, and htclow already knows
// how to raise one on demand.
type linkBulkChannel struct{ link *htclow.Link }

func (b linkBulkChannel) OpenBulkChannel(id uint16) (htcfs.BulkStream, error) {
	s, err := b.link.OpenChannel(htclow.Channel{Module: htcfs.Module, ID: id}, htclow.DefaultReceiveBuffer)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// drainChannel keeps a channel this build has no use for from backing up.
// What arrives is reported under -v rather than silently dropped: a channel
// that turns out to carry real traffic is a service worth implementing, and
// discarding it quietly would hide that.
func drainChannel(ch htclow.Channel, s *htclow.Stream, verbose bool) {
	buf := make([]byte, 4096)
	total := 0
	for {
		n, err := s.Read(buf)
		if n > 0 {
			total += n
			if verbose {
				fmt.Printf("  %s: dropped %d bytes (%d total)\n", ch, n, total)
			}
		}
		if err != nil {
			if verbose {
				fmt.Printf("  %s: %v\n", ch, err)
			}
			return
		}
	}
}

// printPortSummary lists every service this build knows about and whether the
// target published it. The absent ones matter as much as the present: a
// missing channel is the usual reason something doesn't respond.
func printPortSummary(srv *htcs.Server) {
	up := map[string]string{}
	for _, p := range srv.Ports() {
		up[p.Name] = p.Addr
	}
	fmt.Printf("\n%d services published\n", len(up))
	for _, s := range htc.Services() {
		if addr, ok := up[s.Port]; ok {
			fmt.Printf("  ✓ %-16s %-24s %s\n", s.Key, s.Port, addr)
			delete(up, s.Port)
		} else {
			fmt.Printf("  ✗ %-16s %-24s %s\n", s.Key, s.Port, "not published")
		}
	}
	rest := make([]string, 0, len(up))
	for name := range up {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		fmt.Printf("  ✓ %-16s %-24s %s\n", "(unknown)", name, up[name])
	}
	fmt.Println()

	// The gdb stub is the one service most people will actually want, and
	// nothing else in this output says so. It is listed above as a port
	// number like any other, which is exactly how it gets missed.
	if _, ok := srv.Addr(gdbPort); ok {
		fmt.Println("  To attach gdb, LLDB, VS Code, IDA or Ghidra to this target, run:")
		fmt.Println("      nxdbg gdb <serial>")
		fmt.Println()
	}
}

// gdbPort is the target's gdb stub, called out separately in the summary.
const gdbPort = "iywys@$gdb"

// displayPort keeps the raw name, since that's what the target bound and what
// a lookup has to match.
func displayPort(name string) string {
	if name == "" {
		return "(unnamed)"
	}
	return name
}
