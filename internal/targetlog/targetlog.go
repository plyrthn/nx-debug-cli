// Package targetlog decodes the target's log stream.
//
// A program on the target writes through nn::diag, and what comes out of
// `iywys@$LogManager` is that log in its packed binary form: a fixed header
// with the process and thread that wrote it, then a payload of key/length/value
// chunks. One record can be split across several packets, so the head and tail
// flags in the header are what say where a record begins and ends.
//
// Nothing here needs a daemon. The daemon's own log server reads exactly this
// stream and then republishes it on host-side ports for other tools; this
// decodes it directly.
package targetlog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// HeaderSize is the fixed part of every packet.
const HeaderSize = 24

// Header field offsets.
const (
	offProcessID   = 0
	offThreadID    = 8
	offFlags       = 16
	offSeverity    = 18
	offVerbosity   = 19
	offPayloadSize = 20
)

// Flag bits in the header.
const (
	flagHead         = 1 << 0
	flagTail         = 1 << 1
	flagLittleEndian = 1 << 2
)

// Severity, as nn::diag numbers it.
type Severity int

const (
	Trace Severity = 0
	Info  Severity = 1
	Warn  Severity = 2
	Error Severity = 3
	Fatal Severity = 4
)

var severityNames = map[Severity]string{
	Trace: "TRACE",
	Info:  "INFO",
	Warn:  "WARN",
	Error: "ERROR",
	Fatal: "FATAL",
}

func (s Severity) String() string {
	if n, ok := severityNames[s]; ok {
		return n
	}
	return fmt.Sprintf("SEV%d", int(s))
}

// ChunkKey identifies one field within a record's payload.
type ChunkKey int

const (
	ChunkLogSessionBegin ChunkKey = 0
	ChunkLogSessionEnd   ChunkKey = 1
	ChunkTextLog         ChunkKey = 2
	ChunkLineNumber      ChunkKey = 3
	ChunkFileName        ChunkKey = 4
	ChunkFunctionName    ChunkKey = 5
	ChunkModuleName      ChunkKey = 6
	ChunkThreadName      ChunkKey = 7
	ChunkReserved        ChunkKey = 8
	ChunkUserSystemClock ChunkKey = 9
	ChunkProcessName     ChunkKey = 10
)

var chunkNames = map[ChunkKey]string{
	ChunkLogSessionBegin: "LogSessionBegin",
	ChunkLogSessionEnd:   "LogSessionEnd",
	ChunkTextLog:         "TextLog",
	ChunkLineNumber:      "LineNumber",
	ChunkFileName:        "FileName",
	ChunkFunctionName:    "FunctionName",
	ChunkModuleName:      "ModuleName",
	ChunkThreadName:      "ThreadName",
	ChunkReserved:        "Reserved",
	ChunkUserSystemClock: "UserSystemClock",
	ChunkProcessName:     "ProcessName",
}

func (k ChunkKey) String() string {
	if n, ok := chunkNames[k]; ok {
		return n
	}
	return fmt.Sprintf("chunk %d", int(k))
}

// maxChunkKey is the highest key the target defines. A payload can carry a
// higher one, and the right thing is to skip it rather than to fail: the
// length is right there, so an unknown field costs nothing to step over.
const maxChunkKey = ChunkProcessName

// Header is one packet's fixed part.
type Header struct {
	ProcessID   uint64
	ThreadID    uint64
	Head        bool
	Tail        bool
	Severity    Severity
	Verbosity   int
	PayloadSize int
}

// maxPayload bounds a declared payload. Log records are text, so anything
// large is a desynchronised stream.
const maxPayload = 1 << 24

// ParseHeader reads a 24-byte packet header.
//
// The byte order is in the packet itself, because the target and the host need
// not agree on one. Every devkit seen here is little-endian, but reading the
// flag rather than assuming costs one branch.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("targetlog: header is %d bytes, need %d", len(b), HeaderSize)
	}
	flags := b[offFlags]
	var order binary.ByteOrder = binary.BigEndian
	if flags&flagLittleEndian != 0 {
		order = binary.LittleEndian
	}
	size := int32(order.Uint32(b[offPayloadSize:]))
	if size < 0 || size > maxPayload {
		return Header{}, fmt.Errorf("targetlog: payload size %d is out of range", size)
	}
	return Header{
		ProcessID:   order.Uint64(b[offProcessID:]),
		ThreadID:    order.Uint64(b[offThreadID:]),
		Head:        flags&flagHead != 0,
		Tail:        flags&flagTail != 0,
		Severity:    Severity(b[offSeverity]),
		Verbosity:   int(b[offVerbosity]),
		PayloadSize: int(size),
	}, nil
}

// Record is one complete log entry, reassembled from however many packets
// carried it.
type Record struct {
	ProcessID uint64
	ThreadID  uint64
	Severity  Severity
	Verbosity int

	Text         string
	FileName     string
	FunctionName string
	ModuleName   string
	ThreadName   string
	ProcessName  string
	LineNumber   int32

	// Clock is when the target says the record was written, and is zero when
	// the record carried no clock chunk.
	Clock time.Time

	SessionBegin bool
	SessionEnd   bool
}

// String renders a record the way a log viewer would: the identifying fields
// first, then the message. Empty fields are left out rather than printed as
// blanks, since most records carry only some of them.
func (r Record) String() string {
	var b strings.Builder
	if !r.Clock.IsZero() {
		b.WriteString(r.Clock.Format("15:04:05.000") + " ")
	}
	fmt.Fprintf(&b, "%-5s ", r.Severity)
	if r.ProcessName != "" {
		b.WriteString(r.ProcessName)
	} else {
		fmt.Fprintf(&b, "pid %d", r.ProcessID)
	}
	if r.ModuleName != "" {
		b.WriteString(" [" + r.ModuleName + "]")
	}
	b.WriteString(" ")

	// A session marker carries no text. Printing the empty string leaves a
	// line that looks like a message that failed to decode.
	text := strings.TrimRight(r.Text, "\r\n")
	switch {
	case text != "":
		b.WriteString(text)
	case r.SessionBegin:
		b.WriteString("(log session begin)")
	case r.SessionEnd:
		b.WriteString("(log session end)")
	}
	if r.FileName != "" {
		fmt.Fprintf(&b, "  (%s:%d)", r.FileName, r.LineNumber)
	}
	return b.String()
}

// Reader turns a stream of packets into records.
//
// Records from different threads interleave on the wire, so partial ones are
// held per process and thread until their tail arrives. A record whose tail
// never comes is simply never emitted, which is what the reference does too.
type Reader struct {
	r       io.Reader
	partial map[recordKey][]byte

	// Dropped counts packets discarded because their record had no head. That
	// happens when a stream is joined mid-record, and it is worth reporting
	// rather than hiding: it explains a missing first line.
	Dropped int
}

type recordKey struct {
	process   uint64
	thread    uint64
	severity  Severity
	verbosity int
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: r, partial: make(map[recordKey][]byte)}
}

// Next returns the next complete record.
func (rd *Reader) Next() (Record, error) {
	head := make([]byte, HeaderSize)
	for {
		if _, err := io.ReadFull(rd.r, head); err != nil {
			return Record{}, err
		}
		h, err := ParseHeader(head)
		if err != nil {
			return Record{}, err
		}
		payload := make([]byte, h.PayloadSize)
		if _, err := io.ReadFull(rd.r, payload); err != nil {
			return Record{}, err
		}

		key := recordKey{h.ProcessID, h.ThreadID, h.Severity, h.Verbosity}
		switch {
		case h.Head:
			rd.partial[key] = payload
		default:
			prev, ok := rd.partial[key]
			if !ok {
				// A continuation with nothing to continue: the stream was
				// joined part way through a record.
				rd.Dropped++
				continue
			}
			rd.partial[key] = append(prev, payload...)
		}
		if !h.Tail {
			continue
		}
		body := rd.partial[key]
		delete(rd.partial, key)
		return decode(h, body), nil
	}
}

// decode turns a reassembled payload into a record.
func decode(h Header, body []byte) Record {
	r := Record{
		ProcessID: h.ProcessID,
		ThreadID:  h.ThreadID,
		Severity:  h.Severity,
		Verbosity: h.Verbosity,
	}
	// A key can appear more than once, and the pieces belong together: that is
	// how a long message arrives. Appending rather than replacing is what keeps
	// the second half of a split line.
	text := map[ChunkKey][]byte{}
	for _, c := range Chunks(body) {
		if c.Key > maxChunkKey {
			continue
		}
		text[c.Key] = append(text[c.Key], c.Value...)
	}
	r.Text = string(text[ChunkTextLog])
	r.FileName = trimNul(text[ChunkFileName])
	r.FunctionName = trimNul(text[ChunkFunctionName])
	r.ModuleName = trimNul(text[ChunkModuleName])
	r.ThreadName = trimNul(text[ChunkThreadName])
	r.ProcessName = trimNul(text[ChunkProcessName])
	if b, ok := text[ChunkLineNumber]; ok && len(b) >= 4 {
		r.LineNumber = int32(binary.LittleEndian.Uint32(b))
	}
	if b, ok := text[ChunkUserSystemClock]; ok && len(b) >= 8 {
		// The target's clock is POSIX seconds.
		r.Clock = time.Unix(int64(binary.LittleEndian.Uint64(b)), 0)
	}
	_, r.SessionBegin = text[ChunkLogSessionBegin]
	_, r.SessionEnd = text[ChunkLogSessionEnd]
	return r
}

func trimNul(b []byte) string {
	return strings.TrimRight(string(b), "\x00")
}

// Chunk is one key/length/value field within a payload.
type Chunk struct {
	Key   ChunkKey
	Value []byte
}

// Chunks splits a payload into its fields. A malformed tail is dropped rather
// than reported, because a truncated payload still has usable fields in front
// of the damage and losing all of them would be worse.
func Chunks(b []byte) []Chunk {
	var out []Chunk
	for i := 0; i < len(b); {
		key, n, err := uleb128(b[i:])
		if err != nil {
			return out
		}
		i += n
		size, n, err := uleb128(b[i:])
		if err != nil {
			return out
		}
		i += n
		if i+int(size) > len(b) {
			return out
		}
		out = append(out, Chunk{Key: ChunkKey(key), Value: b[i : i+int(size)]})
		i += int(size)
	}
	return out
}

// errUleb is returned for a length that runs off the end or overflows.
var errUleb = errors.New("targetlog: malformed ULEB128")

// uleb128 reads one variable-length integer, returning it and how many bytes
// it took.
func uleb128(b []byte) (uint32, int, error) {
	var v uint32
	shift := 0
	for i := 0; i < len(b); i++ {
		if shift >= 32 {
			return 0, 0, errUleb
		}
		v |= uint32(b[i]&0x7f) << uint(shift)
		shift += 7
		if b[i]&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, errUleb
}
