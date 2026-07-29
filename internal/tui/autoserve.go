package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// With neither a daemon nor a session running, the TUI used to be a dead end:
// it knew exactly what to start and made the user go and start it. It can do
// that itself, because `nxdbg serve` is this same binary and nothing else is
// holding the devkit - which is precisely what having got here proves.
//
// The session is started as a child process rather than in this one. serve
// writes a running commentary to stdout, which would scribble over the UI, and
// it wants a signal to stop that the TUI has its own use for.
//
// Starting and waiting are two steps on purpose. A single command that did
// both would leave the process handle unknown for the twenty-odd seconds the
// link takes to come up, and quitting in that window would orphan a process
// holding the devkit's USB interface - which the next session cannot recover
// from, because the device is genuinely busy rather than merely half-open.

// serveStartTimeout is how long the session gets to publish its control port.
// The link handshake, and the reset-and-retry when a previous session left it
// half-open, both happen inside this.
const serveStartTimeout = 25 * time.Second

// selfPath is how this package finds the binary to re-invoke.
//
// It is a variable because os.Executable is not the right answer everywhere:
// under `go test` it names the test binary, so a test that exercised this for
// real would fork the test suite instead of nxdbg - and since the suite starts
// a session, which starts the suite, that is a fork bomb rather than a failed
// assertion. It has been one once.
var selfPath = os.Executable

// serveStartedMsg carries the child process, as soon as there is one.
//
// exited reports the process ending. It is a channel rather than a call to
// Signal(0), because on Windows that is not a liveness probe: it answers "not
// supported" for a perfectly healthy process, which reads as death.
type serveStartedMsg struct {
	proc   *os.Process
	exited <-chan error
	log    string
	err    error
}

// serveReadyMsg says the session's control port is answering, or why it never
// did.
type serveReadyMsg struct {
	err error
}

// startServeCmd launches a session and returns immediately.
//
// Its output goes to a file rather than to the null device: when this fails,
// what serve printed is the only explanation there is, and "exit status 1" is
// not one.
func startServeCmd() tea.Cmd {
	return func() tea.Msg {
		self, err := selfPath()
		if err != nil {
			return serveStartedMsg{err: fmt.Errorf("locate own binary: %w", err)}
		}
		logFile, err := os.CreateTemp("", "nxdbg-serve-*.log")
		if err != nil {
			return serveStartedMsg{err: err}
		}
		defer logFile.Close()

		cmd := exec.Command(self, "serve")
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Start(); err != nil {
			return serveStartedMsg{err: err, log: logFile.Name()}
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		return serveStartedMsg{proc: cmd.Process, exited: exited, log: logFile.Name()}
	}
}

// waitForServeCmd polls the control port until the session answers.
//
// It watches for the process dying as well: a serve that exits immediately -
// no devkit attached, or another process holding it - would otherwise be
// waited on for the full timeout before saying anything.
func waitForServeCmd(exited <-chan error, logPath string) tea.Cmd {
	return func() tea.Msg {
		deadline := time.Now().Add(serveStartTimeout)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, err := htc.PortMap(ctx, htc.ControlAddr())
			cancel()
			if err == nil {
				return serveReadyMsg{}
			}
			select {
			case <-exited:
				return serveReadyMsg{err: fmt.Errorf("the session exited immediately, see %s", logPath)}
			default:
			}
			if time.Now().After(deadline) {
				return serveReadyMsg{err: fmt.Errorf("the session did not come up within %v, see %s", serveStartTimeout, logPath)}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// killProcess is what stopServe uses. It is a variable so a test can watch
// the session being stopped without a real process to stop: a hand-built
// os.Process has no handle behind it, and killing one panics on Windows
// rather than returning an error.
var killProcess = func(p *os.Process) error { return p.Kill() }

// stopServe ends a session this TUI started. One that was already running is
// left alone: it belongs to whoever started it, and stopping it on the way out
// would drop whatever is attached through it.
func (m model) stopServe() {
	if m.serve != nil {
		killProcess(m.serve)
	}
}
