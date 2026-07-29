// Package usbdev opens the devkit's USB interface directly, without the
// daemon in the way.
//
// On Windows the devkit binds to Microsoft's generic WinUSB driver rather
// than a custom kernel driver (its DEVPKEY_Device_Service really is
// "WINUSB"), so the daemon has no privileged access to it - it is an
// ordinary user-mode WinUSB client, and so is this. On Linux and macOS it
// enumerates as an equally ordinary vendor-specific USB device with no
// driver binding at all, opened through libusb instead. Either way, nothing
// here needs a kernel-mode driver install or admin/root beyond whatever the
// platform normally requires for raw USB access. Everything here is the
// plumbing under the HTC transport, not the protocol itself.
package usbdev

import "fmt"

// DevkitVendorID and DevkitProductID identify the devkit on backends that
// enumerate by VID/PID rather than by interface GUID (everywhere but
// Windows, which instead matches on DevkitInterface below).
const (
	DevkitVendorID  = 0x057e
	DevkitProductID = 0x3005
)

// GUID is a Windows interface GUID, declared locally so this package's API
// is the same shape on every platform.
type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// DevkitInterface is the device interface GUID the devkit's INF registers.
// It is the same GUID that shows up on the end of the connection destination
// the daemon reports for a USB target, which is where it was found.
var DevkitInterface = GUID{
	Data1: 0xf44ff076,
	Data2: 0x9fd0,
	Data3: 0x467d,
	Data4: [8]byte{0x87, 0x2e, 0xe9, 0x91, 0x33, 0x34, 0x89, 0xe9},
}

// PipeType is the USB endpoint transfer type.
type PipeType uint8

const (
	PipeControl PipeType = iota
	PipeIsochronous
	PipeBulk
	PipeInterrupt
)

func (t PipeType) String() string {
	switch t {
	case PipeControl:
		return "control"
	case PipeIsochronous:
		return "isochronous"
	case PipeBulk:
		return "bulk"
	case PipeInterrupt:
		return "interrupt"
	}
	return fmt.Sprintf("type %d", uint8(t))
}

// Pipe is one endpoint on the interface.
type Pipe struct {
	ID        uint8
	Type      PipeType
	MaxPacket uint16
	Interval  uint8
}

// In reports whether the pipe carries data from the device to the host. The
// direction is the top bit of the endpoint address.
func (p Pipe) In() bool { return p.ID&0x80 != 0 }

func (p Pipe) String() string {
	dir := "out"
	if p.In() {
		dir = "in"
	}
	return fmt.Sprintf("ep 0x%02x %-3s %-9s %d byte packets", p.ID, dir, p.Type, p.MaxPacket)
}

// USB speeds as reported by the backend.
const (
	SpeedLow  = 0x01
	SpeedFull = 0x02
	SpeedHigh = 0x03
)

func SpeedName(s uint8) string {
	switch s {
	case SpeedLow:
		return "low"
	case SpeedFull:
		return "full"
	case SpeedHigh:
		return "high or above"
	}
	return fmt.Sprintf("speed %d", s)
}
