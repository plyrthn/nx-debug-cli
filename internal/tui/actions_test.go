package tui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

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

// A process that dies before the grace period is up never opened a window,
// so it has to be reported as a failure, not the success text - this was the
// original bug: the child could die instantly and the caller never found out.
func TestVideoLaunchResultReportsAProcessThatDiesFast(t *testing.T) {
	var stderr bytes.Buffer
	stderr.WriteString("error: no session on 127.0.0.1:20184.\n  Start one with:  nxdbg serve\n")
	exited := make(chan error, 1)
	exited <- errors.New("exit status 1")

	msg := videoLaunchResult("SERIAL", &stderr, exited, time.Second)
	got, ok := msg.(actionResultMsg)
	if !ok {
		t.Fatalf("got %T, want actionResultMsg", msg)
	}
	if got.err == nil {
		t.Fatal("a process that exited immediately was reported as success")
	}
	if !strings.Contains(got.err.Error(), "no session") {
		t.Errorf("error %v does not carry what the process printed", got.err)
	}
}

// A process that dies fast with nothing on stderr still has to say something
// useful rather than an empty error.
func TestVideoLaunchResultReportsAProcessThatDiesFastWithNoOutput(t *testing.T) {
	exited := make(chan error, 1)
	exited <- errors.New("exit status 1")

	msg := videoLaunchResult("SERIAL", &bytes.Buffer{}, exited, time.Second)
	got, ok := msg.(actionResultMsg)
	if !ok {
		t.Fatalf("got %T, want actionResultMsg", msg)
	}
	if got.err == nil || !strings.Contains(got.err.Error(), "SERIAL") {
		t.Errorf("error %v does not name the target", got.err)
	}
}

// A process still running once the grace period passes is the window having
// opened successfully - that's the whole reason there's a grace period
// instead of waiting for the process to end.
func TestVideoLaunchResultReportsSuccessOnceGracePasses(t *testing.T) {
	exited := make(chan error) // never fires

	msg := videoLaunchResult("SERIAL", &bytes.Buffer{}, exited, 10*time.Millisecond)
	got, ok := msg.(actionResultMsg)
	if !ok {
		t.Fatalf("got %T, want actionResultMsg", msg)
	}
	if got.err != nil {
		t.Errorf("a still-running process was reported as a failure: %v", got.err)
	}
	if !strings.Contains(got.text, "SERIAL") {
		t.Errorf("success message %q does not name the target", got.text)
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
