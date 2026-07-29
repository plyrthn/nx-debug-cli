package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plyrthn/nx-debug-cli/internal/nxdmp"
)

// dumpSubcommands is what `nxdbg dump ...` accepts.
var dumpSubcommands = []string{"read", "list"}

func cmdDump(rest []string) error {
	if !knownSubcommand(dumpSubcommands, rest[0]) {
		return unknownSubcommand("dump", rest[0], dumpSubcommands)
	}

	switch rest[0] {
	case "read":
		if len(rest) < 2 {
			return fmt.Errorf("usage: nxdbg dump read <file> [--all-threads]")
		}
		return cmdDumpRead(rest[1], rest[2:])
	case "list":
		if len(rest) < 2 {
			return fmt.Errorf("usage: nxdbg dump list <dir>")
		}
		return cmdDumpList(rest[1])
	default:
		return fmt.Errorf("unknown dump subcommand: %s", rest[0])
	}
}

// cmdDumpRead prints a text report for one .nxdmp file: exception
// code/address/thread, the module list, and the exception thread's
// registers and stack trace.
func cmdDumpRead(path string, flags []string) error {
	allThreads := false
	for _, f := range flags {
		switch f {
		case "--all-threads", "-a":
			allThreads = true
		default:
			return fmt.Errorf("unknown flag %q", f)
		}
	}

	d, err := nxdmp.Parse(path)
	if err != nil {
		return err
	}
	fmt.Print(d.Report(allThreads))
	if len(d.TTY) > 0 {
		fmt.Printf("TTY output:\n%s\n", d.TTY)
	}
	return nil
}

// cmdDumpList summarizes every .nxdmp file in a directory, one line each, so
// a folder of crash dumps (the devkit's default NXDMP directory fills up
// fast) can be triaged without opening each one by hand. A file that fails
// to parse is reported inline rather than aborting the rest of the listing.
func cmdDumpList(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".nxdmp") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Println("no .nxdmp files found")
		return nil
	}

	for _, name := range names {
		d, err := nxdmp.Parse(filepath.Join(dir, name))
		if err != nil {
			fmt.Printf("%s: %v\n", name, err)
			continue
		}
		fmt.Printf("%s: %s\n", name, d.Summary())
	}
	return nil
}
