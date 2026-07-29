package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

func testModel(targets ...htc.Target) model {
	m := initialModel()
	m.loading = false
	m.targets = targets
	return m
}

func update(m model, msg tea.Msg) (model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(model), cmd
}

func key(s string) tea.Msg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestCursorMovementIsClamped(t *testing.T) {
	m := testModel(htc.Target{Name: "SERIAL1"}, htc.Target{Name: "SERIAL2"})

	m, _ = update(m, key("k"))
	if m.cursor != 0 {
		t.Errorf("cursor = %d after up at the top, want 0", m.cursor)
	}
	m, _ = update(m, key("j"))
	m, _ = update(m, key("j"))
	if m.cursor != 1 {
		t.Errorf("cursor = %d after two downs over two targets, want 1", m.cursor)
	}
}

// A refreshed list that lost entries must not leave the cursor pointing off
// the end of it.
func TestCursorResetsWhenTargetsShrink(t *testing.T) {
	m := testModel(htc.Target{Name: "SERIAL1"}, htc.Target{Name: "SERIAL2"})
	m.cursor = 1
	m, _ = update(m, peersMsg{targets: []htc.Target{{Name: "SERIAL1"}}})
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
	if _, ok := m.selectedTarget(); !ok {
		t.Error("no target selected after the list shrank")
	}
}

func TestSelectedTargetOnEmptyList(t *testing.T) {
	m := testModel()
	if _, ok := m.selectedTarget(); ok {
		t.Error("a target was selected from an empty list")
	}
}

// A target-scoped action with nothing selected must report it rather than run
// against a zero-valued target.
func TestTargetActionWithoutSelectionReportsIt(t *testing.T) {
	m := testModel()
	for _, a := range actions {
		if !a.NeedsTarget {
			continue
		}
		next, cmd := m.handleKey(a.Key)
		got := next.(model)
		if cmd != nil {
			t.Errorf("action %q ran with no target selected", a.Key)
		}
		if !got.statusErr || !strings.Contains(got.statusMsg, "no target") {
			t.Errorf("action %q status = %q, want a no-target error", a.Key, got.statusMsg)
		}
	}
}

// An action that needs no target selection - refresh is the only one - must
// run regardless of what's selected.
func TestActionWithoutTargetAlwaysRuns(t *testing.T) {
	m := testModel()
	for _, a := range actions {
		if a.NeedsTarget {
			continue
		}
		_, cmd := m.handleKey(a.Key)
		if cmd == nil {
			t.Errorf("action %q did not run", a.Key)
		}
	}
}

func TestUnboundKeyIsIgnored(t *testing.T) {
	m := testModel(htc.Target{Name: "SERIAL1"})
	m.statusMsg = "unchanged"
	next, cmd := m.handleKey("z")
	got := next.(model)
	if cmd != nil {
		t.Error("an unbound key ran a command")
	}
	if got.statusMsg != "unchanged" {
		t.Errorf("an unbound key changed the status to %q", got.statusMsg)
	}
}

func TestQuitKeys(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		m := testModel()
		_, cmd := m.handleKey(k)
		if cmd == nil {
			t.Errorf("%q did not quit", k)
		}
	}
}

func TestEscapeClosesPanel(t *testing.T) {
	m := testModel()
	m.panelTitle, m.panelBody = "title", "body"
	next, _ := m.handleKey("esc")
	got := next.(model)
	if got.panelBody != "" || got.panelTitle != "" {
		t.Errorf("panel still set after esc: %q / %q", got.panelTitle, got.panelBody)
	}
}

func TestPanelMsgClearsStatus(t *testing.T) {
	m := testModel()
	m.statusMsg = "loading..."
	m, _ = update(m, panelMsg{title: "ports", body: "line"})
	if m.statusMsg != "" {
		t.Errorf("status = %q, want cleared once the panel filled", m.statusMsg)
	}
	if m.panelTitle != "ports" || m.panelBody != "line" {
		t.Errorf("panel = %q / %q", m.panelTitle, m.panelBody)
	}
}

func TestActionResultMsg(t *testing.T) {
	m := testModel()
	m, _ = update(m, actionResultMsg{err: errors.New("boom")})
	if !m.statusErr || m.statusMsg != "boom" {
		t.Errorf("status = %q err=%v", m.statusMsg, m.statusErr)
	}

	m, _ = update(m, actionResultMsg{text: "done"})
	if m.statusErr || m.statusMsg != "done" {
		t.Errorf("status = %q err=%v", m.statusMsg, m.statusErr)
	}

	// A success with nothing to say still has to clear the previous error.
	m, _ = update(m, actionResultMsg{})
	if m.statusErr || m.statusMsg != "ok" {
		t.Errorf("status = %q err=%v", m.statusMsg, m.statusErr)
	}
}

func TestViewRendersTargetsAndHelp(t *testing.T) {
	m := testModel(htc.Target{Name: "SERIAL", HardwareType: "EDEV", CommunicationMethod: "USB"})
	out := m.View()
	for _, want := range []string{"SERIAL", "EDEV", "USB", "v: video window", "p: ports"} {
		if !strings.Contains(out, want) {
			t.Errorf("view is missing %q:\n%s", want, out)
		}
	}
}

func TestViewWithNoTargets(t *testing.T) {
	out := testModel().View()
	if !strings.Contains(out, "no targets connected") {
		t.Errorf("view:\n%s", out)
	}
}

func TestViewShowsError(t *testing.T) {
	m := testModel()
	m.err = errors.New("no session")
	out := m.View()
	if !strings.Contains(out, "no session") {
		t.Errorf("view:\n%s", out)
	}
}

// A long panel must not push the help line off the bottom of the terminal.
func TestClipPanelKeepsTheTail(t *testing.T) {
	m := testModel(htc.Target{Name: "SERIAL1"})
	m.height = 20

	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "line")
	}
	lines = append(lines, "LAST")

	got := m.clipPanel(strings.Join(lines, "\n"))
	gotLines := strings.Split(got, "\n")
	if len(gotLines) > m.height {
		t.Errorf("clipped to %d lines, terminal is %d high", len(gotLines), m.height)
	}
	if gotLines[len(gotLines)-1] != "LAST" {
		t.Errorf("last line = %q, want the tail of the input", gotLines[len(gotLines)-1])
	}
	if gotLines[0] != "…" {
		t.Errorf("no truncation marker, first line = %q", gotLines[0])
	}
}

// Short bodies pass through untouched, including before the terminal size is
// known.
func TestClipPanelLeavesShortBodiesAlone(t *testing.T) {
	m := testModel()
	body := "a\nb\nc"
	if got := m.clipPanel(body); got != body {
		t.Errorf("clipPanel = %q, want %q", got, body)
	}
	m.height = 100
	if got := m.clipPanel(body); got != body {
		t.Errorf("clipPanel = %q, want %q", got, body)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in, want string
		n        int
	}{
		{"short", "short", 10},
		{"exactly-10", "exactly-10", 10},
		{"much too long to fit", "much too …", 10},
		{"x", "x", 1},
	}
	for _, tc := range cases {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
