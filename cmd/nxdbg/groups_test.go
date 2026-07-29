package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// captureUsage runs printUsage with stdout redirected, so the test can assert
// on what a user actually sees rather than on a duplicate copy of the text.
//
// The read has to run concurrently with printUsage's writes: the catalog is
// well past what an OS pipe buffers before a reader drains it, so writing
// everything first and reading after deadlocks instead of just running slow.
func captureUsage(t *testing.T) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w

	read := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		read <- string(out)
	}()

	printUsage()
	w.Close()
	os.Stdout = old

	return <-read
}

// expandAlias must leave everything that isn't a retired name alone, including
// the empty argument list.
func TestExpandAliasLeavesLiveCommandsAlone(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"serve"},
		{"logging", "watch", "SERIAL"},
		{"input", "SERIAL", "tap", "1", "2"},
	} {
		got := expandAlias(args)
		if len(got) != len(args) {
			t.Errorf("expandAlias(%v) = %v, wanted it unchanged", args, got)
		}
	}
}

// Each alias must point at a subcommand its group actually declares. This
// reads the tables rather than calling the handlers, so it stays a pure unit
// test with nothing to dial.
func TestGroupsDeclareEverySubcommandAnAliasPointsAt(t *testing.T) {
	for old, repl := range commandAliases {
		g, ok := findGroup(repl[0])
		if !ok {
			t.Errorf("%s: no dispatcher for group %q", old, repl[0])
			continue
		}
		if _, ok := g.find(repl[1]); !ok {
			t.Errorf("%s -> %s: group does not declare it", old, strings.Join(repl, " "))
		}
	}
}

// Every declared subcommand needs a handler and a unique name. What it is
// called and what it does is all a group declares now: the usage line and the
// argument count come from the catalog, which the completeness tests check it
// has an entry in.
func TestEverySubcommandIsFullyDeclared(t *testing.T) {
	for _, g := range dispatchGroups {
		seen := map[string]bool{}
		for _, s := range g.subs {
			if seen[s.name] {
				t.Errorf("%s %s: declared twice", g.name, s.name)
			}
			seen[s.name] = true
			if s.run == nil {
				t.Errorf("%s %s: no handler", g.name, s.name)
			}
		}
	}
}

// An unknown verb has to name the group it was unknown in, since "unknown
// command: on" from a mistyped group is otherwise baffling.
func TestUnknownSubcommandNamesItsGroup(t *testing.T) {
	err := loggingGroup.run(context.Background(), []string{"explode"})
	if err == nil || !strings.Contains(err.Error(), "unknown logging subcommand") {
		t.Errorf("got %v, want an error naming the logging group", err)
	}
}

// A verb given too few arguments must be told what the command takes, in the
// same words the help uses.
func TestTooFewArgumentsPrintsTheCatalogUsage(t *testing.T) {
	err := loggingGroup.run(context.Background(), []string{"watch"})
	if err == nil || !strings.Contains(err.Error(), "nxdbg logging watch <serial> [seconds]") {
		t.Errorf("got %v, want the usage line from the catalog", err)
	}
}

// Retired names must not collide with a live command or a group, or the alias
// would shadow it.
func TestAliasesDoNotShadowLiveCommands(t *testing.T) {
	for _, name := range topLevelNames() {
		if _, ok := commandAliases[name]; ok {
			t.Errorf("%q is both a live command and an alias", name)
		}
	}
	for name := range groupDispatch {
		if _, ok := commandAliases[name]; ok {
			t.Errorf("%q is both a group and an alias", name)
		}
	}
}
