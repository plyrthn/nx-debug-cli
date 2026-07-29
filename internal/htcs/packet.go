// Package htcs implements the host side of HTCS, the socket API the target
// calls across the htclow link.
//
// The direction is the thing to get straight: the *target* makes the socket
// calls and the host carries them out. A devkit publishing "iywys@$hid" has
// called socket/bind/listen/accept on that name, and it's the host that turns
// that into a real listener something can connect to. Being the daemon means
// answering those calls, not making them.
package htcs

import (
	"encoding/binary"
	"fmt"
)

// HeaderSize is fixed; a body, when there is one, follows it.
const HeaderSize = 64

// Protocol and Version as the target expects them. A packet with the wrong
// protocol is rejected outright rather than interpreted.
const (
	Protocol   int16 = 5
	MaxVersion int16 = 6
)

// Category says whether a packet asks, answers, or just informs.
type Category int16

const (
	Request      Category = 0
	Response     Category = 1
	Notification Category = 2
)

var categoryNames = map[Category]string{
	Request:      "Request",
	Response:     "Response",
	Notification: "Notification",
}

func (c Category) String() string {
	if n, ok := categoryNames[c]; ok {
		return n
	}
	return fmt.Sprintf("category %d", int16(c))
}

// Type is the operation. The numbering starts at 32 and is contiguous.
type Type int16

const (
	Receive          Type = 32
	Send             Type = 33
	Shutdown         Type = 34
	Close            Type = 35
	Connect          Type = 36
	Listen           Type = 37
	Accept           Type = 38
	Socket           Type = 39
	Bind             Type = 40
	Fcntl            Type = 41
	ReceiveLarge     Type = 42
	SendLarge        Type = 43
	Select           Type = 44
	GetTCPPortNumber Type = 45
	EventFd          Type = 46
	EventFdRead      Type = 47
	EventFdWrite     Type = 48
)

var typeNames = map[Type]string{
	Receive:          "Receive",
	Send:             "Send",
	Shutdown:         "Shutdown",
	Close:            "Close",
	Connect:          "Connect",
	Listen:           "Listen",
	Accept:           "Accept",
	Socket:           "Socket",
	Bind:             "Bind",
	Fcntl:            "Fcntl",
	ReceiveLarge:     "ReceiveLarge",
	SendLarge:        "SendLarge",
	Select:           "Select",
	GetTCPPortNumber: "GetTcpPortNumber",
	EventFd:          "EventFd",
	EventFdRead:      "EventFdRead",
	EventFdWrite:     "EventFdWrite",
}

// String names a known operation and reports an unknown one by number, so a
// type this build doesn't handle is visible rather than silently treated as a
// neighbour.
func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type %d", int16(t))
}

// Types lists every operation this build knows by name. Tests use it to check
// the dispatch table is complete.
func Types() []Type {
	out := make([]Type, 0, len(typeNames))
	for t := Receive; t <= EventFdWrite; t++ {
		if _, ok := typeNames[t]; ok {
			out = append(out, t)
		}
	}
	return out
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

// Packet is one RPC packet: a fixed header and an optional body.
type Packet struct {
	Category Category
	Type     Type
	Version  int16
	TaskID   int32
	Param    [5]int64
	Body     []byte
}

// ParseHeader decodes a header. It checks the protocol and version because a
// mismatch means the peer is speaking something else, and carrying on would
// mean acting on fields that don't mean what this code thinks.
func ParseHeader(buf []byte) (Packet, int64, error) {
	if len(buf) < HeaderSize {
		return Packet{}, 0, fmt.Errorf("htcs: %d bytes is short of a %d byte header", len(buf), HeaderSize)
	}
	p := Packet{
		Version:  int16(binary.LittleEndian.Uint16(buf[offVersion:])),
		Category: Category(binary.LittleEndian.Uint16(buf[offCategory:])),
		Type:     Type(binary.LittleEndian.Uint16(buf[offType:])),
		TaskID:   int32(binary.LittleEndian.Uint32(buf[offTaskID:])),
	}
	if proto := int16(binary.LittleEndian.Uint16(buf[offProtocol:])); proto != Protocol {
		return Packet{}, 0, fmt.Errorf("htcs: protocol %d, want %d", proto, Protocol)
	}
	if p.Version > MaxVersion {
		return Packet{}, 0, fmt.Errorf("htcs: version %d, this handles up to %d", p.Version, MaxVersion)
	}
	bodySize := int64(binary.LittleEndian.Uint64(buf[offBodySize:]))
	if bodySize < 0 {
		return Packet{}, 0, fmt.Errorf("htcs: negative body size %d", bodySize)
	}
	for i, off := range []int{offParam0, offParam1, offParam2, offParam3, offParam4} {
		p.Param[i] = int64(binary.LittleEndian.Uint64(buf[off:]))
	}
	return p, bodySize, nil
}

// Encode lays the packet out for the wire.
func (p Packet) Encode() []byte {
	buf := make([]byte, HeaderSize+len(p.Body))
	version := p.Version
	if version == 0 {
		version = MaxVersion
	}
	binary.LittleEndian.PutUint16(buf[offProtocol:], uint16(Protocol))
	binary.LittleEndian.PutUint16(buf[offVersion:], uint16(version))
	binary.LittleEndian.PutUint16(buf[offCategory:], uint16(p.Category))
	binary.LittleEndian.PutUint16(buf[offType:], uint16(p.Type))
	binary.LittleEndian.PutUint64(buf[offBodySize:], uint64(len(p.Body)))
	binary.LittleEndian.PutUint32(buf[offTaskID:], uint32(p.TaskID))
	for i, off := range []int{offParam0, offParam1, offParam2, offParam3, offParam4} {
		binary.LittleEndian.PutUint64(buf[off:], uint64(p.Param[i]))
	}
	copy(buf[HeaderSize:], p.Body)
	return buf
}

func (p Packet) String() string {
	s := fmt.Sprintf("%s %s(%d,%d,%d,%d,%d) task=%d body=%d",
		p.Category, p.Type, p.Param[0], p.Param[1], p.Param[2], p.Param[3], p.Param[4], p.TaskID, len(p.Body))
	// Show short bodies inline. The ones that matter for reading a trace are
	// handle lists and name fields, and both are small; a video frame is not
	// worth 4 KB of hex.
	if n := len(p.Body); n > 0 && n <= 32 {
		s += fmt.Sprintf(" [% x]", p.Body)
	}
	return s
}

// response builds a reply carrying a result code in Param0, which is where
// every response in the protocol puts it.
func (p Packet) response(err Errno, params ...int64) Packet {
	out := Packet{Category: Response, Type: p.Type, Version: p.Version, TaskID: p.TaskID}
	out.Param[0] = int64(err)
	for i, v := range params {
		out.Param[i+1] = v
	}
	return out
}

// notification builds the bare acknowledgement some requests need before the
// work starts. It carries nothing but the task id: its whole job is to tell
// the target the host has picked the request up.
func (p Packet) notification() Packet {
	return Packet{Category: Notification, Type: p.Type, Version: p.Version, TaskID: p.TaskID}
}

// NameFieldSize is the width of each of the two fixed name fields in a Bind
// or Connect body: a peer name then a port name, each NUL-padded.
const NameFieldSize = 32

// ParseNames pulls the peer and port out of a Bind or Connect body. A body
// too short to hold both is an error rather than a pair of empty strings,
// since binding the empty port name would silently publish nothing.
func ParseNames(body []byte) (peer, port string, err error) {
	if len(body) < 2*NameFieldSize {
		return "", "", fmt.Errorf("htcs: name body is %d bytes, want %d", len(body), 2*NameFieldSize)
	}
	return cstring(body[:NameFieldSize]), cstring(body[NameFieldSize : 2*NameFieldSize]), nil
}

func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
