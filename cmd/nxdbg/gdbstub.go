package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/plyrthn/nx-debug-cli/internal/commands"
	"github.com/plyrthn/nx-debug-cli/internal/htc"
	"github.com/plyrthn/nx-debug-cli/internal/symbols"
)

// The target's real GDB Remote Serial Protocol stub, over iywys@$gdb - not
// dmnt's proprietary wire format, but the same standard protocol a real
// gdb/lldb speaks. `nxdbg gdb <serial>` already forwards this port for use
// with an actual debugger client; these subcommands are one-shot scripting
// equivalents of the same operations - attach, read/write memory, registers,
// breakpoints, step and continue - for use without launching one.
//
// Each command attaches by kernel pid, does one thing, and detaches, the
// same one-shot-per-invocation shape as the debug group's dmnt commands. A
// real interactive session - several breakpoints, repeated stepping, all
// under one attach - is what `nxdbg gdb <serial>` plus an actual gdb/lldb is
// for.

type gdbstubSub struct {
	name string
	run  func(ctx context.Context, g *htc.GdbStub, rest []string) error
}

var gdbstubSubs = []gdbstubSub{
	{"attach", gdbstubAttach},
	{"regs", gdbstubRegs},
	{"setreg", gdbstubSetReg},
	{"read", gdbstubRead},
	{"write", gdbstubWrite},
	{"break", gdbstubBreak},
	{"clear", gdbstubClear},
	{"hwbreak", gdbstubHwBreak},
	{"hwclear", gdbstubHwClear},
	{"watch", gdbstubWatch},
	{"unwatch", gdbstubUnwatch},
	{"step", gdbstubStep},
	{"continue", gdbstubContinue},
	{"step-legacy", gdbstubStepLegacy},
	{"continue-legacy", gdbstubContinueLegacy},
	{"modules", gdbstubModules},
	{"threads", gdbstubThreads},
	{"backtrace", gdbstubBacktrace},
}

func findGdbstubSub(name string) (gdbstubSub, bool) {
	for _, s := range gdbstubSubs {
		if s.name == name {
			return s, true
		}
	}
	return gdbstubSub{}, false
}

func runGdbStub(ctx context.Context, serial string, rest []string) error {
	sub, ok := findGdbstubSub(rest[0])
	if !ok {
		return fmt.Errorf("unknown gdbstub subcommand: %s (try `nxdbg help gdbstub`)", rest[0])
	}
	spec, ok := commands.Find("gdbstub " + sub.name)
	if !ok {
		return fmt.Errorf("gdbstub %s is dispatched but missing from the command catalog", sub.name)
	}
	// The serial is already consumed by the time this runs, so the target
	// placeholder is not part of what's left to check.
	if len(rest) < spec.MinArgs()-1 {
		return fmt.Errorf("usage: %s", spec.Usage())
	}
	g, err := htc.DialGdbStub(ctx, serial)
	if err != nil {
		return err
	}
	defer g.Close()
	return sub.run(ctx, g, rest)
}

// printStopReply reports a StopReply the way every one of these commands
// wants it shown: sp/pc first since they're the ones worth reading at a
// glance, then whatever else the stub happened to include inline.
func printStopReply(sr htc.StopReply) {
	if sr.Exited {
		fmt.Printf("exited, code %d\n", sr.ExitCode)
		return
	}
	fmt.Printf("stopped: signal %d, thread %#x\n", sr.Signal, sr.ThreadID)
	switch sr.Reason {
	case htc.StopWatch, htc.StopRWatch, htc.StopAWatch:
		fmt.Printf("  reason %s, address %#x\n", sr.Reason, sr.WatchAddr)
	case htc.StopHardwareBreak, htc.StopSoftwareBreak:
		fmt.Printf("  reason %s\n", sr.Reason)
	}
	if v, ok := sr.Registers[gdbRegSPForDisplay]; ok {
		fmt.Printf("  sp  %#x\n", v)
	}
	if v, ok := sr.Registers[gdbRegPCForDisplay]; ok {
		fmt.Printf("  pc  %#x\n", v)
	}
	for reg, v := range sr.Registers {
		if reg == gdbRegSPForDisplay || reg == gdbRegPCForDisplay {
			continue
		}
		fmt.Printf("  x%d  %#x\n", reg, v)
	}
}

// gdbRegSPForDisplay/gdbRegPCForDisplay mirror the GDB register numbers
// StopReply keys its inline registers by (see aarch64-core.xml): sp is 31,
// pc is 32. Named here rather than imported since the numbering is part of
// the wire format, not something internal/htc exports as a constant.
const (
	gdbRegSPForDisplay = 31
	gdbRegPCForDisplay = 32
)

func gdbstubAttach(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	sr, err := g.Attach(pid)
	if err != nil {
		return err
	}
	printStopReply(sr)
	if len(rest) > 2 {
		printSymbol(g, sr, rest[2])
	}
	return g.Detach()
}

func gdbstubRegs(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	regs, err := g.ReadRegisters()
	if err != nil {
		return err
	}
	for i, x := range regs.X {
		fmt.Printf("x%-3d %#016x\n", i, x)
	}
	fmt.Printf("sp   %#016x\n", regs.SP)
	fmt.Printf("pc   %#016x\n", regs.PC)
	fmt.Printf("cpsr %#08x\n", regs.CPSR)
	return nil
}

func gdbstubSetReg(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	regNum, err := parseGdbRegName(rest[2])
	if err != nil {
		return err
	}
	value, err := strconv.ParseUint(strings.TrimPrefix(rest[3], "0x"), 16, 64)
	if err != nil {
		return fmt.Errorf("bad value %q: %w", rest[3], err)
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	if err := g.WriteRegister(regNum, value); err != nil {
		return err
	}
	fmt.Printf("wrote %s = %#x\n", strings.ToLower(rest[2]), value)
	return nil
}

func gdbstubRead(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	addr, err := parseGdbAddr(rest[2])
	if err != nil {
		return err
	}
	count, err := strconv.Atoi(rest[3])
	if err != nil {
		return fmt.Errorf("bad count %q: %w", rest[3], err)
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	data, err := g.ReadMemory(addr, count)
	if err != nil {
		return err
	}
	fmt.Print(hex.Dump(data))
	return nil
}

func gdbstubWrite(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	addr, err := parseGdbAddr(rest[2])
	if err != nil {
		return err
	}
	data, err := hex.DecodeString(strings.TrimPrefix(rest[3], "0x"))
	if err != nil {
		return fmt.Errorf("bad hex bytes %q: %w", rest[3], err)
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	if err := g.WriteMemory(addr, data); err != nil {
		return err
	}
	fmt.Printf("wrote %d bytes at %#x\n", len(data), addr)
	return nil
}

func gdbstubBreak(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	addr, err := parseGdbAddr(rest[2])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	if err := g.SetBreakpoint(addr); err != nil {
		return err
	}
	fmt.Printf("breakpoint set at %#x\n", addr)
	return nil
}

func gdbstubClear(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	addr, err := parseGdbAddr(rest[2])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	if err := g.ClearBreakpoint(addr); err != nil {
		return err
	}
	fmt.Printf("breakpoint cleared at %#x\n", addr)
	return nil
}

func gdbstubHwBreak(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	addr, err := parseGdbAddr(rest[2])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	if err := g.SetHardwareBreakpoint(addr); err != nil {
		return err
	}
	fmt.Printf("hardware breakpoint set at %#x\n", addr)
	return nil
}

func gdbstubHwClear(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	addr, err := parseGdbAddr(rest[2])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	if err := g.ClearHardwareBreakpoint(addr); err != nil {
		return err
	}
	fmt.Printf("hardware breakpoint cleared at %#x\n", addr)
	return nil
}

func gdbstubWatch(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	addr, err := parseGdbAddr(rest[2])
	if err != nil {
		return err
	}
	length, err := strconv.ParseUint(strings.TrimPrefix(rest[3], "0x"), 16, 64)
	if err != nil {
		return fmt.Errorf("bad length %q: %w", rest[3], err)
	}
	kind, err := parseWatchpointKind(rest[4])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	if err := g.SetWatchpoint(addr, length, kind); err != nil {
		return err
	}
	fmt.Printf("%s watchpoint set at %#x, length %#x\n", kind, addr, length)
	return nil
}

func gdbstubUnwatch(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	addr, err := parseGdbAddr(rest[2])
	if err != nil {
		return err
	}
	length, err := strconv.ParseUint(strings.TrimPrefix(rest[3], "0x"), 16, 64)
	if err != nil {
		return fmt.Errorf("bad length %q: %w", rest[3], err)
	}
	kind, err := parseWatchpointKind(rest[4])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	if err := g.ClearWatchpoint(addr, length, kind); err != nil {
		return err
	}
	fmt.Printf("%s watchpoint cleared at %#x, length %#x\n", kind, addr, length)
	return nil
}

func gdbstubStep(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	sr, err := g.Step()
	if err != nil {
		return err
	}
	printStopReply(sr)
	if len(rest) > 2 {
		printSymbol(g, sr, rest[2])
	}
	return nil
}

func gdbstubContinue(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	sr, err := g.Continue()
	if err != nil {
		return err
	}
	printStopReply(sr)
	if len(rest) > 2 {
		printSymbol(g, sr, rest[2])
	}
	return nil
}

// gdbstubStepLegacy and gdbstubContinueLegacy send the pre-vCont "s"/"c"
// packets instead - try these when step/continue return ErrResumeHung
// (every real devkit tested here hangs on vCont's resume actions despite
// advertising vContSupported+; see StepLegacy/ContinueLegacy).
func gdbstubStepLegacy(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	sr, err := g.StepLegacy()
	if err != nil {
		return err
	}
	printStopReply(sr)
	if len(rest) > 2 {
		printSymbol(g, sr, rest[2])
	}
	return nil
}

func gdbstubContinueLegacy(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	sr, err := g.ContinueLegacy()
	if err != nil {
		return err
	}
	printStopReply(sr)
	if len(rest) > 2 {
		printSymbol(g, sr, rest[2])
	}
	return nil
}

func gdbstubModules(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	mods, err := g.Modules()
	if err != nil {
		return err
	}
	for _, m := range mods {
		fmt.Printf("%#016x  %s\n", m.Load, m.Name)
	}
	return nil
}

func gdbstubThreads(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	if _, err := g.Attach(pid); err != nil {
		return err
	}
	defer g.Detach()
	ids, err := g.ThreadIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Printf("%#x\n", id)
	}
	return nil
}

// gdbstubBacktrace walks the AArch64 frame-pointer chain from the attach
// point and prints one line per frame - the manual version of this (find the
// runtime pc, translate it through the ASLR slide, disassemble to find the
// return address, repeat) was done by hand throughout this project's own OOM
// investigation before this existed. AAPCS64 fixes the frame layout every
// compiler here follows: x29 (fp) points at a stack pair {saved fp, saved
// lr}, so the chain is walkable with nothing but memory reads, no DWARF
// unwind info required.
//
// This stops at the first frame that doesn't look like a real one (a zero
// return address, an unreadable fp, or a saved fp that doesn't move further
// up the stack) rather than guessing past it, since a corrupt or truncated
// chain is exactly the kind of thing worth stopping to look at rather than
// reporting garbage frames for.
func gdbstubBacktrace(ctx context.Context, g *htc.GdbStub, rest []string) error {
	pid, err := parseGdbPid(rest[1])
	if err != nil {
		return err
	}
	symFile := ""
	if len(rest) > 2 {
		symFile = rest[2]
	}
	maxFrames := 32
	if len(rest) > 3 {
		n, err := strconv.Atoi(rest[3])
		if err != nil {
			return fmt.Errorf("bad max frames %q: %w", rest[3], err)
		}
		maxFrames = n
	}

	sr, err := g.Attach(pid)
	if err != nil {
		return err
	}
	defer g.Detach()
	if sr.Exited {
		return fmt.Errorf("process already exited (code %d)", sr.ExitCode)
	}
	regs, err := g.ReadRegisters()
	if err != nil {
		return err
	}

	mods, table := loadSymbolContext(g, symFile)

	printFrame := func(i int, pc uint64) {
		line := fmt.Sprintf("#%-2d %#016x", i, pc)
		if resolved := resolveModuleOffset(mods, table, symFile, pc); resolved != "" {
			line += "  " + resolved
		}
		fmt.Println(line)
	}

	printFrame(0, regs.PC)
	fp := regs.X[29]
	for i := 1; i < maxFrames && fp != 0; i++ {
		// AAPCS64 frame record: [fp+0] = saved fp, [fp+8] = saved lr.
		buf, err := g.ReadMemory(fp, 16)
		if err != nil || len(buf) < 16 {
			fmt.Printf("  (stack read failed at fp %#x: %v)\n", fp, err)
			break
		}
		savedFP := binary.LittleEndian.Uint64(buf[0:8])
		savedLR := binary.LittleEndian.Uint64(buf[8:16])
		if savedLR == 0 {
			break
		}
		printFrame(i, savedLR)
		if savedFP <= fp {
			// Frames sit at strictly increasing addresses walking up the
			// stack; anything else means the chain is corrupt or cyclic.
			break
		}
		fp = savedFP
	}
	return nil
}

// loadSymbolContext fetches the module list and, if symFile is given, its
// symbol table - the shared setup both printSymbol and gdbstubBacktrace need
// before resolving any address. Errors are reported inline and treated as
// "no symbol context available" rather than aborting the caller, since a
// backtrace with raw addresses is still useful without one.
func loadSymbolContext(g *htc.GdbStub, symFile string) ([]htc.Module, *symbols.Table) {
	mods, err := g.Modules()
	if err != nil {
		fmt.Printf("  modules: %v\n", err)
		return nil, nil
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Load < mods[j].Load })
	if symFile == "" {
		return mods, nil
	}
	table, err := symbols.Load(symFile)
	if err != nil {
		fmt.Printf("  symbols: %v\n", err)
		return mods, nil
	}
	return mods, table
}

// resolveModuleOffset finds which module pc falls in and formats it as
// "module+offset", or "module (symbol+offset)" when table's own file is the
// one containing pc. symFile is matched against the module list by base
// name, so it can be given as any local path to the matching .nss (or other
// ELF) - the module list itself only ever reports the build-time path, not
// wherever this copy happens to live now.
//
// The module list gives each module's load address but not its size, so
// which module a pc falls in has to come from the other modules around it:
// sorted by load address, pc belongs to whichever one has the highest load
// address at or below it - the same assumption real gdb makes from the same
// query, since Horizon's modules don't overlap.
func resolveModuleOffset(mods []htc.Module, table *symbols.Table, symFile string, pc uint64) string {
	if len(mods) == 0 {
		return ""
	}
	idx := sort.Search(len(mods), func(i int) bool { return mods[i].Load > pc }) - 1
	if idx < 0 {
		return "before every known module"
	}
	m := mods[idx]
	off := pc - m.Load
	name := path.Base(strings.ReplaceAll(m.Name, "\\", "/"))
	if table != nil && name == path.Base(strings.ReplaceAll(symFile, "\\", "/")) {
		if symName, delta, ok := table.Resolve(off); ok {
			return fmt.Sprintf("%s (%s+%#x)", name, symName, delta)
		}
		return fmt.Sprintf("%s+%#x (no matching symbol)", name, off)
	}
	return fmt.Sprintf("%s+%#x", name, off)
}

// printSymbol resolves a stop reply's pc the same way gdbstubBacktrace
// resolves each frame - see resolveModuleOffset for how.
func printSymbol(g *htc.GdbStub, sr htc.StopReply, symFile string) {
	pc, ok := sr.Registers[gdbRegPCForDisplay]
	if !ok {
		return
	}
	mods, table := loadSymbolContext(g, symFile)
	if resolved := resolveModuleOffset(mods, table, symFile, pc); resolved != "" {
		fmt.Printf("  in    %s\n", resolved)
	} else {
		fmt.Printf("  pc %#x is before every known module\n", pc)
	}
}

// parseGdbPid parses a kernel process id. Unlike dmnt's opaque handles,
// these are small Horizon PIDs, ordinarily written and read back in decimal.
func parseGdbPid(s string) (uint64, error) {
	pid, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad pid %q: %w", s, err)
	}
	return pid, nil
}

func parseGdbAddr(s string) (uint64, error) {
	addr, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("bad address %q: %w", s, err)
	}
	return addr, nil
}

// parseGdbRegName accepts the same register names gdbstubRegs prints
// (x0..x30, sp, pc, cpsr) and returns the GDB register number ReadRegisters/
// WriteRegister use.
func parseGdbRegName(s string) (int, error) {
	switch strings.ToLower(s) {
	case "sp":
		return 31, nil
	case "pc":
		return 32, nil
	case "cpsr":
		return 33, nil
	}
	if lower := strings.ToLower(s); strings.HasPrefix(lower, "x") {
		if n, err := strconv.Atoi(lower[1:]); err == nil && n >= 0 && n <= 30 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("bad register name %q (want x0-x30, sp, pc, or cpsr)", s)
}

func parseWatchpointKind(s string) (htc.WatchpointKind, error) {
	switch strings.ToLower(s) {
	case "write":
		return htc.WatchpointWrite, nil
	case "read":
		return htc.WatchpointRead, nil
	case "access", "rw", "readwrite":
		return htc.WatchpointAccess, nil
	}
	return 0, fmt.Errorf("bad watchpoint kind %q (want write, read, or access)", s)
}
