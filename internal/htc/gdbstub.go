package htc

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GdbPortName is the well-known HTCS port the target's real GDB Remote
// Serial Protocol stub listens on. Unlike dmnt, this is not a proprietary
// protocol to reverse-engineer - the target genuinely speaks GDB's own
// standard remote protocol (the same one real gdb/lldb use over a serial
// line or TCP), so a client only has to implement that publicly documented
// wire format, not guess at it from captures.
const GdbPortName = "iywys@$gdb"

// GdbStub is a connection to the target's GDB stub.
type GdbStub struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex

	// Serial is the target this connection was resolved for.
	Serial string

	// lastThread is the most recently reported thread id, from whichever of
	// Attach/Step/Continue last got a stop reply - vCont's resume actions
	// are addressed to a specific thread, and this is the only one this
	// client has ever seen.
	lastThread uint64

	// hcUnsupported records that this stub already refused "Hc" once (see
	// legacyResume), so later legacy resume calls on this connection skip
	// straight to the resume packet instead of paying for a refusal every
	// time.
	hcUnsupported bool
}

// DialGdbStub resolves the target's GDB stub and opens it.
func DialGdbStub(ctx context.Context, serial string) (*GdbStub, error) {
	addr, err := resolvePortAddr(ctx, serial, GdbPortName)
	if err != nil {
		return nil, err
	}
	g, err := DialGdbStubAddr(ctx, addr)
	if err != nil {
		return nil, err
	}
	g.Serial = serial
	return g, nil
}

// DialGdbStubAddr opens an already-resolved GDB stub address. There is no
// handshake - the wire protocol is request/reply from the first packet.
func DialGdbStubAddr(ctx context.Context, addr string) (*GdbStub, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("htc: dial gdb stub %s: %w", addr, err)
	}
	return &GdbStub{conn: conn, r: bufio.NewReader(conn)}, nil
}

func (g *GdbStub) Close() error {
	return g.conn.Close()
}

// sendPacket writes one GDB remote protocol packet: "$<payload>#<checksum>",
// the checksum being the payload's bytes summed mod 256, in lowercase hex.
func (g *GdbStub) sendPacket(payload string) error {
	sum := 0
	for i := 0; i < len(payload); i++ {
		sum += int(payload[i])
	}
	_, err := fmt.Fprintf(g.conn, "$%s#%02x", payload, sum%256)
	return err
}

// ack sends the client's "+" acknowledgement of a reply packet - required
// after every reply, or the stub stalls waiting for it before answering
// anything else.
func (g *GdbStub) ack() error {
	_, err := g.conn.Write([]byte("+"))
	return err
}

// readReply reads one reply: the server's own "+"/"-" ack of the request
// just sent, then the actual "$...#cc" packet, RLE-decoded and with its
// trailing checksum stripped. Acknowledges the reply itself before
// returning, since the stub will not process a further request otherwise.
func (g *GdbStub) readReply(timeout time.Duration) (string, error) {
	if timeout > 0 {
		g.conn.SetReadDeadline(time.Now().Add(timeout))
		defer g.conn.SetReadDeadline(time.Time{})
	}
	first, err := g.r.ReadByte()
	if err != nil {
		return "", fmt.Errorf("htc: gdb stub: read ack: %w", err)
	}
	if first == '-' {
		return "", fmt.Errorf("htc: gdb stub: request was nacked")
	}
	if first != '+' {
		return "", fmt.Errorf("htc: gdb stub: expected +/- ack, got %q", first)
	}
	// Some replies are just the ack with nothing else pending briefly after;
	// give the actual packet a moment to arrive.
	dollar, err := g.r.ReadByte()
	if err != nil {
		return "", fmt.Errorf("htc: gdb stub: read reply: %w", err)
	}
	if dollar != '$' {
		return "", fmt.Errorf("htc: gdb stub: expected '$', got %q", dollar)
	}
	raw, err := g.r.ReadString('#')
	if err != nil {
		return "", fmt.Errorf("htc: gdb stub: read reply body: %w", err)
	}
	raw = raw[:len(raw)-1] // drop the trailing '#'
	checksum := make([]byte, 2)
	if _, err := g.r.Read(checksum); err != nil {
		return "", fmt.Errorf("htc: gdb stub: read checksum: %w", err)
	}
	if err := g.ack(); err != nil {
		return "", fmt.Errorf("htc: gdb stub: ack reply: %w", err)
	}
	return gdbRLEDecode(raw), nil
}

// roundTrip sends a request and returns its decoded reply.
func (g *GdbStub) roundTrip(payload string, timeout time.Duration) (string, error) {
	if err := g.sendPacket(payload); err != nil {
		return "", fmt.Errorf("htc: gdb stub: send %s: %w", payload, err)
	}
	reply, err := g.readReply(timeout)
	if err != nil {
		return "", fmt.Errorf("htc: gdb stub: %s: %w", payload, err)
	}
	return reply, nil
}

// gdbRLEDecode expands the run-length encoding the protocol allows in reply
// data: a byte followed by '*' and a repeat character means the byte
// repeats (that character's value - 29) additional times.
func gdbRLEDecode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '*' && i > 0 && i+1 < len(s) {
			count := int(s[i+1]) - 29
			for j := 0; j < count; j++ {
				b.WriteByte(s[i-1])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// gdbError reports whether a reply is an "Ennn" error and, if so, its code.
func gdbError(reply string) (code string, isError bool) {
	if len(reply) >= 3 && reply[0] == 'E' {
		return reply[1:], true
	}
	return "", false
}

// StopReason is one of the optional stop-reply reason fields GDB's remote
// protocol defines - which one, if any, a T-packet carries. A hardware
// instruction breakpoint hit reports StopHardwareBreak, a software
// (patched-instruction) breakpoint reports StopSoftwareBreak, and a data
// breakpoint reports one of StopWatch, StopRWatch or StopAWatch depending on
// whether it was armed for writes, reads, or both - real fields the target's
// stub emits for these, not just the plain thread/signal fields already
// handled below.
type StopReason string

const (
	StopHardwareBreak StopReason = "hwbreak"
	StopSoftwareBreak StopReason = "swbreak"
	StopWatch         StopReason = "watch"
	StopRWatch        StopReason = "rwatch"
	StopAWatch        StopReason = "awatch"
)

// StopReply is a decoded "T" stop-reply packet: the process/thread stopped
// (or exited), with whichever registers the stub chose to include inline.
type StopReply struct {
	Signal   int
	ThreadID uint64
	// Registers present in the T-packet itself, keyed by the GDB register
	// number the stub reported them under.
	Registers map[int]uint64
	Exited    bool
	ExitCode  int

	// Reason names why the stop happened, if the reply said so - empty for
	// a plain step or a signal with no more specific reason attached.
	Reason StopReason
	// WatchAddr is the data address that tripped a watchpoint, valid only
	// when Reason is StopWatch, StopRWatch or StopAWatch.
	WatchAddr uint64
}

// ErrNotSupported is returned when the stub answers a request with GDB's
// standard "unsupported" reply: an entirely empty packet body.
var ErrNotSupported = fmt.Errorf("htc: gdb stub: not supported by this stub")

func parseStopReply(reply string) (StopReply, error) {
	if len(reply) == 0 {
		return StopReply{}, ErrNotSupported
	}
	switch reply[0] {
	case 'W':
		code, err := strconv.ParseInt(reply[1:3], 16, 32)
		if err != nil {
			return StopReply{}, fmt.Errorf("htc: gdb stub: bad exit reply %q: %w", reply, err)
		}
		return StopReply{Exited: true, ExitCode: int(code)}, nil
	case 'T':
		if len(reply) < 3 {
			return StopReply{}, fmt.Errorf("htc: gdb stub: short T reply %q", reply)
		}
		sig, err := strconv.ParseInt(reply[1:3], 16, 32)
		if err != nil {
			return StopReply{}, fmt.Errorf("htc: gdb stub: bad signal in %q: %w", reply, err)
		}
		sr := StopReply{Signal: int(sig), Registers: map[int]uint64{}}
		for _, field := range strings.Split(strings.TrimSuffix(reply[3:], ";"), ";") {
			// Cut alone can't tell "no colon at all" (a malformed field,
			// worth skipping) apart from "colon with nothing after it" (a
			// real, valid field - hwbreak/swbreak carry no value at all,
			// see StopReason above). Only the former is worth dropping here.
			key, value, ok := strings.Cut(field, ":")
			if !ok {
				continue
			}
			switch StopReason(key) {
			case StopHardwareBreak, StopSoftwareBreak:
				sr.Reason = StopReason(key)
				continue
			case StopWatch, StopRWatch, StopAWatch:
				sr.Reason = StopReason(key)
				// Plain hex, same as the thread field just below - not the
				// little-endian byte-pair encoding the register fields use
				// elsewhere in this packet.
				if addr, err := strconv.ParseUint(value, 16, 64); err == nil {
					sr.WatchAddr = addr
				}
				continue
			}
			if key == "thread" {
				tid, err := strconv.ParseUint(value, 16, 64)
				if err == nil {
					sr.ThreadID = tid
				}
				continue
			}
			if value == "" {
				continue
			}
			regNum, err := strconv.ParseInt(key, 16, 32)
			if err != nil {
				continue
			}
			sr.Registers[int(regNum)] = gdbLEHexToUint64(value)
		}
		return sr, nil
	default:
		return StopReply{}, fmt.Errorf("htc: gdb stub: unrecognised stop reply %q", reply)
	}
}

// gdbLEHexToUint64 decodes a little-endian hex byte string (as GDB encodes
// register/memory values) into a uint64, right-padding short values.
func gdbLEHexToUint64(hexStr string) uint64 {
	var v uint64
	for i := 0; i+1 < len(hexStr) && i < 16; i += 2 {
		b, err := strconv.ParseUint(hexStr[i:i+2], 16, 8)
		if err != nil {
			break
		}
		v |= uint64(b) << (8 * (i / 2))
	}
	return v
}

// gdbUint64ToLEHex encodes a uint64 as little-endian hex bytes, width bytes
// wide (GDB register/memory writes are fixed-width per field).
func gdbUint64ToLEHex(v uint64, width int) string {
	var b strings.Builder
	for i := 0; i < width; i++ {
		fmt.Fprintf(&b, "%02x", byte(v>>(8*i)))
	}
	return b.String()
}

// Attach binds this connection to a running process by its kernel process
// ID (not a dmnt-style opaque handle - GDB's vAttach wants the real PID).
// The reply is the process's current stop state, same shape a breakpoint
// hit or a step would report.
func (g *GdbStub) Attach(pid uint64) (StopReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	reply, err := g.roundTrip(fmt.Sprintf("vAttach;%x", pid), 5*time.Second)
	if err != nil {
		return StopReply{}, err
	}
	if code, isErr := gdbError(reply); isErr {
		return StopReply{}, fmt.Errorf("htc: gdb stub: attach to pid %#x refused (E%s)", pid, code)
	}
	sr, err := parseStopReply(reply)
	if err == nil && sr.ThreadID != 0 {
		g.lastThread = sr.ThreadID
		if selErr := g.selectGeneralThread(sr.ThreadID); selErr != nil {
			return sr, selErr
		}
	}
	return sr, err
}

// selectGeneralThread sends Hg<tid>, which is what actually controls which
// thread "g"/"G"/"m"/"M"/"Z"/"z" (register, memory and breakpoint)
// operations apply to. vCont's own per-action thread suffix (see
// resumeThread) only controls resume actions - a separate selector this
// stub does not default sensibly on its own.
//
// Without this, ReadRegisters/ReadMemory/WriteMemory/SetBreakpoint/
// ClearBreakpoint silently acted on whatever thread happened to already be
// selected, not the one Attach/Step/Continue just reported stopped -
// confirmed live: attach reported the real exception thread in its stop
// reply, but an immediately following register read came back from a
// different, unrelated thread parked in SleepThread. Called with g.mu
// already held, same as roundTrip.
func (g *GdbStub) selectGeneralThread(tid uint64) error {
	reply, err := g.roundTrip(fmt.Sprintf("Hg%x", tid), 5*time.Second)
	if err != nil {
		return err
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: select thread %#x refused (E%s)", tid, code)
	}
	return nil
}

// Detach releases the target without stopping it, so a later Attach (from
// this or another client) is not refused as already-attached.
func (g *GdbStub) Detach() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	reply, err := g.roundTrip("D", 5*time.Second)
	if err != nil {
		return err
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: detach refused (E%s)", code)
	}
	return nil
}

// AArch64 GDB register numbers, per aarch64-core.xml: x0-x30 are 0-30, sp
// is 31, pc is 32, cpsr is 33.
const (
	gdbRegSP   = 31
	gdbRegPC   = 32
	gdbRegCPSR = 33
)

// Registers is the general-purpose AArch64 register set, decoded from a GDB
// "g" (read all registers) reply.
type Registers struct {
	X    [31]uint64
	SP   uint64
	PC   uint64
	CPSR uint32
}

// ReadRegisters reads the full register set via the standard "g" packet.
// AArch64's core register layout (x0..x30, sp, pc, cpsr, each little-endian
// hex) is fixed by aarch64-core.xml, confirmed live against this target's
// own qXfer:features:read:target.xml reply, so the offsets here are not
// guessed.
func (g *GdbStub) ReadRegisters() (Registers, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	reply, err := g.roundTrip("g", 5*time.Second)
	if err != nil {
		return Registers{}, err
	}
	if code, isErr := gdbError(reply); isErr {
		return Registers{}, fmt.Errorf("htc: gdb stub: read registers refused (E%s)", code)
	}
	// x0..x30: 31 registers * 8 bytes = 62 hex bytes each * 2 hex chars.
	const gpBytes = 31 * 8 * 2
	const spBytes = 8 * 2
	const pcBytes = 8 * 2
	if len(reply) < gpBytes+spBytes+pcBytes {
		return Registers{}, fmt.Errorf("htc: gdb stub: register dump too short (%d hex chars)", len(reply))
	}
	var regs Registers
	for i := 0; i < 31; i++ {
		regs.X[i] = gdbLEHexToUint64(reply[i*16 : i*16+16])
	}
	off := gpBytes
	regs.SP = gdbLEHexToUint64(reply[off : off+16])
	off += spBytes
	regs.PC = gdbLEHexToUint64(reply[off : off+16])
	off += pcBytes
	if len(reply) >= off+8 {
		regs.CPSR = uint32(gdbLEHexToUint64(reply[off : off+8]))
	}
	return regs, nil
}

// ReadMemory reads count bytes at addr via the standard "m" packet.
func (g *GdbStub) ReadMemory(addr uint64, count int) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	reply, err := g.roundTrip(fmt.Sprintf("m%x,%x", addr, count), 5*time.Second)
	if err != nil {
		return nil, err
	}
	if code, isErr := gdbError(reply); isErr {
		return nil, fmt.Errorf("htc: gdb stub: read memory at %#x refused (E%s)", addr, code)
	}
	data := make([]byte, len(reply)/2)
	for i := range data {
		v, err := strconv.ParseUint(reply[i*2:i*2+2], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("htc: gdb stub: malformed memory reply: %w", err)
		}
		data[i] = byte(v)
	}
	return data, nil
}

// WriteMemory writes data at addr via the standard "M" packet.
func (g *GdbStub) WriteMemory(addr uint64, data []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	var hexData strings.Builder
	for _, b := range data {
		fmt.Fprintf(&hexData, "%02x", b)
	}
	reply, err := g.roundTrip(fmt.Sprintf("M%x,%x:%s", addr, len(data), hexData.String()), 5*time.Second)
	if err != nil {
		return err
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: write memory at %#x refused (E%s)", addr, code)
	}
	if reply != "OK" {
		return fmt.Errorf("htc: gdb stub: write memory at %#x: unexpected reply %q", addr, reply)
	}
	return nil
}

// gdbBreakpointKind is the instruction-size hint AArch64 software
// breakpoints use in Z0/z0 packets - a fixed 4-byte A64 instruction.
const gdbBreakpointKind = 4

// SetBreakpoint installs a software breakpoint at addr via the standard
// "Z0" packet - the stub itself patches and restores the instruction, so
// this client never needs to know or store the original bytes.
func (g *GdbStub) SetBreakpoint(addr uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	reply, err := g.roundTrip(fmt.Sprintf("Z0,%x,%x", addr, gdbBreakpointKind), 10*time.Second)
	if err != nil {
		return err
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: set breakpoint at %#x refused (E%s)", addr, code)
	}
	if reply != "OK" {
		return fmt.Errorf("htc: gdb stub: set breakpoint at %#x: unexpected reply %q", addr, reply)
	}
	return nil
}

// ClearBreakpoint removes a previously set software breakpoint.
func (g *GdbStub) ClearBreakpoint(addr uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	reply, err := g.roundTrip(fmt.Sprintf("z0,%x,%x", addr, gdbBreakpointKind), 10*time.Second)
	if err != nil {
		return err
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: clear breakpoint at %#x refused (E%s)", addr, code)
	}
	if reply != "OK" {
		return fmt.Errorf("htc: gdb stub: clear breakpoint at %#x: unexpected reply %q", addr, reply)
	}
	return nil
}

// SetHardwareBreakpoint installs a hardware breakpoint at addr via the
// standard "Z1" packet - unlike SetBreakpoint this does not patch the
// instruction stream at all, the CPU's own debug registers trap execution
// there instead. Useful anywhere a software breakpoint can't go: read-only
// or hashed/verified code pages (this SDK's NSO segments carry a SHA256
// the loader checks, so a live byte patch there would only work if it was
// baked in before signing, not poked in after attach).
func (g *GdbStub) SetHardwareBreakpoint(addr uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	reply, err := g.roundTrip(fmt.Sprintf("Z1,%x,%x", addr, gdbBreakpointKind), 10*time.Second)
	if err != nil {
		return err
	}
	if reply == "" {
		return fmt.Errorf("htc: gdb stub: hardware breakpoint at %#x: not supported by this stub", addr)
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: set hardware breakpoint at %#x refused (E%s)", addr, code)
	}
	if reply != "OK" {
		return fmt.Errorf("htc: gdb stub: set hardware breakpoint at %#x: unexpected reply %q", addr, reply)
	}
	return nil
}

// ClearHardwareBreakpoint removes a previously set hardware breakpoint.
func (g *GdbStub) ClearHardwareBreakpoint(addr uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	reply, err := g.roundTrip(fmt.Sprintf("z1,%x,%x", addr, gdbBreakpointKind), 10*time.Second)
	if err != nil {
		return err
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: clear hardware breakpoint at %#x refused (E%s)", addr, code)
	}
	if reply != "OK" {
		return fmt.Errorf("htc: gdb stub: clear hardware breakpoint at %#x: unexpected reply %q", addr, reply)
	}
	return nil
}

// WatchpointKind selects which kind of memory access a hardware watchpoint
// traps on, matching the type digit GDB's remote protocol gives the "Z"/"z"
// packet: 2 for a write, 3 for a read, 4 for either.
type WatchpointKind int

const (
	WatchpointWrite  WatchpointKind = 2
	WatchpointRead   WatchpointKind = 3
	WatchpointAccess WatchpointKind = 4
)

func (k WatchpointKind) String() string {
	switch k {
	case WatchpointWrite:
		return "write"
	case WatchpointRead:
		return "read"
	case WatchpointAccess:
		return "access"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// validateWatchpointRange checks the alignment and length rules this stub's
// own watchpoint validation enforces before it will even try to program a
// debug register: a range of 8 bytes or less has to fit inside one aligned
// double-word, and anything longer has to be a power of two no larger than
// 0x80000000 with the address aligned to its own length. The server gates
// Z2/Z3/Z4 on this same check and, on failure, sends back the identical
// empty reply a genuinely unsupported watchpoint would - so failing this
// locally gives a real reason instead of a misleading not-supported error.
func validateWatchpointRange(addr, length uint64) error {
	if length == 0 {
		return fmt.Errorf("length cannot be 0")
	}
	if length <= 8 {
		base := addr &^ 7
		if base != (addr+length-1)&^7 {
			return fmt.Errorf("a range of 8 bytes or less must fit inside one aligned double-word (addr %#x, length %#x crosses one)", addr, length)
		}
		return nil
	}
	if length > 0x80000000 {
		return fmt.Errorf("length %#x is larger than the maximum (0x80000000)", length)
	}
	if length&(length-1) != 0 {
		return fmt.Errorf("length %#x over 8 bytes must be a power of two", length)
	}
	if addr&(length-1) != 0 {
		return fmt.Errorf("addr %#x is not aligned to length %#x", addr, length)
	}
	return nil
}

// SetWatchpoint installs a hardware watchpoint over the length bytes
// starting at addr via the standard "Z2"/"Z3"/"Z4" packets. Like a hardware
// breakpoint this traps through the CPU's own debug registers rather than
// patching anything, so it can watch data the target never executes and
// data on a page nothing lets this client write to directly.
func (g *GdbStub) SetWatchpoint(addr, length uint64, kind WatchpointKind) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := validateWatchpointRange(addr, length); err != nil {
		return fmt.Errorf("htc: gdb stub: %s watchpoint at %#x: %w", kind, addr, err)
	}

	reply, err := g.roundTrip(fmt.Sprintf("Z%d,%x,%x", kind, addr, length), 10*time.Second)
	if err != nil {
		return err
	}
	if reply == "" {
		return fmt.Errorf("htc: gdb stub: %s watchpoint at %#x: not supported by this stub", kind, addr)
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: set %s watchpoint at %#x refused (E%s)", kind, addr, code)
	}
	if reply != "OK" {
		return fmt.Errorf("htc: gdb stub: set %s watchpoint at %#x: unexpected reply %q", kind, addr, reply)
	}
	return nil
}

// ClearWatchpoint removes a previously set hardware watchpoint. length and
// kind must match what was passed to SetWatchpoint.
func (g *GdbStub) ClearWatchpoint(addr, length uint64, kind WatchpointKind) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	reply, err := g.roundTrip(fmt.Sprintf("z%d,%x,%x", kind, addr, length), 10*time.Second)
	if err != nil {
		return err
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: clear %s watchpoint at %#x refused (E%s)", kind, addr, code)
	}
	if reply != "OK" {
		return fmt.Errorf("htc: gdb stub: clear %s watchpoint at %#x: unexpected reply %q", kind, addr, reply)
	}
	return nil
}

// WriteRegister writes a single register via the standard "P" packet.
// regNum follows the same numbering ReadRegisters decodes (x0-x30 are
// 0-30, sp is 31, pc is 32, cpsr is 33); every register is 8 bytes except
// cpsr, which is 4, matching aarch64-core.xml.
func (g *GdbStub) WriteRegister(regNum int, value uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	width := 8
	if regNum == gdbRegCPSR {
		width = 4
	}
	reply, err := g.roundTrip(fmt.Sprintf("P%x=%s", regNum, gdbUint64ToLEHex(value, width)), 5*time.Second)
	if err != nil {
		return err
	}
	if reply == "" {
		return fmt.Errorf("htc: gdb stub: write register %d: not supported by this stub", regNum)
	}
	if code, isErr := gdbError(reply); isErr {
		return fmt.Errorf("htc: gdb stub: write register %d refused (E%s)", regNum, code)
	}
	if reply != "OK" {
		return fmt.Errorf("htc: gdb stub: write register %d: unexpected reply %q", regNum, reply)
	}
	return nil
}

// ErrResumeHung is what a vCont resume request (Step or Continue) turns
// into when the stub never replies at all, not even the initial ack -
// this stub's own observed failure mode for execution control, live-tested
// against a crashed process, a parked syscall thread, and three different
// healthy running applications (including a first-party Debug-config
// sample), all with the identical result. It is a harder failure than
// ErrNotSupported: the target does not release the pid afterward, so a
// later Attach to the same pid refuses until it's terminated or the
// console reboots.
var ErrResumeHung = errors.New("htc: gdb stub: no reply from the target (this stub advertises vCont step/continue via qSupported, but has never been observed to actually complete one; the pid is likely stuck attached now until it's terminated or the console reboots)")

// resumeThread returns the vCont thread-id suffix for the last thread this
// connection has seen stop, or "" if none is known yet, in which case the
// action applies as vCont's default to whatever is there.
func (g *GdbStub) resumeThread() string {
	if g.lastThread == 0 {
		return ""
	}
	return fmt.Sprintf(":%x", g.lastThread)
}

// vContResume sends one vCont resume action and reports where the target
// stopped. This is GDB's modern resume mechanism, superseding the legacy
// bare "s"/"c" packets - real gdb/lldb use it when a stub advertises it via
// qSupported's vContSupported+, which this stub's own qSupported reply
// does. Whether it actually works on real hardware is a separate question
// from whether it's the right thing to send; see ErrResumeHung.
func (g *GdbStub) vContResume(action string, timeout time.Duration) (StopReply, error) {
	reply, err := g.roundTrip("vCont;"+action+g.resumeThread(), timeout)
	if err != nil {
		if isTimeout(err) {
			return StopReply{}, ErrResumeHung
		}
		return StopReply{}, err
	}
	if code, isErr := gdbError(reply); isErr {
		return StopReply{}, fmt.Errorf("htc: gdb stub: %s refused (E%s)", action, code)
	}
	sr, err := parseStopReply(reply)
	if err == nil && sr.ThreadID != 0 {
		g.lastThread = sr.ThreadID
		if selErr := g.selectGeneralThread(sr.ThreadID); selErr != nil {
			return sr, selErr
		}
	}
	return sr, err
}

// isTimeout reports whether err is (or wraps) a network read deadline
// expiring, as opposed to any other kind of failure.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Step single-steps the current thread via vCont and reports where it
// stopped.
func (g *GdbStub) Step() (StopReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.vContResume("s", 15*time.Second)
}

// Continue resumes the target via vCont and blocks until it stops again (a
// breakpoint hit, a signal, or exit).
func (g *GdbStub) Continue() (StopReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.vContResume("c", 60*time.Second)
}

// legacyResume sends a bare "s"/"c" packet - the pre-vCont resume mechanism
// every gdb stub is required to support, since vCont itself is an optional
// extension a stub may advertise in qSupported without actually completing
// (see ErrResumeHung; this stub does exactly that on every real target
// tested here). Legacy step/continue apply to whichever thread "Hc" last
// selected, a third thread selector alongside Hg (selectGeneralThread) and
// vCont's own per-action suffix (resumeThread) - not interchangeable with
// either, so it's attempted explicitly here rather than assumed.
//
// A refusal of Hc itself (confirmed live: this project's own devkit answers
// E01) is not fatal - it means this stub doesn't support per-thread legacy
// selection, not that legacy resume itself won't work, so this falls
// through to sending the resume packet anyway rather than giving up before
// even trying it. Only a transport-level failure (not a well-formed E-code
// reply) aborts here.
func (g *GdbStub) legacyResume(action string, timeout time.Duration) (StopReply, error) {
	if g.lastThread != 0 && !g.hcUnsupported {
		hcReply, err := g.roundTrip(fmt.Sprintf("Hc%x", g.lastThread), 5*time.Second)
		if err != nil {
			return StopReply{}, err
		}
		if _, isErr := gdbError(hcReply); isErr {
			g.hcUnsupported = true
		}
	}
	reply, err := g.roundTrip(action, timeout)
	if err != nil {
		if isTimeout(err) {
			return StopReply{}, ErrResumeHung
		}
		return StopReply{}, err
	}
	if code, isErr := gdbError(reply); isErr {
		return StopReply{}, fmt.Errorf("htc: gdb stub: legacy %q refused (E%s)", action, code)
	}
	sr, err := parseStopReply(reply)
	if err == nil && sr.ThreadID != 0 {
		g.lastThread = sr.ThreadID
		if selErr := g.selectGeneralThread(sr.ThreadID); selErr != nil {
			return sr, selErr
		}
	}
	return sr, err
}

// StepLegacy is Step over the legacy "s" packet instead of vCont;s. Try this
// when Step returns ErrResumeHung.
func (g *GdbStub) StepLegacy() (StopReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.legacyResume("s", 15*time.Second)
}

// ContinueLegacy is Continue over the legacy "c" packet instead of vCont;c.
// Try this when Continue returns ErrResumeHung.
func (g *GdbStub) ContinueLegacy() (StopReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.legacyResume("c", 60*time.Second)
}

// ThreadIDs lists the attached process's live threads via the standard
// qfThreadInfo/qsThreadInfo query pair - real gdb's own mechanism for
// enumerating threads, and a real capability on the target's stub rather
// than an unimplemented query that just echoes back empty. qfThreadInfo
// answers with the whole thread list in one reply, and a follow-up
// qsThreadInfo always answers with no-more immediately after - but this
// still sends that follow-up at least once rather than assuming so, in case
// a given firmware's stub actually pages its reply instead.
func (g *GdbStub) ThreadIDs() ([]uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var ids []uint64
	packet := "qfThreadInfo"
	for {
		reply, err := g.roundTrip(packet, 5*time.Second)
		if err != nil {
			return nil, err
		}
		if reply == "" {
			return nil, ErrNotSupported
		}
		if code, isErr := gdbError(reply); isErr {
			return nil, fmt.Errorf("htc: gdb stub: list threads refused (E%s)", code)
		}
		if reply == "l" {
			break
		}
		if reply[0] != 'm' {
			return nil, fmt.Errorf("htc: gdb stub: list threads: unexpected reply %q", reply)
		}
		for _, tok := range strings.Split(reply[1:], ",") {
			if tok == "" {
				continue
			}
			tid, err := strconv.ParseUint(tok, 16, 64)
			if err != nil {
				return nil, fmt.Errorf("htc: gdb stub: list threads: bad thread id %q: %w", tok, err)
			}
			ids = append(ids, tid)
		}
		packet = "qsThreadInfo"
	}
	return ids, nil
}

// Module is one entry from the target's own live module list. Name is the
// module's link-time path exactly as recorded at build time - for anything
// built with this SDK's toolchain that means the unstripped ".nss" ELF
// (see internal/symbols), the same name this project's .nxdmp reader finds
// in a crash dump's module table, not a name invented for the wire.
type Module struct {
	Name string
	Load uint64
}

// Modules lists the shared objects loaded in the attached process via the
// standard "qXfer:libraries-svr4:read" query, the same mechanism real gdb
// uses to locate a module's runtime load address for symbolication.
func (g *GdbStub) Modules() ([]Module, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	data, err := g.xferRead("libraries-svr4", "")
	if err != nil {
		return nil, err
	}
	return parseLibraryListSvr4(data)
}

// xferRead reads one qXfer object in full, paging through "m" (more)
// replies until the stub marks one "l" (last) - a qXfer reply can be
// larger than fits in one packet, and nothing about the object's total
// size is known up front.
func (g *GdbStub) xferRead(object, annex string) (string, error) {
	const chunk = 0xfff
	var buf strings.Builder
	offset := 0
	for {
		pkt := fmt.Sprintf("qXfer:%s:read:%s:%x,%x", object, annex, offset, chunk)
		reply, err := g.roundTrip(pkt, 5*time.Second)
		if err != nil {
			return "", err
		}
		if len(reply) == 0 {
			return "", ErrNotSupported
		}
		kind, data := reply[0], gdbBinaryUnescape(reply[1:])
		buf.WriteString(data)
		if kind == 'l' {
			return buf.String(), nil
		}
		if kind != 'm' {
			return "", fmt.Errorf("htc: gdb stub: xfer %s: unexpected chunk marker %q", object, kind)
		}
		offset += len(data)
	}
}

// gdbBinaryUnescape reverses GDB's "binary data encoding": a '}' marks the
// next byte as escaped, XORed with 0x20 to get the real value - the only
// way the wire format can carry a literal '$', '#', '}' or '*' in data that
// isn't itself framed as a packet.
func gdbBinaryUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '}' && i+1 < len(s) {
			b.WriteByte(s[i+1] ^ 0x20)
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// libraryListSvr4 is the standard SVR4 shared library list qXfer reply,
// confirmed live against this target (see docs/protocol-notes.md, task #80
// addendum 3): a "library-list-svr4" element with one "library" child per
// loaded module, each carrying its own runtime load address.
type libraryListSvr4 struct {
	XMLName   xml.Name `xml:"library-list-svr4"`
	Libraries []struct {
		Name  string `xml:"name,attr"`
		LAddr string `xml:"l_addr,attr"`
	} `xml:"library"`
}

func parseLibraryListSvr4(data string) ([]Module, error) {
	var list libraryListSvr4
	if err := xml.Unmarshal([]byte(data), &list); err != nil {
		return nil, fmt.Errorf("htc: gdb stub: malformed library list: %w", err)
	}
	mods := make([]Module, 0, len(list.Libraries))
	for _, lib := range list.Libraries {
		addr, err := strconv.ParseUint(strings.TrimPrefix(lib.LAddr, "0x"), 16, 64)
		if err != nil {
			continue
		}
		mods = append(mods, Module{Name: lib.Name, Load: addr})
	}
	return mods, nil
}
