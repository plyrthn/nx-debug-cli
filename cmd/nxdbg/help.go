package main

import (
	"fmt"
	"strings"

	"github.com/plyrthn/nx-debug-cli/internal/commands"
)

// Help is generated from the command catalog rather than written out beside
// it. A hand-maintained help text is the first thing to go stale: it is the
// one place a missing entry causes no failure, so nothing ever catches it.

// printUsage is `nxdbg help`. It leads with what to do rather than with an
// alphabetical dump, because the first question is almost always "how do I get
// a session", not "what is the exact name of the workdir verb".
func printUsage() {
	fmt.Println(`nxdbg - open source NX devkit target management CLI/TUI

  nxdbg                       no args: the interactive TUI, which can run
                              everything below and needs nothing memorised

Getting a session. Everything else needs one:

  nxdbg serve                 drive the devkit yourself over USB, direct.
                              Nothing else to install. Leave it running.

Then, in another terminal:

  nxdbg htcs ports             which devkits are there, and what they publish
  nxdbg shell <serial> screenshot-fg out.png    see the screen, no decoding
  nxdbg logging watch <serial>                  the target log, live
  nxdbg gdb <serial>                            attach gdb/LLDB/VS Code/IDA/Ghidra`)

	fmt.Println("\nEveryday:")
	printCommands(commands.Everyday())

	fmt.Println("\nMore, by group. Run \"nxdbg help <group>\" for any of them:")
	groups := commands.AllGroups()
	width := 0
	for _, g := range groups {
		if n := len(g.Name); n > width {
			width = n
		}
	}
	for _, g := range groups {
		fmt.Printf("  nxdbg %-*s ...   %s\n", width, g.Name, g.Summary)
	}

	// The remaining top-level commands are the ones that did not earn a place
	// in the everyday list but are not in a group either.
	var rest []commands.Command
	for _, c := range commands.TopLevel() {
		if !c.Everyday {
			rest = append(rest, c)
		}
	}
	if len(rest) > 0 {
		fmt.Println("\nAlso:")
		printCommands(rest)
	}

	fmt.Println(`
Config file (key = value, # comments), see "nxdbg config path" for its location:
  output_dir    where locally-written video and image output goes by default

Commands marked "session" need "nxdbg serve" running.
Commands marked "usb" open the devkit's USB interface directly, so nothing
else may hold it while they run.`)
}

// printCommands renders an aligned block, with what each one needs in the
// margin. Knowing a command needs a session is the difference between "this is
// broken" and "start serve first", so it is worth the column - except when
// every command in the block needs the same thing, which is a sentence rather
// than a column repeated down the page.
func printCommands(cmds []commands.Command) {
	width := commands.UsageWidth(cmds)
	uniform := sharedNeed(cmds)
	for _, c := range cmds {
		need := ""
		if !uniform {
			need = needTag(c.Needs)
		}
		fmt.Printf("  %-*s  %s%s\n", width, c.Usage(), c.Summary, need)
	}
}

func needTag(n commands.Needs) string {
	switch n {
	case commands.NeedsSession:
		return "  [session]"
	case commands.NeedsDevice:
		return "  [usb]"
	}
	return ""
}

// sharedNeed reports whether every command in a block needs the same thing.
func sharedNeed(cmds []commands.Command) bool {
	for _, c := range cmds {
		if c.Needs != cmds[0].Needs {
			return false
		}
	}
	return len(cmds) > 0
}

// needSentence is how a requirement is stated in prose: once above a group's
// list instead of tagged onto every line in it, and again for a single
// command's own help.
type needSentence struct{ many, one string }

var needSentences = map[commands.Needs]needSentence{
	commands.NeedsSession: {
		"All of these need a session: `nxdbg serve`.",
		"Needs a session: `nxdbg serve`.",
	},
	commands.NeedsDevice: {
		"All of these open the devkit's USB interface, so nothing else may hold it.",
		"Opens the devkit's USB interface, so nothing else may hold it.",
	},
}

// printGroupUsage is `nxdbg help <group>`, and `nxdbg help <command>` too:
// nobody remembers which of the two a given word is, and answering "unknown
// group: launch" to a real command is a pointless dead end.
func printGroupUsage(group string) error {
	cmds := commands.InGroup(group)
	if len(cmds) == 0 {
		return printCommandUsage(group)
	}
	fmt.Printf("nxdbg %s - %s\n\n", group, commands.GroupOf(group).Summary)
	if note := groupNotes[group]; note != "" {
		fmt.Println(note)
		fmt.Println()
	}
	if sharedNeed(cmds) {
		if s := needSentences[cmds[0].Needs].many; s != "" {
			fmt.Printf("%s\n\n", s)
		}
	}
	printCommands(cmds)

	if old := aliasesFor(group); len(old) > 0 {
		fmt.Println("\nStill accepted, from before these were grouped:")
		for _, a := range old {
			fmt.Println("  " + a)
		}
	}
	return nil
}

// printCommandUsage answers `nxdbg help <command>` for a single command, and
// for anything that is neither a command nor a group it says so plus what the
// nearest matches were, since the usual cause is a half-remembered name.
func printCommandUsage(name string) error {
	if c, ok := commands.Find(name); ok {
		fmt.Printf("%s\n  %s\n", c.Usage(), c.Summary)
		if s := needSentences[c.Needs].one; s != "" {
			fmt.Printf("  %s\n", s)
		}
		if c.Destructive {
			fmt.Println("  This changes the state of the target.")
		}
		if c.Group != "" {
			fmt.Printf("\nOne of `nxdbg help %s`.\n", c.Group)
		}
		return nil
	}

	var near []string
	for _, c := range commands.All() {
		if strings.Contains(c.Path(), name) {
			near = append(near, "nxdbg help "+c.Path())
		}
	}
	if len(near) == 0 {
		return fmt.Errorf("no command or group called %q (try `nxdbg help`)", name)
	}
	return fmt.Errorf("no command or group called %q. Did you mean:\n  %s",
		name, strings.Join(near, "\n  "))
}

// groupNotes is the prose a group needs beyond its one-line summary: the
// caveats and the "which of these two routes do I want" explanations that do
// not fit in a per-command summary.
var groupNotes = map[string]string{
	"shell": `This is the target's own control service, reached over its HTCS port.
Screenshots here are the target compositing and handing back a finished
image, which is why they need no video decoding.

devmenu and launch-system are not available on every target: a devkit that
refuses them answers 2204-0001, which is the command shell saying no rather
than anything being wrong with the request.`,

	"input": `All of these talk to the target's iywys@$hid port directly. Use
"input status" to see whether the channel is published.`,

	"video": `These read the target's own stream directly.

The target sends no SPS/PPS, so "video record" writes the encoder's parameter
sets in front of the stream to make the file decodable. There is no keyframe
in it either, so a decoder needs -flags2 +showall to emit frames at all.`,

	"logging": `watch reads the target's log stream directly.`,

	"lock": `A file at %TEMP%\edev_<serial>.lock, for sessions sharing one devkit.
Check status before an exclusive action (serve, reboot, install, USB open,
gdb/debug attach); acquire before starting one; release when done. A lock
older than 5 minutes shows as stale, and acquire/release both refuse to
touch another session's live lock unless given --force.`,
}
