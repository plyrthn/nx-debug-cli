package htc

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// Virtual controllers on the target are "abstracted pads": the host declares
// a device (type, interface, connected), then streams button and stick
// state for it. Everything rides the same fixed 12-byte chunk format as the
// mouse and touch messages on the same channel.
//
// Up to 8 pads can exist at once, addressed by ID, so a second player is
// just another ID rather than a second connection.

const (
	msgPadDeviceData inputMessageType = 21
	msgPadColor      inputMessageType = 22
	msgPadPower      inputMessageType = 23
	msgPadButton     inputMessageType = 24
	msgPadStick      inputMessageType = 25
)

// PadButton is the target's button bitmask. The stick-direction bits are
// separate from the analogue stick position: a game reading the D-pad-style
// digital stick sees these, so a pad that only sets the analogue position
// won't register as pushing a direction.
type PadButton uint32

const (
	PadA PadButton = 1 << iota
	PadB
	PadX
	PadY
	PadStickLPress
	PadStickRPress
	PadL
	PadR
	PadZL
	PadZR
	PadPlus
	PadMinus
	PadLeft
	PadUp
	PadRight
	PadDown
	PadSL
	PadSR
	PadHome
	// PadCapture is the screenshot button.
	PadCapture
	PadStickLLeft
	PadStickLUp
	PadStickLRight
	PadStickLDown
	PadStickRLeft
	PadStickRUp
	PadStickRRight
	PadStickRDown
)

// PadDeviceType picks what the target thinks is plugged in. The gaps in the
// numbering are intentional, not a mistake.
type PadDeviceType uint8

const (
	PadProController PadDeviceType = 0
	PadHandheldJoyL  PadDeviceType = 2
	PadHandheldJoyR  PadDeviceType = 3
	PadJoyConLeft    PadDeviceType = 4
	PadJoyConRight   PadDeviceType = 5
)

// PadInterfaceType is how the device claims to be attached.
type PadInterfaceType uint8

const (
	PadInterfaceUnknown PadInterfaceType = iota
	PadInterfaceBluetooth
	PadInterfaceRail
	PadInterfaceUSB
)

// PadStickSide selects which analogue stick a position applies to.
type PadStickSide uint8

const (
	PadStickLeft PadStickSide = iota
	PadStickRight
)

// PadBatteryLevel is what the target reports for the virtual pad's battery.
type PadBatteryLevel uint8

const (
	PadBatteryEmpty PadBatteryLevel = iota
	PadBatteryCritical
	PadBatteryLow
	PadBatteryMedium
	PadBatteryHigh
)

// PadColorKind selects the body colour or the button/grip colour.
type PadColorKind uint8

const (
	PadColorMain PadColorKind = iota
	PadColorSub
)

// pad device attribute bits.
const (
	padAttrConnected          uint32 = 1
	padAttrSixAxisSensorReady uint32 = 2
)

// PadConnect declares a virtual controller to the target. Nothing else about
// a pad is accepted until it's been declared, so this comes first.
func (r *RemoteInput) PadConnect(id uint8, device PadDeviceType, iface PadInterfaceType) error {
	return r.padDeviceData(id, device, iface, padAttrConnected)
}

// PadDisconnect removes a virtual controller.
func (r *RemoteInput) PadDisconnect(id uint8) error {
	return r.padDeviceData(id, PadProController, PadInterfaceUnknown, 0)
}

func (r *RemoteInput) padDeviceData(id uint8, device PadDeviceType, iface PadInterfaceType, attr uint32) error {
	return r.send(msgPadDeviceData, func(b []byte) {
		b[4] = id
		b[5] = byte(iface)
		b[6] = byte(device)
		binary.BigEndian.PutUint32(b[8:], attr)
	})
}

// PadButtons sets the held-button mask. Button state is absolute, so send
// the whole mask every time any of it changes.
func (r *RemoteInput) PadButtons(id uint8, buttons PadButton) error {
	return r.send(msgPadButton, func(b []byte) {
		b[4] = id
		binary.BigEndian.PutUint32(b[8:], uint32(buttons))
	})
}

// PadStick sets an analogue stick position. The axes run the full int16
// range with y positive upwards, which is the opposite of screen
// coordinates.
func (r *RemoteInput) PadStick(id uint8, side PadStickSide, x, y int16) error {
	return r.send(msgPadStick, func(b []byte) {
		b[4] = id
		b[5] = byte(side)
		binary.BigEndian.PutUint16(b[8:], uint16(x))
		binary.BigEndian.PutUint16(b[10:], uint16(y))
	})
}

// PadPower sets the virtual pad's battery state. Some UI on the target shows
// it, and a pad left at "empty" can read as flat.
func (r *RemoteInput) PadPower(id uint8, powered, charging bool, level PadBatteryLevel) error {
	return r.send(msgPadPower, func(b []byte) {
		b[4] = id
		b[5] = boolByte(powered)
		b[6] = boolByte(charging)
		b[7] = byte(level)
	})
}

// PadColor sets one of the pad's two colours.
func (r *RemoteInput) PadColor(id uint8, kind PadColorKind, red, green, blue, alpha uint8) error {
	return r.send(msgPadColor, func(b []byte) {
		b[4] = id
		b[5] = byte(kind)
		b[8], b[9], b[10], b[11] = red, green, blue, alpha
	})
}

// PadAttach brings a virtual Pro Controller up in one call: declared,
// powered, centred and with nothing held. That's the state a real pad is in
// the moment it connects, and skipping any of it leaves the target showing a
// half-configured device.
func (r *RemoteInput) PadAttach(id uint8) error {
	if err := r.PadConnect(id, PadProController, PadInterfaceUSB); err != nil {
		return err
	}
	if err := r.PadPower(id, true, false, PadBatteryHigh); err != nil {
		return err
	}
	if err := r.PadButtons(id, 0); err != nil {
		return err
	}
	if err := r.PadStick(id, PadStickLeft, 0, 0); err != nil {
		return err
	}
	return r.PadStick(id, PadStickRight, 0, 0)
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// The HDLS pad messages are a second, newer way to drive the same virtual
// controllers. They carry the same button bits as the abstracted-pad messages
// above but in wider fields, and they add an npad id so a pad can be tied to a
// player slot.
//
// Which set a target speaks is not a matter of preference: it announces its own
// pads on connect using one of them, and this firmware uses HDLS. Both are
// implemented because neither is a superset of the other.

// HdlsPadButtons sets the held-button mask on an HDLS virtual controller.
// Button state is absolute: send the whole mask on every change, and 0 to
// release everything.
func (r *RemoteInput) HdlsPadButtons(id uint8, buttons PadButton) error {
	return r.sendSized(msgHdlsPadButton, 16, func(b []byte) {
		b[4] = id
		binary.BigEndian.PutUint64(b[8:], uint64(buttons))
	})
}

// HdlsPadStick sets an analogue stick position on an HDLS pad.
func (r *RemoteInput) HdlsPadStick(id uint8, side PadStickSide, x, y int16) error {
	return r.send(msgHdlsPadStick, func(b []byte) {
		b[4] = id
		b[5] = byte(side)
		binary.BigEndian.PutUint16(b[8:], uint16(x))
		binary.BigEndian.PutUint16(b[10:], uint16(y))
	})
}

// HdlsPadNpadID assigns the player slot an HDLS pad reports as. The target
// announces this for its own pads, and until a pad has one there is no slot to
// attribute its input to.
func (r *RemoteInput) HdlsPadNpadID(id uint8, npadID uint32) error {
	return r.send(msgHdlsPadNpadID, func(b []byte) {
		b[4] = id
		binary.BigEndian.PutUint32(b[8:], npadID)
	})
}

// HdlsPadConnect announces an HDLS virtual controller as present and
// connected. This is the same message the target sends to describe its own
// pads.
func (r *RemoteInput) HdlsPadConnect(id uint8) error {
	return r.hdlsPadDeviceData(id, padAttrConnected)
}

// HdlsPadDisconnect takes an HDLS virtual controller away again.
func (r *RemoteInput) HdlsPadDisconnect(id uint8) error {
	return r.hdlsPadDeviceData(id, 0)
}

func (r *RemoteInput) hdlsPadDeviceData(id uint8, attr uint32) error {
	return r.sendSized(msgHdlsPadDeviceData, 20, func(b []byte) {
		b[4] = id
		b[5] = boolByte(true)       // powered
		b[7] = byte(PadBatteryHigh) // battery level
		binary.BigEndian.PutUint32(b[8:], attr)
		// Body then button colour, RGBA each. Cosmetic, but the message
		// carries them and a pad announced in all-zero black looks broken in
		// the target's own UI.
		copy(b[12:], []byte{0x82, 0x82, 0x82, 0xff, 0x0f, 0x0f, 0x0f, 0xff})
	})
}

// padButtonNames maps a name to its bit, so a command line can take button
// names instead of a hex mask. It lives next to the constants deliberately: a
// button added above without a name here is a button nothing can reach, and
// TestEveryPadButtonHasAName fails when that happens.
var padButtonNames = map[string]PadButton{
	"a": PadA, "b": PadB, "x": PadX, "y": PadY,
	"stick-l": PadStickLPress, "stick-r": PadStickRPress,
	"l": PadL, "r": PadR, "zl": PadZL, "zr": PadZR,
	"plus": PadPlus, "minus": PadMinus,
	"left": PadLeft, "up": PadUp, "right": PadRight, "down": PadDown,
	"sl": PadSL, "sr": PadSR,
	"home": PadHome, "capture": PadCapture,
	"stick-l-left": PadStickLLeft, "stick-l-up": PadStickLUp,
	"stick-l-right": PadStickLRight, "stick-l-down": PadStickLDown,
	"stick-r-left": PadStickRLeft, "stick-r-up": PadStickRUp,
	"stick-r-right": PadStickRRight, "stick-r-down": PadStickRDown,
}

// ParsePadButtons turns button names into a mask. An unknown name is an error
// rather than a silent zero: a typo would otherwise read as "nothing held" and
// look exactly like the target ignoring the input.
func ParsePadButtons(names []string) (PadButton, error) {
	var mask PadButton
	for _, n := range names {
		b, ok := padButtonNames[strings.ToLower(strings.TrimSpace(n))]
		if !ok {
			return 0, fmt.Errorf("htc: unknown button %q (have %s)", n, strings.Join(PadButtonNames(), " "))
		}
		mask |= b
	}
	return mask, nil
}

// PadButtonNames lists what ParsePadButtons accepts, sorted so usage text and
// tests are stable.
func PadButtonNames() []string {
	names := make([]string, 0, len(padButtonNames))
	for n := range padButtonNames {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
