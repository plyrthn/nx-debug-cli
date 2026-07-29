package edevlock

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  Lock
	}{
		{"empty", "", Lock{}},
		{
			"session and timestamp, no message",
			"claude-mario-kart-debug 2026-07-28T16:21:39.0490978-04:00",
			Lock{
				Session: "claude-mario-kart-debug",
				Written: mustParseTime(t, "2026-07-28T16:21:39.0490978-04:00"),
			},
		},
		{
			"a message follows the first line",
			"claude-mario-kart-debug 2026-07-28T16:21:39.0490978-04:00\n" +
				"status: handing off, see R:\\Downloads\\mk8d-dev\\CLAUDE.md",
			Lock{
				Session: "claude-mario-kart-debug",
				Written: mustParseTime(t, "2026-07-28T16:21:39.0490978-04:00"),
				Message: "status: handing off, see R:\\Downloads\\mk8d-dev\\CLAUDE.md",
			},
		},
		{
			"multi-line message preserved as-is",
			"agent 2026-07-28T16:21:39-04:00\nline one\nline two\n",
			Lock{
				Session: "agent",
				Written: mustParseTime(t, "2026-07-28T16:21:39-04:00"),
				Message: "line one\nline two\n",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parse(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("parse() error = %v", err)
			}
			if !got.Written.Equal(tc.want.Written) || got.Session != tc.want.Session || got.Message != tc.want.Message {
				t.Errorf("parse() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseMalformedFirstLine(t *testing.T) {
	if _, err := parse(strings.NewReader("no-timestamp-here")); err == nil {
		t.Error("parse() with no space in the first line succeeded, want error")
	}
	if _, err := parse(strings.NewReader("session not-a-timestamp")); err == nil {
		t.Error("parse() with an unparsable timestamp succeeded, want error")
	}
}

func TestPath(t *testing.T) {
	path := Path("<serial>")
	if !strings.Contains(path, "edev_<serial>.lock") {
		t.Errorf("Path() = %q, want it to contain \"edev_<serial>.lock\"", path)
	}
}

func TestReadMissingFileReturnsZeroValue(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TEMP", t.TempDir())
	t.Setenv("TMP", t.TempDir())
	l, err := Read("no-such-serial")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if l != (Lock{}) {
		t.Errorf("Read() with no lock file = %+v, want zero value", l)
	}
}

func TestWriteThenRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)

	want := Lock{
		Session: "claude-gta5nx-debug",
		Written: mustParseTime(t, "2026-07-28T20:00:00Z"),
		Message: "patched MountSaveData, still crashes",
	}
	if err := Write("SERIAL1", want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := Read("SERIAL1")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !got.Written.Equal(want.Written) || got.Session != want.Session || got.Message != want.Message {
		t.Errorf("Read() after Write() = %+v, want %+v", got, want)
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)

	if err := Write("SERIAL1", Lock{Session: "agent", Written: time.Now()}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := Clear("SERIAL1"); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	got, err := Read("SERIAL1")
	if err != nil {
		t.Fatalf("Read() after Clear() error = %v", err)
	}
	if got != (Lock{}) {
		t.Errorf("Read() after Clear() = %+v, want zero value", got)
	}
	// Clearing an already-clear lock is not an error.
	if err := Clear("SERIAL1"); err != nil {
		t.Errorf("Clear() on an already-missing file: %v", err)
	}
}

func TestStale(t *testing.T) {
	now := mustParseTime(t, "2026-07-28T20:00:00Z")
	cases := []struct {
		name string
		l    Lock
		want bool
	}{
		{"no lock held", Lock{}, true},
		{"just written", Lock{Session: "agent", Written: now}, false},
		{"four minutes old", Lock{Session: "agent", Written: now.Add(-4 * time.Minute)}, false},
		{"six minutes old", Lock{Session: "agent", Written: now.Add(-6 * time.Minute)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Stale(tc.l, now); got != tc.want {
				t.Errorf("Stale() = %v, want %v", got, tc.want)
			}
		})
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parsing test timestamp %q: %v", s, err)
	}
	return tm
}
