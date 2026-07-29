package tui

import (
	"strings"
	"testing"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// Every action has to be complete and uniquely bound. A duplicate key would
// make one action unreachable, and the registry is the only place the key
// map is written down, so this is where that gets caught.
func TestActionRegistryIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range actions {
		if a.Key == "" || a.Label == "" || a.Run == nil {
			t.Errorf("incomplete action: %+v", a)
		}
		if seen[a.Key] {
			t.Errorf("duplicate key %q", a.Key)
		}
		seen[a.Key] = true
	}
	if len(actionsByKey) != len(actions) {
		t.Errorf("index holds %d actions, registry has %d", len(actionsByKey), len(actions))
	}
}

// The navigation and panel keys are handled before the registry is consulted,
// so an action bound to one of them would be dead code.
func TestActionsDoNotShadowNavigationKeys(t *testing.T) {
	reserved := []string{"up", "down", "j", "k", "q", "ctrl+c", "esc"}
	for _, a := range actions {
		for _, r := range reserved {
			if a.Key == r {
				t.Errorf("action %q is bound to reserved key %q and can never fire", a.Label, r)
			}
		}
	}
}

func TestHelpLineCoversEveryAction(t *testing.T) {
	help := helpLine()
	for _, a := range actions {
		want := a.Key + ": " + a.Label
		if !strings.Contains(help, want) {
			t.Errorf("help line is missing %q", want)
		}
	}
	for _, want := range []string{"↑/↓", "q: quit", "esc: close panel"} {
		if !strings.Contains(help, want) {
			t.Errorf("help line is missing %q", want)
		}
	}
}

func TestLookupUnboundKey(t *testing.T) {
	if a, ok := lookupAction("z"); ok {
		t.Errorf("unbound key resolved to %+v", a)
	}
}

func TestTargetSerial(t *testing.T) {
	cases := []struct {
		name   string
		target htc.Target
		want   string
	}{
		{"prefers unique identifier", htc.Target{UniqueIdentifier: "SERIAL", Name: "nickname"}, "SERIAL"},
		{"falls back to name", htc.Target{Name: "SERIAL"}, "SERIAL"},
		{"empty when neither is set", htc.Target{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetSerial(tc.target); got != tc.want {
				t.Errorf("targetSerial = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderPorts(t *testing.T) {
	snap := &htc.PortMapSnapshot{
		Entries: []htc.PortMapEntry{
			{Peer: "SERIAL", Port: "iywys@$gdb", Address: "127.0.0.1", TCPPort: 5000},
			{Peer: "SERIAL", Port: "iywys@$brandNew", Address: "127.0.0.1", TCPPort: 5001},
			{Peer: "OTHER", Port: "iywys@$hid", Address: "127.0.0.1", TCPPort: 5002},
		},
	}
	out := renderPorts(snap, "SERIAL")

	if !strings.Contains(out, "✓ gdb") {
		t.Error("published gdb not marked up")
	}
	// hid belongs to a different target, so it must show as down here.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "iywys@$hid") && !strings.Contains(line, "not published") {
			t.Errorf("another target's port leaked in: %q", line)
		}
	}
	// A port the registry has never seen is listed, but not labelled as
	// some other service.
	var unknown string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "iywys@$brandNew") {
			unknown = line
		}
	}
	if unknown == "" {
		t.Fatal("unrecognised port was dropped from the listing")
	}
	if !strings.Contains(unknown, "(unknown)") {
		t.Errorf("unrecognised port was given a label: %q", unknown)
	}
}

// With no serial to filter on, every entry counts as this target's. That's
// the single-devkit case and it should still render something useful.
func TestRenderPortsWithoutSerial(t *testing.T) {
	snap := &htc.PortMapSnapshot{
		Entries: []htc.PortMapEntry{{Peer: "SERIAL", Port: "iywys@$hid", Address: "127.0.0.1", TCPPort: 5000}},
	}
	out := renderPorts(snap, "")
	if !strings.Contains(out, "✓ hid") {
		t.Errorf("hid not marked up:\n%s", out)
	}
}

// Every registry service appears in the listing whether it's up or not - the
// absent ones are the whole point when something isn't responding.
func TestRenderPortsListsEveryKnownService(t *testing.T) {
	out := renderPorts(&htc.PortMapSnapshot{}, "SERIAL")
	for _, s := range htc.Services() {
		if !strings.Contains(out, s.Port) {
			t.Errorf("service %q missing from the listing", s.Port)
		}
	}
	if strings.Contains(out, "✓") {
		t.Error("an empty port map should have nothing marked up")
	}
}
