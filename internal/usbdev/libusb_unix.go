//go:build linux || darwin

// This backend talks to the devkit through libusb (via gousb) instead of
// WinUSB. The devkit enumerates as an ordinary vendor-specific USB device
// (see DevkitVendorID/DevkitProductID) with no driver binding at all, so
// opening it is plain user-space libusb access - no kernel driver install
// needed. On Linux this still typically needs either root or a udev rule
// granting the calling user access to that VID/PID; there is nothing this
// package can do about that itself.
package usbdev

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/gousb"
)

// IsTimeout reports whether an error is just an idle pipe.
func IsTimeout(err error) bool {
	var status gousb.TransferStatus
	if errors.As(err, &status) {
		return status == gousb.TransferCancelled || status == gousb.TransferTimedOut
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// MaxPacketAlignment matches the Windows backend's constant so callers can
// size buffers the same way everywhere. libusb has no equivalent to
// WinUSB's RAW_IO alignment requirement, so this is only for parity.
const MaxPacketAlignment = 65536

// FindPaths returns one identifier per present devkit, encoding the USB bus
// and device address libusb enumerated it at. The GUID parameter is a
// Windows device-interface concept and is unused here; it stays so the
// signature matches every platform.
func FindPaths(GUID) ([]string, error) {
	ctx := gousb.NewContext()
	defer ctx.Close()

	devs, err := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
		return uint16(desc.Vendor) == DevkitVendorID && uint16(desc.Product) == DevkitProductID
	})
	paths := make([]string, 0, len(devs))
	for _, d := range devs {
		paths = append(paths, fmt.Sprintf("%d:%d", d.Desc.Bus, d.Desc.Address))
		d.Close()
	}
	if err != nil {
		return paths, fmt.Errorf("usbdev: enumerate: %w", err)
	}
	return paths, nil
}

// Device is an open libusb handle on the devkit's single vendor-specific
// interface.
type Device struct {
	Path string

	ctx   *gousb.Context
	dev   *gousb.Device
	cfg   *gousb.Config
	iface *gousb.Interface

	// altSettings is interface 0's alternate settings, fetched once at Open
	// so Interface/Pipes can answer without disturbing the claimed setting.
	altSettings []gousb.InterfaceSetting

	mu      sync.Mutex
	in      map[uint8]*gousb.InEndpoint
	out     map[uint8]*gousb.OutEndpoint
	timeout map[uint8]time.Duration
	cancel  map[uint8]context.CancelFunc
}

// Open takes exclusive use of the device named by a path from FindPaths.
// libusb claims the interface exclusively the same way WinUSB does, so this
// fails the same way while the daemon (or another client) already holds it.
func Open(path string) (*Device, error) {
	bus, addr, err := parsePath(path)
	if err != nil {
		return nil, err
	}

	ctx := gousb.NewContext()
	devs, listErr := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
		return desc.Bus == bus && desc.Address == addr
	})
	var found *gousb.Device
	for _, d := range devs {
		if found == nil {
			found = d
			continue
		}
		d.Close() // duplicate match, shouldn't happen for a bus:address pair
	}
	if found == nil {
		ctx.Close()
		if listErr != nil {
			return nil, fmt.Errorf("usbdev: open %s: %w", path, listErr)
		}
		return nil, fmt.Errorf("usbdev: %s is no longer present", path)
	}

	cfgNum, err := found.ActiveConfigNum()
	if err != nil {
		found.Close()
		ctx.Close()
		return nil, fmt.Errorf("usbdev: active config: %w", err)
	}
	cfg, err := found.Config(cfgNum)
	if err != nil {
		found.Close()
		ctx.Close()
		return nil, fmt.Errorf("usbdev: claim config %d: %w (the daemon holds this device open while it runs)", cfgNum, err)
	}
	iface, err := cfg.Interface(0, 0)
	if err != nil {
		cfg.Close()
		found.Close()
		ctx.Close()
		return nil, fmt.Errorf("usbdev: claim interface: %w (the daemon holds this device open while it runs)", err)
	}

	var alts []gousb.InterfaceSetting
	for _, i := range cfg.Desc.Interfaces {
		if i.Number == 0 {
			alts = i.AltSettings
			break
		}
	}

	return &Device{
		Path:        path,
		ctx:         ctx,
		dev:         found,
		cfg:         cfg,
		iface:       iface,
		altSettings: alts,
		in:          make(map[uint8]*gousb.InEndpoint),
		out:         make(map[uint8]*gousb.OutEndpoint),
		timeout:     make(map[uint8]time.Duration),
		cancel:      make(map[uint8]context.CancelFunc),
	}, nil
}

func parsePath(path string) (bus, addr int, err error) {
	parts := strings.SplitN(path, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("usbdev: malformed device path %q", path)
	}
	if bus, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, fmt.Errorf("usbdev: malformed device path %q: %w", path, err)
	}
	if addr, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, fmt.Errorf("usbdev: malformed device path %q: %w", path, err)
	}
	return bus, addr, nil
}

func (d *Device) Close() error {
	d.iface.Close()
	if err := d.cfg.Close(); err != nil {
		return err
	}
	if err := d.dev.Close(); err != nil {
		return err
	}
	return d.ctx.Close()
}

// Interface reports the interface's class triplet and endpoint count for the
// given alternate setting, read from the descriptor fetched at Open rather
// than a live query - libusb has no read-only equivalent of
// WinUsb_QueryInterfaceSettings that doesn't touch the claimed setting.
func (d *Device) Interface(alt uint8) (class, subClass, protocol, endpoints uint8, err error) {
	for _, s := range d.altSettings {
		if s.Alternate == int(alt) {
			return uint8(s.Class), uint8(s.SubClass), uint8(s.Protocol), uint8(len(s.Endpoints)), nil
		}
	}
	return 0, 0, 0, 0, fmt.Errorf("usbdev: no alternate setting %d", alt)
}

// Pipes lists the endpoints on an alternate setting.
func (d *Device) Pipes(alt uint8) ([]Pipe, error) {
	for _, s := range d.altSettings {
		if s.Alternate != int(alt) {
			continue
		}
		pipes := make([]Pipe, 0, len(s.Endpoints))
		for _, ep := range s.Endpoints {
			pipes = append(pipes, Pipe{
				ID:        uint8(ep.Address),
				Type:      pipeTypeFromTransfer(ep.TransferType),
				MaxPacket: uint16(ep.MaxPacketSize),
			})
		}
		return pipes, nil
	}
	return nil, fmt.Errorf("usbdev: no alternate setting %d", alt)
}

func pipeTypeFromTransfer(t gousb.TransferType) PipeType {
	switch t {
	case gousb.TransferTypeControl:
		return PipeControl
	case gousb.TransferTypeIsochronous:
		return PipeIsochronous
	case gousb.TransferTypeInterrupt:
		return PipeInterrupt
	default:
		return PipeBulk
	}
}

// Speed reports the negotiated USB speed. libusb's own Speed enum values
// line up with this package's SpeedLow/SpeedFull/SpeedHigh, so anything at
// or above SpeedHigh (SuperSpeed included) reports as SpeedHigh, matching
// SpeedName's "high or above".
func (d *Device) Speed() (uint8, error) {
	s := uint8(d.dev.Desc.Speed)
	if s > SpeedHigh {
		s = SpeedHigh
	}
	return s, nil
}

// SetTimeout bounds how long a read or write on this pipe waits. Without it
// a transfer on an idle pipe never returns.
func (d *Device) SetTimeout(pipeID uint8, ms uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.timeout[pipeID] = time.Duration(ms) * time.Millisecond
	return nil
}

// SetRawIO is a no-op here: a libusb bulk read already returns exactly what
// the device sent for that transfer, without WinUSB's short-read buffering,
// so there is nothing to turn on.
func (d *Device) SetRawIO(uint8, bool) error { return nil }

func (d *Device) inEndpoint(pipeID uint8) (*gousb.InEndpoint, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ep, ok := d.in[pipeID]; ok {
		return ep, nil
	}
	ep, err := d.iface.InEndpoint(int(pipeID & 0x0f))
	if err != nil {
		return nil, err
	}
	d.in[pipeID] = ep
	return ep, nil
}

func (d *Device) outEndpoint(pipeID uint8) (*gousb.OutEndpoint, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ep, ok := d.out[pipeID]; ok {
		return ep, nil
	}
	ep, err := d.iface.OutEndpoint(int(pipeID & 0x0f))
	if err != nil {
		return nil, err
	}
	d.out[pipeID] = ep
	return ep, nil
}

// beginTransfer opens a cancellable context for a transfer on a pipe and
// records its cancel func, so a concurrent AbortPipe on the same pipe can
// reach it. The returned done func both cancels and forgets it - always
// deferred, never skipped, so a transfer never leaks its context.
func (d *Device) beginTransfer(pipeID uint8) (context.Context, func()) {
	d.mu.Lock()
	timeout, hasTimeout := d.timeout[pipeID]
	d.mu.Unlock()

	var ctx context.Context
	var cancel context.CancelFunc
	if hasTimeout && timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}

	d.mu.Lock()
	d.cancel[pipeID] = cancel
	d.mu.Unlock()

	return ctx, func() {
		d.mu.Lock()
		delete(d.cancel, pipeID)
		d.mu.Unlock()
		cancel()
	}
}

// Read pulls one transfer off an IN pipe.
func (d *Device) Read(pipeID uint8, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	ep, err := d.inEndpoint(pipeID)
	if err != nil {
		return 0, fmt.Errorf("usbdev: read 0x%02x: %w", pipeID, err)
	}
	ctx, done := d.beginTransfer(pipeID)
	defer done()
	n, err := ep.ReadContext(ctx, buf)
	if err != nil {
		return n, fmt.Errorf("usbdev: read 0x%02x: %w", pipeID, err)
	}
	return n, nil
}

// Write pushes one transfer down an OUT pipe.
func (d *Device) Write(pipeID uint8, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	ep, err := d.outEndpoint(pipeID)
	if err != nil {
		return 0, fmt.Errorf("usbdev: write 0x%02x: %w", pipeID, err)
	}
	ctx, done := d.beginTransfer(pipeID)
	defer done()
	n, err := ep.WriteContext(ctx, buf)
	if err != nil {
		return n, fmt.Errorf("usbdev: write 0x%02x: %w", pipeID, err)
	}
	return n, nil
}

// AbortPipe cancels any transfer in flight on a pipe, by cancelling that
// transfer's own context from beginTransfer. Needed to get out of a wedged
// link, where a write blocks forever because the target stopped draining
// the endpoint, and to unblock a concurrent Read during Close.
func (d *Device) AbortPipe(pipeID uint8) error {
	d.mu.Lock()
	cancel := d.cancel[pipeID]
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// ResetPipe and FlushPipe are no-ops on this backend: gousb doesn't expose
// libusb_clear_halt or an equivalent of WinUSB's driver-level pipe flush,
// and there's no portable way to reach past it to the raw handle. A wedged
// link on Linux/macOS recovers by closing and reopening the device instead
// of resetting one pipe in place - a real gap next to the Windows backend,
// not a silent one.
func (d *Device) ResetPipe(uint8) error { return nil }
func (d *Device) FlushPipe(uint8) error { return nil }
