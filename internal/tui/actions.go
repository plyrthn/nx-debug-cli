package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// Action is one thing the user can do to the selected target. The key
// bindings, the help line and the dispatch all read this table, so adding an
// action is one entry here and nothing else - there's no second place that
// has to be remembered.
type Action struct {
	// Key is the keystroke that runs it.
	Key string
	// Label is the short name shown in the help line.
	Label string
	// Busy is the status text shown while it's running. Empty means the
	// action is instant and needs no placeholder.
	Busy string
	// NeedsTarget gates the action on a target being selected.
	NeedsTarget bool
	// Command is the catalog path this hotkey is a shortcut for, or empty
	// for the few that have no CLI equivalent.
	Command string
	// Run builds the command. target is the zero value when NeedsTarget is
	// false.
	Run func(target htc.Target) tea.Cmd
}

// actionTimeout is the ceiling on a single action. Long enough for a
// screenshot round trip, short enough that a wedged target doesn't look like
// a hung UI.
const actionTimeout = 30 * time.Second

var actions = []Action{
	{
		Key:         "enter",
		Label:       "details",
		NeedsTarget: true,
		Run:         detailAction,
	},
	{
		Key:         "p",
		Label:       "ports",
		Busy:        "reading port map...",
		NeedsTarget: true,
		Command:     "htcs ports",
		Run:         portsAction,
	},
	{
		Key:         "i",
		Label:       "input routes",
		Busy:        "checking input routes...",
		NeedsTarget: true,
		Command:     "input status",
		Run:         inputStatusAction,
	},
	{
		Key:         "v",
		Label:       "video window",
		NeedsTarget: true,
		Command:     "video",
		Run:         videoAction,
	},
	{
		Key:         "g",
		Label:       "attach debugger",
		Busy:        "starting gdb bridge...",
		NeedsTarget: true,
		Command:     "gdb",
		Run:         gdbAction,
	},
	{
		Key:   "r",
		Label: "refresh",
		Busy:  "refreshing...",
		Run:   func(_ htc.Target) tea.Cmd { return loadPeersCmd() },
	},
}

var actionsByKey = map[string]Action{}

func init() {
	for _, a := range actions {
		actionsByKey[a.Key] = a
	}
}

// lookupAction resolves a keystroke. An unbound key reports false rather
// than falling through to some default action.
func lookupAction(key string) (Action, bool) {
	a, ok := actionsByKey[key]
	return a, ok
}

// helpLine renders the key hints in registry order, so it can never drift
// out of sync with what the keys actually do. The palette hint goes last
// because it is the answer to "where is everything else".
func helpLine() string {
	parts := make([]string, 0, len(actions)+3)
	parts = append(parts, "↑/↓ select")
	for _, a := range actions {
		parts = append(parts, a.Key+": "+a.Label)
	}
	parts = append(parts, "esc: close panel", "q: quit", paletteHint())
	return strings.Join(parts, "   ")
}

// ---- action implementations ----

func detailAction(t htc.Target) tea.Cmd {
	return func() tea.Msg {
		var b strings.Builder
		fmt.Fprintf(&b, "serial:    %s\n", targetSerial(t))
		fmt.Fprintf(&b, "hardware:  %s\n", t.HardwareType)
		fmt.Fprintf(&b, "transport: %s", t.CommunicationMethod)
		return panelMsg{title: "target " + targetSerial(t), body: b.String()}
	}
}

// portsAction shows what the target is listening on right now, read straight
// off the HTCS control port. Which services are up is the difference between
// "input works" and "input silently does nothing", so it's worth surfacing.
func portsAction(t htc.Target) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		snap, err := htc.PortMap(ctx, htc.ControlAddr())
		if err != nil {
			return actionResultMsg{err: err}
		}
		return panelMsg{title: "htcs ports", body: renderPorts(snap, targetSerial(t))}
	}
}

// renderPorts lists every known service and whether this target publishes
// it, then anything published that isn't in the registry. Showing the "down"
// ones matters as much as the up ones: an absent channel is the usual reason
// something doesn't respond.
func renderPorts(snap *htc.PortMapSnapshot, serial string) string {
	up := map[string]htc.PortMapEntry{}
	for _, e := range snap.Entries {
		if serial == "" || e.Peer == serial {
			up[e.Port] = e
		}
	}

	var b strings.Builder
	for _, s := range htc.Services() {
		if e, ok := up[s.Port]; ok {
			fmt.Fprintf(&b, "✓ %-16s %-27s %s\n", s.Key, s.Port, e.Addr())
			delete(up, s.Port)
		} else {
			fmt.Fprintf(&b, "✗ %-16s %-27s %s\n", s.Key, s.Port, "not published")
		}
	}
	// Anything left is a port this build has never heard of. Listing it
	// unlabelled is honest; guessing at which known service it resembles
	// would not be.
	rest := make([]string, 0, len(up))
	for port := range up {
		rest = append(rest, port)
	}
	sort.Strings(rest)
	for _, port := range rest {
		fmt.Fprintf(&b, "✓ %-16s %-27s %s\n", "(unknown)", port, up[port].Addr())
	}
	return strings.TrimRight(b.String(), "\n")
}

// inputStatusAction reports whether the target's raw HID channel is
// reachable. When clicking does nothing, this is the difference between
// guessing and knowing whether the channel is even published.
func inputStatusAction(t htc.Target) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return panelMsg{title: "input routes", body: renderInputStatus(ctx, targetSerial(t))}
	}
}

func renderInputStatus(ctx context.Context, serial string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "raw HTCS channel (%s)\n", htc.ControlAddr())
	if entry, err := htc.ResolvePort(ctx, serial, "hid"); err == nil {
		fmt.Fprintf(&b, "  ✓ %s at %s\n", entry.Port, entry.Addr())
	} else {
		fmt.Fprintf(&b, "  ✗ %v\n", err)
	}
	return strings.TrimRight(b.String(), "\n")
}

// gdbAction starts the gdb bridge in the background and tells the user the one
// thing they need next: the address to point their debugger at. The bridge
// outlives the TUI on purpose - it is a port forward, and closing it when the
// list refreshes would drop whatever is attached to it.
func gdbAction(t htc.Target) tea.Cmd {
	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return actionResultMsg{err: fmt.Errorf("locate own binary: %w", err)}
		}
		serial := targetSerial(t)
		if serial == "" {
			return actionResultMsg{err: fmt.Errorf("target has no serial to resolve its gdb stub")}
		}
		cmd := exec.Command(self, "gdb", serial)
		if err := cmd.Start(); err != nil {
			return actionResultMsg{err: err}
		}
		go cmd.Wait()
		return panelMsg{title: "attach your own debugger", body: gdbPanel}
	}
}

// gdbPanel is the connect string for each debugger people actually use. The
// port is the IANA-registered gdb-remote one, which is what `nxdbg gdb`
// defaults to.
const gdbPanel = `The target's gdb stub is now forwarded to localhost:2159.

  gdb        target remote localhost:2159
  LLDB       gdb-remote localhost:2159
  VS Code    "type": "cppdbg", "miDebuggerServerAddress": "localhost:2159"
  IDA        Debugger > Attach > Remote GDB debugger, localhost:2159
  Ghidra     Debugger > Connect, target remote localhost:2159

Leave this running while you debug. Close it with "nxdbg gdb" in a terminal
if you want to see its log.`

// videoAction opens the interactive remote-screen window. It re-execs this
// same binary rather than opening the window in-process: the window's event
// loop wants the main thread on some platforms, which the TUI already owns.
func videoAction(t htc.Target) tea.Cmd {
	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return actionResultMsg{err: fmt.Errorf("locate own binary: %w", err)}
		}
		serial := targetSerial(t)
		cmd := exec.Command(self, "video", serial)
		if err := cmd.Start(); err != nil {
			return actionResultMsg{err: err}
		}
		// Nothing waits on it - the window belongs to the user now, and
		// reaping it would mean blocking the UI until they close it.
		go cmd.Wait()
		return actionResultMsg{text: "opened video window for " + serial}
	}
}

// targetSerial is the target's HTCS peer name. Both fields carry the serial
// for a USB devkit, but only Name is reliably populated across transports.
func targetSerial(t htc.Target) string {
	if t.UniqueIdentifier != "" {
		return t.UniqueIdentifier
	}
	return t.Name
}
