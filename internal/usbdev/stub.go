//go:build !windows && !linux && !darwin

package usbdev

import "errors"

// ErrUnsupported is what every entry point returns on a platform with
// neither the WinUSB backend (Windows) nor the libusb one (Linux, macOS).
var ErrUnsupported = errors.New("usbdev: direct USB access is not implemented on this platform")

// MaxPacketAlignment matches the Windows backend so callers can size their
// buffers the same way everywhere.
const MaxPacketAlignment = 65536

// IsTimeout has nothing to report off Windows: nothing here ever gets far
// enough to time out.
func IsTimeout(error) bool { return false }

type Device struct{ Path string }

func FindPaths(GUID) ([]string, error) { return nil, ErrUnsupported }

func Open(string) (*Device, error) { return nil, ErrUnsupported }

func (d *Device) Close() error { return ErrUnsupported }

func (d *Device) Interface(uint8) (class, subClass, protocol, endpoints uint8, err error) {
	return 0, 0, 0, 0, ErrUnsupported
}

func (d *Device) Pipes(uint8) ([]Pipe, error) { return nil, ErrUnsupported }

func (d *Device) Speed() (uint8, error) { return 0, ErrUnsupported }

func (d *Device) SetTimeout(uint8, uint32) error { return ErrUnsupported }

func (d *Device) SetRawIO(uint8, bool) error { return ErrUnsupported }

func (d *Device) Read(uint8, []byte) (int, error) { return 0, ErrUnsupported }

func (d *Device) Write(uint8, []byte) (int, error) { return 0, ErrUnsupported }

func (d *Device) AbortPipe(uint8) error { return ErrUnsupported }

func (d *Device) ResetPipe(uint8) error { return ErrUnsupported }

func (d *Device) FlushPipe(uint8) error { return ErrUnsupported }
