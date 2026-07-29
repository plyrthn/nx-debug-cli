package htc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// DmntPortName is the well-known HTCS port the debug monitor listens on.
// It handles breakpoints, memory access and stepping for a JIT-debug
// attached process, and it is an ordinary HTCS port like the rest in this
// package, so it works the same daemon-free way against nxdbg serve.
const DmntPortName = "iywys@$dmnt"

// dmntTMIPCVersion is the protocol version this client declares in its
// connection greeting. The target echoes it back as "TMIPC: 4" in its
// banner.
const dmntTMIPCVersion = 4

// Debug monitor opcodes. The target waits to receive one of these,
// small-integer-prefixed, and answers on the same stream. A live capture
// showed several more (0, 4, 5, 9, 13, 16, 18, 19, 20, 22, 27, 33...) but
// their request/response shape isn't confirmed, so they aren't implemented.
const (
	dmntOpSelectTarget     int32 = 3
	dmntOpReadMemory       int32 = 7
	dmntOpGetModuleByIndex int32 = 17
	dmntOpGetThreadByIndex int32 = 21
)

// Record opcodes the target's *answers* are tagged with, distinct from the
// request opcodes above - GetModuleByIndex's reply is tagged 10, not 17.
const (
	dmntRecModule int32 = 10
	dmntRecThread int32 = 12
)

// dmntTokenThreshold tells the opcode command channel apart from a second
// sub-protocol that shares the same stream, framed with a 4-byte token
// instead of a small opcode and carrying keepalives and acknowledgements.
// Every opcode seen on the wire is a small integer and every token is a
// large, essentially random 32-bit value, so a value below this line is
// always a command and never a token.
const dmntTokenThreshold = 1 << 16

// dmntMaxBody caps a declared token-frame body length, refusing anything
// that can only be a desynchronised stream.
const dmntMaxBody = 16 << 20

// DmntBanner is the target's greeting, sent once as plain "Key: Value"
// lines right after the connection handshake.
type DmntBanner struct {
	Spec  string
	TMA   string
	TMIPC string
	Conn  string
	HW    string
	BCID  string
	PS    string
	PMS   string
	CD    string
}

// DebugMonitor is a connection to the target's debug monitor.
type DebugMonitor struct {
	conn net.Conn
	r    *bufio.Reader

	mu sync.Mutex

	// Serial is the target this connection was resolved for. Set by
	// DialDebugMonitor; empty when opened directly through
	// DialDebugMonitorAddr, which has no serial to record.
	Serial string

	// Banner is the target's own description of itself, read once at
	// connect time.
	Banner DmntBanner
}

// DialDebugMonitor resolves the target's debug monitor and opens it.
func DialDebugMonitor(ctx context.Context, serial string) (*DebugMonitor, error) {
	addr, err := resolvePortAddr(ctx, serial, DmntPortName)
	if err != nil {
		return nil, err
	}
	m, err := DialDebugMonitorAddr(ctx, addr)
	if err != nil {
		return nil, err
	}
	m.Serial = serial
	return m, nil
}

// DialDebugMonitorAddr opens an already-resolved debug monitor address and
// runs the connection handshake.
func DialDebugMonitorAddr(ctx context.Context, addr string) (*DebugMonitor, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("htc: dial debug monitor %s: %w", addr, err)
	}
	m := &DebugMonitor{conn: conn, r: bufio.NewReader(conn)}
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
		defer conn.SetDeadline(time.Time{})
	}
	if err := m.handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return m, nil
}

func (m *DebugMonitor) Close() error {
	return m.conn.Close()
}

// handshake runs the fixed connection sequence: a 32-byte greeting, a
// 4-byte follow-up, then the target's banner. The greeting carries a token
// the target echoes back in the banner, and a version number the banner
// repeats as "TMIPC: 4"; the follow-up's own value has been seen accepted
// as all zero against a live target.
func (m *DebugMonitor) handshake() error {
	var token [4]byte
	if _, err := rand.Read(token[:]); err != nil {
		return fmt.Errorf("htc: debug monitor: generate token: %w", err)
	}

	greeting := make([]byte, 32)
	copy(greeting[0:4], token[:])
	binary.LittleEndian.PutUint32(greeting[12:16], dmntTMIPCVersion)
	if _, err := m.conn.Write(greeting); err != nil {
		return fmt.Errorf("htc: debug monitor: send greeting: %w", err)
	}

	followUp := make([]byte, 4)
	if _, err := m.conn.Write(followUp); err != nil {
		return fmt.Errorf("htc: debug monitor: send follow-up: %w", err)
	}

	head := make([]byte, 32)
	if _, err := io.ReadFull(m.r, head); err != nil {
		return fmt.Errorf("htc: debug monitor: read banner header: %w", err)
	}
	if !bytes.Equal(head[0:4], token[:]) {
		return fmt.Errorf("htc: debug monitor: banner echoed a different token than sent")
	}
	length := binary.LittleEndian.Uint32(head[12:16])
	if length > dmntMaxBody {
		return fmt.Errorf("htc: debug monitor: banner declared a %d byte body, refusing", length)
	}
	text := make([]byte, length)
	if _, err := io.ReadFull(m.r, text); err != nil {
		return fmt.Errorf("htc: debug monitor: read banner text: %w", err)
	}
	m.Banner = parseDmntBanner(text)
	return nil
}

func parseDmntBanner(text []byte) DmntBanner {
	var b DmntBanner
	fields := map[string]*string{
		"Spec":  &b.Spec,
		"TMA":   &b.TMA,
		"TMIPC": &b.TMIPC,
		"Conn":  &b.Conn,
		"HW":    &b.HW,
		"BCID":  &b.BCID,
		"PS":    &b.PS,
		"PMS":   &b.PMS,
		"CD":    &b.CD,
	}
	for _, line := range strings.Split(string(text), "\n") {
		line = strings.TrimRight(line, "\x00")
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if dst, ok := fields[key]; ok {
			*dst = value
		}
	}
	return b
}

// dmntTokenFrame is one message from the keepalive/acknowledgement
// sub-protocol: a 4-byte token that is opaque here and just echoed back and
// forth for the life of the connection, a counter, a message type, a
// direction flag, and a length-prefixed body.
type dmntTokenFrame struct {
	token   [4]byte
	counter uint16
	msgType uint16
	dir     uint16
	body    []byte
}

// readTokenFrame reads one frame from the token-framed sub-protocol. It
// reports an error rather than guessing if the next 4 bytes on the wire
// look like a raw opcode instead of a token.
func (m *DebugMonitor) readTokenFrame() (dmntTokenFrame, error) {
	var head [16]byte
	if _, err := io.ReadFull(m.r, head[:]); err != nil {
		return dmntTokenFrame{}, err
	}
	first := binary.LittleEndian.Uint32(head[0:4])
	if first < dmntTokenThreshold {
		return dmntTokenFrame{}, fmt.Errorf("htc: debug monitor: expected a token frame, got opcode %d", first)
	}
	length := binary.LittleEndian.Uint32(head[12:16])
	if length > dmntMaxBody {
		return dmntTokenFrame{}, fmt.Errorf("htc: debug monitor: token frame declared a %d byte body, refusing", length)
	}
	var f dmntTokenFrame
	copy(f.token[:], head[0:4])
	f.counter = binary.LittleEndian.Uint16(head[4:6])
	f.msgType = binary.LittleEndian.Uint16(head[8:10])
	f.dir = binary.LittleEndian.Uint16(head[10:12])
	f.body = make([]byte, length)
	if _, err := io.ReadFull(m.r, f.body); err != nil {
		return dmntTokenFrame{}, err
	}
	return f, nil
}

// SelectTarget tells the debug monitor which process handle subsequent
// commands apply to. handle comes from wherever the caller obtained it; this
// client has no confirmed way to enumerate running targets on its own yet,
// so the caller has to already know it (for example from a JIT-debug
// notification on the command shell, which reports a handle when a title
// launches with debug-on-launch set).
func (m *DebugMonitor) SelectTarget(handle uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	req := make([]byte, 12)
	binary.LittleEndian.PutUint32(req[0:4], uint32(dmntOpSelectTarget))
	binary.LittleEndian.PutUint32(req[4:8], handle)
	if _, err := m.conn.Write(req); err != nil {
		return fmt.Errorf("htc: debug monitor: select target: %w", err)
	}
	if _, err := m.readTokenFrame(); err != nil {
		return fmt.Errorf("htc: debug monitor: select target: read acknowledgement: %w", err)
	}
	if err := m.drainSelectTargetNotification(handle); err != nil {
		return fmt.Errorf("htc: debug monitor: select target: %w", err)
	}
	return nil
}

// drainSelectTargetNotification consumes a 16-byte notification (opcode 14:
// the same opcode, the handle just selected, and 8 zero bytes) that a live
// capture showed the target coalescing into the same write as the
// acknowledgement above. Its meaning is not decoded, but it has to be read
// here or it desyncs whatever call reads the stream next. It is not always
// present, so this only waits a short, bounded window for it rather than
// blocking on data that may never come.
func (m *DebugMonitor) drainSelectTargetNotification(handle uint32) error {
	if m.r.Buffered() < 4 {
		m.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		_, err := m.r.Peek(4)
		m.conn.SetReadDeadline(time.Time{})
		if err != nil {
			return nil
		}
	}
	peek, err := m.r.Peek(4)
	if err != nil || binary.LittleEndian.Uint32(peek) != 14 {
		return nil
	}
	rest := make([]byte, 16)
	_, err = io.ReadFull(m.r, rest)
	return err
}

// ReadMemory reads count bytes from the target's address space at addr, in
// whichever target SelectTarget last selected. A live target has been seen
// answering 1024-byte requests in a tight polling loop; nothing in the
// captured traffic suggests a smaller hidden limit, but a very large count
// has not been tried against real hardware.
func (m *DebugMonitor) ReadMemory(handle uint32, addr uint64, count uint32) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	req := make([]byte, 28)
	binary.LittleEndian.PutUint32(req[0:4], uint32(dmntOpReadMemory))
	binary.LittleEndian.PutUint32(req[4:8], handle)
	binary.LittleEndian.PutUint64(req[12:20], addr)
	binary.LittleEndian.PutUint32(req[20:24], count)
	if _, err := m.conn.Write(req); err != nil {
		return nil, fmt.Errorf("htc: debug monitor: read memory: %w", err)
	}

	frame, err := m.readTokenFrame()
	if err != nil {
		return nil, fmt.Errorf("htc: debug monitor: read memory: %w", err)
	}
	if uint32(len(frame.body)) < count {
		return nil, fmt.Errorf("htc: debug monitor: read memory: target returned %d bytes, wanted %d", len(frame.body), count)
	}
	// The response body is a short, undecoded sub-header followed by the
	// requested bytes; the sub-header's size is whatever is left over once
	// the requested count is accounted for, rather than a size assumed
	// fixed.
	return frame.body[uint32(len(frame.body))-count:], nil
}

// DmntModule is one of the target's loaded modules, as reported by
// ModuleAt.
type DmntModule struct {
	Base    uint64
	Size    uint64
	BuildID [32]byte
	Path    string
}

// ModuleAt asks for the loaded module at the given index. A live session
// polls a small, fixed set of indices (0 upward) repeatedly rather than
// growing over time, so the caller has to already know roughly how many
// there are; nothing in the captured traffic identifies a call that reports
// the count itself, so this has no way to say when the list ends other than
// the target's own answer no longer decoding as a module record.
func (m *DebugMonitor) ModuleAt(handle uint32, index uint32) (DmntModule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.recordAt(dmntOpGetModuleByIndex, dmntRecModule, handle, index, "module")
	if err != nil {
		return DmntModule{}, err
	}
	if len(rec) < 48 {
		return DmntModule{}, fmt.Errorf("htc: debug monitor: module at %d: response too short", index)
	}
	var mod DmntModule
	mod.Base = binary.LittleEndian.Uint64(rec[0:8])
	mod.Size = binary.LittleEndian.Uint64(rec[8:16])
	copy(mod.BuildID[:], rec[16:48])
	mod.Path = dmntCString(rec[48:])
	return mod, nil
}

// dmntThreadNameOffset is where a thread's name starts within a thread
// record, once the record's own 16-byte header (opcode, handle, two
// reserved fields) is already consumed. In between are a handful of other
// fields - a counter shared with other opcodes, two small flags, and what
// looks like it might be the name's own length - seen consistently across
// every sample but not decoded with enough confidence to expose.
const dmntThreadNameOffset = 44

// ThreadAt asks for the thread at the given index, same polling model as
// ModuleAt. Only the name is exposed - see dmntThreadNameOffset for why the
// rest of the record isn't.
func (m *DebugMonitor) ThreadAt(handle uint32, index uint32) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.recordAt(dmntOpGetThreadByIndex, dmntRecThread, handle, index, "thread")
	if err != nil {
		return "", err
	}
	if len(rec) < dmntThreadNameOffset {
		return "", fmt.Errorf("htc: debug monitor: thread at %d: response too short", index)
	}
	return dmntCString(rec[dmntThreadNameOffset:]), nil
}

// recordAt sends a `[opcode][handle][0][index]` request and returns the
// answer's own payload, past its 16-byte undecoded sub-header (the same
// shape ReadMemory's response has) and its 16-byte record header (whose
// first 4 bytes have to match wantRecord, or this is answering something
// else entirely).
//
// A live capture of the reference client showed it pipelining several of
// these requests without waiting for each answer, which made the target
// coalesce a second answer's leading bytes into the same physical write as
// the first. This client sends one request and waits for its answer before
// the next, which should not trigger the same thing, but that has not been
// confirmed against real hardware - if a future caller pipelines calls to
// this method concurrently, the stream may desync the same way
// SelectTarget's coalesced notification did.
func (m *DebugMonitor) recordAt(reqOp, wantRecord int32, handle, index uint32, what string) ([]byte, error) {
	req := make([]byte, 16)
	binary.LittleEndian.PutUint32(req[0:4], uint32(reqOp))
	binary.LittleEndian.PutUint32(req[4:8], handle)
	binary.LittleEndian.PutUint32(req[12:16], index)
	if _, err := m.conn.Write(req); err != nil {
		return nil, fmt.Errorf("htc: debug monitor: %s at %d: %w", what, index, err)
	}

	frame, err := m.readTokenFrame()
	if err != nil {
		return nil, fmt.Errorf("htc: debug monitor: %s at %d: %w", what, index, err)
	}
	const subHeader, recHeader = 16, 16
	if len(frame.body) < subHeader+recHeader {
		return nil, fmt.Errorf("htc: debug monitor: %s at %d: response too short", what, index)
	}
	rec := frame.body[subHeader:]
	if got := int32(binary.LittleEndian.Uint32(rec[0:4])); got != wantRecord {
		return nil, fmt.Errorf("htc: debug monitor: %s at %d: got record opcode %d, want %d", what, index, got, wantRecord)
	}
	return rec[recHeader:], nil
}

// DmntRawFrame is one raw token-framed message, exposed for probing opcodes
// whose request/response shape isn't confirmed yet - see the opcode table in
// docs/protocol-notes.md for what's already understood and wired up as a
// real method above.
type DmntRawFrame struct {
	Token   [4]byte
	Counter uint16
	MsgType uint16
	Dir     uint16
	Body    []byte
}

// SendOpcode writes a raw opcode-prefixed request: the opcode itself
// followed by whatever bytes the caller supplies. Low-level escape hatch for
// probing, not a stable protocol operation.
func (m *DebugMonitor) SendOpcode(opcode int32, args []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	req := make([]byte, 4+len(args))
	binary.LittleEndian.PutUint32(req[0:4], uint32(opcode))
	copy(req[4:], args)
	if _, err := m.conn.Write(req); err != nil {
		return fmt.Errorf("htc: debug monitor: send opcode %d: %w", opcode, err)
	}
	return nil
}

// ReadFrame reads one raw token-framed message, same probing purpose as
// SendOpcode. A zero timeout waits indefinitely.
func (m *DebugMonitor) ReadFrame(timeout time.Duration) (DmntRawFrame, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if timeout > 0 {
		m.conn.SetReadDeadline(time.Now().Add(timeout))
		defer m.conn.SetReadDeadline(time.Time{})
	}
	f, err := m.readTokenFrame()
	if err != nil {
		return DmntRawFrame{}, err
	}
	return DmntRawFrame{Token: f.token, Counter: f.counter, MsgType: f.msgType, Dir: f.dir, Body: f.body}, nil
}

// dmntCString reads a NUL-terminated string, or the whole slice if there is
// no terminator in it.
func dmntCString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
