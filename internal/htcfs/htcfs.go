// Package htcfs implements the host side of HTCFS, the protocol a program
// running on the target uses to reach the host's filesystem.
//
// This is the third of the three services a target manager brings up on
// connect, alongside HTCS and HTCMISC. The target opens its channel the moment
// the link comes up and asks which protocol version the host speaks; a host
// that never answers leaves that request outstanding for the whole session.
//
// The header is the same 64-byte shape the other two use, with one real
// difference: there is **no task id**, and the parameters start at offset 16
// rather than 24. Requests are answered strictly in order, which is why no id
// is needed.
package htcfs

import (
	"encoding/binary"
	"fmt"
)

// HeaderSize is fixed; a body, when there is one, follows it.
const HeaderSize = 64

// Protocol and MaxVersion as the target expects them.
const (
	Protocol   int16 = 1
	MaxVersion int16 = 1
)

// Module and Channel. The target advertises this as 1:0:0 in its ready
// handshake, which is the first channel it names.
const (
	Module  = 1
	Channel = 0
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

// Type is the operation. The numbering is the target's, and the gaps are real.
type Type int16

const (
	GetMaxProtocolVersion   Type = 0
	SetProtocolVersion      Type = 1
	GetEntryType            Type = 16
	OpenFile                Type = 32
	CloseFile               Type = 33
	GetPriorityForFile      Type = 34
	SetPriorityForFile      Type = 35
	CreateFile              Type = 36
	DeleteFile              Type = 37
	RenameFile              Type = 38
	FileExists              Type = 39
	ReadFile                Type = 40
	WriteFile               Type = 41
	FlushFile               Type = 42
	GetFileTimeStamp        Type = 43
	GetFileSize             Type = 44
	SetFileSize             Type = 45
	ReadFileLarge           Type = 46
	WriteFileLarge          Type = 47
	OpenDirectory           Type = 48
	CloseDirectory          Type = 49
	GetPriorityForDirectory Type = 50
	SetPriorityForDirectory Type = 51
	CreateDirectory         Type = 52
	DeleteDirectory         Type = 53
	RenameDirectory         Type = 54
	DirectoryExists         Type = 55
	ReadDirectory           Type = 56
	GetEntryCount           Type = 57
	GetWorkingDirectory     Type = 58
	GetWorkingDirectorySize Type = 59
	GetCaseSensitivePath    Type = 60
	GetDiskFreeSpace        Type = 61
	ReadDirectoryLarge      Type = 62
	GetFileSystemAttribute  Type = 63
)

var typeNames = map[Type]string{
	GetMaxProtocolVersion:   "GetMaxProtocolVersion",
	SetProtocolVersion:      "SetProtocolVersion",
	GetEntryType:            "GetEntryType",
	OpenFile:                "OpenFile",
	CloseFile:               "CloseFile",
	GetPriorityForFile:      "GetPriorityForFile",
	SetPriorityForFile:      "SetPriorityForFile",
	CreateFile:              "CreateFile",
	DeleteFile:              "DeleteFile",
	RenameFile:              "RenameFile",
	FileExists:              "FileExists",
	ReadFile:                "ReadFile",
	WriteFile:               "WriteFile",
	FlushFile:               "FlushFile",
	GetFileTimeStamp:        "GetFileTimeStamp",
	GetFileSize:             "GetFileSize",
	SetFileSize:             "SetFileSize",
	ReadFileLarge:           "ReadFileLarge",
	WriteFileLarge:          "WriteFileLarge",
	OpenDirectory:           "OpenDirectory",
	CloseDirectory:          "CloseDirectory",
	GetPriorityForDirectory: "GetPriorityForDirectory",
	SetPriorityForDirectory: "SetPriorityForDirectory",
	CreateDirectory:         "CreateDirectory",
	DeleteDirectory:         "DeleteDirectory",
	RenameDirectory:         "RenameDirectory",
	DirectoryExists:         "DirectoryExists",
	ReadDirectory:           "ReadDirectory",
	GetEntryCount:           "GetEntryCount",
	GetWorkingDirectory:     "GetWorkingDirectory",
	GetWorkingDirectorySize: "GetWorkingDirectorySize",
	GetCaseSensitivePath:    "GetCaseSensitivePath",
	GetDiskFreeSpace:        "GetDiskFreeSpace",
	ReadDirectoryLarge:      "ReadDirectoryLarge",
	GetFileSystemAttribute:  "GetFileSystemAttribute",
}

func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type %d", int16(t))
}

// Result is the protocol-level status, carried in Param0 of every response.
//
// It is not the filesystem's answer. "The file does not exist" is a Success at
// this level with the filesystem's own result in Param1; a non-Success here
// means the request itself was unusable.
type Result int64

const (
	Success         Result = 0
	UnknownError    Result = 1
	UnsupportedVer  Result = 2
	InvalidRequest  Result = 3
	InvalidHandle   Result = 4
	OutOfHandle     Result = 5
	Ready           Result = 6
	NotEnoughBuffer Result = 7
)

var resultNames = map[Result]string{
	Success:         "Success",
	UnknownError:    "UnknownError",
	UnsupportedVer:  "UnsupportedVersion",
	InvalidRequest:  "InvalidRequest",
	InvalidHandle:   "InvalidHandle",
	OutOfHandle:     "OutOfHandle",
	Ready:           "Ready",
	NotEnoughBuffer: "NotEnoughBuffer",
}

func (r Result) String() string {
	if n, ok := resultNames[r]; ok {
		return n
	}
	return fmt.Sprintf("result %d", int64(r))
}

// OpenMode bits for OpenFile.
const (
	OpenRead        = 1
	OpenWrite       = 2
	OpenAllowAppend = 4
)

// OpenDirectoryMode bits for OpenDirectory: which entry kinds to list.
const (
	ListDirectories = 1
	ListFiles       = 2
	ListAll         = 3
)

// Entry types, as the target numbers them.
const (
	EntryDirectory = 0
	EntryFile      = 1
)

// WriteOptionFlush asks for the write to be flushed before it is acknowledged.
const WriteOptionFlush = 1

// Packet is one message. Body is nil when BodySize is zero.
type Packet struct {
	Version  int16
	Category Category
	Type     Type
	Param0   int64
	Param1   int64
	Param2   int64
	Param3   int64
	Param4   int64
	Body     []byte
}

// Field offsets within the header. Note that the parameters begin at 16, not
// at 24 as in HTCS and HTCMISC: this protocol has no task id.
const (
	offProtocol = 0
	offVersion  = 2
	offCategory = 4
	offType     = 6
	offBodySize = 8
	offParam0   = 16
	offParam1   = 24
	offParam2   = 32
	offParam3   = 40
	offParam4   = 48
)

// MaxPathLength bounds a path body. The target enforces the same limit, so
// anything longer is a desynchronised stream rather than a long name.
const MaxPathLength = 768

// maxBody bounds a declared body generally. File data can be large, so this is
// far above the path limit while still refusing a length that can only be
// garbage.
const maxBody = 1 << 30

// ParseHeader reads a 64-byte header. The body is read separately, because how
// much to read is what the header says.
func ParseHeader(b []byte) (p Packet, bodySize int64, err error) {
	if len(b) < HeaderSize {
		return Packet{}, 0, fmt.Errorf("htcfs: header is %d bytes, need %d", len(b), HeaderSize)
	}
	proto := int16(binary.LittleEndian.Uint16(b[offProtocol:]))
	if proto != Protocol {
		return Packet{}, 0, fmt.Errorf("htcfs: protocol %d, expected %d", proto, Protocol)
	}
	p.Version = int16(binary.LittleEndian.Uint16(b[offVersion:]))
	if p.Version > MaxVersion {
		return Packet{}, 0, fmt.Errorf("htcfs: version %d, this build speaks up to %d", p.Version, MaxVersion)
	}
	p.Category = Category(binary.LittleEndian.Uint16(b[offCategory:]))
	p.Type = Type(binary.LittleEndian.Uint16(b[offType:]))
	size := int64(binary.LittleEndian.Uint64(b[offBodySize:]))
	if size < 0 || size > maxBody {
		return Packet{}, 0, fmt.Errorf("htcfs: body size %d is out of range", size)
	}
	p.Param0 = int64(binary.LittleEndian.Uint64(b[offParam0:]))
	p.Param1 = int64(binary.LittleEndian.Uint64(b[offParam1:]))
	p.Param2 = int64(binary.LittleEndian.Uint64(b[offParam2:]))
	p.Param3 = int64(binary.LittleEndian.Uint64(b[offParam3:]))
	p.Param4 = int64(binary.LittleEndian.Uint64(b[offParam4:]))
	return p, size, nil
}

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
	binary.LittleEndian.PutUint64(out[offParam0:], uint64(p.Param0))
	binary.LittleEndian.PutUint64(out[offParam1:], uint64(p.Param1))
	binary.LittleEndian.PutUint64(out[offParam2:], uint64(p.Param2))
	binary.LittleEndian.PutUint64(out[offParam3:], uint64(p.Param3))
	binary.LittleEndian.PutUint64(out[offParam4:], uint64(p.Param4))
	copy(out[HeaderSize:], p.Body)
	return out
}

// respond builds the answer to a request. params fills Param1 onward, since
// Param0 is always the protocol-level result.
func (p Packet) respond(result Result, params ...int64) Packet {
	out := Packet{
		Version:  p.Version,
		Category: Response,
		Type:     p.Type,
		Param0:   int64(result),
	}
	for i, v := range params {
		switch i {
		case 0:
			out.Param1 = v
		case 1:
			out.Param2 = v
		case 2:
			out.Param3 = v
		case 3:
			out.Param4 = v
		}
	}
	return out
}

func (p Packet) String() string {
	return fmt.Sprintf("%s %s params=%d,%d,%d,%d,%d body=%d",
		p.Category, p.Type, p.Param0, p.Param1, p.Param2, p.Param3, p.Param4, len(p.Body))
}

// Filesystem results, as nn::fs defines them: a module in the low 9 bits and a
// description above. The target compares against these exact values, so they
// are not guesses.
const (
	fsSuccess              int64 = 0
	fsPathNotFound         int64 = 2 | (1 << 9)
	fsPathAlreadyExists    int64 = 2 | (2 << 9)
	fsTargetLocked         int64 = 2 | (7 << 9)
	fsDirectoryNotEmpty    int64 = 2 | (8 << 9)
	fsUsableSpaceNotEnough int64 = 2 | (30 << 9)
	fsOutOfRange           int64 = 2 | (3005 << 9)
	fsInvalidCharacter     int64 = 2 | (6004 << 9)
	fsUnexpected           int64 = 2 | (5000 << 9)
)

// DirectoryEntrySize is the on-wire size of one nn::fs::DirectoryEntry:
// a 769-byte name, three reserved bytes, a one-byte type, three more reserved
// bytes and an eight-byte size.
const DirectoryEntrySize = 784

const (
	entryNameSize = 769
	entryOffType  = 772
	entryOffSize  = 776
)
