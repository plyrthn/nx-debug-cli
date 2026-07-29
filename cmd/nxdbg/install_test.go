package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseInstallArgs(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		storage string
		force   bool
		path    string
		wantErr bool
	}{
		{name: "defaults to the sd card and to overwriting", in: []string{"a.nsp"}, storage: "sdcard", force: true, path: "a.nsp"},
		{name: "builtin", in: []string{"a.nsp", "--builtin"}, storage: "builtin", force: true, path: "a.nsp"},
		{name: "auto", in: []string{"--auto", "a.nsp"}, storage: "auto", force: true, path: "a.nsp"},
		{name: "no-force", in: []string{"a.nsp", "--no-force"}, storage: "sdcard", path: "a.nsp"},
		{name: "no file", in: []string{"--builtin"}, wantErr: true},
		{name: "two files", in: []string{"a.nsp", "b.nsp"}, wantErr: true},
		{name: "unknown option", in: []string{"a.nsp", "--wherever"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseInstallArgs(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseInstallArgs(%q) = %+v, want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.storage != c.storage || got.force != c.force || got.path != c.path {
				t.Errorf("= %+v, want storage=%q force=%v path=%q", got, c.storage, c.force, c.path)
			}
		})
	}
}

// The argument string is what the target actually parses, so its shape matters
// more than the options struct it came from.
func TestInstallArgsBuildsTheDevMenuLine(t *testing.T) {
	got := installArgs(`R:\pkg\game.nsp`, installOptions{storage: "sdcard", force: true})
	want := `application install R:\pkg\game.nsp --force -s sdcard`
	if got != want {
		t.Errorf("= %q, want %q", got, want)
	}

	got = installArgs(`R:\pkg\game.nsp`, installOptions{storage: "builtin"})
	want = `application install R:\pkg\game.nsp -s builtin`
	if got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// The target log is shared, so an install prints alongside whatever else is
// running. Only the command's own output should be shown.
func TestIsDevMenuNoise(t *testing.T) {
	noise := []string{
		"",
		"   ",
		"[2026.07.26-22.10.15:000][393]LogCriWare: W2016041302:Output buffer underrun.",
	}
	for _, l := range noise {
		if !isDevMenuNoise(l) {
			t.Errorf("%q should be filtered out", l)
		}
	}
	keep := []string{
		"Preparing to install game.nsp...",
		"9192849408 / 9192849408",
		"[SUCCESS]",
		"Same or higher version nsp is already installed",
	}
	for _, l := range keep {
		if isDevMenuNoise(l) {
			t.Errorf("%q should be shown", l)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KB"},
		{9192850112, "8.6 GB"},
	}
	for _, c := range cases {
		if got := humanSize(c.in); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// install is a top-level command like any other, so it has to be reachable and
// described the same way.
func TestInstallIsWiredUp(t *testing.T) {
	if _, ok := findTopCommand("install"); !ok {
		t.Error("install is in the catalog but not dispatched")
	}
	defer quietStdout(t)()
	if err := printCommandUsage("install"); err != nil {
		t.Errorf("no help for install: %v", err)
	}
	for _, name := range []string{"uninstall", "apps"} {
		if _, ok := findTopCommand(name); !ok {
			t.Errorf("%s is in the catalog but not dispatched", name)
		}
	}
}

func TestWarnIfOutsideRoot(t *testing.T) {
	wd := filepath.Join(t.TempDir(), "work")
	cases := []struct {
		name    string
		abs     string
		wantMsg bool
	}{
		{"same directory", filepath.Join(wd, "game.nsp"), false},
		{"subdirectory", filepath.Join(wd, "pkg", "game.nsp"), false},
		{"outside entirely", filepath.Join(t.TempDir(), "other", "game.nsp"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := warnIfOutsideRoot(c.abs, wd)
			if c.wantMsg && got == "" {
				t.Errorf("warnIfOutsideRoot(%q, %q) = \"\", want a warning", c.abs, wd)
			}
			if !c.wantMsg && got != "" {
				t.Errorf("warnIfOutsideRoot(%q, %q) = %q, want none", c.abs, wd, got)
			}
		})
	}
}

func TestFormatDevMenuLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"9192849408 / 9192849408", "8.6 GB / 8.6 GB (100%)"},
		{"512 / 1024", "512 B / 1.0 KB (50%)"},
		{"[SUCCESS]", "[SUCCESS]"},
		{"Preparing to install game.nsp...", "Preparing to install game.nsp..."},
	}
	for _, c := range cases {
		if got := formatDevMenuLine(c.in); got != c.want {
			t.Errorf("formatDevMenuLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInstallRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	defer quietStdout(t)()
	err := cmdInstall(t.Context(), "SERIAL", []string{dir})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("err = %v, want it to say the path is a directory", err)
	}
}
