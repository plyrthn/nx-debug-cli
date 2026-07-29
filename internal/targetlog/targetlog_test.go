package targetlog

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"
)

// packet builds one wire packet, which is what every test here needs.
func packet(pid, tid uint64, sev Severity, verbosity int, head, tail bool, payload []byte) []byte {
	b := make([]byte, HeaderSize+len(payload))
	binary.LittleEndian.PutUint64(b[offProcessID:], pid)
	binary.LittleEndian.PutUint64(b[offThreadID:], tid)
	flags := byte(flagLittleEndian)
	if head {
		flags |= flagHead
	}
	if tail {
		flags |= flagTail
	}
	b[offFlags] = flags
	b[offSeverity] = byte(sev)
	b[offVerbosity] = byte(verbosity)
	binary.LittleEndian.PutUint32(b[offPayloadSize:], uint32(len(payload)))
	copy(b[HeaderSize:], payload)
	return b
}

// chunk encodes one key/length/value field.
func chunk(key ChunkKey, value []byte) []byte {
	var b []byte
	b = appendUleb(b, uint32(key))
	b = appendUleb(b, uint32(len(value)))
	return append(b, value...)
}

func appendUleb(b []byte, v uint32) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if v == 0 {
			return b
		}
	}
}

func TestParseHeader(t *testing.T) {
	b := packet(11, 22, Warn, 3, true, false, []byte("xy"))
	h, err := ParseHeader(b[:HeaderSize])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h.ProcessID != 11 || h.ThreadID != 22 {
		t.Errorf("ids = %d/%d, want 11/22", h.ProcessID, h.ThreadID)
	}
	if h.Severity != Warn || h.Verbosity != 3 {
		t.Errorf("severity/verbosity = %v/%d, want Warn/3", h.Severity, h.Verbosity)
	}
	if !h.Head || h.Tail {
		t.Errorf("head/tail = %v/%v, want true/false", h.Head, h.Tail)
	}
	if h.PayloadSize != 2 {
		t.Errorf("payload size = %d, want 2", h.PayloadSize)
	}
}

// The packet says which byte order it used, and a host that assumes its own
// reads every id and length as garbage.
func TestParseHeaderHonoursByteOrder(t *testing.T) {
	b := make([]byte, HeaderSize)
	binary.BigEndian.PutUint64(b[offProcessID:], 0x0102030405060708)
	binary.BigEndian.PutUint32(b[offPayloadSize:], 5)
	b[offFlags] = flagHead | flagTail // little-endian bit clear

	h, err := ParseHeader(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h.ProcessID != 0x0102030405060708 {
		t.Errorf("process id = %#x, want %#x", h.ProcessID, uint64(0x0102030405060708))
	}
	if h.PayloadSize != 5 {
		t.Errorf("payload size = %d, want 5", h.PayloadSize)
	}
}

func TestParseHeaderRejectsBadPackets(t *testing.T) {
	t.Run("short", func(t *testing.T) {
		if _, err := ParseHeader(make([]byte, HeaderSize-1)); err == nil {
			t.Error("a short header was accepted")
		}
	})
	t.Run("negative payload", func(t *testing.T) {
		b := make([]byte, HeaderSize)
		b[offFlags] = flagLittleEndian
		binary.LittleEndian.PutUint32(b[offPayloadSize:], 0xffffffff)
		if _, err := ParseHeader(b); err == nil {
			t.Error("a negative payload size was accepted")
		}
	})
	t.Run("oversized payload", func(t *testing.T) {
		b := make([]byte, HeaderSize)
		b[offFlags] = flagLittleEndian
		binary.LittleEndian.PutUint32(b[offPayloadSize:], maxPayload+1)
		if _, err := ParseHeader(b); err == nil {
			t.Error("an oversized payload was accepted")
		}
	})
}

func TestReadOneRecord(t *testing.T) {
	payload := bytes.Join([][]byte{
		chunk(ChunkTextLog, []byte("hello world\n")),
		chunk(ChunkModuleName, []byte("MyModule\x00")),
		chunk(ChunkProcessName, []byte("MyApp\x00")),
		chunk(ChunkFileName, []byte("main.cpp\x00")),
		chunk(ChunkLineNumber, []byte{42, 0, 0, 0}),
		chunk(ChunkUserSystemClock, func() []byte {
			b := make([]byte, 8)
			binary.LittleEndian.PutUint64(b, 1700000000)
			return b
		}()),
	}, nil)

	r := NewReader(bytes.NewReader(packet(5, 6, Error, 0, true, true, payload)))
	rec, err := r.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if rec.Text != "hello world\n" {
		t.Errorf("text = %q, want %q", rec.Text, "hello world\n")
	}
	if rec.ModuleName != "MyModule" || rec.ProcessName != "MyApp" || rec.FileName != "main.cpp" {
		t.Errorf("names = %q/%q/%q", rec.ModuleName, rec.ProcessName, rec.FileName)
	}
	if rec.LineNumber != 42 {
		t.Errorf("line = %d, want 42", rec.LineNumber)
	}
	if !rec.Clock.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("clock = %v, want %v", rec.Clock, time.Unix(1700000000, 0))
	}
	if rec.Severity != Error {
		t.Errorf("severity = %v, want Error", rec.Severity)
	}
}

// A long message arrives split across packets, and only the head/tail flags
// say where it starts and stops.
func TestRecordsAreReassembled(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(packet(1, 1, Info, 0, true, false, chunk(ChunkTextLog, []byte("first "))))
	buf.Write(packet(1, 1, Info, 0, false, false, chunk(ChunkTextLog, []byte("second "))))
	buf.Write(packet(1, 1, Info, 0, false, true, chunk(ChunkTextLog, []byte("third"))))

	r := NewReader(&buf)
	rec, err := r.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if rec.Text != "first second third" {
		t.Errorf("text = %q, want %q", rec.Text, "first second third")
	}
}

// Threads write concurrently, so their packets interleave. Reassembling by
// arrival order rather than by thread would splice two messages together.
func TestInterleavedThreadsStaySeparate(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(packet(1, 100, Info, 0, true, false, chunk(ChunkTextLog, []byte("thread A "))))
	buf.Write(packet(1, 200, Info, 0, true, false, chunk(ChunkTextLog, []byte("thread B "))))
	buf.Write(packet(1, 200, Info, 0, false, true, chunk(ChunkTextLog, []byte("end"))))
	buf.Write(packet(1, 100, Info, 0, false, true, chunk(ChunkTextLog, []byte("end"))))

	r := NewReader(&buf)
	first, err := r.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if first.Text != "thread B end" || first.ThreadID != 200 {
		t.Errorf("first = %q from thread %d, want %q from 200", first.Text, first.ThreadID, "thread B end")
	}
	second, err := r.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if second.Text != "thread A end" || second.ThreadID != 100 {
		t.Errorf("second = %q from thread %d, want %q from 100", second.Text, second.ThreadID, "thread A end")
	}
}

// Joining a live stream lands mid-record. Those packets have to be dropped and
// counted, not glued onto the next record.
func TestContinuationWithoutAHeadIsDropped(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(packet(1, 1, Info, 0, false, false, chunk(ChunkTextLog, []byte("orphan "))))
	buf.Write(packet(1, 1, Info, 0, false, true, chunk(ChunkTextLog, []byte("tail"))))
	buf.Write(packet(1, 1, Info, 0, true, true, chunk(ChunkTextLog, []byte("clean"))))

	r := NewReader(&buf)
	rec, err := r.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if rec.Text != "clean" {
		t.Errorf("text = %q, want %q: an orphan continuation leaked in", rec.Text, "clean")
	}
	if r.Dropped != 2 {
		t.Errorf("dropped = %d, want 2", r.Dropped)
	}
}

func TestReaderReportsEOF(t *testing.T) {
	r := NewReader(bytes.NewReader(nil))
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("error = %v, want io.EOF", err)
	}
}

// An unknown key is skipped using its own length, so a firmware that adds a
// field does not make the rest of the payload unreadable.
func TestUnknownChunkKeysAreSkipped(t *testing.T) {
	payload := bytes.Join([][]byte{
		chunk(ChunkKey(200), []byte("something new")),
		chunk(ChunkTextLog, []byte("still here")),
	}, nil)

	r := NewReader(bytes.NewReader(packet(1, 1, Info, 0, true, true, payload)))
	rec, err := r.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if rec.Text != "still here" {
		t.Errorf("text = %q, want %q", rec.Text, "still here")
	}
}

func TestChunksStopsAtATruncatedTail(t *testing.T) {
	good := chunk(ChunkTextLog, []byte("kept"))
	truncated := append(good, chunk(ChunkModuleName, []byte("lost"))...)
	truncated = truncated[:len(truncated)-2] // cut into the value

	got := Chunks(truncated)
	if len(got) != 1 || string(got[0].Value) != "kept" {
		t.Errorf("chunks = %v, want just the complete one", got)
	}
}

func TestUleb128(t *testing.T) {
	for _, v := range []uint32{0, 1, 127, 128, 300, 16384, 0x7fffffff} {
		b := appendUleb(nil, v)
		got, n, err := uleb128(b)
		if err != nil {
			t.Errorf("uleb128(%d): %v", v, err)
			continue
		}
		if got != v || n != len(b) {
			t.Errorf("uleb128(%d) = %d in %d bytes, want %d in %d", v, got, n, v, len(b))
		}
	}
	t.Run("unterminated", func(t *testing.T) {
		if _, _, err := uleb128([]byte{0x80, 0x80}); err == nil {
			t.Error("an unterminated value was accepted")
		}
	})
	t.Run("overflow", func(t *testing.T) {
		if _, _, err := uleb128([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80}); err == nil {
			t.Error("an oversized value was accepted")
		}
	})
}

func TestSeverityNames(t *testing.T) {
	for sev, want := range map[Severity]string{
		Trace: "TRACE", Info: "INFO", Warn: "WARN", Error: "ERROR", Fatal: "FATAL",
	} {
		if got := sev.String(); got != want {
			t.Errorf("severity %d = %q, want %q", int(sev), got, want)
		}
	}
	if got := Severity(9).String(); got != "SEV9" {
		t.Errorf("unknown severity = %q, want SEV9", got)
	}
}

// Every key the target defines needs a name, or a chunk dump reports numbers.
func TestEveryChunkKeyHasAName(t *testing.T) {
	for k := ChunkLogSessionBegin; k <= maxChunkKey; k++ {
		if _, ok := chunkNames[k]; !ok {
			t.Errorf("chunk key %d has no name", int(k))
		}
	}
}

func TestRecordString(t *testing.T) {
	rec := Record{
		Severity:    Warn,
		ProcessName: "MyApp",
		ModuleName:  "Net",
		Text:        "connection lost\n",
		FileName:    "net.cpp",
		LineNumber:  17,
	}
	got := rec.String()
	for _, want := range []string{"WARN", "MyApp", "[Net]", "connection lost", "net.cpp:17"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "connection lost\n") {
		t.Errorf("%q kept the trailing newline", got)
	}
}

// A record with no process name still has to identify where it came from.
func TestRecordStringFallsBackToTheProcessId(t *testing.T) {
	got := Record{Severity: Info, ProcessID: 77, Text: "x"}.String()
	if !strings.Contains(got, "pid 77") {
		t.Errorf("%q does not name the process", got)
	}
}

// Session markers carry no text, and a bare severity and pid reads like a
// message that failed to decode.
func TestSessionMarkersSayWhatTheyAre(t *testing.T) {
	begin := Record{Severity: Info, ProcessID: 151, SessionBegin: true}.String()
	if !strings.Contains(begin, "session begin") {
		t.Errorf("%q does not say it is a session begin", begin)
	}
	end := Record{Severity: Info, ProcessID: 151, SessionEnd: true}.String()
	if !strings.Contains(end, "session end") {
		t.Errorf("%q does not say it is a session end", end)
	}
}

// Verified against a real capture: a game launched on the devkit produced
// these, split across packets, with the file and line in their own chunks.
func TestDecodesARealRecordShape(t *testing.T) {
	payload := bytes.Join([][]byte{
		chunk(ChunkProcessName, []byte("game_nx_release\x00")),
		chunk(ChunkTextLog, []byte("total heap size: 4316 MB")),
		chunk(ChunkFileName, []byte("../../../system/main.h\x00")),
		chunk(ChunkLineNumber, []byte{0xa7, 0x0a, 0, 0}), // 2727
	}, nil)

	r := NewReader(bytes.NewReader(packet(151, 300, Info, 0, true, true, payload)))
	rec, err := r.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if rec.ProcessName != "game_nx_release" {
		t.Errorf("process = %q, want %q", rec.ProcessName, "game_nx_release")
	}
	if rec.LineNumber != 2727 {
		t.Errorf("line = %d, want 2727", rec.LineNumber)
	}
	got := rec.String()
	for _, want := range []string{"game_nx_release", "total heap size: 4316 MB", "main.h:2727"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing %q", got, want)
		}
	}
}
