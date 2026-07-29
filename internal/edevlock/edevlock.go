// Package edevlock reads and writes the informal lock file agents already
// use by hand to share a single devkit: a first line naming who holds it and
// when, followed by a free-text message for whatever the other session
// should know. It formalizes that convention rather than inventing a new
// one, so a file written by hand and one written by this package are
// interchangeable.
package edevlock

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StaleAfter is how old a lock can be before it's treated as abandoned
// rather than actively held, matching the ~5 minute convention already in
// use between sessions sharing a devkit.
const StaleAfter = 5 * time.Minute

// Lock is one session's claim on a devkit. The zero value means no lock is
// held.
type Lock struct {
	// Session names whoever wrote the lock, e.g. "claude-mario-kart-debug".
	Session string
	// Written is when the lock was last written.
	Written time.Time
	// Message is free text for the other session: what's being done, where
	// a handoff doc lives, anything worth leaving behind.
	Message string
}

// Path returns the lock file's location for a devkit serial:
// %TEMP%\edev_<serial>.lock (or the platform equivalent of os.TempDir()).
func Path(serial string) string {
	return filepath.Join(os.TempDir(), "edev_"+serial+".lock")
}

// Read returns the lock currently on file for a serial. A missing file is
// not an error; it just means the zero Lock, i.e. nothing is held.
func Read(serial string) (Lock, error) {
	f, err := os.Open(Path(serial))
	if os.IsNotExist(err) {
		return Lock{}, nil
	}
	if err != nil {
		return Lock{}, fmt.Errorf("edevlock: %w", err)
	}
	defer f.Close()

	l, err := parse(f)
	if err != nil {
		return Lock{}, fmt.Errorf("edevlock: %w", err)
	}
	return l, nil
}

// Write records a lock, overwriting whatever was there.
func Write(serial string, l Lock) error {
	body := l.Session + " " + l.Written.Format(time.RFC3339Nano)
	if l.Message != "" {
		body += "\n" + l.Message
	}
	if err := os.WriteFile(Path(serial), []byte(body), 0o644); err != nil {
		return fmt.Errorf("edevlock: %w", err)
	}
	return nil
}

// Clear removes a serial's lock file. Nothing on disk is not an error, since
// that's the same end state Clear is trying to reach.
func Clear(serial string) error {
	if err := os.Remove(Path(serial)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("edevlock: %w", err)
	}
	return nil
}

// Stale reports whether a lock is old enough to no longer count as actively
// held, as of now. A zero Lock (nothing on file) is always stale, so callers
// can use Stale to mean "safe to proceed" without a separate empty check.
func Stale(l Lock, now time.Time) bool {
	if l.Session == "" {
		return true
	}
	return now.Sub(l.Written) > StaleAfter
}

// parse reads the on-disk format: "<session> <RFC3339Nano timestamp>" on the
// first line, then whatever follows as Message. Split out from Read so the
// format is testable without touching the filesystem.
func parse(r io.Reader) (Lock, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Lock{}, err
	}
	if len(data) == 0 {
		return Lock{}, nil
	}

	firstLine, rest, _ := strings.Cut(string(data), "\n")
	firstLine = strings.TrimRight(firstLine, "\r")
	session, tsStr, ok := strings.Cut(firstLine, " ")
	if !ok {
		return Lock{}, fmt.Errorf("malformed first line %q", firstLine)
	}
	written, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return Lock{}, fmt.Errorf("parsing timestamp: %w", err)
	}
	return Lock{Session: session, Written: written, Message: rest}, nil
}
