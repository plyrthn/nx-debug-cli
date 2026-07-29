package htc

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/plyrthn/nx-debug-cli/internal/targetlog"
)

// DevMenuCommand is the target's own command-line tool: it installs and removes
// applications, reads storage sizes, and carries most of the devkit settings
// that have no RPC of their own. Running one is how an nsp gets installed, and
// none of it involves the daemon - the shell that launches it is an ordinary
// HTCS port, the output comes back on another, and the nsp itself is read off
// the host over HTCFS.
//
// The awkward part is that those are three separate channels. The command shell
// says only that the program launched and, later, that it exited; everything
// the program actually reported - progress, the reason it refused, the
// [SUCCESS] or [FAILURE] it ends on - arrives on the target log. So a run that
// means anything has to be reading the log before it launches anything, which
// is what RunDevMenu does and why it is not simply a shell call.
//
// There are two ways to read it, and which one works depends on who else is
// connected. `iywys@$LogManager` is the target's real log, the one it publishes
// itself and always keeps open, in its own packed binary form; `@Log` is the
// daemon's own decode of that same stream, republished as plain text for other
// tools to read without doing the decoding themselves. When a daemon is running
// it already holds the one open connection LogManager answers to a session's
// first reader, so a second reader (this one, or `nxdbg logging watch`) reading
// LogManager directly gets nothing - proven by tapping it directly during a
// devmenu run under a live daemon and getting zero bytes while @Log, in the
// same moment, carried the output verbatim. Under `nxdbg serve`, with no daemon
// to have taken that slot first, LogManager answers directly and there is
// nothing to bridge - proven the same way, watching a devmenu run under serve
// with nothing else attached, and reading the output straight off it.
//
// So the two are not two implementations of the same thing so much as the same
// data from two different distances, and which one is reachable is decided by
// who got there first, not by what this process asks for.

// LogService is the registry key of the daemon's decoded plain target log.
const LogService = "log"

// devMenuProcessName is what the target itself calls the process, confirmed
// against a live run's decoded records.
const devMenuProcessName = "DevMenuCommand"

// devMenuSuccess and devMenuFailure are the last thing a DevMenu command
// prints. There is no exit status on the wire, so these are how a run is known
// to be over.
const (
	devMenuSuccess = "[SUCCESS]"
	devMenuFailure = "[FAILURE]"
)

// DevMenuRun is how a DevMenu command ended.
type DevMenuRun struct {
	// Lines is everything the command printed, in order.
	Lines []string
	// Succeeded is whether it finished on [SUCCESS].
	Succeeded bool
}

// DevMenuError is a DevMenu command that ran and reported failure. The output
// is carried along because the reason is in it and nowhere else - the shell
// reports the same launch-and-exit either way.
type DevMenuError struct {
	Args  string
	Lines []string
}

func (e *DevMenuError) Error() string {
	if reason := lastMeaningfulLine(e.Lines); reason != "" {
		return fmt.Sprintf("devmenu %s failed: %s", e.Args, reason)
	}
	return fmt.Sprintf("devmenu %s failed", e.Args)
}

// lastMeaningfulLine picks the line most likely to say why something failed:
// the last one before the [FAILURE] marker, since DevMenu prints the reason
// and then the marker.
func lastMeaningfulLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" || s == devMenuFailure || s == devMenuSuccess {
			continue
		}
		return s
	}
	return ""
}

// RunDevMenu runs a DevMenu command on the target and reports what it printed.
//
// onLine, if given, is called for each line as it arrives, so a caller can show
// progress on a long install rather than waiting for the whole thing.
func RunDevMenu(ctx context.Context, serial, args string, onLine func(string)) (*DevMenuRun, error) {
	// The log has to be open before the command starts, or its first lines are
	// gone before anything is listening for them.
	lines, closeLog, logErr := openDevMenuLog(ctx, serial)
	if logErr == nil {
		defer closeLog()
	}

	shell, err := DialCommandShell(ctx, serial)
	if err != nil {
		return nil, err
	}
	defer shell.Close()

	if _, err := shell.DevMenuCommand(ctx, args); err != nil {
		return nil, err
	}

	if lines == nil {
		return waitForDevMenuExit(ctx, shell, args, logErr)
	}

	run := &DevMenuRun{}
	for {
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case line, ok := <-lines:
			if !ok {
				if len(run.Lines) == 0 {
					return run, fmt.Errorf("the target log closed before devmenu %s finished (no output arrived at all - this is LogManager's single-reader slot stuck from an earlier session that never disconnected cleanly, not this command; a target reboot clears it: `nxdbg shell %s reboot`)", args, serial)
				}
				return run, fmt.Errorf("the target log closed before devmenu %s finished", args)
			}
			run.Lines = append(run.Lines, line)
			if onLine != nil {
				onLine(line)
			}
			switch strings.TrimSpace(line) {
			case devMenuSuccess:
				run.Succeeded = true
				return run, nil
			case devMenuFailure:
				return run, &DevMenuError{Args: args, Lines: run.Lines}
			}
		}
	}
}

// UnreadableError says a command ran to completion but nothing could be read
// back, because the target log could not be opened.
//
// This is deliberately an error rather than a quiet success. A DevMenu command
// reports what it did only through its output, so with no output there is no
// difference on the wire between an install that worked and one the target
// refused - and reporting the latter as success is exactly the failure worth
// avoiding here. The reason the log could not be opened is carried along,
// because "not published" and "published but the dial failed" want different
// things done about them.
type UnreadableError struct {
	Err error
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("the command ran, but its result could not be read: %v", e.Err)
}

func (e *UnreadableError) Unwrap() error { return e.Err }

// waitForDevMenuExit waits for the command to finish without being able to
// read what it said.
func waitForDevMenuExit(ctx context.Context, shell *CommandShell, args string, logErr error) (*DevMenuRun, error) {
	if err := shell.SubscribeProcessEvents(ctx, true); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case e, ok := <-shell.Events:
			if !ok {
				return nil, fmt.Errorf("the command shell closed before devmenu %s finished", args)
			}
			if e.Kind == "exited" {
				return &DevMenuRun{}, &UnreadableError{Err: logErr}
			}
		}
	}
}

// openDevMenuLog opens whichever devmenu log source is reachable and returns
// a channel of DevMenuCommand's own lines, closed when the source runs out.
//
// The fanout `nxdbg serve` publishes (see DevMenuLogFanoutPortName) is tried
// first: it's the only path that can be reused across multiple devmenu
// invocations in one session without burning the target's single accept
// slot. dialDirectDevMenuLog is the fallback, for a plain daemon session or
// an nxdbg serve build too old to publish the fanout - each call then pays
// that one-time cost on its own, same as before this existed.
func openDevMenuLog(ctx context.Context, serial string) (<-chan string, func(), error) {
	if conn, err := dialPort(ctx, serial, DevMenuLogFanoutPortName); err == nil {
		lines := make(chan string, 64)
		go readLogLines(conn, lines)
		return lines, func() { conn.Close() }, nil
	} else if _, ok := err.(*PortNotPublishedError); !ok {
		return nil, nil, err
	}
	return dialDirectDevMenuLog(ctx, serial)
}

// dialDirectDevMenuLog talks to the target's own log services with nothing
// in between. This is what the fanout uses for its one upstream connection,
// and what openDevMenuLog falls back to when no fanout is published - trying
// the plain decoded log first, since it needs no decoding here, and falling
// back to the target's own binary log, decoded here instead, if that isn't
// published.
func dialDirectDevMenuLog(ctx context.Context, serial string) (<-chan string, func(), error) {
	if conn, err := dialPort(ctx, serial, LogService); err == nil {
		lines := make(chan string, 64)
		go readLogLines(conn, lines)
		return lines, func() { conn.Close() }, nil
	} else if _, ok := err.(*PortNotPublishedError); !ok {
		return nil, nil, err
	}

	rd, conn, err := DialTargetLog(ctx, serial)
	if err != nil {
		return nil, nil, err
	}
	lines := make(chan string, 64)
	go readDevMenuRecords(rd, lines)
	return lines, func() { conn.Close() }, nil
}

// dialPort opens a plain-text, newline-delimited port by name.
func dialPort(ctx context.Context, serial, port string) (net.Conn, error) {
	entry, err := ResolvePort(ctx, serial, port)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", entry.Addr())
	if err != nil {
		return nil, fmt.Errorf("htc: dial %s %s: %w", port, entry.Addr(), err)
	}
	return conn, nil
}

// readLogLines forwards the log a line at a time until it closes.
//
// The log is plain text with no framing, and it carries whatever else on the
// target is logging at the time, so the caller filters rather than this.
func readLogLines(conn net.Conn, out chan<- string) {
	defer close(out)
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		out <- strings.TrimRight(sc.Text(), "\r")
	}
}

// readDevMenuRecords decodes the target's own log and reconstructs
// DevMenuCommand's console output from it: one record per write, concatenated
// in arrival order exactly as written, then split on the newlines those writes
// actually carry. That reconstruction is what makes this equivalent to reading
// @Log rather than an approximation of it - confirmed by decoding a live run
// and comparing the result byte for byte against a raw capture of @Log for the
// same command.
//
// The target log is shared by everything running, so only records from
// DevMenuCommand's own process are kept.
func readDevMenuRecords(rd *targetlog.Reader, out chan<- string) {
	defer close(out)
	var buf strings.Builder
	for {
		rec, err := rd.Next()
		if err != nil {
			return
		}
		if rec.ProcessName != devMenuProcessName {
			continue
		}
		buf.WriteString(rec.Text)
		for {
			s := buf.String()
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				break
			}
			out <- strings.TrimRight(s[:i], "\r")
			buf.Reset()
			buf.WriteString(s[i+1:])
		}
	}
}

// DevMenuTimeout is how long a DevMenu run is given when the caller has no
// deadline of its own. Installing a large nsp is minutes of copying over USB,
// so this is generous rather than responsive.
const DevMenuTimeout = 2 * time.Hour
