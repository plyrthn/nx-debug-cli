// Package htcmisc implements the host side of HTCMISC, the small control
// protocol the target uses to ask the host for things that are not sockets and
// not files: environment variables, the host's clock, the host's working
// directory, and running a command on the host.
//
// It matters more than its size suggests. HTCMISC is one of the three services
// the reference host brings up on connect (with HTCFS and HTCS), and a target
// whose HTCMISC never answers is a target that has not finished attaching to a
// host, whatever else works.
//
// The packet layout is the same 64-byte header HTCS uses, with its own protocol
// number and its own operation numbering.
package htcmisc

import (
	"encoding/binary"
	"fmt"
)

// HeaderSize is fixed; a body, when there is one, follows it.
const HeaderSize = 64

// Protocol and MaxVersion as the target expects them.
const (
	Protocol   int16 = 4
	MaxVersion int16 = 2
)

// Module and the two channels within it. The target advertises both in its
// ready handshake as 3:0:1 and 3:0:2.
const (
	Module = 3
	// ServerChannel carries requests from the target that the host answers.
	ServerChannel = 1
	// ClientChannel carries requests the host makes of the target.
	ClientChannel = 2
)

// Category says whether a packet asks or answers.
type Category int16

const (
	Request  Category = 0
	Response Category = 1
)

func (c Category) String() string {
	switch c {
	case Request:
		return "Request"
	case Response:
		return "Response"
	}
	return fmt.Sprintf("category %d", int16(c))
}

// Type is the operation. The numbering is the target's, and the gap between
// the version pair and the rest is real.
type Type int16

const (
	GetMaxProtocolVersion        Type = 0
	SetProtocolVersion           Type = 1
	GetEnvironmentVariable       Type = 16
	GetEnvironmentVariableLength Type = 17
	SetTargetStatus              Type = 18
	RunOnHost                    Type = 19
	GetWorkingDirectory          Type = 20
	GetWorkingDirectorySize      Type = 21
	SetTargetName                Type = 22
	SyncTime                     Type = 23
)

var typeNames = map[Type]string{
	GetMaxProtocolVersion:        "GetMaxProtocolVersion",
	SetProtocolVersion:           "SetProtocolVersion",
	GetEnvironmentVariable:       "GetEnvironmentVariable",
	GetEnvironmentVariableLength: "GetEnvironmentVariableLength",
	SetTargetStatus:              "SetTargetStatus",
	RunOnHost:                    "RunOnHost",
	GetWorkingDirectory:          "GetWorkingDirectory",
	GetWorkingDirectorySize:      "GetWorkingDirectorySize",
	SetTargetName:                "SetTargetName",
	SyncTime:                     "SyncTime",
}

func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type %d", int16(t))
}

// Result is the status the host reports back in Param0.
type Result int64

const (
	Success        Result = 0
	UnknownError   Result = 1
	UnsupportedVer Result = 2
	InvalidRequest Result = 3
)

var resultNames = map[Result]string{
	Success:        "Success",
	UnknownError:   "UnknownError",
	UnsupportedVer: "UnsupportedVersion",
	InvalidRequest: "InvalidRequest",
}

func (r Result) String() string {
	if n, ok := resultNames[r]; ok {
		return n
	}
	return fmt.Sprintf("result %d", int64(r))
}

// SyncTime methods, in Param0 of a SyncTime request. The target picks which
// one it wants, and the two are answered completely differently.
const (
	// SyncByDevMenuCommand asks the host to set the clock by running a
	// command on the target. Answering it means acting, not replying with
	// data.
	SyncByDevMenuCommand int64 = 0
	// SyncByResponse asks the host for its own clock, as a string.
	SyncByResponse int64 = 1
)

// Packet is one message. Body is nil when BodySize is zero.
type Packet struct {
	Version  int16
	Category Category
	Type     Type
	TaskID   uint32
	Param0   int64
	Param1   int64
	Param2   int64
	Param3   int64
	Param4   int64
	Body     []byte
}

// Field offsets within the header.
const (
	offProtocol = 0
	offVersion  = 2
	offCategory = 4
	offType     = 6
	offBodySize = 8
	offTaskID   = 16
	offParam0   = 24
	offParam1   = 32
	offParam2   = 40
	offParam3   = 48
	offParam4   = 56
)

// ParseHeader reads a 64-byte header. The body is read separately, because how
// much to read is what the header says.
func ParseHeader(b []byte) (p Packet, bodySize int, err error) {
	if len(b) < HeaderSize {
		return Packet{}, 0, fmt.Errorf("htcmisc: header is %d bytes, need %d", len(b), HeaderSize)
	}
	proto := int16(binary.LittleEndian.Uint16(b[offProtocol:]))
	if proto != Protocol {
		return Packet{}, 0, fmt.Errorf("htcmisc: protocol %d, expected %d", proto, Protocol)
	}
	p.Version = int16(binary.LittleEndian.Uint16(b[offVersion:]))
	if p.Version > MaxVersion {
		return Packet{}, 0, fmt.Errorf("htcmisc: version %d, this build speaks up to %d", p.Version, MaxVersion)
	}
	p.Category = Category(binary.LittleEndian.Uint16(b[offCategory:]))
	p.Type = Type(binary.LittleEndian.Uint16(b[offType:]))
	size := int64(binary.LittleEndian.Uint64(b[offBodySize:]))
	if size < 0 || size > maxBody {
		return Packet{}, 0, fmt.Errorf("htcmisc: body size %d is out of range", size)
	}
	p.TaskID = binary.LittleEndian.Uint32(b[offTaskID:])
	p.Param0 = int64(binary.LittleEndian.Uint64(b[offParam0:]))
	p.Param1 = int64(binary.LittleEndian.Uint64(b[offParam1:]))
	p.Param2 = int64(binary.LittleEndian.Uint64(b[offParam2:]))
	p.Param3 = int64(binary.LittleEndian.Uint64(b[offParam3:]))
	p.Param4 = int64(binary.LittleEndian.Uint64(b[offParam4:]))
	return p, int(size), nil
}

// maxBody bounds a declared body. Everything this protocol carries is a short
// string, so anything large is a desynchronised stream rather than a real
// message.
const maxBody = 1 << 20

// Encode renders a packet as header plus body.
func (p Packet) Encode() []byte {
	out := make([]byte, HeaderSize+len(p.Body))
	binary.LittleEndian.PutUint16(out[offProtocol:], uint16(Protocol))
	version := p.Version
	if version == 0 {
		version = MaxVersion
	}
	binary.LittleEndian.PutUint16(out[offVersion:], uint16(version))
	binary.LittleEndian.PutUint16(out[offCategory:], uint16(p.Category))
	binary.LittleEndian.PutUint16(out[offType:], uint16(p.Type))
	binary.LittleEndian.PutUint64(out[offBodySize:], uint64(len(p.Body)))
	binary.LittleEndian.PutUint32(out[offTaskID:], p.TaskID)
	binary.LittleEndian.PutUint64(out[offParam0:], uint64(p.Param0))
	binary.LittleEndian.PutUint64(out[offParam1:], uint64(p.Param1))
	binary.LittleEndian.PutUint64(out[offParam2:], uint64(p.Param2))
	binary.LittleEndian.PutUint64(out[offParam3:], uint64(p.Param3))
	binary.LittleEndian.PutUint64(out[offParam4:], uint64(p.Param4))
	copy(out[HeaderSize:], p.Body)
	return out
}

// respond builds the answer to a request, carrying its task id back so the
// target can match it.
func (p Packet) respond(result Result, body []byte) Packet {
	return Packet{
		Version:  p.Version,
		Category: Response,
		Type:     p.Type,
		TaskID:   p.TaskID,
		Param0:   int64(result),
		Body:     body,
	}
}

func (p Packet) String() string {
	s := fmt.Sprintf("%s %s task=%d param0=%d body=%d", p.Category, p.Type, p.TaskID, p.Param0, len(p.Body))
	if len(p.Body) > 0 && len(p.Body) <= 64 {
		s += fmt.Sprintf(" %q", string(p.Body))
	}
	return s
}
