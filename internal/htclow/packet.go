// Package htclow builds the wire packets the devkit's transport speaks, so
// the host side of the link can be driven directly over USB instead of
// through the daemon.
//
// Two packet families share one 32-byte header shape but not their
// signatures: the control channel (htcctrl), which does connection setup, and
// the mux channel (htc gen2), which carries every service's data once the
// link is up.
package htclow

import (
	"encoding/binary"
	"fmt"
)

// HeaderSize is the same for both packet families.
const HeaderSize = 32

// Signatures. A packet whose first four bytes are neither of these is not
// ours, which is what makes it safe to sniff the pipe.
const (
	CtrlSignature uint32 = 0x78825637
	MuxSignature  uint32 = 0xA79F3540
)

// Protocol versions the two families currently speak.
const (
	CtrlVersion uint16 = 1
	MuxVersion  uint16 = 5
)

// Body size limits the wire enforces.
const (
	CtrlMaxBodySize = 57344
	MuxMaxBodySize  = 253952
	MuxDefaultBody  = 57376
)

// CtrlType is a control-channel packet type. The names say which side sends
// them, which is the whole shape of the handshake: the host connects, the
// target answers, then both declare ready.
type CtrlType uint16

const (
	ConnectFromHost       CtrlType = 16
	ConnectFromTarget     CtrlType = 17
	ReadyFromHost         CtrlType = 18
	ReadyFromTarget       CtrlType = 19
	SuspendFromHost       CtrlType = 20
	SuspendFromTarget     CtrlType = 21
	ResumeFromHost        CtrlType = 22
	ResumeFromTarget      CtrlType = 23
	DisconnectFromHost    CtrlType = 24
	DisconnectFromTarget  CtrlType = 25
	BeaconQuery           CtrlType = 28
	BeaconResponse        CtrlType = 29
	InformationFromTarget CtrlType = 33
)

var ctrlNames = map[CtrlType]string{
	ConnectFromHost:       "ConnectFromHost",
	ConnectFromTarget:     "ConnectFromTarget",
	ReadyFromHost:         "ReadyFromHost",
	ReadyFromTarget:       "ReadyFromTarget",
	SuspendFromHost:       "SuspendFromHost",
	SuspendFromTarget:     "SuspendFromTarget",
	ResumeFromHost:        "ResumeFromHost",
	ResumeFromTarget:      "ResumeFromTarget",
	DisconnectFromHost:    "DisconnectFromHost",
	DisconnectFromTarget:  "DisconnectFromTarget",
	BeaconQuery:           "BeaconQuery",
	BeaconResponse:        "BeaconResponse",
	InformationFromTarget: "InformationFromTarget",
}

// String names a known type and reports an unknown one by number rather than
// pretending it's one of the known ones.
func (t CtrlType) String() string {
	if name, ok := ctrlNames[t]; ok {
		return name
	}
	return fmt.Sprintf("ctrl type %d", uint16(t))
}

// MuxType is a data-channel packet type.
type MuxType uint16

const (
	MuxData    MuxType = 24
	MuxMaxData MuxType = 25
	MuxError   MuxType = 26
)

func (t MuxType) String() string {
	switch t {
	case MuxData:
		return "Data"
	case MuxMaxData:
		return "MaxData"
	case MuxError:
		return "Error"
	}
	return fmt.Sprintf("mux type %d", uint16(t))
}

// Field offsets. The two families agree on signature, body size, version and
// packet type, and disagree on what sits at offset 4: a sequence number for
// control, a stream offset for data.
const (
	offSignature = 0
	offSequence  = 4 // ctrl
	offOffset    = 4 // mux
	offBodySize  = 12
	offVersion   = 16
	offType      = 18
	offChannelID = 20 // mux only
	offModuleID  = 23 // mux only
	offShare     = 24 // mux only
)

// CtrlPacket builds a control packet with an optional body.
func CtrlPacket(t CtrlType, sequence uint32, body []byte) ([]byte, error) {
	if len(body) > CtrlMaxBodySize {
		return nil, fmt.Errorf("htclow: ctrl body %d bytes, limit %d", len(body), CtrlMaxBodySize)
	}
	buf := make([]byte, HeaderSize+len(body))
	binary.LittleEndian.PutUint32(buf[offSignature:], CtrlSignature)
	binary.LittleEndian.PutUint32(buf[offSequence:], sequence)
	binary.LittleEndian.PutUint32(buf[offBodySize:], uint32(len(body)))
	binary.LittleEndian.PutUint16(buf[offVersion:], CtrlVersion)
	binary.LittleEndian.PutUint16(buf[offType:], uint16(t))
	copy(buf[HeaderSize:], body)
	return buf, nil
}

// MuxPacket builds a mux packet for one channel.
//
// The two numeric fields are not interchangeable and are easy to swap.
// counterOffset is the 32-bit stream position at offset 4: the receiver
// checks it against its own running total and drops the link if they
// disagree. share is the 64-bit flow-control credit at offset 24, and it
// means the same thing on Data as on MaxData - the *sender's* own receive
// window - so a Data packet renews credit as a side effect of carrying
// payload.
func MuxPacket(t MuxType, ch Channel, counterOffset uint32, share uint64, body []byte) ([]byte, error) {
	if len(body) > MuxMaxBodySize {
		return nil, fmt.Errorf("htclow: mux body %d bytes, limit %d", len(body), MuxMaxBodySize)
	}
	buf := make([]byte, HeaderSize+len(body))
	binary.LittleEndian.PutUint32(buf[offSignature:], MuxSignature)
	binary.LittleEndian.PutUint32(buf[offOffset:], counterOffset)
	binary.LittleEndian.PutUint32(buf[offBodySize:], uint32(len(body)))
	binary.LittleEndian.PutUint16(buf[offVersion:], MuxVersion)
	binary.LittleEndian.PutUint16(buf[offType:], uint16(t))
	binary.LittleEndian.PutUint16(buf[offChannelID:], ch.ID)
	buf[offModuleID] = ch.Module
	binary.LittleEndian.PutUint64(buf[offShare:], share)
	copy(buf[HeaderSize:], body)
	return buf, nil
}

// DataPacket carries payload on a channel. counterOffset is the stream
// position of the first byte of body; share is this side's receive credit.
func DataPacket(ch Channel, counterOffset uint32, share uint64, body []byte) ([]byte, error) {
	return MuxPacket(MuxData, ch, counterOffset, share, body)
}

// MaxDataPacket advertises how much the host can currently receive on a
// channel. The host sends one per channel as soon as the channel opens, and
// again as its receive buffer drains: it's the flow control window, and
// without it the peer has no reason to believe it may send anything.
func MaxDataPacket(ch Channel, window uint64) ([]byte, error) {
	return MuxPacket(MuxMaxData, ch, 0, window, nil)
}

// ErrorPacket tells the peer a channel has failed.
func ErrorPacket(ch Channel) ([]byte, error) {
	return MuxPacket(MuxError, ch, 0, 0, nil)
}

// Channel identifies one multiplexed service channel. The host advertises
// the set it supports during the handshake, and every data packet afterwards
// names one. The ID is 16 bits on the wire even though every channel in use
// fits in a byte.
type Channel struct {
	Module uint8
	ID     uint16
}

func (c Channel) String() string { return fmt.Sprintf("%d:0:%d", c.Module, c.ID) }

// ServiceChannels is the exact set the host advertises in ReadyFromHost.
// Advertising nothing gets no ReadyFromTarget back, so this list is load
// bearing rather than informational.
var ServiceChannels = []Channel{
	{Module: 1, ID: 0},
	{Module: 3, ID: 1},
	{Module: 3, ID: 2},
	{Module: 4, ID: 0},
}

// ReadyFromHostBody builds the channel advertisement the host sends to
// finish the handshake. The middle field of each entry is a reserved zero.
// The layout is fixed by the target's parser, including the CRLF line
// endings, so this is written out literally rather than via encoding/json.
func ReadyFromHostBody(channels []Channel) []byte {
	body := "{\r\n  \"Chan\" : ["
	for i, c := range channels {
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf("\r\n \"%d:0:%d\"", c.Module, c.ID)
	}
	body += "\r\n],\r\n"
	body += fmt.Sprintf("  \"Prot\" : %d\r\n", MuxVersion)
	body += "}\r\n"
	return []byte(body)
}

// Header is a decoded packet header of either family.
type Header struct {
	Signature uint32
	Word1     uint32 // sequence for ctrl, stream offset for mux
	BodySize  uint32
	Version   uint16
	Type      uint16
	ChannelID uint16
	ModuleID  uint8
	Share     uint64
}

// Ctrl and Mux report which family a header belongs to.
func (h Header) Ctrl() bool { return h.Signature == CtrlSignature }
func (h Header) Mux() bool  { return h.Signature == MuxSignature }

// TypeName describes the packet type in terms of its own family, and refuses
// to guess for a header that is neither.
func (h Header) TypeName() string {
	switch {
	case h.Ctrl():
		return CtrlType(h.Type).String()
	case h.Mux():
		return MuxType(h.Type).String()
	}
	return fmt.Sprintf("type %d of unknown family", h.Type)
}

func (h Header) String() string {
	switch {
	case h.Ctrl():
		return fmt.Sprintf("ctrl %s seq=%d body=%d v%d", h.TypeName(), h.Word1, h.BodySize, h.Version)
	case h.Mux():
		return fmt.Sprintf("mux %s ch=%d mod=%d offset=%d body=%d share=%d v%d",
			h.TypeName(), h.ChannelID, h.ModuleID, h.Word1, h.BodySize, h.Share, h.Version)
	}
	return fmt.Sprintf("unrecognised packet, signature %#08x", h.Signature)
}

// ParseHeader decodes a header. It does not reject unknown signatures - the
// caller usually wants to see what actually arrived, and Ctrl/Mux answer that
// question.
func ParseHeader(buf []byte) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, fmt.Errorf("htclow: %d bytes is short of a %d byte header", len(buf), HeaderSize)
	}
	return Header{
		Signature: binary.LittleEndian.Uint32(buf[offSignature:]),
		Word1:     binary.LittleEndian.Uint32(buf[offOffset:]),
		BodySize:  binary.LittleEndian.Uint32(buf[offBodySize:]),
		Version:   binary.LittleEndian.Uint16(buf[offVersion:]),
		Type:      binary.LittleEndian.Uint16(buf[offType:]),
		ChannelID: binary.LittleEndian.Uint16(buf[offChannelID:]),
		ModuleID:  buf[offModuleID],
		Share:     binary.LittleEndian.Uint64(buf[offShare:]),
	}, nil
}
