package htclow

import (
	"fmt"
	"io"
	"sync/atomic"

	"github.com/plyrthn/nx-debug-cli/internal/usbdev"
)

// The devkit's only two endpoints: one bulk pair on a vendor-specific
// interface, which is what nn::htclow's UsbInterface driver multiplexes every
// channel over.
const (
	usbBulkIn  = 0x81
	usbBulkOut = 0x01
)

// usbReadTimeout bounds a single read so a closed transport can break out of
// the receive loop. A quiet link is normal - the target only speaks when it
// has something to say - so a timeout is retried rather than reported.
const usbReadTimeout = 2000

// USB is a Transport over the devkit's bulk pipes.
type USB struct {
	dev *usbdev.Device

	// Reads go through a staging buffer because RAW_IO requires every read
	// length to be a multiple of the endpoint's packet size, while the
	// framing above wants arbitrary lengths. Keeping the two apart is what
	// stops a 33-byte read from being rejected as an invalid function.
	stage   []byte
	pending []byte

	closed atomic.Bool
}

// OpenUSB finds the devkit and opens its interface. It does not speak to the
// target: the link handshake does that.
func OpenUSB() (*USB, error) {
	paths, err := usbdev.FindPaths(usbdev.DevkitInterface)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("htclow: no devkit USB interface present")
	}
	dev, err := usbdev.Open(paths[0])
	if err != nil {
		return nil, err
	}
	u := &USB{dev: dev}
	if err := u.configure(); err != nil {
		dev.Close()
		return nil, err
	}
	return u, nil
}

// NewUSB wraps an already-open device.
func NewUSB(dev *usbdev.Device) (*USB, error) {
	u := &USB{dev: dev}
	if err := u.configure(); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *USB) configure() error {
	u.stage = make([]byte, usbdev.MaxPacketAlignment)
	if err := u.dev.SetRawIO(usbBulkIn, true); err != nil {
		return err
	}
	if err := u.dev.SetTimeout(usbBulkIn, usbReadTimeout); err != nil {
		return err
	}
	// Bound the write side too. The target stops draining the OUT pipe the
	// moment the link goes bad, and an unbounded write there blocks forever.
	return u.dev.SetTimeout(usbBulkOut, 5000)
}

// WritePacket sends the header and the body as separate bulk transfers.
//
// The boundary is part of the framing here, not an implementation detail: the
// target reads a 32-byte header and then reads exactly BodySize more, so one
// combined transfer overruns its first read and it stalls the endpoint. That
// costs a physical USB replug to clear, so this is the single most important
// line in the transport.
func (u *USB) WritePacket(pkt []byte) error {
	if len(pkt) < HeaderSize {
		return fmt.Errorf("htclow: packet is %d bytes, short of a header", len(pkt))
	}
	if _, err := u.dev.Write(usbBulkOut, pkt[:HeaderSize]); err != nil {
		return err
	}
	if len(pkt) == HeaderSize {
		return nil
	}
	_, err := u.dev.Write(usbBulkOut, pkt[HeaderSize:])
	return err
}

// Read serves from the staging buffer, refilling it with a full-size aligned
// transfer when it runs dry. An idle pipe is retried rather than reported: a
// link with nothing to say is the normal state, not a failure.
func (u *USB) Read(p []byte) (int, error) {
	for len(u.pending) == 0 {
		if u.closed.Load() {
			return 0, io.EOF
		}
		n, err := u.dev.Read(usbBulkIn, u.stage)
		if n > 0 {
			u.pending = u.stage[:n]
			break
		}
		if err != nil && !usbdev.IsTimeout(err) {
			return 0, err
		}
	}
	n := copy(p, u.pending)
	u.pending = u.pending[n:]
	return n, nil
}

func (u *USB) Close() error {
	if !u.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Cancel the read in flight, or Close blocks behind it for as long as the
	// pipe timeout.
	u.dev.AbortPipe(usbBulkIn)
	return u.dev.Close()
}

// Reset clears a link left wedged by an interrupted session: abort anything
// in flight, reset the stall and the data toggle, then drop whatever the IN
// pipe was still holding.
func (u *USB) Reset() {
	for _, pipe := range []uint8{usbBulkIn, usbBulkOut} {
		u.dev.AbortPipe(pipe)
		u.dev.ResetPipe(pipe)
	}
	u.dev.FlushPipe(usbBulkIn)
}
