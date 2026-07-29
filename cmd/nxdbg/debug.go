package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/plyrthn/nx-debug-cli/internal/commands"
	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// The debug monitor is the target's own debug service - breakpoints,
// registers and memory, over iywys@$dmnt, an ordinary HTCS port like the
// command shell's. It only answers once a process is actually attached to:
// connecting with nothing running succeeds at the TCP level, but the target
// never answers the greeting, so every subcommand here just runs out its
// context deadline in that state rather than failing fast.

// debugSub is one debug-monitor verb, same shape as shellSub.
type debugSub struct {
	name string
	run  func(ctx context.Context, m *htc.DebugMonitor, rest []string) error
}

var debugSubs = []debugSub{
	{"banner", debugBanner},
	{"select", debugSelect},
	{"read", debugRead},
	{"modules", debugModules},
	{"threads", debugThreads},
}

// dmntDefaultListCount is how many indices to poll for `debug modules`/
// `debug threads` when the caller doesn't say. Nothing in the captured
// protocol reports a count directly (see ModuleAt's doc comment), so this
// is just a generous guess, not a confirmed limit.
const dmntDefaultListCount = 32

func findDebugSub(name string) (debugSub, bool) {
	for _, s := range debugSubs {
		if s.name == name {
			return s, true
		}
	}
	return debugSub{}, false
}

func runDebug(ctx context.Context, serial string, rest []string) error {
	sub, ok := findDebugSub(rest[0])
	if !ok {
		return fmt.Errorf("unknown debug subcommand: %s (try `nxdbg help debug`)", rest[0])
	}
	spec, ok := commands.Find("debug " + sub.name)
	if !ok {
		return fmt.Errorf("debug %s is dispatched but missing from the command catalog", sub.name)
	}
	// The serial is already consumed by the time this runs, so the target
	// placeholder is not part of what's left to check.
	if len(rest) < spec.MinArgs()-1 {
		return fmt.Errorf("usage: %s", spec.Usage())
	}
	m, err := htc.DialDebugMonitor(ctx, serial)
	if err != nil {
		return err
	}
	defer m.Close()
	return sub.run(ctx, m, rest)
}

func debugBanner(ctx context.Context, m *htc.DebugMonitor, rest []string) error {
	b := m.Banner
	fmt.Printf("Spec: %s\nTMA: %s\nTMIPC: %s\nConn: %s\nHW: %s\nBCID: %s\nPS: %s\nPMS: %s\nCD: %s\n",
		b.Spec, b.TMA, b.TMIPC, b.Conn, b.HW, b.BCID, b.PS, b.PMS, b.CD)
	return nil
}

func debugSelect(ctx context.Context, m *htc.DebugMonitor, rest []string) error {
	handle, err := parseDebugHandle(rest[1])
	if err != nil {
		return err
	}
	if err := m.SelectTarget(handle); err != nil {
		return err
	}
	fmt.Printf("selected target %#x\n", handle)
	return nil
}

func debugRead(ctx context.Context, m *htc.DebugMonitor, rest []string) error {
	handle, err := parseDebugHandle(rest[1])
	if err != nil {
		return err
	}
	addr, err := strconv.ParseUint(strings.TrimPrefix(rest[2], "0x"), 16, 64)
	if err != nil {
		return fmt.Errorf("bad address %q: %w", rest[2], err)
	}
	count, err := strconv.ParseUint(rest[3], 10, 32)
	if err != nil {
		return fmt.Errorf("bad count %q: %w", rest[3], err)
	}
	data, err := m.ReadMemory(handle, addr, uint32(count))
	if err != nil {
		return err
	}
	fmt.Print(hex.Dump(data))
	return nil
}

func debugModules(ctx context.Context, m *htc.DebugMonitor, rest []string) error {
	handle, err := parseDebugHandle(rest[1])
	if err != nil {
		return err
	}
	count, err := parseDebugListCount(rest)
	if err != nil {
		return err
	}
	for i := uint32(0); i < count; i++ {
		mod, err := m.ModuleAt(handle, i)
		if err != nil {
			if i == 0 {
				return err
			}
			break
		}
		fmt.Printf("%2d  %#010x  %#010x  %s\n", i, mod.Base, mod.Size, mod.Path)
	}
	return nil
}

func debugThreads(ctx context.Context, m *htc.DebugMonitor, rest []string) error {
	handle, err := parseDebugHandle(rest[1])
	if err != nil {
		return err
	}
	count, err := parseDebugListCount(rest)
	if err != nil {
		return err
	}
	for i := uint32(0); i < count; i++ {
		name, err := m.ThreadAt(handle, i)
		if err != nil {
			if i == 0 {
				return err
			}
			break
		}
		fmt.Printf("%2d  %s\n", i, name)
	}
	return nil
}

// parseDebugListCount reads the optional trailing count argument shared by
// `debug modules`/`debug threads`.
func parseDebugListCount(rest []string) (uint32, error) {
	if len(rest) <= 2 {
		return dmntDefaultListCount, nil
	}
	n, err := strconv.ParseUint(rest[2], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("bad count %q: %w", rest[2], err)
	}
	return uint32(n), nil
}

func parseDebugHandle(s string) (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 32)
	if err != nil {
		return 0, fmt.Errorf("bad handle %q: %w", s, err)
	}
	return uint32(v), nil
}
