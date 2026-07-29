//go:build windows

package usbdev

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// errSemTimeout is what WinUSB reports when a pipe's transfer timeout
// expires. On a long-lived link that means nothing happened, not that the
// link is broken, so callers need to be able to tell the two apart.
const errSemTimeout = syscall.Errno(121)

// IsTimeout reports whether an error is just an idle pipe.
func IsTimeout(err error) bool { return errors.Is(err, errSemTimeout) }

// MaxPacketAlignment is the block size a read buffer must be a multiple of
// while RAW_IO is on. WinUSB rejects anything else outright, and 64 KiB is a
// multiple of every bulk endpoint size in use (512 at high speed, 1024 at
// SuperSpeed).
const MaxPacketAlignment = 65536

var (
	setupapi = windows.NewLazySystemDLL("setupapi.dll")
	winusb   = windows.NewLazySystemDLL("winusb.dll")

	procGetClassDevsW            = setupapi.NewProc("SetupDiGetClassDevsW")
	procEnumDeviceInterfaces     = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procGetDeviceInterfaceDetail = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procDestroyDeviceInfoList    = setupapi.NewProc("SetupDiDestroyDeviceInfoList")

	procInitialize             = winusb.NewProc("WinUsb_Initialize")
	procFree                   = winusb.NewProc("WinUsb_Free")
	procQueryInterfaceSettings = winusb.NewProc("WinUsb_QueryInterfaceSettings")
	procQueryPipe              = winusb.NewProc("WinUsb_QueryPipe")
	procQueryDeviceInformation = winusb.NewProc("WinUsb_QueryDeviceInformation")
	procSetPipePolicy          = winusb.NewProc("WinUsb_SetPipePolicy")
	procReadPipe               = winusb.NewProc("WinUsb_ReadPipe")
	procWritePipe              = winusb.NewProc("WinUsb_WritePipe")
	procResetPipe              = winusb.NewProc("WinUsb_ResetPipe")
	procAbortPipe              = winusb.NewProc("WinUsb_AbortPipe")
	procFlushPipe              = winusb.NewProc("WinUsb_FlushPipe")
)

const (
	digcfPresent         = 0x02
	digcfDeviceInterface = 0x10

	// Pipe policy IDs. Only the two that matter for a framed protocol are
	// here: without RAW_IO a read shorter than the endpoint's packet size
	// gets buffered by the driver, and without a timeout a read on an idle
	// channel blocks forever.
	pipeTransferTimeout = 0x03
	pipeRawIO           = 0x07
)

type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGuid windows.GUID
	flags              uint32
	reserved           uintptr
}

// winusbPipeInfo mirrors WINUSB_PIPE_INFORMATION.
type winusbPipeInfo struct {
	pipeType      uint32
	pipeID        uint8
	maximumPacket uint16
	interval      uint8
	_             [0]byte
}

// usbInterfaceDescriptor mirrors USB_INTERFACE_DESCRIPTOR.
type usbInterfaceDescriptor struct {
	length            uint8
	descriptorType    uint8
	interfaceNumber   uint8
	alternateSetting  uint8
	numEndpoints      uint8
	interfaceClass    uint8
	interfaceSubClass uint8
	interfaceProtocol uint8
	iInterface        uint8
}

// FindPaths returns the device paths for every present device exposing the
// given interface GUID. One devkit, one path.
func FindPaths(id GUID) ([]string, error) {
	guid := windows.GUID{Data1: id.Data1, Data2: id.Data2, Data3: id.Data3, Data4: id.Data4}
	set, _, err := procGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&guid)), 0, 0, digcfPresent|digcfDeviceInterface)
	if set == uintptr(windows.InvalidHandle) {
		return nil, fmt.Errorf("usbdev: SetupDiGetClassDevs: %w", err)
	}
	defer procDestroyDeviceInfoList.Call(set)

	var paths []string
	for index := 0; ; index++ {
		var data spDeviceInterfaceData
		data.cbSize = uint32(unsafe.Sizeof(data))
		ok, _, err := procEnumDeviceInterfaces.Call(
			set, 0, uintptr(unsafe.Pointer(&guid)), uintptr(index), uintptr(unsafe.Pointer(&data)))
		if ok == 0 {
			if err == windows.ERROR_NO_MORE_ITEMS {
				return paths, nil
			}
			if index == 0 {
				return nil, fmt.Errorf("usbdev: SetupDiEnumDeviceInterfaces: %w", err)
			}
			return paths, nil
		}

		// Ask once for the size, then again for the data.
		var needed uint32
		procGetDeviceInterfaceDetail.Call(
			set, uintptr(unsafe.Pointer(&data)), 0, 0, uintptr(unsafe.Pointer(&needed)), 0)
		if needed < 8 {
			continue
		}
		buf := make([]byte, needed)
		// cbSize is the size of the fixed part of the struct, not of the
		// buffer: 8 on 64-bit (a uint32 plus padding to the WCHAR array's
		// alignment). Passing the buffer size here is the classic way to
		// get ERROR_INVALID_USER_BUFFER out of this call.
		*(*uint32)(unsafe.Pointer(&buf[0])) = 8
		ok, _, err = procGetDeviceInterfaceDetail.Call(
			set, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&buf[0])),
			uintptr(needed), uintptr(unsafe.Pointer(&needed)), 0)
		if ok == 0 {
			return nil, fmt.Errorf("usbdev: SetupDiGetDeviceInterfaceDetail: %w", err)
		}
		paths = append(paths, windows.UTF16PtrToString(
			(*uint16)(unsafe.Pointer(&buf[4]))))
	}
}

// Device is an open WinUSB handle on the devkit.
type Device struct {
	Path   string
	file   windows.Handle
	handle uintptr
}

// Open takes exclusive use of the device. The daemon holds it open while it's
// running, so this fails with a sharing violation until the daemon is
// stopped - that's expected, not a bug, and the error says so.
func Open(path string) (*Device, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	file, err := windows.CreateFile(p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("usbdev: open %s: %w (the daemon holds this device open while it runs)", path, err)
	}

	var handle uintptr
	ok, _, err := procInitialize.Call(uintptr(file), uintptr(unsafe.Pointer(&handle)))
	if ok == 0 {
		windows.CloseHandle(file)
		return nil, fmt.Errorf("usbdev: WinUsb_Initialize: %w", err)
	}
	return &Device{Path: path, file: file, handle: handle}, nil
}

func (d *Device) Close() error {
	if d.handle != 0 {
		procFree.Call(d.handle)
		d.handle = 0
	}
	if d.file != 0 {
		err := windows.CloseHandle(d.file)
		d.file = 0
		return err
	}
	return nil
}

// Interface reports the interface's class triplet and endpoint count for the
// given alternate setting.
func (d *Device) Interface(alt uint8) (class, subClass, protocol, endpoints uint8, err error) {
	var desc usbInterfaceDescriptor
	ok, _, callErr := procQueryInterfaceSettings.Call(
		d.handle, uintptr(alt), uintptr(unsafe.Pointer(&desc)))
	if ok == 0 {
		return 0, 0, 0, 0, fmt.Errorf("usbdev: WinUsb_QueryInterfaceSettings(%d): %w", alt, callErr)
	}
	return desc.interfaceClass, desc.interfaceSubClass, desc.interfaceProtocol, desc.numEndpoints, nil
}

// Pipes lists the endpoints on an alternate setting.
func (d *Device) Pipes(alt uint8) ([]Pipe, error) {
	_, _, _, count, err := d.Interface(alt)
	if err != nil {
		return nil, err
	}
	pipes := make([]Pipe, 0, count)
	for i := uint8(0); i < count; i++ {
		var info winusbPipeInfo
		ok, _, callErr := procQueryPipe.Call(
			d.handle, uintptr(alt), uintptr(i), uintptr(unsafe.Pointer(&info)))
		if ok == 0 {
			return nil, fmt.Errorf("usbdev: WinUsb_QueryPipe(%d,%d): %w", alt, i, callErr)
		}
		pipes = append(pipes, Pipe{
			ID:        info.pipeID,
			Type:      PipeType(info.pipeType),
			MaxPacket: info.maximumPacket,
			Interval:  info.interval,
		})
	}
	return pipes, nil
}

const deviceSpeedInfo = 0x01

// Speed reports the negotiated USB speed. The devkit's "USB 3.0 SuperSpeed
// for HTC connection" DevMenu toggle shows up here.
func (d *Device) Speed() (uint8, error) {
	var speed uint8
	length := uint32(1)
	ok, _, err := procQueryDeviceInformation.Call(
		d.handle, deviceSpeedInfo,
		uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&speed)))
	if ok == 0 {
		return 0, fmt.Errorf("usbdev: WinUsb_QueryDeviceInformation: %w", err)
	}
	return speed, nil
}

// SetTimeout bounds how long a read on this pipe waits. Without it a read on
// an idle channel never returns.
func (d *Device) SetTimeout(pipeID uint8, ms uint32) error {
	ok, _, err := procSetPipePolicy.Call(
		d.handle, uintptr(pipeID), pipeTransferTimeout,
		unsafe.Sizeof(ms), uintptr(unsafe.Pointer(&ms)))
	if ok == 0 {
		return fmt.Errorf("usbdev: set timeout on 0x%02x: %w", pipeID, err)
	}
	return nil
}

// SetRawIO turns off the driver's read buffering, so a read returns exactly
// the packet the device sent instead of waiting to fill a buffer. A framed
// protocol needs this to see message boundaries.
func (d *Device) SetRawIO(pipeID uint8, on bool) error {
	var v uint8
	if on {
		v = 1
	}
	ok, _, err := procSetPipePolicy.Call(
		d.handle, uintptr(pipeID), pipeRawIO,
		unsafe.Sizeof(v), uintptr(unsafe.Pointer(&v)))
	if ok == 0 {
		return fmt.Errorf("usbdev: set raw io on 0x%02x: %w", pipeID, err)
	}
	return nil
}

// Read pulls one transfer off an IN pipe.
func (d *Device) Read(pipeID uint8, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	var got uint32
	ok, _, err := procReadPipe.Call(
		d.handle, uintptr(pipeID), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)), uintptr(unsafe.Pointer(&got)), 0)
	if ok == 0 {
		return int(got), fmt.Errorf("usbdev: read 0x%02x: %w", pipeID, err)
	}
	return int(got), nil
}

// Write pushes one transfer down an OUT pipe.
func (d *Device) Write(pipeID uint8, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	var sent uint32
	ok, _, err := procWritePipe.Call(
		d.handle, uintptr(pipeID), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)), uintptr(unsafe.Pointer(&sent)), 0)
	if ok == 0 {
		return int(sent), fmt.Errorf("usbdev: write 0x%02x: %w", pipeID, err)
	}
	return int(sent), nil
}

// AbortPipe cancels any transfer in flight on a pipe. Needed to get out of a
// wedged link, where a write blocks forever because the target stopped
// draining the endpoint.
func (d *Device) AbortPipe(pipeID uint8) error {
	ok, _, err := procAbortPipe.Call(d.handle, uintptr(pipeID))
	if ok == 0 {
		return fmt.Errorf("usbdev: abort 0x%02x: %w", pipeID, err)
	}
	return nil
}

// ResetPipe clears a stalled endpoint and resets its data toggle, which is
// what actually makes a wedged pipe usable again rather than just cancelling
// the current transfer.
func (d *Device) ResetPipe(pipeID uint8) error {
	ok, _, err := procResetPipe.Call(d.handle, uintptr(pipeID))
	if ok == 0 {
		return fmt.Errorf("usbdev: reset 0x%02x: %w", pipeID, err)
	}
	return nil
}

// FlushPipe discards data the driver has buffered for an IN pipe.
func (d *Device) FlushPipe(pipeID uint8) error {
	ok, _, err := procFlushPipe.Call(d.handle, uintptr(pipeID))
	if ok == 0 {
		return fmt.Errorf("usbdev: flush 0x%02x: %w", pipeID, err)
	}
	return nil
}
