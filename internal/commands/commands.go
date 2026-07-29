// Package commands is the catalog of everything nxdbg can do.
//
// It exists so there is one list rather than several. The CLI dispatches from
// it, `nxdbg help` is generated from it, and the TUI builds its command palette
// from it - which means a command cannot be added to the CLI and quietly stay
// missing from the TUI, or renamed in one and not the other. A test checks the
// catalog against the real dispatch tables in both directions.
//
// It deliberately holds no handlers. The handlers live with the CLI, because
// they need its flag parsing and its output conventions, and the TUI runs a
// command by invoking the binary rather than by calling into it.
package commands

import (
	"fmt"
	"strings"
)

// Target says what a command needs to name the devkit it acts on. A serial is
// the HTCS peer name, which is what `nxdbg serve` addresses targets by.
type Target int

const (
	// NoTarget commands act on the host, or on every target at once.
	NoTarget Target = iota
	// SerialTarget commands take a devkit serial.
	SerialTarget
	// OptionalSerialTarget commands take a serial but fall back to whichever
	// single target is connected without one.
	OptionalSerialTarget
)

// Required reports whether the target has to be given.
func (t Target) Required() bool { return t == SerialTarget }

// Needs says what has to be running for a command to work.
type Needs int

const (
	// NeedsNothing is a command that only reads local state.
	NeedsNothing Needs = iota
	// NeedsSession is a command that resolves an HTCS port, which
	// `nxdbg serve` provides.
	NeedsSession
	// NeedsDevice is a command that opens the devkit's USB interface itself,
	// so nothing else may hold it.
	NeedsDevice
)

func (n Needs) String() string {
	switch n {
	case NeedsSession:
		return "session"
	case NeedsDevice:
		return "usb"
	}
	return "-"
}

// Command is one thing nxdbg can do.
type Command struct {
	// Group is the subcommand group, or empty for a top-level command.
	Group string
	// Name is the verb.
	Name string
	// Args is the argument part of the usage line, without the target.
	Args string
	// Summary is one line, lowercase, no trailing stop.
	Summary string

	Target Target
	Needs  Needs

	// Long marks a command that runs until interrupted or opens a window.
	// The TUI starts these detached rather than waiting for output.
	Long bool
	// Streamed marks a Long command with no window of its own, whose output
	// is exactly what's worth watching (an install's progress, say) - the
	// TUI feeds it into the panel live, one line at a time, instead of the
	// detached/discarded default a Long command gets otherwise. Meaningless
	// unless Long is also set.
	Streamed bool
	// Everyday marks the handful of commands that appear at the top of
	// `nxdbg help` rather than under a group.
	Everyday bool
	// Destructive marks a command that changes target state in a way worth
	// confirming before running it from a menu.
	Destructive bool
}

// Path is how the command is typed, without the binary name.
func (c Command) Path() string {
	if c.Group == "" {
		return c.Name
	}
	return c.Group + " " + c.Name
}

// RequiredArgs counts the arguments the usage line marks as required, which
// are the angle-bracketed ones. Optional arguments are in square brackets and
// do not count.
func (c Command) RequiredArgs() int { return strings.Count(c.Args, "<") }

// MinArgs is the smallest number of words a dispatcher needs to see: the verb
// itself, the target placeholder if the command takes one, and every required
// argument. Deriving it from the usage line rather than declaring it beside
// the handler is what stops the two disagreeing, which shows up as either a
// misleading usage message or an index past the end of the arguments.
func (c Command) MinArgs() int {
	n := 1 + c.RequiredArgs()
	if c.Target.Required() {
		n++
	}
	return n
}

// TargetPlaceholder is how the target is written in a usage line, or empty for
// a command that does not name one.
func (c Command) TargetPlaceholder() string {
	switch c.Target {
	case SerialTarget:
		return "<serial>"
	case OptionalSerialTarget:
		return "[serial]"
	}
	return ""
}

// Argv is the command line for this command, with target substituted in the
// position the group puts it and no binary name in front. An empty target is
// left out, which is what a command that names no target wants and what an
// optional one falls back to.
func (c Command) Argv(target string) []string {
	var out []string
	if c.Group != "" {
		out = append(out, c.Group)
	}
	if target != "" && GroupOf(c.Group).TargetFirst {
		return append(out, target, c.Name)
	}
	out = append(out, c.Name)
	if target != "" {
		out = append(out, target)
	}
	return out
}

// Usage is the full command line, in the order the command is actually typed.
// Which side of the verb the target goes is a property of the group, not of
// the command.
func (c Command) Usage() string {
	words := append([]string{"nxdbg"}, c.Argv(c.TargetPlaceholder())...)
	if c.Args != "" {
		words = append(words, c.Args)
	}
	return strings.Join(words, " ")
}

// catalog is the list. Order within a group is the order it is shown in.
var catalog = []Command{
	// Everyday, top level.
	{Name: "video", Summary: "interactive window: live screen, mouse, keyboard and gamepad", Target: OptionalSerialTarget, Needs: NeedsSession, Long: true, Everyday: true},
	{Name: "serve", Args: "[-v|-t] [--root DIR] [--read-only]", Summary: "run the whole session, driving the devkit's USB link directly", Needs: NeedsDevice, Long: true, Everyday: true},
	{Name: "gdb", Args: "[--port N]", Summary: "forward the target's gdb stub to a local port", Target: SerialTarget, Needs: NeedsSession, Long: true, Everyday: true},
	{Name: "usb", Args: "[paths|reset]", Summary: "inspect or reset the devkit's USB interface", Needs: NeedsDevice},
	{Name: "install", Args: "<file.nsp> [--sdcard|--builtin|--auto] [--no-force]", Summary: "install an nsp on the target", Target: SerialTarget, Needs: NeedsSession, Long: true, Streamed: true, Everyday: true, Destructive: true},
	{Name: "uninstall", Args: "<application-id>", Summary: "remove an installed application", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Name: "apps", Summary: "list the applications installed on the target", Target: SerialTarget, Needs: NeedsSession},

	// logging.
	{Group: "logging", Name: "watch", Args: "[seconds]", Summary: "print the target's log as it arrives", Target: SerialTarget, Needs: NeedsSession, Long: true},

	// shell: the target's own command shell.
	{Group: "shell", Name: "screenshot", Args: "[dir-or-file]", Summary: "capture the screen to a PNG", Target: SerialTarget, Needs: NeedsSession},
	{Group: "shell", Name: "screenshot-fg", Args: "[dir-or-file]", Summary: "capture the foreground application only", Target: SerialTarget, Needs: NeedsSession},
	{Group: "shell", Name: "firmware", Summary: "the target's system version", Target: SerialTarget, Needs: NeedsSession},
	{Group: "shell", Name: "app", Summary: "the running application's id and version", Target: SerialTarget, Needs: NeedsSession},
	{Group: "shell", Name: "title", Args: "[process-index]", Summary: "a process's display title", Target: SerialTarget, Needs: NeedsSession},
	{Group: "shell", Name: "program-id", Args: "[process-index]", Summary: "a process's program id", Target: SerialTarget, Needs: NeedsSession},
	{Group: "shell", Name: "launch", Args: "<program-id-hex> [arguments]", Summary: "launch an installed application", Target: SerialTarget, Needs: NeedsSession},
	{Group: "shell", Name: "launch-system", Args: "<program-id-hex> [arguments]", Summary: "launch a system program", Target: SerialTarget, Needs: NeedsSession},
	{Group: "shell", Name: "terminate", Summary: "stop the running application", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Group: "shell", Name: "terminate-all", Summary: "stop every process", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Group: "shell", Name: "reboot", Summary: "restart the target", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Group: "shell", Name: "shutdown", Summary: "power the target off", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Group: "shell", Name: "events", Args: "[seconds]", Summary: "watch program launches and exits", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "shell", Name: "devmenu", Args: "<command...>", Summary: "run one of the target's DevMenu commands", Target: SerialTarget, Needs: NeedsSession},
	{Group: "shell", Name: "watch", Summary: "interactive window fed by repeated screenshots, no drift", Target: SerialTarget, Needs: NeedsSession, Long: true, Everyday: true},

	// debug: the target's debug monitor. Only answers once a process is
	// actually attached to - see the group summary.
	{Group: "debug", Name: "banner", Summary: "the debug monitor's own greeting", Target: SerialTarget, Needs: NeedsSession},
	{Group: "debug", Name: "select", Args: "<handle-hex>", Summary: "choose which attached process later commands apply to", Target: SerialTarget, Needs: NeedsSession},
	{Group: "debug", Name: "read", Args: "<handle-hex> <addr-hex> <count>", Summary: "read target memory", Target: SerialTarget, Needs: NeedsSession},
	{Group: "debug", Name: "modules", Args: "<handle-hex> [count]", Summary: "list loaded modules", Target: SerialTarget, Needs: NeedsSession},
	{Group: "debug", Name: "threads", Args: "<handle-hex> [count]", Summary: "list threads", Target: SerialTarget, Needs: NeedsSession},

	// gdbstub: the target's real GDB Remote Serial Protocol stub, over
	// iywys@$gdb. One-shot equivalents of what a real gdb/lldb session gets
	// from `nxdbg gdb <serial>` - each of these attaches by kernel pid, does
	// one thing and detaches.
	{Group: "gdbstub", Name: "attach", Args: "<pid> [symbol-file]", Summary: "attach by kernel pid and report the stop state", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "regs", Args: "<pid>", Summary: "read the general-purpose register set", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "setreg", Args: "<pid> <reg> <value-hex>", Summary: "write one register (x0-x30, sp, pc, cpsr)", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Group: "gdbstub", Name: "read", Args: "<pid> <addr-hex> <count>", Summary: "read target memory", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "write", Args: "<pid> <addr-hex> <hex-bytes>", Summary: "write target memory", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Group: "gdbstub", Name: "break", Args: "<pid> <addr-hex>", Summary: "set a software breakpoint", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Group: "gdbstub", Name: "clear", Args: "<pid> <addr-hex>", Summary: "remove a software breakpoint", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "hwbreak", Args: "<pid> <addr-hex>", Summary: "set a hardware breakpoint (works on read-only/hash-checked code)", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Group: "gdbstub", Name: "hwclear", Args: "<pid> <addr-hex>", Summary: "remove a hardware breakpoint", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "watch", Args: "<pid> <addr-hex> <length-hex> <write|read|access>", Summary: "set a hardware watchpoint on a memory range", Target: SerialTarget, Needs: NeedsSession, Destructive: true},
	{Group: "gdbstub", Name: "unwatch", Args: "<pid> <addr-hex> <length-hex> <write|read|access>", Summary: "remove a hardware watchpoint", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "step", Args: "<pid> [symbol-file]", Summary: "single-step and report where it stopped", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "continue", Args: "<pid> [symbol-file]", Summary: "resume and block until the next stop", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "gdbstub", Name: "step-legacy", Args: "<pid> [symbol-file]", Summary: "single-step via the legacy \"s\" packet (try this if step hangs)", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "continue-legacy", Args: "<pid> [symbol-file]", Summary: "resume via the legacy \"c\" packet (try this if continue hangs)", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "gdbstub", Name: "modules", Args: "<pid>", Summary: "list the process's loaded modules and load addresses", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "threads", Args: "<pid>", Summary: "list the process's live thread ids", Target: SerialTarget, Needs: NeedsSession},
	{Group: "gdbstub", Name: "backtrace", Args: "<pid> [symbol-file] [max-frames]", Summary: "walk the frame-pointer chain and print each return address", Target: SerialTarget, Needs: NeedsSession},

	// input.
	{Group: "input", Name: "status", Summary: "both routes into the target's HID, and what blocks each", Target: SerialTarget, Needs: NeedsSession},
	{Group: "input", Name: "tap", Args: "<x> <y>", Summary: "tap the touchscreen", Target: SerialTarget, Needs: NeedsSession},
	{Group: "input", Name: "touch", Args: "<begin|move|end> <finger> [x] [y]", Summary: "drive a single touch contact", Target: SerialTarget, Needs: NeedsSession},
	{Group: "input", Name: "mouse", Args: "<move|button|wheel> ...", Summary: "move the pointer, hold buttons or scroll", Target: SerialTarget, Needs: NeedsSession},
	{Group: "input", Name: "key", Args: "<down|up> <usage-id>", Summary: "press or release a USB HID key", Target: SerialTarget, Needs: NeedsSession},
	{Group: "input", Name: "home", Args: "<down|up>", Summary: "press or release the HOME button", Target: SerialTarget, Needs: NeedsSession},
	{Group: "input", Name: "raw-dump", Summary: "watch the chunks the target sends", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "input", Name: "raw-tap", Args: "<x> <y> [warmup-s]", Summary: "tap over the raw channel", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "input", Name: "raw-home", Summary: "press HOME over the raw channel", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "input", Name: "raw-pad", Args: "<button[,button...]> [pad-id] [hold-ms] [warmup-s]", Summary: "press pad buttons over the raw channel", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "input", Name: "raw-probe", Args: "<x> <y> [minutes]", Summary: "tap on a schedule over one long-lived session", Target: SerialTarget, Needs: NeedsSession, Long: true},

	// video: recording and stream capture. The bare `nxdbg video` window is a
	// top-level command above.
	{Group: "video", Name: "dump", Args: "[seconds] [all]", Summary: "print frame headers off the target's own stream", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "video", Name: "dump-audio", Args: "[seconds] [all]", Summary: "print audio frame headers off the target's stream", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "video", Name: "grab", Args: "[out-file]", Summary: "save one frame, self-contained encodings only", Target: SerialTarget, Needs: NeedsSession},
	{Group: "video", Name: "record", Args: "<seconds> <file> [--raw]", Summary: "write a decodable Annex B elementary stream", Target: SerialTarget, Needs: NeedsSession, Long: true},
	{Group: "video", Name: "raw", Args: "[bytes]", Summary: "hexdump the head of the video stream", Target: SerialTarget, Needs: NeedsSession},
	{Group: "video", Name: "raw-audio", Args: "[bytes]", Summary: "hexdump the head of the audio stream", Target: SerialTarget, Needs: NeedsSession},

	// htcs: the port map.
	{Group: "htcs", Name: "ports", Args: "[peer] [--watch]", Summary: "every port the targets publish", Needs: NeedsSession},
	{Group: "htcs", Name: "services", Summary: "the service names this build knows", Needs: NeedsNothing},
	{Group: "htcs", Name: "resolve", Args: "<peer> <service>", Summary: "resolve a service to a host address", Needs: NeedsSession},

	// config: local settings.
	{Group: "config", Name: "show", Summary: "the config file path and effective settings", Needs: NeedsNothing},
	{Group: "config", Name: "path", Summary: "just the config file path", Needs: NeedsNothing},

	// dump: the target's own .nxdmp crash dumps, read straight off disk - no
	// target connection needed.
	{Group: "dump", Name: "read", Args: "<file> [--all-threads]", Summary: "print a crash dump's exception, modules, registers and stack trace", Needs: NeedsNothing},
	{Group: "dump", Name: "list", Args: "<dir>", Summary: "summarize every .nxdmp file in a directory", Needs: NeedsNothing},

	// lock: the informal shared-devkit lock file, formalized. No connection
	// to the target itself, just a file in the temp directory.
	{Group: "lock", Name: "status", Summary: "who holds the lock, since when, and their message", Target: SerialTarget, Needs: NeedsNothing},
	{Group: "lock", Name: "acquire", Args: "<session> [message] [--force]", Summary: "take the lock, refusing over another session's unless forced", Target: SerialTarget, Needs: NeedsNothing},
	{Group: "lock", Name: "release", Args: "<session> [--force]", Summary: "clear the lock, refusing to clear another session's unless forced", Target: SerialTarget, Needs: NeedsNothing},
}

// All returns every command, in catalog order.
func All() []Command {
	out := make([]Command, len(catalog))
	copy(out, catalog)
	return out
}

// Find looks a command up by the way it is typed, e.g. "logging watch".
func Find(path string) (Command, bool) {
	for _, c := range catalog {
		if c.Path() == path {
			return c, true
		}
	}
	return Command{}, false
}

// InGroup returns a group's commands in catalog order. The empty group is the
// top-level commands.
func InGroup(group string) []Command {
	var out []Command
	for _, c := range catalog {
		if c.Group == group {
			out = append(out, c)
		}
	}
	return out
}

// Groups lists the group names in help order.
func Groups() []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Name)
	}
	return out
}

// TopLevel returns the commands that are not in any group.
func TopLevel() []Command { return InGroup("") }

// Everyday returns the commands worth showing before any grouping.
func Everyday() []Command {
	var out []Command
	for _, c := range catalog {
		if c.Everyday {
			out = append(out, c)
		}
	}
	return out
}

// Group is a named set of commands. A group needs a description of its own,
// and it needs to say where the target goes, which is not something any single
// command in it can know.
type Group struct {
	Name    string
	Summary string
	// TargetFirst says the target is named before the verb, as in
	// `nxdbg shell <serial> screenshot`. The default is after it, as in
	// `nxdbg power reboot <handle>`.
	TargetFirst bool
}

// groups is declared in the order the help shows them, which runs from what
// most people need to what most people never touch.
var groups = []Group{
	{Name: "logging", Summary: "the target log"},
	{Name: "shell", Summary: "the target's own command shell", TargetFirst: true},
	{Name: "debug", Summary: "breakpoints, registers and memory - needs an attached target", TargetFirst: true},
	{Name: "gdbstub", Summary: "the same, over the target's real GDB stub - attaches itself", TargetFirst: true},
	{Name: "input", Summary: "touch, mouse, keyboard, gamepad and HOME", TargetFirst: true},
	{Name: "video", Summary: "recording, stream capture and frame grabs"},
	{Name: "htcs", Summary: "the htcs port map"},
	{Name: "config", Summary: "local settings"},
	{Name: "dump", Summary: "the target's own .nxdmp crash dumps"},
	{Name: "lock", Summary: "the shared-devkit coordination lock"},
}

// GroupOf returns a group by name. An unknown name gives a zero Group rather
// than the nearest match, so a typo shows up as a blank description instead of
// silently describing something else.
func GroupOf(name string) Group {
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	return Group{Name: name}
}

// AllGroups returns the declared groups, in help order.
func AllGroups() []Group {
	out := make([]Group, len(groups))
	copy(out, groups)
	return out
}

// Describe renders a command as one aligned help line.
func Describe(c Command, width int) string {
	return fmt.Sprintf("  %-*s  %s", width, c.Usage(), c.Summary)
}

// UsageWidth is the widest usage line across the given commands, for aligning
// a help block.
func UsageWidth(cmds []Command) int {
	w := 0
	for _, c := range cmds {
		if n := len(c.Usage()); n > w {
			w = n
		}
	}
	return w
}
