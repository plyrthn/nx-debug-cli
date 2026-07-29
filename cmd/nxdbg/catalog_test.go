package main

import (
	"os"
	"strings"
	"testing"

	"github.com/plyrthn/nx-debug-cli/internal/commands"
)

// The catalog is only worth having if it is the whole truth. These check it
// against the real dispatch tables in both directions: a command in the
// catalog that nothing dispatches is a help entry and a TUI menu item that do
// nothing, and a dispatched command missing from the catalog is one the TUI
// never offers and help never mentions.

// dispatched returns every command path the CLI actually handles, gathered
// from the tables it dispatches from rather than from a second list written
// out for the test.
func dispatched() map[string]bool {
	out := map[string]bool{}
	for _, name := range topLevelNames() {
		out[name] = true
	}
	for _, g := range dispatchGroups {
		for _, s := range g.subs {
			out[g.name+" "+s.name] = true
		}
	}
	for _, s := range shellSubs {
		out["shell "+s.name] = true
	}
	for _, s := range debugSubs {
		out["debug "+s.name] = true
	}
	for _, s := range gdbstubSubs {
		out["gdbstub "+s.name] = true
	}
	for name := range videoSubcommands {
		out["video "+name] = true
	}
	for _, name := range inputSubcommands {
		out["input "+name] = true
	}
	for _, name := range htcsSubcommands {
		out["htcs "+name] = true
	}
	for _, name := range configSubcommands {
		out["config "+name] = true
	}
	for _, name := range dumpSubcommands {
		out["dump "+name] = true
	}
	return out
}

func TestEveryCatalogCommandIsDispatched(t *testing.T) {
	live := dispatched()
	for _, c := range commands.All() {
		if !live[c.Path()] {
			t.Errorf("%q is in the catalog but nothing dispatches it", c.Path())
		}
	}
}

func TestEveryDispatchedCommandIsInTheCatalog(t *testing.T) {
	for path := range dispatched() {
		if _, ok := commands.Find(path); !ok {
			t.Errorf("%q is dispatched but missing from the catalog, so help and the TUI never show it", path)
		}
	}
}

// Every group needs a summary and at least one command, or it is either a
// blank line in `nxdbg help` or a heading with nothing under it.
func TestEveryGroupIsDeclaredAndPopulated(t *testing.T) {
	declared := map[string]bool{}
	for _, g := range commands.AllGroups() {
		declared[g.Name] = true
		if g.Summary == "" {
			t.Errorf("group %q has no summary", g.Name)
		}
		if len(commands.InGroup(g.Name)) == 0 {
			t.Errorf("group %q is declared but has no commands", g.Name)
		}
		if _, ok := groupDispatch[g.Name]; !ok {
			t.Errorf("group %q is declared but nothing dispatches it", g.Name)
		}
	}
	for _, c := range commands.All() {
		if c.Group != "" && !declared[c.Group] {
			t.Errorf("%s is in group %q, which is not declared", c.Path(), c.Group)
		}
	}
	for name := range groupDispatch {
		if !declared[name] {
			t.Errorf("group %q is dispatched but not declared", name)
		}
	}
}

// The usage line has to read the way the command is actually typed. The two
// orders are easy to get backwards, and a help line that puts the serial on
// the wrong side of the verb is worse than none.
func TestUsageLinesReadInTypingOrder(t *testing.T) {
	for path, want := range map[string]string{
		"shell reboot":  "nxdbg shell <serial> reboot",
		"shell launch":  "nxdbg shell <serial> launch <program-id-hex> [arguments]",
		"input tap":     "nxdbg input <serial> tap <x> <y>",
		"logging watch": "nxdbg logging watch <serial> [seconds]",
		"gdb":           "nxdbg gdb <serial> [--port N]",
		"serve":         "nxdbg serve [-v|-t] [--root DIR] [--read-only]",
	} {
		c, ok := commands.Find(path)
		if !ok {
			t.Errorf("%s: not in the catalog", path)
			continue
		}
		if got := c.Usage(); got != want {
			t.Errorf("%s: usage is %q, want %q", path, got, want)
		}
	}
}

// A summary is what someone reads to decide whether a command is the one they
// want, so an empty or shouty one is a real defect rather than a style nit.
func TestEveryCommandHasAUsableSummary(t *testing.T) {
	for _, c := range commands.All() {
		switch {
		case c.Summary == "":
			t.Errorf("%s: no summary", c.Path())
		case strings.HasSuffix(c.Summary, "."):
			t.Errorf("%s: summary ends with a full stop: %q", c.Path(), c.Summary)
		case len(c.Summary) > 72:
			t.Errorf("%s: summary is %d chars, too long to align: %q", c.Path(), len(c.Summary), c.Summary)
		}
	}
}

// MinArgs is derived from the usage line, so an argument written with the
// wrong brackets silently changes how many arguments the dispatcher demands.
func TestArgumentCountsComeOutOfTheUsageLines(t *testing.T) {
	for _, tc := range []struct {
		path string
		want int
	}{
		{"install", 3},       // verb + <serial> + <file.nsp>
		{"input touch", 4},   // verb + <serial> + <begin|move|end> + <finger>
		{"htcs resolve", 3},  // verb + two required
		{"shell devmenu", 3}, // verb + <serial> + <command...>
		{"logging watch", 2}, // verb + <serial>, [seconds] optional
	} {
		c, ok := commands.Find(tc.path)
		if !ok {
			t.Errorf("%s: not in the catalog", tc.path)
			continue
		}
		if got := c.MinArgs(); got != tc.want {
			t.Errorf("%s: MinArgs = %d, want %d (usage %q)", tc.path, got, tc.want, c.Usage())
		}
	}
}

// The alias table is the compatibility layer for the old flat names, so each
// entry has to land on something the catalog knows about.
func TestEveryAliasResolvesToACatalogCommand(t *testing.T) {
	for old, repl := range commandAliases {
		if len(repl) != 2 {
			t.Errorf("%s: expected a group and a subcommand, got %v", old, repl)
			continue
		}
		if _, ok := commands.Find(strings.Join(repl, " ")); !ok {
			t.Errorf("%s -> %s: no such command", old, strings.Join(repl, " "))
		}
	}
}

// Everything advertised in the top-level help needs detail behind it, or
// "nxdbg help <group>" sends people nowhere.
func TestHelpAdvertisesEveryGroup(t *testing.T) {
	printed := captureUsage(t)
	defer quietStdout(t)()
	for _, g := range commands.Groups() {
		if !strings.Contains(printed, "nxdbg "+g+" ") {
			t.Errorf("group %q is in the catalog but `nxdbg help` never mentions it", g)
		}
		if err := printGroupUsage(g); err != nil {
			t.Errorf("nxdbg help %s: %v", g, err)
		}
	}
}

// An unknown group has to fail rather than print something plausible.
func TestUnknownGroupHelpFails(t *testing.T) {
	if err := printGroupUsage("nonsense"); err == nil {
		t.Error("help for a group that does not exist succeeded")
	}
}

// `nxdbg help <thing>` has to work whether the thing is a group or a command.
// Nobody remembers which it is, and answering "unknown group" to a real
// command is a dead end.
func TestHelpWorksForCommandsAsWellAsGroups(t *testing.T) {
	defer quietStdout(t)()
	for _, c := range commands.All() {
		if err := printGroupUsage(c.Path()); err != nil {
			t.Errorf("nxdbg help %s: %v", c.Path(), err)
		}
	}
}

// quietStdout sends the help output nowhere for the duration of a test that
// only cares whether it succeeded.
func quietStdout(t *testing.T) func() {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = devnull
	return func() {
		os.Stdout = old
		devnull.Close()
	}
}

// A half-remembered name should point at the real ones rather than just
// failing.
func TestHelpForAPartialNameSuggests(t *testing.T) {
	err := printGroupUsage("watch")
	if err == nil {
		t.Fatal("a name shared by several commands resolved to one of them")
	}
	for _, want := range []string{"logging watch", "shell watch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("suggestion is missing %q: %v", want, err)
		}
	}
}
