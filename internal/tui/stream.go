package tui

import (
	"bufio"
	"io"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// A Streamed Long command (currently just install) is the one case where a
// detached, output-discarded run - the default for Long commands, since most
// of them open their own window (gdb, video) or run until interrupted
// (serve) - is the wrong call: there's no window, nothing else to look at,
// and the whole point of watching is the progress going by. This is that
// third mode: run it, and feed its combined stdout/stderr into the panel one
// line at a time as they arrive, the same lines a terminal running the plain
// CLI would see.

// streamStartedMsg carries a running command's line feed, as soon as there
// is one.
type streamStartedMsg struct {
	title string
	lines <-chan string
	done  <-chan error
	err   error
}

// streamLineMsg is one line of a streamed command's output.
type streamLineMsg struct {
	line string
}

// streamDoneMsg is a streamed command finishing, one way or another.
type streamDoneMsg struct {
	err error
}

// startStreamCmd runs a command with stdout and stderr merged into one live
// feed - the same merge CombinedOutput does for a normal (non-Long)
// command, just delivered as it happens instead of all at once when the
// process exits.
func startStreamCmd(self string, argv []string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command(self, argv...)
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw
		if err := cmd.Start(); err != nil {
			return streamStartedMsg{err: err}
		}
		lines := make(chan string, 64)
		done := make(chan error, 1)
		go func() {
			defer close(lines)
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
			for sc.Scan() {
				lines <- sc.Text()
			}
		}()
		go func() {
			err := cmd.Wait()
			pw.Close()
			done <- err
		}()
		return streamStartedMsg{title: "nxdbg " + strings.Join(argv, " "), lines: lines, done: done}
	}
}

// waitForStreamLineCmd reads one more line, or the command's exit once the
// line channel has closed. Update() calls this again after every
// streamLineMsg, the same read-one-then-come-back-for-more shape
// waitForServeCmd uses for polling, except here each line gets its own
// message instead of each poll, so the panel updates as output actually
// arrives rather than only at the end.
func waitForStreamLineCmd(lines <-chan string, done <-chan error) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-lines
		if ok {
			return streamLineMsg{line: line}
		}
		return streamDoneMsg{err: <-done}
	}
}
