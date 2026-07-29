package tui

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// With nothing serving the control port, the TUI starts a session rather than
// stopping to tell the user to. That is the whole point of the fallback.
func TestNothingRunningStartsASession(t *testing.T) {
	m := model{loading: true}
	next, cmd := m.Update(peersMsg{err: errors.New("no session")})
	got := next.(model)
	if cmd == nil {
		t.Fatal("nothing was started")
	}
	if !got.triedServe {
		t.Error("the attempt was not recorded, so it can be made again in a loop")
	}
	if !strings.Contains(got.statusMsg, "starting a session") {
		t.Errorf("status is %q, which does not say what is happening", got.statusMsg)
	}
}

// Once tried, a second failure has to be reported rather than starting another
// session. A retry loop here would spawn processes until the box gave up.
func TestASecondFailureIsReportedNotRetried(t *testing.T) {
	m := model{triedServe: true, serveErr: errors.New("no session on 127.0.0.1:20184")}
	next, cmd := m.Update(peersMsg{err: errors.New("no session")})
	got := next.(model)
	if cmd != nil {
		t.Error("a second session was started")
	}
	if got.err == nil {
		t.Fatal("the failure was not reported")
	}
	if !strings.Contains(got.err.Error(), "no session") {
		t.Errorf("error never mentions %q: %v", "no session", got.err)
	}
}

// The process handle has to be recorded before the wait, or quitting during
// the twenty-odd seconds the link takes leaves a process holding the devkit.
func TestTheProcessIsRecordedBeforeWaiting(t *testing.T) {
	m := model{serveErr: errors.New("no daemon")}
	proc := &os.Process{Pid: -1}
	next, cmd := m.Update(serveStartedMsg{proc: proc, exited: make(chan error), log: "somewhere.log"})
	got := next.(model)
	if got.serve != proc {
		t.Error("the process was not recorded before the wait began")
	}
	if got.serveLog != "somewhere.log" {
		t.Errorf("log path is %q, so a failure could not be explained", got.serveLog)
	}
	if cmd == nil {
		t.Error("nothing waited for the session to come up")
	}
}

// A session that never comes up must be cleaned up, not left running with the
// TUI showing an error.
func TestAFailedStartIsCleanedUp(t *testing.T) {
	proc := &os.Process{Pid: -1}
	killed := watchKills(t)

	m := model{serveErr: errors.New("no daemon on 127.0.0.1:20182"), serve: proc}
	next, _ := m.Update(serveReadyMsg{err: errors.New("did not come up")})
	got := next.(model)

	if len(*killed) != 1 || (*killed)[0] != proc {
		t.Errorf("the failed session was not stopped (killed %d)", len(*killed))
	}
	if got.serve != nil {
		t.Error("the failed session was left recorded, so quitting would try to kill it again")
	}
	if got.err == nil || !strings.Contains(got.err.Error(), "did not come up") {
		t.Errorf("error is %v, want the start failure", got.err)
	}
}

// Quitting takes the session with it, so nothing is left holding the devkit's
// USB interface.
func TestQuittingStopsOurOwnSession(t *testing.T) {
	proc := &os.Process{Pid: -1}
	killed := watchKills(t)

	m := model{serve: proc}
	if _, cmd := m.handleKey("q"); cmd == nil {
		t.Error("q did not quit")
	}
	if len(*killed) != 1 {
		t.Errorf("quitting stopped %d sessions, want 1", len(*killed))
	}
}

// watchKills records what would have been killed, instead of killing it. A
// hand-built os.Process has no handle behind it and killing one panics.
func watchKills(t *testing.T) *[]*os.Process {
	t.Helper()
	var killed []*os.Process
	old := killProcess
	killProcess = func(p *os.Process) error {
		killed = append(killed, p)
		return nil
	}
	t.Cleanup(func() { killProcess = old })
	return &killed
}

// A session the TUI did not start belongs to whoever did, and killing it on
// the way out would drop whatever is attached through it.
func TestStopServeLeavesAForeignSessionAlone(t *testing.T) {
	killed := watchKills(t)
	model{}.stopServe()
	if len(*killed) != 0 {
		t.Error("a session this TUI did not start was stopped")
	}
}

// The header has to say which of the two situations this is, because what
// works differs in each.
func TestHeaderNamesTheSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model model
		want  string
	}{
		{"a session found on the control port", model{}, htc.ControlAddr()},
		{"our own session", model{serve: &os.Process{Pid: -1}}, "own session over USB"},
	} {
		if got := tc.model.View(); !strings.Contains(got, tc.want) {
			t.Errorf("%s: header never says %q:\n%s", tc.name, tc.want, got)
		}
	}
}
