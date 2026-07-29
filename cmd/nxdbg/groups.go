package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/plyrthn/nx-debug-cli/internal/commands"
)

// The command surface is deliberately shallow at the top and grouped below it.
// The handful of things you do constantly - list, connect, run something, look
// at the screen - stay one word deep. Everything rarer lives under a group, so
// `nxdbg help` reads as a short list rather than forty flat verbs of which
// thirty are for situations most people never hit.
//
// The grouping is a rename, not a removal. Every old flat name still works via
// commandAliases below.

// subcommand is one verb inside a group. Declaring these as data rather than
// as switch arms means the dispatcher, the alias table and the tests all read
// the same list, so a verb cannot be renamed in one place and missed in
// another - and the tests can check the wiring without calling anything that
// would try to reach a daemon.
//
// What the verb is called and what it does is all that lives here. Its usage
// line, its summary and how many arguments it needs come from the catalog in
// internal/commands, so there is one description of a command rather than one
// per place that needs to know about it.
type subcommand struct {
	name string
	run  func(ctx context.Context, rest []string) error
}

// group is a named set of subcommands.
type group struct {
	name string
	subs []subcommand
}

func (g group) find(name string) (subcommand, bool) {
	for _, s := range g.subs {
		if s.name == name {
			return s, true
		}
	}
	return subcommand{}, false
}

func (g group) run(ctx context.Context, rest []string) error {
	sub, ok := g.find(rest[0])
	if !ok {
		return fmt.Errorf("unknown %s subcommand: %s (try `nxdbg help %s`)", g.name, rest[0], g.name)
	}
	spec, ok := commands.Find(g.name + " " + sub.name)
	if !ok {
		return fmt.Errorf("%s %s is dispatched but missing from the command catalog", g.name, sub.name)
	}
	if len(rest) < spec.MinArgs() {
		return fmt.Errorf("usage: %s", spec.Usage())
	}
	return sub.run(ctx, rest)
}

var loggingGroup = group{"logging", []subcommand{
	// watch takes a serial and goes straight to the target, nothing else to
	// ask.
	{"watch", func(ctx context.Context, r []string) error {
		return cmdWatchLog(ctx, r[1], r[2:])
	}},
}}

// dispatchGroups are the groups that expandAlias can target. Groups handled
// elsewhere (input, video, htcs, config) predate this and keep their own
// dispatchers.
var dispatchGroups = []group{loggingGroup, lockGroup}

// commandAliases maps each retired flat command to its grouped form, so
// existing scripts and muscle memory keep working.
var commandAliases = map[string][]string{}

// expandAlias rewrites a retired flat command into its grouped form, leaving
// anything else alone.
func expandAlias(args []string) []string {
	if len(args) == 0 {
		return args
	}
	repl, ok := commandAliases[args[0]]
	if !ok {
		return args
	}
	out := make([]string, 0, len(repl)+len(args)-1)
	out = append(out, repl...)
	return append(out, args[1:]...)
}

// findGroup looks up one of the dispatched groups by name.
func findGroup(name string) (group, bool) {
	for _, g := range dispatchGroups {
		if g.name == name {
			return g, true
		}
	}
	return group{}, false
}

// aliasesFor lists the old names that now resolve into a group, sorted, for
// the group's own help output.
func aliasesFor(name string) []string {
	var out []string
	for old, repl := range commandAliases {
		if repl[0] == name {
			out = append(out, fmt.Sprintf("%s -> %s", old, strings.Join(repl, " ")))
		}
	}
	sort.Strings(out)
	return out
}
