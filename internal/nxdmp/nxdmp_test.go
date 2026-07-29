package nxdmp

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chunkBuilder assembles a synthetic .nxdmp file out of chunks, the same
// tag+size+body shape Parse reads back.
type chunkBuilder struct {
	buf bytes.Buffer
}

func (b *chunkBuilder) chunk(tag string, body []byte) *chunkBuilder {
	var hdr [16]byte
	copy(hdr[:8], tag)
	binary.LittleEndian.PutUint64(hdr[8:], uint64(len(body)))
	b.buf.Write(hdr[:])
	b.buf.Write(body)
	return b
}

func headerBody(processID uint64, arch string, excNum uint64, processName, osVersion string, loadAddr, size uint64) []byte {
	body := make([]byte, 2160)
	binary.LittleEndian.PutUint64(body[0:], processID)
	copy(body[8:24], arch)
	binary.LittleEndian.PutUint64(body[24:], excNum)
	copy(body[32:1056], processName)
	copy(body[2080:2144], osVersion)
	binary.LittleEndian.PutUint64(body[2144:], loadAddr)
	binary.LittleEndian.PutUint64(body[2152:], size)
	return body
}

func versionBody(major, minor, build uint32) []byte {
	body := make([]byte, 12)
	binary.LittleEndian.PutUint32(body[0:], major)
	binary.LittleEndian.PutUint32(body[4:], minor)
	binary.LittleEndian.PutUint32(body[8:], build)
	return body
}

func appIDBody(id uint64) []byte {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, id)
	return body
}

// rdfsBody writes just the four ULEB fields Parse reads (two ignored, then
// register size and address size); real RDFS chunks carry a much longer
// register layout table after this that nothing here needs.
func rdfsBody(regSize, addrSize byte) []byte {
	return []byte{0, 0, regSize, addrSize}
}

func moduleBody(name string, id []byte, loadAddress, size uint64) []byte {
	body := make([]byte, 1072)
	copy(body[0:1024], name)
	copy(body[1024:1056], id)
	binary.LittleEndian.PutUint64(body[1056:], loadAddress)
	binary.LittleEndian.PutUint64(body[1064:], size)
	return body
}

func mmryinfBody(address uint64, typ MemoryType, size uint64) []byte {
	body := make([]byte, 24)
	binary.LittleEndian.PutUint64(body[0:], address)
	binary.LittleEndian.PutUint64(body[8:], uint64(typ))
	binary.LittleEndian.PutUint64(body[16:], size)
	return body
}

func threadBody(id uint64, current bool, status byte, priority, idealCore, core, affinity byte, ip, sp uint64, name string, gp, gpCtrl, fp, fpCtrl []uint64) []byte {
	fixed := make([]byte, 96)
	binary.LittleEndian.PutUint64(fixed[0:], id)
	var cur uint16
	if current {
		cur = 1
	}
	binary.LittleEndian.PutUint16(fixed[8:], cur)
	binary.LittleEndian.PutUint16(fixed[10:], uint16(status))
	fixed[12] = priority
	fixed[13] = idealCore
	fixed[14] = core
	fixed[15] = affinity
	binary.LittleEndian.PutUint64(fixed[16:], ip)
	binary.LittleEndian.PutUint64(fixed[24:], sp)
	copy(fixed[32:64], name)
	binary.LittleEndian.PutUint64(fixed[64:], uint64(len(gp)))
	binary.LittleEndian.PutUint64(fixed[72:], uint64(len(gpCtrl)))
	binary.LittleEndian.PutUint64(fixed[80:], uint64(len(fp)))
	binary.LittleEndian.PutUint64(fixed[88:], uint64(len(fpCtrl)))

	var buf bytes.Buffer
	buf.Write(fixed)
	for _, regs := range [][]uint64{gp, gpCtrl, fp, fpCtrl} {
		for _, r := range regs {
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], r)
			buf.Write(b[:])
		}
	}
	return buf.Bytes()
}

func stackFramesBody(threadID uint64, frames []uint64) []byte {
	body := make([]byte, 16+8*len(frames))
	binary.LittleEndian.PutUint64(body[0:], threadID)
	binary.LittleEndian.PutUint64(body[8:], uint64(len(frames)))
	off := 16
	for _, f := range frames {
		binary.LittleEndian.PutUint64(body[off:], f)
		off += 8
	}
	return body
}

func exceptionBody(threadID uint64, is64Bit bool, code, addr uint64) []byte {
	body := make([]byte, 56)
	binary.LittleEndian.PutUint64(body[0:], threadID)
	var is64 uint32
	if is64Bit {
		is64 = 1
	}
	binary.LittleEndian.PutUint32(body[8:], is64)
	binary.LittleEndian.PutUint32(body[12:], uint32(code))
	binary.LittleEndian.PutUint64(body[16:], addr)
	return body
}

func writeDump(t *testing.T, b *chunkBuilder) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.nxdmp")
	if err := os.WriteFile(path, b.buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write synthetic dump: %v", err)
	}
	return path
}

func TestReadULEBMatchesTargetContinuationRule(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want uint32
	}{
		{"single byte, high nibble zero stops", []byte{0x04}, 4},
		{"single byte value eight", []byte{0x08}, 8},
		{"two bytes, high nibble set continues", []byte{0xAC, 0x02}, 300},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readULEB(bytes.NewReader(c.in))
			if err != nil {
				t.Fatalf("readULEB(%x): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("readULEB(%x) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestParseFullSyntheticDump(t *testing.T) {
	moduleID := bytes.Repeat([]byte{0xAA}, 32)
	gp := make([]uint64, 33)
	for i := 0; i < 29; i++ {
		gp[i] = uint64(i)
	}
	gp[29] = 0x9600 // fp
	gp[30] = 0x1400 // lr
	gp[31] = 0x9500 // sp
	gp[32] = 0x1600 // pc

	var b chunkBuilder
	b.chunk("DUMP", headerBody(163, "NX", 6, "TestGame", "1.0.0", 0, 0))
	b.chunk("VERSION", versionBody(1, 2, 0))
	b.chunk("APPID", appIDBody(0x0100152000069000))
	b.chunk("RDFS", rdfsBody(8, 8))
	b.chunk("MODL", moduleBody("main", moduleID, 0x1000, 0x2000))
	b.chunk("MMRYINF", mmryinfBody(0x1000, MemoryReadExecute, 0x1000))
	b.chunk("MMRYINF", mmryinfBody(0x2000, MemoryRead, 0x800))
	b.chunk("MMRYINF", mmryinfBody(0x2800, MemoryReadWrite, 0x800))
	b.chunk("MMRYINF", mmryinfBody(0x9000, MemoryReadWrite, 0x1000))
	b.chunk("THRD", threadBody(42, true, 'R', 1, 0, 0, 1, 0x1500, 0x9500, "Main Thread",
		gp, []uint64{0x60000000}, make([]uint64, 64), []uint64{0, 0}))
	b.chunk("STCKFRMS", stackFramesBody(42, []uint64{0x1400, 0x1500}))
	b.chunk("EXCPINF", exceptionBody(42, true, 6, 0x1600))
	b.chunk("TTY", []byte("Failed: result\n"))
	b.chunk("LZ4MRY", []byte{0xDE, 0xAD, 0xBE, 0xEF})

	path := writeDump(t, &b)
	d, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if d.Header.ProcessID != 163 || d.Header.ProcessName != "TestGame" || d.Header.Architecture != "NX" {
		t.Errorf("header = %+v", d.Header)
	}
	if d.Version != (Version{1, 2, 0}) {
		t.Errorf("version = %v, want 1.2.0", d.Version)
	}
	if !d.HasApplicationID || d.ApplicationID != 0x0100152000069000 {
		t.Errorf("application id = %#x, have=%v", d.ApplicationID, d.HasApplicationID)
	}
	if !d.Is64Bit || !d.Is64BitAddress {
		t.Errorf("Is64Bit=%v Is64BitAddress=%v, want both true", d.Is64Bit, d.Is64BitAddress)
	}

	if len(d.Modules) != 1 {
		t.Fatalf("modules = %d, want 1", len(d.Modules))
	}
	m := d.Modules[0]
	if m.Name != "main" || m.LoadAddress != 0x1000 {
		t.Errorf("module = %+v", m)
	}
	if m.CodeBase != 0x1000 || m.CodeEnd != 0x2000 {
		t.Errorf("code range = [%#x, %#x), want [0x1000, 0x2000)", m.CodeBase, m.CodeEnd)
	}
	if m.CodeRoDataBase != 0x2000 || m.CodeRoDataEnd != 0x2800 {
		t.Errorf("rodata range = [%#x, %#x), want [0x2000, 0x2800)", m.CodeRoDataBase, m.CodeRoDataEnd)
	}
	if m.CodeDataBase != 0x2800 || m.CodeDataEnd != 0x3000 {
		t.Errorf("data range = [%#x, %#x), want [0x2800, 0x3000)", m.CodeDataBase, m.CodeDataEnd)
	}

	if len(d.Threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(d.Threads))
	}
	th := d.Threads[0]
	if th.ID != 42 || !th.IsExceptionThread || th.Status != ThreadRunning {
		t.Errorf("thread = %+v", th)
	}
	if th.StackAreaBase != 0x9000 || th.StackAreaEnd != 0xA000 {
		t.Errorf("stack area = [%#x, %#x), want [0x9000, 0xa000)", th.StackAreaBase, th.StackAreaEnd)
	}
	if len(th.StackFrames) != 2 || th.StackFrames[0] != 0x1400 || th.StackFrames[1] != 0x1500 {
		t.Errorf("stack frames = %v", th.StackFrames)
	}
	if d.ExceptionThread == nil || d.ExceptionThread.ID != 42 {
		t.Errorf("exception thread = %v", d.ExceptionThread)
	}

	if d.Exception == nil {
		t.Fatal("exception info missing")
	}
	if d.Exception.Code != 6 || d.Exception.Address != 0x1600 || d.Exception.ThreadID != 42 {
		t.Errorf("exception = %+v", d.Exception)
	}

	if want := "Failed: result\n"; string(d.TTY) != want {
		t.Errorf("tty = %q, want %q", d.TTY, want)
	}

	if len(d.RawChunks) != 1 || d.RawChunks[0].Tag != "LZ4MRY" || d.RawChunks[0].Size != 4 {
		t.Fatalf("raw chunks = %+v", d.RawChunks)
	}
	raw, err := d.ReadRawChunk(d.RawChunks[0])
	if err != nil {
		t.Fatalf("ReadRawChunk: %v", err)
	}
	if !bytes.Equal(raw, []byte{0xDE, 0xAD, 0xBE, 0xEF}) {
		t.Errorf("raw chunk payload = %x", raw)
	}

	report := d.Report(true)
	for _, want := range []string{"User Break", "main", "TestGame", "0000000000001600", "X00:"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

func TestModuleWithoutMemoryRegionsFallsBackToLoadRange(t *testing.T) {
	var b chunkBuilder
	b.chunk("DUMP", headerBody(1, "NX", 6, "NoRegions", "1.0.0", 0, 0))
	b.chunk("RDFS", rdfsBody(8, 8))
	b.chunk("MODL", moduleBody("bare", bytes.Repeat([]byte{0}, 32), 0x5000, 0x2000))

	path := writeDump(t, &b)
	d, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Modules) != 1 {
		t.Fatalf("modules = %d, want 1", len(d.Modules))
	}
	m := d.Modules[0]
	if m.CodeBase != 0x5000 || m.CodeEnd != 0x7000 {
		t.Errorf("code range = [%#x, %#x), want [0x5000, 0x7000) (the raw load range)", m.CodeBase, m.CodeEnd)
	}
}

func TestReportSaysSoWhenADumpHasNoStackFrameChunk(t *testing.T) {
	// A real dump seen live (a "User Break" MarioKart8P dump): THRD and
	// registers are there, but no STCKFRMS and no MMRYINF at all - only
	// LZ4MRY compressed memory, which this reader leaves as an opaque raw
	// chunk. Report should say so plainly rather than print a misleading
	// "0x0 - 0x0" stack range and a silently empty trace that reads like
	// this project's own parser failed.
	gp := make([]uint64, 33)
	var b chunkBuilder
	b.chunk("DUMP", headerBody(141, "NX", 6, "MarioKart8P", "1.0.0", 0, 0))
	b.chunk("RDFS", rdfsBody(8, 8))
	b.chunk("THRD", threadBody(0x2b2, true, 'R', 1, 0, 0, 1, 0x1500, 0x2875b208, "Main Thread",
		gp, nil, nil, nil))

	path := writeDump(t, &b)
	d, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.HasStackFrames {
		t.Fatal("HasStackFrames = true for a dump with no STCKFRMS chunk")
	}

	report := d.Report(true)
	if !strings.Contains(report, "no memory region info in this dump") {
		t.Errorf("report should say the stack range is unknown, got:\n%s", report)
	}
	if !strings.Contains(report, "not recorded in this dump") {
		t.Errorf("report should say the stack trace was never recorded, got:\n%s", report)
	}
	if strings.Contains(report, "0x0000000000000000 - 0x0000000000000000") {
		t.Errorf("report should not print a bare zero stack range:\n%s", report)
	}
}

func TestReportSaysSoWhenNoThreadIsMarkedAsTheExceptionThread(t *testing.T) {
	// Seen live in real MarioKart8P dumps: THRD entries exist but none has
	// the CurrentThread flag set, so Report(false) (the CLI's default)
	// would otherwise silently print nothing past the module list.
	gp := make([]uint64, 33)
	var b chunkBuilder
	b.chunk("DUMP", headerBody(141, "NX", 4, "MarioKart8P", "1.0.0", 0, 0))
	b.chunk("RDFS", rdfsBody(8, 8))
	b.chunk("THRD", threadBody(0x2b2, false, 'R', 1, 0, 0, 1, 0x1500, 0x9500, "Main Thread",
		gp, nil, nil, nil))

	path := writeDump(t, &b)
	d, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.ExceptionThread != nil {
		t.Fatalf("ExceptionThread = %+v, want nil (no thread has the current-thread flag)", d.ExceptionThread)
	}

	report := d.Report(false)
	if !strings.Contains(report, "no thread in this dump is marked as the exception thread") {
		t.Errorf("report should explain why nothing follows the module list, got:\n%s", report)
	}
	if strings.Contains(report, "ThreadId") {
		t.Errorf("Report(false) should not print any thread block here:\n%s", report)
	}

	// allThreads=true should still show it despite the missing flag.
	if all := d.Report(true); !strings.Contains(all, "ThreadId") {
		t.Errorf("Report(true) should show the thread anyway:\n%s", all)
	}
}

func TestParseRejectsAMissingHeader(t *testing.T) {
	var b chunkBuilder
	b.chunk("RDFS", rdfsBody(8, 8))
	path := writeDump(t, &b)
	if _, err := Parse(path); err == nil {
		t.Fatal("expected an error for a dump with no DUMP chunk")
	}
}

func TestParseRejectsATruncatedHeaderChunk(t *testing.T) {
	var b chunkBuilder
	b.chunk("DUMP", make([]byte, 10)) // far short of the 2160-byte struct
	path := writeDump(t, &b)
	if _, err := Parse(path); err == nil {
		t.Fatal("expected an error for a truncated DUMP chunk")
	}
}

func TestExceptionTypeNameCoversTheKnownTable(t *testing.T) {
	if got := ExceptionTypeName(6); got != "User Break" {
		t.Errorf("ExceptionTypeName(6) = %q, want %q", got, "User Break")
	}
	if got := ExceptionTypeName(99); !strings.Contains(got, "99") {
		t.Errorf("ExceptionTypeName(99) = %q, want it to mention the unknown code", got)
	}
}
