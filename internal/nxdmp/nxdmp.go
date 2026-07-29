// Package nxdmp reads the Nintendo Switch devkit's .nxdmp crash dump format:
// a flat sequence of tagged, length-prefixed chunks (an 8-byte ASCII tag
// padded with nulls, an 8-byte little-endian size, then that many bytes of
// payload), one after another until end of file.
//
// This is a clean-room reimplementation of the format: only the resulting
// wire-format facts (chunk tags, field layouts, sizes) are reproduced here,
// not anyone else's code.
package nxdmp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const chunkHeaderSize = 16

// MemoryType is the protection bits a memory region reports.
type MemoryType uint64

const (
	MemoryNone        MemoryType = 0
	MemoryExecute     MemoryType = 1
	MemoryWrite       MemoryType = 2
	MemoryRead        MemoryType = 4
	MemoryReadWrite   MemoryType = 6
	MemoryReadExecute MemoryType = 5
	MemoryAll         MemoryType = 7
)

func (t MemoryType) String() string {
	var s string
	if t&MemoryRead != 0 {
		s += "r"
	} else {
		s += "-"
	}
	if t&MemoryWrite != 0 {
		s += "w"
	} else {
		s += "-"
	}
	if t&MemoryExecute != 0 {
		s += "x"
	} else {
		s += "-"
	}
	return s
}

// ThreadStatus is a thread's state at the time of the dump.
type ThreadStatus int

const (
	ThreadUnknown ThreadStatus = iota
	ThreadInitializing
	ThreadRunning
	ThreadStopped
	ThreadWaiting
	ThreadTerminated
)

func threadStatusFromByte(b byte) ThreadStatus {
	switch b {
	case 'I':
		return ThreadInitializing
	case 'R':
		return ThreadRunning
	case 'S':
		return ThreadStopped
	case 'W':
		return ThreadWaiting
	case 'M':
		return ThreadTerminated
	}
	return ThreadUnknown
}

func (s ThreadStatus) String() string {
	switch s {
	case ThreadInitializing:
		return "initializing"
	case ThreadRunning:
		return "running"
	case ThreadStopped:
		return "stopped"
	case ThreadWaiting:
		return "waiting"
	case ThreadTerminated:
		return "terminated"
	}
	return "unknown"
}

// exceptionTypeNames maps a dump's exception code to its display name,
// indexed positionally.
var exceptionTypeNames = [...]string{
	"Undefined Instruction",
	"Access Violation Instruction",
	"Access Violation Data",
	"Data Type Missaligned",
	"Attach Break",
	"Break Point",
	"User Break",
	"Debugger Break",
	"Undefined System Call",
	"Memory System Error",
}

// ExceptionTypeName describes an exception code by name. An out-of-range
// code (a newer SDK's dump, most likely) gets an honest "unknown" rather
// than a guess.
func ExceptionTypeName(code uint64) string {
	if code < uint64(len(exceptionTypeNames)) {
		return exceptionTypeNames[code]
	}
	return fmt.Sprintf("unknown exception %d", code)
}

// Header is the fixed-size DUMP chunk every nxdmp file starts with.
type Header struct {
	ProcessID       uint64
	Architecture    string
	ExceptionNumber uint64
	ProcessName     string
	Args            string
	OSVersion       string
	LoadAddr        uint64
	Size            uint64
}

// Version is the dump format's own version, not the target's OS version.
type Version struct {
	Major, Minor, Build uint32
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Build) }

// Module is one loaded module (NSO/NRO), with its code range narrowed down
// from the raw load range using the dump's memory region info - the load
// range can include RO data and RW data segments past the actual
// executable pages.
type Module struct {
	Name        string
	ID          []byte // build id, 32 bytes
	LoadAddress uint64
	Size        uint64

	CodeBase, CodeEnd             uint64
	CodeRoDataBase, CodeRoDataEnd uint64
	CodeDataBase, CodeDataEnd     uint64
}

// MemoryRegion is one entry from the target's memory map at the time of the
// dump.
type MemoryRegion struct {
	Address uint64
	Type    MemoryType
	Size    uint64
}

// Exception is what the target reported about the fault itself, when the
// dump includes an EXCPINF chunk (older dumps may only have the exception
// number in the header).
type Exception struct {
	ThreadID uint64
	Is64Bit  bool
	Code     uint64
	Address  uint64
	Details  [32]byte
}

// Thread is one thread's state, including its full register set as raw
// arrays - see AArch64Registers/AArch32Registers in the RDFS-derived Is64Bit
// flag for how to read them, or just call Dump.Report.
type Thread struct {
	ID                uint64
	Name              string
	IsExceptionThread bool
	Status            ThreadStatus
	Priority          int
	Core              int
	AffinityMask      uint64
	IP                uint64
	SP                uint64

	GPRegisters        []uint64
	GPControlRegisters []uint64
	FPRegisters        []uint64
	FPControlRegisters []uint64

	StackFrames   []uint64
	StackAreaBase uint64
	StackAreaEnd  uint64
}

// ThreadLocalStorage is one thread's TPIDR_EL0 value.
type ThreadLocalStorage struct {
	ThreadID uint64
	TPIDR    uint64
}

// RawChunk is a chunk this package does not decode: memory pages (raw,
// LZ4-compressed or zlib-compressed), screenshots and video - a text
// report only needs the structured chunks. The payload is left on disk;
// ReadRawChunk fetches it on demand.
//
// Screenshot and video chunks (BMPIMAGE/RAWIMAGE/JPGIMAGE/PNGIMAGE,
// MP4VIDEO/RAWVIDEO) carry no framing of their own beyond the outer
// tag+size header - the body is exactly a standalone BMP/JPEG/PNG/MP4
// file (or, for the "raw" variants, unlabelled pixel/frame data with no
// width/height in the chunk itself), so there is nothing left to decode:
// ReadRawChunk already returns bytes a normal image/video reader can open
// directly. A memory chunk's body opens with a 24-byte address/type/size
// header before the (possibly compressed) page data; that triple is
// already surfaced structurally via MemoryRegion when the dump carries a
// region-info chunk, so decoding it a second time out of a memory chunk
// would be redundant, and decompressing the page bytes themselves would
// only feed a hex/memory view, not the text report - nothing here needs it.
type RawChunk struct {
	Tag    string
	Offset int64 // file offset of the payload, not the chunk header
	Size   int64
}

// Dump is everything this package extracted from one .nxdmp file.
type Dump struct {
	Path string

	Version          Version
	Header           Header
	ApplicationID    uint64
	HasApplicationID bool

	Is64Bit        bool
	Is64BitAddress bool

	Exception       *Exception
	Threads         []*Thread
	ExceptionThread *Thread

	Modules       []*Module
	MemoryRegions []MemoryRegion

	ThreadLocalStorage []ThreadLocalStorage
	UserData           []byte
	// TTY is the target's console output captured up to the crash,
	// concatenated in file order - often the most direct clue to what went
	// wrong (a game's own R_ABORT_UNLESS diagnostic, an assert message,
	// ...), and not something every dump viewer bothers surfacing.
	TTY []byte

	// HasStackFrames is whether this dump carries any STCKFRMS chunk at
	// all. A stack trace only ever comes from that chunk - nothing here
	// reconstructs one from the raw/LZ4MRY memory chunks - so a dump
	// without it (seen live: a "User Break" dump, not a real fault)
	// genuinely has no stack trace to show, for any thread. Report
	// distinguishes that from a thread that has the chunk type but zero
	// frames in it.
	HasStackFrames bool

	RawChunks []RawChunk
}

// ReadRawChunk reads one RawChunk's payload from the dump file. The Dump
// only records raw chunks' location at parse time so a 20MB file full of
// memory pages does not have to be held in memory just to print a report.
func (d *Dump) ReadRawChunk(rc RawChunk) ([]byte, error) {
	f, err := os.Open(d.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(rc.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, rc.Size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("nxdmp: read %s chunk: %w", rc.Tag, err)
	}
	return buf, nil
}

// Parse reads and decodes an .nxdmp file.
func Parse(path string) (*Dump, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f, path)
}

type moduleRaw struct {
	Name        string
	ID          []byte
	LoadAddress uint64
	Size        uint64
}

type threadRaw struct {
	ID            uint64
	CurrentThread bool
	Status        ThreadStatus
	Priority      int
	Core          int
	AffinityMask  uint64
	IP, SP        uint64
	Name          string

	GPRegisters        []uint64
	GPControlRegisters []uint64
	FPRegisters        []uint64
	FPControlRegisters []uint64
}

func parse(f *os.File, path string) (*Dump, error) {
	var (
		header          *Header
		version         Version
		haveVersion     bool
		appID           uint64
		haveAppID       bool
		is64Bit         bool
		is64BitAddr     bool
		haveRDFS        bool
		exception       *Exception
		userData        []byte
		tty             []byte
		modules         []moduleRaw
		threads         []threadRaw
		regions         []MemoryRegion
		tls             []ThreadLocalStorage
		raw             []RawChunk
		stackFrames     = map[uint64][]uint64{}
		haveStackFrames bool
	)

	hdrBuf := make([]byte, chunkHeaderSize)
	for {
		if _, err := io.ReadFull(f, hdrBuf); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("nxdmp: read chunk header: %w", err)
		}
		tag := cString(hdrBuf[:8])
		size := int64(binary.LittleEndian.Uint64(hdrBuf[8:16]))
		payloadOffset, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}

		readBody := func() ([]byte, error) {
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return nil, fmt.Errorf("nxdmp: read %s chunk: %w", tag, err)
			}
			return body, nil
		}

		switch tag {
		case "DUMP":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			h, err := parseHeader(body)
			if err != nil {
				return nil, err
			}
			header = &h
		case "VERSION":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			version, err = parseVersion(body)
			if err != nil {
				return nil, err
			}
			haveVersion = true
		case "APPID":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			if len(body) < 8 {
				return nil, fmt.Errorf("nxdmp: APPID chunk too short: %d bytes", len(body))
			}
			appID = binary.LittleEndian.Uint64(body)
			haveAppID = true
		case "RDFS":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			is64Bit, is64BitAddr, err = parseRegisterDefinitions(body)
			if err != nil {
				return nil, err
			}
			haveRDFS = true
		case "MODL":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			m, err := parseModule(body)
			if err != nil {
				return nil, err
			}
			modules = append(modules, m)
		case "THRD":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			t, err := parseThread(body)
			if err != nil {
				return nil, err
			}
			threads = append(threads, t)
		case "STCKFRMS":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			threadID, frames, err := parseStackFrames(body)
			if err != nil {
				return nil, err
			}
			stackFrames[threadID] = frames
			haveStackFrames = true
		case "THREADLS":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			if len(body) < 16 {
				return nil, fmt.Errorf("nxdmp: THREADLS chunk too short: %d bytes", len(body))
			}
			tls = append(tls, ThreadLocalStorage{
				ThreadID: binary.LittleEndian.Uint64(body[0:]),
				TPIDR:    binary.LittleEndian.Uint64(body[8:]),
			})
		case "MMRYINF":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			if len(body) < 24 {
				return nil, fmt.Errorf("nxdmp: MMRYINF chunk too short: %d bytes", len(body))
			}
			regions = append(regions, MemoryRegion{
				Address: binary.LittleEndian.Uint64(body[0:]),
				Type:    MemoryType(binary.LittleEndian.Uint64(body[8:])),
				Size:    binary.LittleEndian.Uint64(body[16:]),
			})
		case "EXCPINF":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			e, err := parseException(body)
			if err != nil {
				return nil, err
			}
			exception = &e
		case "USERDATA":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			userData = body
		case "TTY":
			body, err := readBody()
			if err != nil {
				return nil, err
			}
			tty = append(tty, body...)
		default:
			// MMRY, LZ4MRY, CMRY, the image and video tags, and anything a
			// newer SDK might add: leave the payload on disk.
			raw = append(raw, RawChunk{Tag: tag, Offset: payloadOffset, Size: size})
			if _, err := f.Seek(size, io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("nxdmp: skip %s chunk: %w", tag, err)
			}
		}
	}

	if header == nil {
		return nil, fmt.Errorf("nxdmp: no DUMP chunk found, not a valid nxdmp file")
	}
	if !haveRDFS {
		return nil, fmt.Errorf("nxdmp: no RDFS chunk found, not a valid nxdmp file")
	}
	if !haveVersion {
		version = Version{Major: 1, Minor: 0, Build: 0}
	}

	sort.Slice(regions, func(i, j int) bool { return regions[i].Address < regions[j].Address })
	regionByAddress := make(map[uint64]int, len(regions))
	for i, r := range regions {
		if _, ok := regionByAddress[r.Address]; !ok {
			regionByAddress[r.Address] = i
		}
	}

	d := &Dump{
		Path:               path,
		Version:            version,
		Header:             *header,
		ApplicationID:      appID,
		HasApplicationID:   haveAppID,
		Is64Bit:            is64Bit,
		Is64BitAddress:     is64BitAddr,
		Exception:          exception,
		MemoryRegions:      regions,
		ThreadLocalStorage: tls,
		UserData:           userData,
		TTY:                tty,
		HasStackFrames:     haveStackFrames,
		RawChunks:          raw,
	}

	for _, t := range threads {
		th := &Thread{
			ID:                 t.ID,
			Name:               t.Name,
			IsExceptionThread:  t.CurrentThread,
			Status:             t.Status,
			Priority:           t.Priority,
			Core:               t.Core,
			AffinityMask:       t.AffinityMask,
			IP:                 t.IP,
			SP:                 t.SP,
			GPRegisters:        t.GPRegisters,
			GPControlRegisters: t.GPControlRegisters,
			FPRegisters:        t.FPRegisters,
			FPControlRegisters: t.FPControlRegisters,
			StackFrames:        stackFrames[t.ID],
		}
		th.StackAreaBase, th.StackAreaEnd = stackArea(regions, t.SP)
		d.Threads = append(d.Threads, th)
		if th.IsExceptionThread {
			d.ExceptionThread = th
		}
	}
	if d.ExceptionThread == nil && exception != nil {
		for _, th := range d.Threads {
			if th.ID == exception.ThreadID {
				d.ExceptionThread = th
				break
			}
		}
	}

	for _, m := range modules {
		mod := &Module{Name: m.Name, ID: m.ID, LoadAddress: m.LoadAddress, Size: m.Size}
		deriveModuleRanges(mod, regions, regionByAddress)
		d.Modules = append(d.Modules, mod)
	}
	sort.Slice(d.Modules, func(i, j int) bool { return d.Modules[i].CodeBase < d.Modules[j].CodeBase })

	return d, nil
}

func parseHeader(body []byte) (Header, error) {
	const size = 2160
	if len(body) < size {
		return Header{}, fmt.Errorf("nxdmp: DUMP chunk too short: %d bytes, want %d", len(body), size)
	}
	return Header{
		ProcessID:       binary.LittleEndian.Uint64(body[0:]),
		Architecture:    cString(body[8:24]),
		ExceptionNumber: binary.LittleEndian.Uint64(body[24:]),
		ProcessName:     cString(body[32:1056]),
		Args:            cString(body[1056:2080]),
		OSVersion:       cString(body[2080:2144]),
		LoadAddr:        binary.LittleEndian.Uint64(body[2144:]),
		Size:            binary.LittleEndian.Uint64(body[2152:]),
	}, nil
}

func parseVersion(body []byte) (Version, error) {
	if len(body) < 12 {
		return Version{}, fmt.Errorf("nxdmp: VERSION chunk too short: %d bytes", len(body))
	}
	return Version{
		Major: binary.LittleEndian.Uint32(body[0:]),
		Minor: binary.LittleEndian.Uint32(body[4:]),
		Build: binary.LittleEndian.Uint32(body[8:]),
	}, nil
}

func parseModule(body []byte) (moduleRaw, error) {
	const size = 1072
	if len(body) < size {
		return moduleRaw{}, fmt.Errorf("nxdmp: MODL chunk too short: %d bytes, want %d", len(body), size)
	}
	id := make([]byte, 32)
	copy(id, body[1024:1056])
	return moduleRaw{
		Name:        cString(body[0:1024]),
		ID:          id,
		LoadAddress: binary.LittleEndian.Uint64(body[1056:]),
		Size:        binary.LittleEndian.Uint64(body[1064:]),
	}, nil
}

func parseThread(body []byte) (threadRaw, error) {
	const fixedSize = 96
	if len(body) < fixedSize {
		return threadRaw{}, fmt.Errorf("nxdmp: THRD chunk too short: %d bytes, want at least %d", len(body), fixedSize)
	}
	nGP := binary.LittleEndian.Uint64(body[64:])
	nGPCtrl := binary.LittleEndian.Uint64(body[72:])
	nFP := binary.LittleEndian.Uint64(body[80:])
	nFPCtrl := binary.LittleEndian.Uint64(body[88:])

	want := fixedSize + 8*(nGP+nGPCtrl+nFP+nFPCtrl)
	if uint64(len(body)) < want {
		return threadRaw{}, fmt.Errorf("nxdmp: THRD chunk too short: %d bytes, want %d", len(body), want)
	}

	off := fixedSize
	readRegs := func(n uint64) []uint64 {
		out := make([]uint64, n)
		for i := range out {
			out[i] = binary.LittleEndian.Uint64(body[off:])
			off += 8
		}
		return out
	}

	return threadRaw{
		ID:            binary.LittleEndian.Uint64(body[0:]),
		CurrentThread: int16(binary.LittleEndian.Uint16(body[8:])) == 1,
		Status:        threadStatusFromByte(body[10]),
		// Priority is a 16-bit field even though real priority values
		// (0-63) never fill more than the low byte; read the full field
		// rather than assuming the high byte is unused padding.
		Priority: int(binary.LittleEndian.Uint16(body[12:])),
		// Core is packed into a single 16-bit field: the low byte is the
		// core index, the high byte is the affinity mask. There is no
		// separate "ideal core" byte anywhere in this record.
		Core:         int(body[14]),
		AffinityMask: uint64(body[15]),
		IP:           binary.LittleEndian.Uint64(body[16:]),
		SP:           binary.LittleEndian.Uint64(body[24:]),
		Name:         cString(body[32:64]),

		GPRegisters:        readRegs(nGP),
		GPControlRegisters: readRegs(nGPCtrl),
		FPRegisters:        readRegs(nFP),
		FPControlRegisters: readRegs(nFPCtrl),
	}, nil
}

func parseStackFrames(body []byte) (threadID uint64, frames []uint64, err error) {
	const fixedSize = 16
	if len(body) < fixedSize {
		return 0, nil, fmt.Errorf("nxdmp: STCKFRMS chunk too short: %d bytes, want at least %d", len(body), fixedSize)
	}
	threadID = binary.LittleEndian.Uint64(body[0:])
	n := binary.LittleEndian.Uint64(body[8:])
	want := fixedSize + 8*n
	if uint64(len(body)) < want {
		return 0, nil, fmt.Errorf("nxdmp: STCKFRMS chunk too short: %d bytes, want %d", len(body), want)
	}
	frames = make([]uint64, n)
	off := fixedSize
	for i := range frames {
		frames[i] = binary.LittleEndian.Uint64(body[off:])
		off += 8
	}
	return threadID, frames, nil
}

func parseException(body []byte) (Exception, error) {
	const size = 56
	if len(body) < size {
		return Exception{}, fmt.Errorf("nxdmp: EXCPINF chunk too short: %d bytes, want %d", len(body), size)
	}
	e := Exception{
		ThreadID: binary.LittleEndian.Uint64(body[0:]),
		Is64Bit:  binary.LittleEndian.Uint32(body[8:]) != 0,
		Code:     uint64(binary.LittleEndian.Uint32(body[12:])),
		Address:  binary.LittleEndian.Uint64(body[16:]),
	}
	copy(e.Details[:], body[24:56])
	return e, nil
}

// parseRegisterDefinitions reads just enough of the RDFS chunk's ULEB128
// stream to learn whether registers and addresses are 32 or 64 bit - those
// are the only two fields this reader needs; the rest of the stream is the
// actual register layout table, which nothing here needs.
func parseRegisterDefinitions(body []byte) (is64Bit, is64BitAddr bool, err error) {
	r := bytes.NewReader(body)
	if _, err := readULEB(r); err != nil {
		return false, false, fmt.Errorf("nxdmp: RDFS chunk: %w", err)
	}
	if _, err := readULEB(r); err != nil {
		return false, false, fmt.Errorf("nxdmp: RDFS chunk: %w", err)
	}
	regSize, err := readULEB(r)
	if err != nil {
		return false, false, fmt.Errorf("nxdmp: RDFS chunk: %w", err)
	}
	switch regSize {
	case 4:
		is64Bit = false
	case 8:
		is64Bit = true
	default:
		return false, false, fmt.Errorf("nxdmp: RDFS chunk: unexpected register size %d", regSize)
	}
	addrSize, err := readULEB(r)
	if err != nil {
		return false, false, fmt.Errorf("nxdmp: RDFS chunk: %w", err)
	}
	switch addrSize {
	case 4:
		is64BitAddr = false
	case 8:
		is64BitAddr = true
	default:
		return false, false, fmt.Errorf("nxdmp: RDFS chunk: unexpected address size %d", addrSize)
	}
	return is64Bit, is64BitAddr, nil
}

// readULEB matches the target's own encoder: a normal ULEB128 low-7-bits
// payload per byte, but the continuation test is "high nibble non-zero"
// rather than the usual high bit, so it is reproduced exactly rather than
// assuming standard ULEB128.
func readULEB(r *bytes.Reader) (uint32, error) {
	var v uint32
	var shift uint
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7F) << shift
		if b&0xF0 == 0 {
			return v, nil
		}
		shift += 7
	}
}

// deriveModuleRanges narrows a module's code/rodata/data ranges down from
// its raw load range using the memory regions: starting from the region
// whose address exactly matches the module's load address, walk forward
// through contiguous regions within the module's mapped span, bucketing
// each by its protection bits.
func deriveModuleRanges(m *Module, regions []MemoryRegion, regionByAddress map[uint64]int) {
	m.CodeBase = m.LoadAddress
	startIdx, ok := regionByAddress[m.LoadAddress]
	if !ok {
		m.CodeEnd = m.LoadAddress + m.Size
		return
	}
	for i := startIdx; i < len(regions); i++ {
		r := regions[i]
		end := r.Address + r.Size
		if m.LoadAddress+m.Size <= r.Address {
			break
		}
		switch {
		case r.Type&MemoryExecute != 0:
			if m.CodeBase == 0 {
				m.CodeBase = r.Address
			}
			m.CodeEnd = end
		case r.Type == MemoryRead:
			if m.CodeRoDataBase == 0 {
				m.CodeRoDataBase = r.Address
			}
			m.CodeRoDataEnd = end
		case r.Type == MemoryReadWrite:
			if m.CodeDataBase == 0 {
				m.CodeDataBase = r.Address
			}
			m.CodeDataEnd = end
		}
	}
	if m.CodeEnd == 0 {
		m.CodeEnd = m.LoadAddress + m.Size
	}
}

// stackArea finds the memory region a thread's stack pointer falls in: if
// the region containing SP is not itself read-write (a guard page below the
// real stack, most likely), check the one region past it too before giving
// up.
func stackArea(regions []MemoryRegion, sp uint64) (base, end uint64) {
	sawCandidate := false
	for _, r := range regions {
		regionEnd := r.Address + r.Size
		if regionEnd > sp && (sawCandidate || (r.Address <= sp && sp < regionEnd)) {
			if r.Type == MemoryReadWrite {
				return r.Address, regionEnd
			}
			if sawCandidate {
				return 0, 0
			}
			sawCandidate = true
		}
	}
	return 0, 0
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// Report renders a text summary: exception code/address/thread, the
// module list, and per-thread registers plus a symbolicated stack trace.
// allThreads includes every thread rather than just the one that raised
// the exception.
func (d *Dump) Report(allThreads bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Process:             %s (pid %d)\n", d.Header.ProcessName, d.Header.ProcessID)
	if d.HasApplicationID {
		fmt.Fprintf(&b, "Application ID:      0x%016x\n", d.ApplicationID)
	}
	if d.Header.OSVersion != "" {
		fmt.Fprintf(&b, "Os version:          %s\n", d.Header.OSVersion)
	}
	fmt.Fprintf(&b, "Architecture:        %s\n", d.Header.Architecture)

	code := d.Header.ExceptionNumber
	var addr, threadID uint64
	haveExcInfo := d.Exception != nil
	if haveExcInfo {
		code = d.Exception.Code
		addr = d.Exception.Address
		threadID = d.Exception.ThreadID
	}
	fmt.Fprintf(&b, "Exception code:      %s\n", ExceptionTypeName(code))
	if haveExcInfo {
		fmt.Fprintf(&b, "Exception address:   0x%016x\n", addr)
		fmt.Fprintf(&b, "Exception Thread ID: 0x%016x\n", threadID)
	}
	b.WriteByte('\n')

	fmt.Fprintf(&b, "Module Information:\n")
	for _, m := range d.Modules {
		fmt.Fprintf(&b, "  0x%016x - 0x%016x %s\n", m.CodeBase, m.CodeEnd, m.Name)
	}
	b.WriteByte('\n')

	if !allThreads && d.ExceptionThread == nil && len(d.Threads) > 0 {
		fmt.Fprintf(&b, "(no thread in this dump is marked as the exception thread; pass --all-threads to see all %d)\n\n", len(d.Threads))
	}

	for _, th := range d.Threads {
		isExceptionThread := d.ExceptionThread != nil && th.ID == d.ExceptionThread.ID
		if !allThreads && !isExceptionThread {
			continue
		}
		fmt.Fprintf(&b, "----\n")
		marker := ""
		if isExceptionThread {
			marker = " <Exception Thread>"
		}
		fmt.Fprintf(&b, "ThreadId :  0x%016x%s\n\n", th.ID, marker)

		if d.Is64Bit {
			d.writeAArch64Registers(&b, th)
		} else {
			d.writeRawRegisters(&b, th)
		}
		b.WriteByte('\n')

		if th.StackAreaBase == 0 && th.StackAreaEnd == 0 {
			fmt.Fprintf(&b, "Stack: (no memory region info in this dump)\n\n")
		} else {
			fmt.Fprintf(&b, "Stack: 0x%016x - 0x%016x\n\n", th.StackAreaBase, th.StackAreaEnd)
		}
		if !d.HasStackFrames {
			fmt.Fprintf(&b, "Stack trace: (not recorded in this dump)\n")
		} else {
			fmt.Fprintf(&b, "Stack trace:\n")
			for _, frame := range th.StackFrames {
				fmt.Fprintf(&b, "  0x%016x%s\n", frame, d.symbolicate(frame))
			}
		}
		b.WriteByte('\n')
	}

	return b.String()
}

func gpAt(regs []uint64, i int) uint64 {
	if i < 0 || i >= len(regs) {
		return 0
	}
	return regs[i]
}

// writeAArch64Registers prints X00-X28, FP, LR, SP, PC and PSTATE:
// GPRegisters holds x0-x28 followed by fp, lr, sp, pc at fixed indices
// 29-32, and GPControlRegisters[0] is pstate.
func (d *Dump) writeAArch64Registers(b *strings.Builder, th *Thread) {
	for i := 0; i < 29; i++ {
		fmt.Fprintf(b, "X%02d:  0x%016x\n", i, gpAt(th.GPRegisters, i))
	}
	fp := gpAt(th.GPRegisters, 29)
	lr := gpAt(th.GPRegisters, 30)
	sp := gpAt(th.GPRegisters, 31)
	pc := gpAt(th.GPRegisters, 32)
	var pstate uint32
	if len(th.GPControlRegisters) > 0 {
		pstate = uint32(th.GPControlRegisters[0])
	}
	fmt.Fprintf(b, "FP :  0x%016x\n", fp)
	fmt.Fprintf(b, "LR :  0x%016x%s\n", lr, d.symbolicate(lr))
	fmt.Fprintf(b, "SP :  0x%016x\n", sp)
	fmt.Fprintf(b, "PC :  0x%016x%s\n", pc, d.symbolicate(pc+4))
	fmt.Fprintf(b, "PSTATE:  0x%08x\n", pstate)
}

// writeRawRegisters is the 32-bit fallback: it just dumps what the RDFS
// chunk says is there, r0-rN plus sp/pc/cpsr.
func (d *Dump) writeRawRegisters(b *strings.Builder, th *Thread) {
	for i, v := range th.GPRegisters {
		fmt.Fprintf(b, "R%02d:  0x%08x\n", i, uint32(v))
	}
	var cpsr uint32
	if len(th.GPControlRegisters) > 0 {
		cpsr = uint32(th.GPControlRegisters[0])
	}
	fmt.Fprintf(b, "SP :  0x%08x\n", uint32(th.SP))
	fmt.Fprintf(b, "PC :  0x%08x%s\n", uint32(th.IP), d.symbolicate(th.IP))
	fmt.Fprintf(b, "CPSR:  0x%08x\n", cpsr)
}

// symbolicate reports an address as an offset into whichever module's code
// range contains it - there's no symbol server or symbol file involved at
// all, so this is the only form addresses are ever annotated with.
func (d *Dump) symbolicate(addr uint64) string {
	for _, m := range d.Modules {
		if addr < m.CodeBase || addr >= m.CodeEnd {
			continue
		}
		return fmt.Sprintf(": 0x%x + 0x%x (%s)", m.CodeBase, addr-m.CodeBase, m.Name)
	}
	return ""
}

// Summary is one line of the essential facts, for listing many dumps.
func (d *Dump) Summary() string {
	code := d.Header.ExceptionNumber
	if d.Exception != nil {
		code = d.Exception.Code
	}
	name := d.Header.ProcessName
	if name == "" {
		name = "unknown process"
	}
	return fmt.Sprintf("%s (pid %d): %s", name, d.Header.ProcessID, ExceptionTypeName(code))
}
