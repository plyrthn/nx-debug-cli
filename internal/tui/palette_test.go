package tui

import (
	"strings"
	"testing"

	"github.com/plyrthn/nx-debug-cli/internal/commands"
	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// The whole reason the palette exists: everything the CLI can do has to be
// reachable from the TUI without a second registration somewhere.
func TestPaletteOffersEveryCommand(t *testing.T) {
	offered := map[string]bool{}
	for _, c := range paletteCommands() {
		offered[c.Path()] = true
	}
	for _, c := range commands.All() {
		if !offered[c.Path()] {
			t.Errorf("%q is in the catalog but the palette never lists it", c.Path())
		}
	}
}

// Each hotkey is a shortcut to a command, and a hotkey pointing at a command
// that no longer exists is a key that does something the help does not
// describe.
func TestEveryHotkeyNamesARealCommand(t *testing.T) {
	for _, a := range actions {
		if a.Command == "" {
			continue
		}
		if _, ok := commands.Find(a.Command); !ok {
			t.Errorf("hotkey %q is a shortcut to %q, which is not in the catalog", a.Key, a.Command)
		}
	}
}

func TestPaletteFilterMatchesPathAndSummary(t *testing.T) {
	for _, tc := range []struct {
		filter string
		want   string
	}{
		{"reboot", "shell reboot"},            // by name
		{"shell reb", "shell reboot"},         // by path
		{"elementary stream", "video record"}, // by summary
	} {
		p := palette{stage: paletteChoosing, filter: tc.filter}
		var found bool
		for _, c := range p.matches() {
			if c.Path() == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("filter %q never matched %q", tc.filter, tc.want)
		}
	}
}

func TestPaletteFilterRejectsNonsense(t *testing.T) {
	p := palette{stage: paletteChoosing, filter: "zzzznotacommand"}
	if got := p.matches(); len(got) != 0 {
		t.Errorf("filter matched %d commands, want none", len(got))
	}
}

// Typing must not fall through to the hotkeys. "q" in a filter is a letter,
// not quit.
func TestTypingIntoTheFilterDoesNotFireHotkeys(t *testing.T) {
	m := model{palette: palette{stage: paletteChoosing}}
	for _, key := range []string{"q", "r", "t"} {
		next, cmd, handled := m.paletteKey(key)
		if !handled {
			t.Fatalf("key %q was not consumed by the palette", key)
		}
		if cmd != nil {
			t.Errorf("key %q ran something instead of filtering", key)
		}
		m = next
	}
	if m.palette.filter != "qrt" {
		t.Errorf("filter is %q, want %q", m.palette.filter, "qrt")
	}
}

// A required and an optional serial are not interchangeable, so the palette
// has to pass whichever the command actually takes.
func TestPaletteSuppliesTheRightKindOfTarget(t *testing.T) {
	m := model{
		targets: []htc.Target{{Name: "devkit", UniqueIdentifier: "SERIAL"}},
	}
	for _, tc := range []struct {
		path string
		want string
	}{
		{"shell firmware", "SERIAL"}, // required serial
		{"video", "SERIAL"},          // optional serial, one selected
		{"htcs services", ""},        // no target at all
	} {
		c, ok := commands.Find(tc.path)
		if !ok {
			t.Fatalf("%s: not in the catalog", tc.path)
		}
		got, ok := m.targetFor(c)
		if !ok {
			t.Errorf("%s: no target available", tc.path)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: target is %q, want %q", tc.path, got, tc.want)
		}
	}
}

// With nothing selected, a command that needs a target must be refused rather
// than run against whatever the empty string resolves to.
func TestPaletteRefusesACommandWithNoTarget(t *testing.T) {
	m := model{}
	c, _ := commands.Find("shell firmware")
	if _, ok := m.targetFor(c); ok {
		t.Error("a serial command was allowed to run with no target selected")
	}
	c, _ = commands.Find("htcs services")
	if _, ok := m.targetFor(c); !ok {
		t.Error("a command that needs no target was refused")
	}
}

// A target with no serial cannot be addressed, and saying so beats sending an
// empty peer name.
func TestPaletteRefusesASerialCommandWithoutASerial(t *testing.T) {
	m := model{targets: []htc.Target{{}}}
	c, _ := commands.Find("shell firmware")
	if _, ok := m.targetFor(c); ok {
		t.Error("a serial command ran against a target with no serial")
	}
}

// Picking a destructive command must stop and ask, not run it.
func TestDestructiveCommandsAskFirst(t *testing.T) {
	m := model{targets: []htc.Target{{Name: "devkit"}}}
	c, _ := commands.Find("shell reboot")
	next, cmd, _ := m.palettePick(c)
	if next.palette.stage != paletteConfirming {
		t.Errorf("stage is %v, want a confirmation", next.palette.stage)
	}
	if cmd != nil {
		t.Error("the command ran before it was confirmed")
	}
	// Declining leaves nothing running.
	after, cmd, _ := next.paletteConfirmKey("n")
	if cmd != nil {
		t.Error("declining still ran the command")
	}
	if after.palette.stage != paletteClosed {
		t.Error("declining left the palette open")
	}
}

// A command that takes arguments has to ask for them, or it runs with none and
// fails in a way that looks like the command is broken.
func TestCommandsWithArgumentsPromptForThem(t *testing.T) {
	m := model{targets: []htc.Target{{Name: "devkit"}}}
	c, _ := commands.Find("shell launch")
	next, cmd, _ := m.palettePick(c)
	if next.palette.stage != paletteArguments {
		t.Errorf("stage is %v, want the argument prompt", next.palette.stage)
	}
	if cmd != nil {
		t.Error("the command ran before its arguments were given")
	}
	view := next.paletteView()
	if !strings.Contains(view, c.Usage()) {
		t.Errorf("the prompt does not show what the command takes:\n%s", view)
	}
}

// The escape hatch out of each stage has to go somewhere sensible, or the only
// way out of a mis-picked command is quitting.
func TestEscapeBacksOutOfThePalette(t *testing.T) {
	m := model{palette: palette{stage: paletteArguments}}
	next, _, _ := m.paletteKey("esc")
	if next.palette.stage != paletteChoosing {
		t.Errorf("esc from the argument prompt went to %v, want the list", next.palette.stage)
	}
	next, _, _ = next.paletteKey("esc")
	if next.palette.stage != paletteClosed {
		t.Error("esc from the list did not close the palette")
	}
}

// A closed palette must not swallow keys, or every hotkey stops working.
func TestClosedPaletteConsumesNothing(t *testing.T) {
	m := model{}
	if _, _, handled := m.paletteKey("q"); handled {
		t.Error("the closed palette consumed a key")
	}
}

func TestPaletteHintCountsTheCatalog(t *testing.T) {
	if !strings.Contains(helpLine(), paletteHint()) {
		t.Error("the help line never mentions the palette")
	}
	if !strings.Contains(paletteHint(), "commands") {
		t.Errorf("hint is %q, which does not say what it opens", paletteHint())
	}
}
