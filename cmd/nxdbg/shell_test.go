package main

import (
	"testing"
)

// The shell group keeps its own table, because its handlers get an open
// connection rather than a daemon address. That means the checks the other
// groups get from TestEverySubcommandIsFullyDeclared have to be repeated here.
// Whether each verb is described is the catalog completeness tests' job.
func TestEveryShellSubIsFullyDeclared(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range shellSubs {
		if seen[s.name] {
			t.Errorf("shell %s: declared twice", s.name)
		}
		seen[s.name] = true
		if s.run == nil {
			t.Errorf("shell %s: no handler", s.name)
		}
	}
}

func TestFindShellSub(t *testing.T) {
	if _, ok := findShellSub("screenshot-fg"); !ok {
		t.Error("screenshot-fg is declared but not findable")
	}
	if _, ok := findShellSub("nonsense"); ok {
		t.Error("an undeclared verb was found")
	}
}

// A reboot or shutdown is answered by the target going away, so the connection
// dropping is the success case and must not be reported as a failure.
func TestShellWentAway(t *testing.T) {
	if !shellWentAway(errShellTimeout{}) {
		t.Error("a timeout was not treated as the target going away")
	}
	if shellWentAway(errShellOther{}) {
		t.Error("an unrelated error was treated as the target going away")
	}
}

type errShellTimeout struct{}

func (errShellTimeout) Error() string   { return "i/o timeout" }
func (errShellTimeout) Timeout() bool   { return true }
func (errShellTimeout) Temporary() bool { return false }

type errShellOther struct{}

func (errShellOther) Error() string { return "malformed reply" }
