package tui

import (
	"errors"
	"strings"
	"testing"
)

// waitForStreamLineCmd is the read-one-then-come-back-for-more primitive
// everything else here builds on, so it's worth pinning on its own: a line
// on the channel becomes a streamLineMsg, and the channel closing falls
// through to whatever done carries.
func TestWaitForStreamLineCmdReadsALineThenTheExit(t *testing.T) {
	lines := make(chan string, 1)
	done := make(chan error, 1)

	lines <- "installing game.nsp (8.6 GB) to sdcard"
	msg := waitForStreamLineCmd(lines, done)()
	line, ok := msg.(streamLineMsg)
	if !ok || line.line != "installing game.nsp (8.6 GB) to sdcard" {
		t.Fatalf("got %#v, want a streamLineMsg with the line", msg)
	}

	close(lines)
	done <- errors.New("exit status 1")
	msg = waitForStreamLineCmd(lines, done)()
	end, ok := msg.(streamDoneMsg)
	if !ok || end.err == nil || end.err.Error() != "exit status 1" {
		t.Fatalf("got %#v, want a streamDoneMsg carrying the exit error", msg)
	}
}

// A started stream sets up the panel and immediately asks for the first
// line - it must not sit idle waiting for a key press, since nothing the
// user does causes output to arrive.
func TestStreamStartedMsgSetsUpThePanelAndKeepsReading(t *testing.T) {
	lines := make(chan string)
	done := make(chan error)
	m := model{}
	next, cmd := m.Update(streamStartedMsg{title: "nxdbg install SERIAL game.nsp", lines: lines, done: done})
	got := next.(model)
	if got.panelTitle != "nxdbg install SERIAL game.nsp" {
		t.Errorf("panelTitle = %q, want the command line", got.panelTitle)
	}
	if got.panelBody != "" {
		t.Errorf("panelBody = %q, want empty until the first line arrives", got.panelBody)
	}
	if got.streamLines == nil || got.streamDone == nil {
		t.Error("the channels were not recorded, so Update can't keep reading")
	}
	if cmd == nil {
		t.Error("nothing was returned to read the first line")
	}
}

// A failure to even start the command has to be reported like any other
// failed action, not left as an empty, silently-stuck panel.
func TestStreamStartedMsgReportsAFailureToLaunch(t *testing.T) {
	m := model{}
	next, cmd := m.Update(streamStartedMsg{err: errors.New("locate own binary: not found")})
	got := next.(model)
	if !got.statusErr || !strings.Contains(got.statusMsg, "not found") {
		t.Errorf("status = (err=%v, %q), want the launch failure reported", got.statusErr, got.statusMsg)
	}
	if cmd != nil {
		t.Error("a failed launch should not try to read a line from nothing")
	}
}

// Each line appends to the panel with its own line break, the same as a
// terminal scrolling by - and each one has to trigger another read, or the
// stream stalls after the first line.
func TestStreamLineMsgAppendsToThePanelAndKeepsReading(t *testing.T) {
	m := model{streamLines: make(chan string), streamDone: make(chan error)}

	next, cmd := m.Update(streamLineMsg{line: "installing game.nsp (8.6 GB) to sdcard"})
	got := next.(model)
	if got.panelBody != "installing game.nsp (8.6 GB) to sdcard" {
		t.Errorf("panelBody = %q after the first line", got.panelBody)
	}
	if cmd == nil {
		t.Error("nothing was returned to read the next line")
	}

	next, _ = got.Update(streamLineMsg{line: "8.6 GB / 8.6 GB (100%)"})
	got = next.(model)
	want := "installing game.nsp (8.6 GB) to sdcard\n8.6 GB / 8.6 GB (100%)"
	if got.panelBody != want {
		t.Errorf("panelBody = %q, want %q", got.panelBody, want)
	}
}

// A clean finish reports ok and stops asking for more lines - reusing the
// stale channels after the command has already exited would just block
// forever.
func TestStreamDoneMsgReportsSuccessAndStopsReading(t *testing.T) {
	m := model{streamLines: make(chan string), streamDone: make(chan error)}
	next, cmd := m.Update(streamDoneMsg{})
	got := next.(model)
	if got.statusErr || got.statusMsg != "ok" {
		t.Errorf("status = (err=%v, %q), want a plain ok", got.statusErr, got.statusMsg)
	}
	if got.streamLines != nil || got.streamDone != nil {
		t.Error("the finished stream's channels were left recorded")
	}
	if cmd != nil {
		t.Error("a finished stream should not try to read another line")
	}
}

// install did not report success (a bad nsp, a refused overwrite, the
// target running out of space) has to surface the same way any other failed
// action does.
func TestStreamDoneMsgReportsFailure(t *testing.T) {
	m := model{streamLines: make(chan string), streamDone: make(chan error)}
	next, _ := m.Update(streamDoneMsg{err: errors.New("install did not report success")})
	got := next.(model)
	if !got.statusErr || !strings.Contains(got.statusMsg, "did not report success") {
		t.Errorf("status = (err=%v, %q), want the failure reported", got.statusErr, got.statusMsg)
	}
}
