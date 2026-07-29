// Command nxdbg is a CLI (and, run with no arguments, a TUI) for the
// Nintendo Switch devkit's own on-target protocol, driving the link directly
// over USB - an independent reimplementation with no other software
// required. All the real logic lives in internal/htc; this file and
// internal/tui are just argument parsing/output formatting and interactive
// presentation, so the library stays easy to script or wrap (e.g. from an
// MCP server) independent of either front end.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/plyrthn/nx-debug-cli/internal/commands"
	"github.com/plyrthn/nx-debug-cli/internal/config"
	"github.com/plyrthn/nx-debug-cli/internal/htc"
	"github.com/plyrthn/nx-debug-cli/internal/htclow"
	"github.com/plyrthn/nx-debug-cli/internal/remoteinput"
	"github.com/plyrthn/nx-debug-cli/internal/tui"
	"github.com/plyrthn/nx-debug-cli/internal/usbdev"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// cliEnv is what every command needs before it has parsed a single argument:
// the local settings.
type cliEnv struct {
	cfg config.Config
}

// topCommand is one top-level verb. As with the groups, only the name and the
// handler live here; the usage line and the argument count come from the
// catalog, so `nxdbg help` and the argument checking cannot disagree.
type topCommand struct {
	name string
	run  func(ctx context.Context, env cliEnv, args []string) error
}

// topLevel is every first word nxdbg accepts that is not a group. Keeping it
// as a table rather than as switch arms means the catalog can be checked
// against what is really dispatched, in both directions, by a test.
var topLevel = []topCommand{
	{"usb", func(ctx context.Context, env cliEnv, a []string) error { return cmdUSB(a[1:]) }},
	// serve and gdb run until interrupted, so neither takes the 30s command
	// timeout the rest are given.
	{"serve", func(ctx context.Context, env cliEnv, a []string) error { return cmdServe(a[1:]) }},
	{"gdb", func(ctx context.Context, env cliEnv, a []string) error {
		return cmdGdb(context.Background(), a[1], a[2:])
	}},
	// Installing an nsp is minutes of copying over the link, so like serve and
	// gdb it opts out of the 30s command timeout.
	{"install", func(ctx context.Context, env cliEnv, a []string) error {
		ictx, cancel := context.WithTimeout(context.Background(), htc.DevMenuTimeout)
		defer cancel()
		return cmdInstall(ictx, a[1], a[2:])
	}},
	{"uninstall", func(ctx context.Context, env cliEnv, a []string) error { return cmdUninstall(ctx, a[1], a[2:]) }},
	{"apps", func(ctx context.Context, env cliEnv, a []string) error { return cmdApps(ctx, a[1], a[2:]) }},
}

// groupDispatch routes a group's name to whatever handles that group. The
// four in dispatchGroups share one table-driven runner; the rest predate it
// and have their own argument layouts, which is why they are functions here
// rather than entries in a fifth table.
var groupDispatch = map[string]func(ctx context.Context, env cliEnv, args []string) error{
	"logging": runDispatchGroup,
	"lock":    runDispatchGroup,
	"shell": func(ctx context.Context, env cliEnv, a []string) error {
		if len(a) < 3 {
			return printGroupUsage("shell")
		}
		return runShell(ctx, a[1], a[2:])
	},
	"input": func(ctx context.Context, env cliEnv, a []string) error {
		if len(a) < 3 {
			return printGroupUsage("input")
		}
		return cmdInput(ctx, a[1], a[2:])
	},
	"htcs": func(ctx context.Context, env cliEnv, a []string) error {
		if len(a) < 2 {
			return printGroupUsage("htcs")
		}
		return cmdHtcs(ctx, a[1:])
	},
	"debug": func(ctx context.Context, env cliEnv, a []string) error {
		if len(a) < 3 {
			return printGroupUsage("debug")
		}
		return runDebug(ctx, a[1], a[2:])
	},
	"gdbstub": func(ctx context.Context, env cliEnv, a []string) error {
		if len(a) < 3 {
			return printGroupUsage("gdbstub")
		}
		return runGdbStub(ctx, a[1], a[2:])
	},
	"config": func(ctx context.Context, env cliEnv, a []string) error { return cmdConfig(env.cfg, a[1:]) },
	"dump": func(ctx context.Context, env cliEnv, a []string) error {
		if len(a) < 2 {
			return printGroupUsage("dump")
		}
		return cmdDump(a[1:])
	},
	"video": func(ctx context.Context, env cliEnv, a []string) error {
		// Bare `nxdbg video [serial]` opens the interactive window; the
		// named subcommands keep their existing one-shot behaviour.
		if len(a) < 2 || !isVideoSubcommand(a[1]) {
			return cmdRemoteView(ctx, a[1:])
		}
		return cmdVideo(ctx, a[1:])
	},
}

// runDispatchGroup is the shared runner for the groups that keep their
// subcommands in a table.
func runDispatchGroup(ctx context.Context, env cliEnv, args []string) error {
	g, ok := findGroup(args[0])
	if !ok {
		return fmt.Errorf("no dispatcher for group %s", args[0])
	}
	if len(args) < 2 {
		return printGroupUsage(g.name)
	}
	return g.run(ctx, args[1:])
}

// knownSubcommand reports whether a verb is one a group declares.
func knownSubcommand(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// unknownSubcommand names the group it was unknown in and lists what the
// group does take, since "unknown subcommand: on" from a mistyped group is
// otherwise baffling.
func unknownSubcommand(group, name string, names []string) error {
	return fmt.Errorf("unknown %s subcommand: %s (want: %s)", group, name, strings.Join(names, ", "))
}

// findTopCommand looks up a top-level verb.
func findTopCommand(name string) (topCommand, bool) {
	for _, c := range topLevel {
		if c.name == name {
			return c, true
		}
	}
	return topCommand{}, false
}

// topLevelNames is every command path the top level dispatches, which is the
// table plus any group name that is also a command in its own right - `video`
// being both the group and the window it opens.
func topLevelNames() []string {
	out := make([]string, 0, len(topLevel)+1)
	for _, c := range topLevel {
		out = append(out, c.name)
	}
	for name := range groupDispatch {
		if _, ok := commands.Find(name); ok {
			out = append(out, name)
		}
	}
	return out
}

func run(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	env := cliEnv{cfg: cfg}

	if len(args) == 0 {
		return tui.Run()
	}
	if cmd := args[0]; cmd == "-h" || cmd == "--help" || cmd == "help" {
		if len(args) > 1 {
			return printGroupUsage(args[1])
		}
		printUsage()
		return nil
	}
	args = expandAlias(args)

	// `shell devmenu` is a passthrough to whatever DevMenu command the
	// caller names, from a near-instant query to something that genuinely
	// takes the target a while to answer - get-memory-mode measured at
	// 90-120s end to end on real hardware, well past the default 30s. The
	// standard timeout is fine for everything else here; this one alone
	// needs the same kind of headroom `install` already gets.
	timeout := 30 * time.Second
	if len(args) > 2 && args[0] == "shell" && args[2] == "devmenu" {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if fn, ok := groupDispatch[args[0]]; ok {
		return fn(ctx, env, args)
	}
	cmd, ok := findTopCommand(args[0])
	if !ok {
		printUsage()
		return fmt.Errorf("unknown command: %s", args[0])
	}
	// The catalog says how many arguments the command needs and what its
	// usage line is, so a missing argument is reported in the same words the
	// help uses rather than in a second copy of them.
	if spec, ok := commands.Find(cmd.name); ok && len(args) < spec.MinArgs() {
		return fmt.Errorf("usage: %s", spec.Usage())
	}
	return cmd.run(ctx, env, args)
}

// cmdUSB inspects the devkit's USB interface with no daemon involved. This
// is the ground floor of getting rid of the daemon: the devkit is bound to
// WinUSB, so the endpoints it exposes are reachable from user space, and
// this reports exactly what there is to work with.
func cmdUSB(rest []string) error {
	paths, err := usbdev.FindPaths(usbdev.DevkitInterface)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no devkit USB interface present (looked for %x)", usbdev.DevkitInterface.Data1)
	}
	for _, path := range paths {
		fmt.Println(path)
	}
	if len(rest) > 0 && rest[0] == "paths" {
		return nil
	}

	dev, err := usbdev.Open(paths[0])
	if err != nil {
		return err
	}
	defer dev.Close()

	switch {
	case len(rest) > 0 && rest[0] == "listen":
		return usbListen(dev)
	case len(rest) > 0 && rest[0] == "handshake":
		return usbHandshake(dev, false)
	case len(rest) > 0 && rest[0] == "link":
		return usbHandshake(dev, true)
	case len(rest) > 0 && rest[0] == "reset":
		// Clear a link left wedged by an interrupted handshake. Abort first
		// to cancel anything in flight, then reset to clear the stall and
		// the data toggle, then tell the target the host is gone.
		for _, pipe := range []uint8{usbBulkIn, usbBulkOut} {
			if err := dev.AbortPipe(pipe); err != nil {
				fmt.Printf("  %v\n", err)
			}
			if err := dev.ResetPipe(pipe); err != nil {
				fmt.Printf("  %v\n", err)
			}
			fmt.Printf("  reset ep 0x%02x\n", pipe)
		}
		if err := dev.FlushPipe(usbBulkIn); err != nil {
			fmt.Printf("  %v\n", err)
		}
		// Bound the write too: on a wedged link this is the call that hangs.
		if err := dev.SetTimeout(usbBulkOut, 2000); err != nil {
			return err
		}
		pkt, err := htclow.CtrlPacket(htclow.DisconnectFromHost, 1, nil)
		if err != nil {
			return err
		}
		if _, err := dev.Write(usbBulkOut, pkt); err != nil {
			fmt.Printf("  sending %s: %v\n", htclow.DisconnectFromHost, err)
		} else {
			fmt.Printf("  -> %s\n", htclow.DisconnectFromHost)
		}
		return nil
	case len(rest) > 0 && rest[0] == "disconnect":
		// Send only, never read: this exists to clear a link left half-open
		// by an interrupted handshake, and a read would be the thing that
		// hangs in exactly that situation.
		pkt, err := htclow.CtrlPacket(htclow.DisconnectFromHost, 1, nil)
		if err != nil {
			return err
		}
		if _, err := dev.Write(usbBulkOut, pkt); err != nil {
			return err
		}
		fmt.Printf("\n-> %s\n", htclow.DisconnectFromHost)
		return nil
	}

	if speed, err := dev.Speed(); err == nil {
		fmt.Printf("\nspeed: %s\n", usbdev.SpeedName(speed))
	}
	// Alternate settings past the first are optional; stop at the first one
	// the device doesn't have rather than guessing how many there are.
	for alt := uint8(0); alt < 8; alt++ {
		class, sub, proto, count, err := dev.Interface(alt)
		if err != nil {
			break
		}
		fmt.Printf("\ninterface alt %d: class %02x subclass %02x protocol %02x, %d endpoints\n",
			alt, class, sub, proto, count)
		pipes, err := dev.Pipes(alt)
		if err != nil {
			return err
		}
		for _, p := range pipes {
			fmt.Printf("  %s\n", p)
		}
	}
	return nil
}

// usbBulkIn/usbBulkOut are the devkit's only two endpoints: one bulk pair on
// a vendor-specific interface, which is the shape nn::htclow's UsbInterface
// driver multiplexes every channel over.
const (
	usbBulkIn  = 0x81
	usbBulkOut = 0x01
)

// usbWritePacket puts one packet on the bulk OUT pipe as two transfers: the
// 32-byte header, then the body.
//
// The transfer boundary is part of the framing on USB, not an implementation
// detail. The target reads a header, then reads exactly BodySize more, so a
// single combined write overruns its first read and it stalls the endpoint -
// which is why every bare 32-byte packet was accepted and the first one
// carrying a body killed the link twice. The host's own driver has a
// splitHeader mode that does exactly this; USB is what it's for.
// pipeWriter is the one thing usbWritePacket needs from a device, which keeps
// the framing rule testable without a devkit plugged in.
type pipeWriter interface {
	Write(pipe uint8, b []byte) (int, error)
}

func usbWritePacket(dev pipeWriter, pkt []byte) error {
	if len(pkt) < htclow.HeaderSize {
		return fmt.Errorf("packet is %d bytes, short of a header", len(pkt))
	}
	if _, err := dev.Write(usbBulkOut, pkt[:htclow.HeaderSize]); err != nil {
		return err
	}
	if len(pkt) == htclow.HeaderSize {
		return nil
	}
	_, err := dev.Write(usbBulkOut, pkt[htclow.HeaderSize:])
	return err
}

// usbListen dumps whatever the target puts on the bulk IN pipe. It answers
// the first question any daemon-free transport has to settle: whether the
// target says anything on its own, or waits to be spoken to.
func usbListen(dev *usbdev.Device) error {
	if err := dev.SetRawIO(usbBulkIn, true); err != nil {
		return err
	}
	// Bounded, or an idle link just hangs with nothing to show for it.
	if err := dev.SetTimeout(usbBulkIn, 2000); err != nil {
		return err
	}

	fmt.Printf("\nreading ep 0x%02x for 5 attempts (2s each)\n", usbBulkIn)
	buf := make([]byte, 4096)
	for i := range 5 {
		n, err := dev.Read(usbBulkIn, buf)
		switch {
		case n > 0:
			fmt.Printf("  [%d] %d bytes\n", i, n)
			dumpHex(buf[:min(n, 128)])
		case err != nil:
			fmt.Printf("  [%d] %v\n", i, err)
		default:
			fmt.Printf("  [%d] nothing\n", i)
		}
	}
	return nil
}

// usbHandshake drives the link with no daemon involved. The target never
// speaks first, so until ConnectFromHost goes out the pipe stays silent no
// matter how long anything listens.
//
// With ready false it stops after the connect exchange, which is safe and
// reversible. With ready true it goes on to open the service channels and
// declare the host ready, which is what a real session needs - but the
// target drops the link permanently if it doesn't like any of it, and
// recovering that needs the USB cable physically replugged.
func usbHandshake(dev *usbdev.Device, ready bool) error {
	if err := dev.SetRawIO(usbBulkIn, true); err != nil {
		return err
	}
	if err := dev.SetTimeout(usbBulkIn, 3000); err != nil {
		return err
	}
	// Bound the write side too. The target stops draining the OUT pipe the
	// moment the link goes bad, and an unbounded write there blocks forever -
	// including the teardown below, which is exactly when it's least welcome.
	if err := dev.SetTimeout(usbBulkOut, 2000); err != nil {
		return err
	}

	seq := uint32(1)
	buf := make([]byte, 4096)

	send := func(t htclow.CtrlType, body []byte) error {
		pkt, err := htclow.CtrlPacket(t, seq, body)
		if err != nil {
			return err
		}
		seq++
		fmt.Printf("-> %s (%d bytes)\n", t, len(pkt))
		if err := usbWritePacket(dev, pkt); err != nil {
			return fmt.Errorf("sending %s: %w", t, err)
		}
		return nil
	}

	recv := func() (htclow.Header, []byte, error) {
		n, err := dev.Read(usbBulkIn, buf)
		if n == 0 {
			return htclow.Header{}, nil, fmt.Errorf("no reply: %w", err)
		}
		h, err := htclow.ParseHeader(buf[:n])
		if err != nil {
			return htclow.Header{}, nil, err
		}
		fmt.Printf("<- %s\n", h)
		// Trust the header's length over the read's: a bulk read can return
		// more than one packet's worth, and handing the tail to a parser as
		// though it were body is how a good packet reads as malformed.
		end := htclow.HeaderSize + int(h.BodySize)
		if end > n {
			end = n
		}
		return h, buf[htclow.HeaderSize:end], nil
	}

	if err := send(htclow.ConnectFromHost, nil); err != nil {
		return err
	}
	// However this ends, tell the target so it doesn't sit holding a
	// half-open link. Skipping this is what makes the next attempt come back
	// as DisconnectFromTarget instead of a fresh handshake.
	defer func() {
		pkt, err := htclow.CtrlPacket(htclow.DisconnectFromHost, seq, nil)
		if err == nil {
			usbWritePacket(dev, pkt)
			fmt.Printf("-> %s\n", htclow.DisconnectFromHost)
		}
	}()

	h, body, err := recv()
	if err != nil {
		return err
	}
	if !h.Ctrl() || htclow.CtrlType(h.Type) != htclow.ConnectFromTarget {
		return fmt.Errorf("expected %s, got %s", htclow.ConnectFromTarget, h.TypeName())
	}
	fmt.Printf("\ntarget says:\n%s\n", strings.TrimRight(string(body), "\x00\r\n"))

	if !ready {
		fmt.Println("\nconnected. stopping here: the ready exchange can take the link down")
		return nil
	}

	// Declare the host's channel set. Nothing goes on the mux side of the
	// wire before this: a channel isn't open until both sides have agreed it
	// exists, and sending flow control for one that hasn't been agreed stalls
	// the pipe outright.
	if err := send(htclow.ReadyFromHost, htclow.ReadyFromHostBody(htclow.ServiceChannels)); err != nil {
		return err
	}

	// The target can say other things first, so read a few rather than
	// insisting the very next packet is the answer.
	var agreed htclow.Ready
	for i := 0; ; i++ {
		if i == 4 {
			return fmt.Errorf("no %s after %d packets", htclow.ReadyFromTarget, i)
		}
		h, body, err = recv()
		if err != nil {
			return err
		}
		if h.Ctrl() && htclow.CtrlType(h.Type) == htclow.ReadyFromTarget {
			agreed, err = htclow.ParseReady(body)
			if err != nil {
				return err
			}
			break
		}
	}
	fmt.Printf("\ntarget supports mux v%d on %d channels:", agreed.Version, len(agreed.Channels))
	for _, ch := range agreed.Channels {
		fmt.Printf(" %s", ch)
	}
	fmt.Println()

	// Now the channels can be opened, and opening one means telling the
	// target how much this side can receive on it. Only the ones the target
	// listed: the rest it never agreed to.
	for _, ch := range htclow.ServiceChannels {
		if !agreed.Supports(ch) {
			fmt.Printf("   skipping %s, target didn't list it\n", ch)
			continue
		}
		pkt, err := htclow.MaxDataPacket(ch, htclow.MuxDefaultBody)
		if err != nil {
			return err
		}
		fmt.Printf("-> MaxData %s window=%d\n", ch, htclow.MuxDefaultBody)
		if err := usbWritePacket(dev, pkt); err != nil {
			return fmt.Errorf("opening channel %s: %w", ch, err)
		}
	}

	fmt.Println("\nlink established: connect, ready and channels open, no daemon involved")
	return nil
}

func dumpHex(b []byte) {
	for off := 0; off < len(b); off += 16 {
		end := min(off+16, len(b))
		row := b[off:end]
		ascii := make([]byte, len(row))
		for i, c := range row {
			if c >= 0x20 && c < 0x7f {
				ascii[i] = c
			} else {
				ascii[i] = '.'
			}
		}
		fmt.Printf("      %04x  %-32x  %s\n", off, row, ascii)
	}
}

// htcsSubcommands is what `nxdbg htcs ...` accepts, gating the switch below
// for the same reason as inputSubcommands.
var htcsSubcommands = []string{"ports", "services", "resolve"}

func cmdHtcs(ctx context.Context, rest []string) error {
	if !knownSubcommand(htcsSubcommands, rest[0]) {
		return unknownSubcommand("htcs", rest[0], htcsSubcommands)
	}

	switch rest[0] {
	case "ports":
		return cmdHtcsPorts(ctx, rest[1:])
	case "services":
		return cmdHtcsServices()
	case "resolve":
		if len(rest) < 3 {
			return fmt.Errorf("usage: nxdbg htcs resolve <peer> <service>")
		}
		entry, err := htc.ResolvePort(ctx, rest[1], rest[2])
		if err != nil {
			return err
		}
		fmt.Println(entry.Addr())
		return nil
	default:
		return fmt.Errorf("unknown htcs subcommand: %s", rest[0])
	}
}

// cmdHtcsPorts prints what each target is currently listening on, straight
// from the HTCS control port. Unlike `htcs list`, this is the target's own
// view rather than the host's registration table, so a port shown here is
// one something is actually behind.
func cmdHtcsPorts(ctx context.Context, rest []string) error {
	var peer string
	watch := false
	for _, a := range rest {
		switch a {
		case "--watch", "-w":
			watch = true
		default:
			peer = a
		}
	}

	if !watch {
		snap, err := htc.PortMap(ctx, htc.ControlAddr())
		if err != nil {
			return err
		}
		printPortMap(snap, peer)
		return nil
	}

	// Watching outlives the caller's 30s command timeout: the point is to
	// sit there until something changes.
	wctx, cancel := signal.NotifyContext(context.WithoutCancel(ctx), os.Interrupt)
	defer cancel()

	w, err := htc.WatchPortMap(wctx, htc.ControlAddr())
	if err != nil {
		return err
	}
	defer w.Close()

	for {
		snap, err := w.Next(wctx)
		if err != nil {
			if wctx.Err() != nil {
				return nil
			}
			return err
		}
		fmt.Printf("--- %s ---\n", time.Now().Format("15:04:05"))
		printPortMap(snap, peer)
	}
}

func printPortMap(snap *htc.PortMapSnapshot, peer string) {
	for _, t := range snap.Targets {
		if peer != "" && t.Peer != peer {
			continue
		}
		fmt.Printf("target %s (%s)\n", t.Peer, t.PeerType)
	}
	for _, e := range snap.Entries {
		if peer != "" && e.Peer != peer {
			continue
		}
		// An unrecognised port gets no label rather than a guessed one.
		label := "?"
		if s, ok := e.Service(); ok {
			label = s.Key
		}
		fmt.Printf("  %-27s %-16s %s\n", e.Port, label, e.Addr())
	}
}

func cmdHtcsServices() error {
	for _, s := range htc.Services() {
		fmt.Printf("%-16s %-27s %s\n", s.Key, s.Port, s.Desc)
	}
	return nil
}

// parseCoord parses a touch/pointer coordinate. They're int16 on the wire.
func parseCoord(s string) (int16, error) {
	v, err := strconv.ParseInt(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("bad coordinate %q: %w", s, err)
	}
	return int16(v), nil
}

// cmdInputStatus reports whether the target's raw HID channel is reachable.
// When input isn't working this is the difference between guessing and
// knowing whether the channel is even published.
func cmdInputStatus(ctx context.Context, serial string) error {
	fmt.Printf("target %s\n\n", serial)

	fmt.Println("raw HTCS channel:")
	entry, err := htc.ResolvePort(ctx, serial, "hid")
	switch {
	case err == nil:
		fmt.Printf("  ✓ %s published at %s\n", entry.Port, entry.Addr())
	default:
		fmt.Printf("  ✗ %v\n", err)
	}
	return nil
}

// tapDwell is how long "input tap" holds the contact down. The target
// samples touch state per frame, so the press has to survive at least a
// couple of frames to be seen at all. 100ms is comfortably above that and
// still fast enough not to feel like a long press.
const tapDwell = 100 * time.Millisecond

// inputQuiet and inputSettle bound the wait for a fresh HID session to finish
// starting up. The target enumerates its virtual pads over the first few
// milliseconds after the pong and ignores input sent during that window, so a
// one-shot command has to let it finish. The cap is what keeps a peer that
// chatters forever from hanging the command.
const (
	inputQuiet  = 250 * time.Millisecond
	inputSettle = 3 * time.Second
)

// inputWarmup is how long a HID session has to have existed before the target
// acts on anything sent over it.
//
// This is not politeness and it is not the enumeration above. Measured against
// hardware with the reference client: the same tap, on the same coordinates,
// does nothing when sent one second after the session opens and works when
// sent thirty seconds after. Nothing on the wire distinguishes the two - the
// target reads the chunks either way and silently drops the early ones.
//
// The reference client never trips over this because its session opens when the
// target connects and lives for the rest of the session, so by the time anyone
// presses anything the wait is long since paid. A command that dials per
// invocation pays it every time, which is why `serve` holds the session open
// instead.
const inputWarmup = 45 * time.Second

// inputSubcommands is what `nxdbg input <serial> ...` accepts. The switch
// below is too long to convert to a table of closures without obscuring what
// each verb does, so the accepted names are declared here and checked before
// dispatch instead: a verb added to the switch and not to this list is
// rejected outright rather than working in a way nothing describes, and a
// test holds the list against the catalog.
var inputSubcommands = []string{
	"status", "tap", "touch", "mouse", "key", "home",
	"raw-dump", "raw-tap", "raw-home", "raw-pad", "raw-probe",
}

func cmdInput(ctx context.Context, serial string, rest []string) error {
	if !knownSubcommand(inputSubcommands, rest[0]) {
		return unknownSubcommand("input", rest[0], inputSubcommands)
	}
	if rest[0] == "status" {
		return cmdInputStatus(ctx, serial)
	}

	l := &lazyInput{serial: serial, ctx: ctx}
	defer l.Close()

	switch sub := rest[0]; sub {
	case "tap":
		if len(rest) < 3 {
			return fmt.Errorf("usage: nxdbg input <serial> tap <x> <y>")
		}
		x, err := parseCoord(rest[1])
		if err != nil {
			return err
		}
		y, err := parseCoord(rest[2])
		if err != nil {
			return err
		}
		return l.with(func(in remoteinput.Sink) error {
			if err := in.TouchBegin(0, x, y); err != nil {
				return err
			}
			// Hold the contact. A down and an up in the same instant is
			// accepted by every layer and then dropped by the target - it
			// never samples a frame with the finger down - so the tap looks
			// like it worked and nothing happens.
			time.Sleep(tapDwell)
			return in.TouchEnd(0)
		})
	case "touch":
		if len(rest) < 3 {
			return fmt.Errorf("usage: nxdbg input <serial> touch <begin|move|end> <finger> [x] [y]")
		}
		finger, err := strconv.ParseUint(rest[2], 10, 8)
		if err != nil {
			return fmt.Errorf("bad finger ID %q: %w", rest[2], err)
		}
		if rest[1] == "end" {
			return l.with(func(in remoteinput.Sink) error { return in.TouchEnd(uint8(finger)) })
		}
		if len(rest) < 5 {
			return fmt.Errorf("usage: nxdbg input <serial> touch %s <finger> <x> <y>", rest[1])
		}
		x, err := parseCoord(rest[3])
		if err != nil {
			return err
		}
		y, err := parseCoord(rest[4])
		if err != nil {
			return err
		}
		switch rest[1] {
		case "begin":
			return l.with(func(in remoteinput.Sink) error { return in.TouchBegin(uint8(finger), x, y) })
		case "move":
			return l.with(func(in remoteinput.Sink) error { return in.TouchMove(uint8(finger), x, y) })
		default:
			return fmt.Errorf("unknown touch phase: %s", rest[1])
		}
	case "mouse":
		in, err := htc.DialRemoteInput(ctx, serial)
		if err != nil {
			return err
		}
		defer in.Close()
		if len(rest) < 3 {
			return fmt.Errorf("usage: nxdbg input <serial> mouse <move|button|wheel> ...")
		}
		switch rest[1] {
		case "move":
			if len(rest) < 4 {
				return fmt.Errorf("usage: nxdbg input <serial> mouse move <dx> <dy>")
			}
			dx, err := parseCoord(rest[2])
			if err != nil {
				return err
			}
			dy, err := parseCoord(rest[3])
			if err != nil {
				return err
			}
			return in.MouseMove(dx, dy)
		case "button":
			mask, err := strconv.ParseUint(rest[2], 0, 8)
			if err != nil {
				return fmt.Errorf("bad button mask %q: %w", rest[2], err)
			}
			return in.MouseButtons(htc.MouseButton(mask))
		case "wheel":
			delta, err := parseCoord(rest[2])
			if err != nil {
				return err
			}
			return in.MouseWheel(delta)
		default:
			return fmt.Errorf("unknown mouse action: %s", rest[1])
		}
	case "key":
		if len(rest) < 3 {
			return fmt.Errorf("usage: nxdbg input <serial> key <down|up> <usage-id>")
		}
		usage, err := strconv.ParseUint(rest[2], 0, 8)
		if err != nil {
			return fmt.Errorf("bad usage ID %q: %w", rest[2], err)
		}
		switch rest[1] {
		case "down":
			return l.with(func(in remoteinput.Sink) error { return in.KeyDown(uint8(usage), 0) })
		case "up":
			return l.with(func(in remoteinput.Sink) error { return in.KeyUp(uint8(usage), 0) })
		default:
			return fmt.Errorf("unknown key action: %s", rest[1])
		}
	case "home":
		if len(rest) < 2 {
			return fmt.Errorf("usage: nxdbg input <serial> home <down|up>")
		}
		pressed := rest[1] == "down"
		return l.with(func(in remoteinput.Sink) error { return in.HomeButton(pressed) })
	case "raw-dump":
		// Opens the raw HID channel and prints everything the target says.
		// The channel is bidirectional and the target talks first-class on
		// it, so watching is the only way to learn what it expects.
		seconds := 5
		if len(rest) > 1 {
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				return fmt.Errorf("invalid seconds %q: %w", rest[1], err)
			}
			seconds = n
		}
		entry, err := htc.ResolvePort(ctx, serial, "hid")
		if err != nil {
			return err
		}
		conn, err := htc.DialRemoteInputAddr(ctx, entry.Addr())
		if err != nil {
			return err
		}
		defer conn.Close()
		fmt.Printf("raw hid channel at %s, listening %ds\n", entry.Addr(), seconds)

		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		conn.SetReadDeadline(deadline)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				chunk, err := conn.ReadChunk()
				if err != nil {
					fmt.Printf("  (%v)\n", err)
					return
				}
				fmt.Printf("  <- %s\n", chunk)
			}
		}()
		// Ping partway through, so the reply (if any) is visible in context
		// rather than being consumed by a separate wait.
		time.Sleep(500 * time.Millisecond)
		fmt.Println("  -> Ping")
		if err := conn.SendPing(); err != nil {
			fmt.Printf("  ping failed: %v\n", err)
		}
		<-done
		return nil

	case "raw-home":
		// A HOME press is the one input whose effect is impossible to miss:
		// it replaces the whole screen. Everything smaller can land and still
		// look like nothing happened, which is exactly the ambiguity that
		// made the touch path hard to judge.
		entry, err := htc.ResolvePort(ctx, serial, "hid")
		if err != nil {
			return err
		}
		conn, err := htc.DialRemoteInputAddr(ctx, entry.Addr())
		if err != nil {
			return err
		}
		defer conn.Close()
		fmt.Printf("raw hid channel at %s\n", entry.Addr())
		if info, err := conn.Ping(3 * time.Second); err != nil {
			fmt.Printf("  no pong: %v\n", err)
		} else {
			fmt.Printf("  session: %s\n", info)
		}
		if err := conn.HomeButton(true); err != nil {
			return err
		}
		time.Sleep(tapDwell)
		if err := conn.HomeButton(false); err != nil {
			return err
		}
		fmt.Println("  HOME pressed and released")
		time.Sleep(500 * time.Millisecond)
		return nil

	case "raw-probe":
		// One long-lived session that taps the same point on a schedule, which
		// is the shape the reference client's session has and the shape a
		// per-command dial cannot reproduce. If the target has a settling
		// period before it acts on a session's input, this is what finds it:
		// the point is tapped repeatedly over minutes, so afterwards the
		// screen says whether any of them landed even though it can't say
		// which.
		if len(rest) < 3 {
			return fmt.Errorf("usage: nxdbg input <serial> raw-probe <x> <y> [minutes]")
		}
		x, err := parseCoord(rest[1])
		if err != nil {
			return err
		}
		y, err := parseCoord(rest[2])
		if err != nil {
			return err
		}
		minutes := 4
		if len(rest) > 3 {
			minutes, err = strconv.Atoi(rest[3])
			if err != nil {
				return fmt.Errorf("invalid minutes %q: %w", rest[3], err)
			}
		}
		entry, err := htc.ResolvePort(ctx, serial, "hid")
		if err != nil {
			return err
		}
		conn, err := htc.DialRemoteInputAddr(ctx, entry.Addr())
		if err != nil {
			return err
		}
		defer conn.Close()
		fmt.Printf("raw hid channel at %s\n", entry.Addr())
		if info, err := conn.Ping(3 * time.Second); err != nil {
			fmt.Printf("  no pong: %v\n", err)
		} else {
			fmt.Printf("  session: %s\n", info)
		}
		if err := conn.WaitSettled(inputQuiet, inputSettle); err != nil {
			fmt.Printf("  settle: %v\n", err)
		}
		stop := conn.KeepAlive(time.Second)
		defer stop()

		start := time.Now()
		deadline := start.Add(time.Duration(minutes) * time.Minute)
		for n := 1; time.Now().Before(deadline); n++ {
			if err := conn.TouchBegin(0, x, y); err != nil {
				return err
			}
			time.Sleep(tapDwell)
			if err := conn.TouchEnd(0); err != nil {
				return err
			}
			fmt.Printf("  tap %d at %v (session age %v)\n", n, time.Since(start).Round(time.Second), time.Since(start).Round(time.Second))
			time.Sleep(30 * time.Second)
		}
		fmt.Println("  probe done")
		return nil

	case "raw-pad":
		// The virtual controllers, over the raw channel. The target announces
		// these pads itself on connect, so unlike the touchscreen there is no
		// question of whether the device exists.
		if len(rest) < 2 {
			return fmt.Errorf("usage: nxdbg input <serial> raw-pad <button>[,<button>...] [pad-id]\n  buttons: %s", strings.Join(htc.PadButtonNames(), " "))
		}
		buttons, err := htc.ParsePadButtons(strings.Split(rest[1], ","))
		if err != nil {
			return err
		}
		var padID uint64
		if len(rest) > 2 {
			padID, err = strconv.ParseUint(rest[2], 10, 8)
			if err != nil {
				return fmt.Errorf("invalid pad id %q: %w", rest[2], err)
			}
		}
		entry, err := htc.ResolvePort(ctx, serial, "hid")
		if err != nil {
			return err
		}
		conn, err := htc.DialRemoteInputAddr(ctx, entry.Addr())
		if err != nil {
			return err
		}
		defer conn.Close()
		fmt.Printf("raw hid channel at %s\n", entry.Addr())
		if info, err := conn.Ping(3 * time.Second); err != nil {
			fmt.Printf("  no pong: %v\n", err)
		} else {
			fmt.Printf("  session: %s\n", info)
		}
		if err := conn.WaitSettled(inputQuiet, inputSettle); err != nil {
			fmt.Printf("  settle: %v\n", err)
		}
		hold := tapDwell
		if len(rest) > 3 {
			ms, err := strconv.Atoi(rest[3])
			if err != nil {
				return fmt.Errorf("invalid hold ms %q: %w", rest[3], err)
			}
			hold = time.Duration(ms) * time.Millisecond
		}
		// Keep the session alive the way the reference client does. A press
		// held across several seconds is worthless if the peer decides the
		// connection went away halfway through it.
		stop := conn.KeepAlive(time.Second)
		defer stop()

		warm := inputWarmup
		if len(rest) > 4 {
			s, err := strconv.Atoi(rest[4])
			if err != nil {
				return fmt.Errorf("invalid warmup seconds %q: %w", rest[4], err)
			}
			warm = time.Duration(s) * time.Second
		}
		if warm > 0 {
			fmt.Printf("  warming the session for %v\n", warm)
			time.Sleep(warm)
		}

		// Two virtual-pad protocols exist on the wire (see CLAUDE.md): HDLS,
		// which is what the target uses to announce its own already-paired
		// controllers, and the older AbstractedPad messages. DevMenu's own UI
		// navigation only reacts to AbstractedPad, hardware-confirmed by a
		// daemon-free "A" press launching a title from the Application list,
		// so that's the one raw-pad drives.
		if err := conn.PadConnect(uint8(padID), htc.PadProController, htc.PadInterfaceUSB); err != nil {
			return err
		}
		if err := conn.PadButtons(uint8(padID), buttons); err != nil {
			return err
		}
		time.Sleep(hold)
		if err := conn.PadButtons(uint8(padID), 0); err != nil {
			return err
		}
		fmt.Printf("  pad %d: %s held %v\n", padID, rest[1], hold)
		time.Sleep(500 * time.Millisecond)
		return nil

	case "raw-tap":
		// Forces the raw HID channel, skipping the input director. The two
		// routes carry different protocols on top of the same bytes, so when
		// one works and the other doesn't this is what tells them apart.
		if len(rest) < 3 {
			return fmt.Errorf("usage: nxdbg input <serial> raw-tap <x> <y>")
		}
		x, err := parseCoord(rest[1])
		if err != nil {
			return err
		}
		y, err := parseCoord(rest[2])
		if err != nil {
			return err
		}
		entry, err := htc.ResolvePort(ctx, serial, "hid")
		if err != nil {
			return err
		}
		conn, err := htc.DialRemoteInputAddr(ctx, entry.Addr())
		if err != nil {
			return err
		}
		defer conn.Close()
		fmt.Printf("raw hid channel at %s\n", entry.Addr())

		info, err := conn.Ping(3 * time.Second)
		if err != nil {
			fmt.Printf("  no pong: %v\n", err)
		} else {
			fmt.Printf("  session: %s\n", info)
		}
		if err := conn.WaitSettled(inputQuiet, inputSettle); err != nil {
			fmt.Printf("  settle: %v\n", err)
		}
		stop := conn.KeepAlive(time.Second)
		defer stop()

		warm := inputWarmup
		if len(rest) > 3 {
			s, err := strconv.Atoi(rest[3])
			if err != nil {
				return fmt.Errorf("invalid warmup seconds %q: %w", rest[3], err)
			}
			warm = time.Duration(s) * time.Second
		}
		if warm > 0 {
			fmt.Printf("  warming the session for %v\n", warm)
			time.Sleep(warm)
		}

		if err := conn.TouchBegin(0, x, y); err != nil {
			return err
		}
		time.Sleep(tapDwell)
		if err := conn.TouchEnd(0); err != nil {
			return err
		}
		// Hold the session open briefly. The real client never disconnects;
		// closing the moment the last chunk is written gives the peer a
		// chance to drop the whole session before it samples the input.
		time.Sleep(500 * time.Millisecond)
		return nil

	default:
		return fmt.Errorf("unknown input subcommand: %s", sub)
	}
}

// videoSubcommands are the one-shot/streaming `nxdbg video <sub>` verbs. A
// first argument that isn't one of these is treated as a target serial for
// the interactive window instead.
var videoSubcommands = map[string]bool{
	"dump": true, "dump-audio": true, "grab": true, "record": true,
	"raw": true, "raw-audio": true,
}

func isVideoSubcommand(s string) bool { return videoSubcommands[s] }

// lazyInput defers opening the raw HTCS route to the target's HID until
// input is actually sent, and keeps retrying. The target only publishes its
// HID channel while remote input is active on its side, so it can show up
// (or vanish) while the window is already open - dropping the video feed
// over that would be the wrong trade.
type lazyInput struct {
	serial string
	ctx    context.Context

	conn   *htc.RemoteInput
	warned bool
}

// with runs an input action against the raw channel, opening it on first use
// and reopening if it went away. No route yet is reported once and then
// quietly ignored.
func (l *lazyInput) with(fn func(remoteinput.Sink) error) error {
	if l.conn == nil {
		if err := l.open(); err != nil {
			if !l.warned {
				l.warned = true
				fmt.Fprintf(os.Stderr,
					"note: target isn't publishing its HID channel, input is inactive (video still works): %v\n", err)
			}
			return nil
		}
		l.warned = false
	}

	if err := fn(l.conn); err != nil {
		l.conn.Close()
		l.conn = nil
	}
	return nil
}

// open dials the raw HTCS channel.
func (l *lazyInput) open() error {
	conn, err := htc.DialRemoteInput(l.ctx, l.serial)
	if err != nil {
		return err
	}
	l.conn = conn
	return nil
}

func (l *lazyInput) Close() error {
	if l.conn != nil {
		return l.conn.Close()
	}
	return nil
}

func (l *lazyInput) TouchBegin(id uint8, x, y int16) error {
	return l.with(func(s remoteinput.Sink) error { return s.TouchBegin(id, x, y) })
}

func (l *lazyInput) TouchMove(id uint8, x, y int16) error {
	return l.with(func(s remoteinput.Sink) error { return s.TouchMove(id, x, y) })
}

func (l *lazyInput) TouchEnd(id uint8) error {
	return l.with(func(s remoteinput.Sink) error { return s.TouchEnd(id) })
}

func (l *lazyInput) HomeButton(pressed bool) error {
	return l.with(func(s remoteinput.Sink) error { return s.HomeButton(pressed) })
}

func (l *lazyInput) KeyDown(usageID, lockedKeys uint8) error {
	return l.with(func(s remoteinput.Sink) error { return s.KeyDown(usageID, lockedKeys) })
}

func (l *lazyInput) KeyUp(usageID, lockedKeys uint8) error {
	return l.with(func(s remoteinput.Sink) error { return s.KeyUp(usageID, lockedKeys) })
}

func (l *lazyInput) PadAttach(id uint8) error {
	return l.with(func(s remoteinput.Sink) error { return s.PadAttach(id) })
}

func (l *lazyInput) PadDisconnect(id uint8) error {
	return l.with(func(s remoteinput.Sink) error { return s.PadDisconnect(id) })
}

func (l *lazyInput) PadButtons(id uint8, buttons htc.PadButton) error {
	return l.with(func(s remoteinput.Sink) error { return s.PadButtons(id, buttons) })
}

func (l *lazyInput) PadStick(id uint8, side htc.PadStickSide, x, y int16) error {
	return l.with(func(s remoteinput.Sink) error { return s.PadStick(id, side, x, y) })
}

// cmdRemoteView opens the interactive remote-video window: the target's
// screen, with mouse clicks and drags forwarded back as touch input. With no
// handle it uses the daemon's default target.
//
// This is one command regardless of whether a daemon is running, and both
// cases now share the same frame source: a screenshot poll over the command
// shell, same as `nxdbg shell watch`. That used to be the daemon-free
// fallback only, with a daemon getting the "fast path" of polling its
// ScreenImage RPC instead - but ScreenImage decodes the target's H.264
// stream, which this devkit never sends an IDR on (see CLAUDE.md), and that
// turns out not to be just a slow drift: against a real running game it
// produced a solid black window throughout, while a screenshot taken at the
// same moment showed the game rendering correctly. A screenshot is a fresh,
// complete capture every time, so there's nothing for a decoder to get
// wrong. A probe RPC still decides whether a daemon is around, but now only
// to resolve the handle to a serial and to offer audio - runScreenshotWindow
// (shell.go) is what actually drives the picture either way.
// cmdRemoteView opens the interactive screen window, driven by shellWatch's
// screenshot polling. rest[0], if given, is a serial. With nothing given, it
// takes whichever single target is connected, the same way `nxdbg serve`
// itself needs no serial to find the one devkit that's plugged in.
func cmdRemoteView(ctx context.Context, rest []string) error {
	// The window runs until it's closed, not under the default command
	// timeout.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	serial := ""
	if len(rest) > 0 {
		serial = rest[0]
	}
	if serial == "" {
		snap, err := htc.PortMap(sigCtx, htc.ControlAddr())
		if err != nil {
			return err
		}
		switch len(snap.Targets) {
		case 0:
			return fmt.Errorf("no target connected (start `nxdbg serve` first)")
		case 1:
			serial = snap.Targets[0].Peer
		default:
			names := make([]string, len(snap.Targets))
			for i, t := range snap.Targets {
				names[i] = t.Peer
			}
			return fmt.Errorf("multiple targets connected, name one: nxdbg video <serial> (%s)", strings.Join(names, ", "))
		}
	}

	s, err := htc.DialCommandShell(sigCtx, serial)
	if err != nil {
		return err
	}
	defer s.Close()
	return shellWatch(sigCtx, s, nil)
}

func cmdVideo(ctx context.Context, rest []string) error {
	sub := rest[0]
	rest = rest[1:]
	if len(rest) < 1 {
		return fmt.Errorf("usage: nxdbg video %s <serial> ...", sub)
	}
	return cmdVideoStream(ctx, sub, rest[0], rest[1:])
}

// configSubcommands is what `nxdbg config ...` accepts. Bare `nxdbg config`
// means show.
var configSubcommands = []string{"show", "path"}

func cmdConfig(cfg config.Config, rest []string) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	sub := "show"
	if len(rest) > 0 {
		sub = rest[0]
	}
	if !knownSubcommand(configSubcommands, sub) {
		return unknownSubcommand("config", sub, configSubcommands)
	}
	switch sub {
	case "path":
		fmt.Println(path)
		return nil
	case "show":
		fmt.Printf("config file: %s\n", path)
		fmt.Printf("output_dir:  %s\n", valueOrUnset(cfg.OutputDir))
		return nil
	default:
		return fmt.Errorf("unknown config subcommand: %s (want: path, show)", sub)
	}
}

func valueOrUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func parseHandle(s string) (uint64, error) {
	h, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid target handle %q: %w", s, err)
	}
	return h, nil
}

func trimHexPrefix(s string) string {
	if len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X") {
		return s[2:]
	}
	return s
}
