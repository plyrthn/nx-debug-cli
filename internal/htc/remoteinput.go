package htc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// hidPortName is the well-known HTCS port name the target's HID service
// listens on. Nintendo namespaces these as "iywys@$<service>" - the same
// convention is visible in Atmosphere's own htc reimplementation
// ("iywys@$gdb", "iywys@$cs", "iywys@$LogManager"), so the prefix is a real
// system-wide naming scheme rather than anything specific to input.
const hidPortName = "iywys@$hid"

// inputChunkSize is the size of every message this package sends on the HID
// channel. The channel is self-describing - byte 3 carries the total
// message length, and a reader treats 0 or 12 as "exactly one chunk" - so
// longer messages are possible in principle, but every input event fits in
// twelve bytes.
const inputChunkSize = 12

// inputMessageType tags a chunk. The numbering is dense and
// position-dependent (it's a plain C# enum on the other side), so the
// values matter and can't be reordered.
type inputMessageType byte

const (
	msgPing        inputMessageType = 1
	msgPong        inputMessageType = 2
	msgKeyDown     inputMessageType = 5
	msgKeyUp       inputMessageType = 6
	msgMouseMove   inputMessageType = 7
	msgMouseButton inputMessageType = 8
	msgMouseWheel  inputMessageType = 9
	msgTouchBegan  inputMessageType = 13
	msgTouchMoved  inputMessageType = 14
	msgTouchEnded  inputMessageType = 15
	msgHomeButton  inputMessageType = 16

	// The HDLS pad messages are the virtual-controller half of the channel.
	// The target announces the pads it has with these same types on connect,
	// so they are both what it sends and what it accepts.
	msgHdlsPadDeviceData inputMessageType = 26
	msgHdlsPadButton     inputMessageType = 27
	msgHdlsPadStick      inputMessageType = 28
	msgHdlsPadNpadID     inputMessageType = 29
	msgHdlsPadRequest    inputMessageType = 30
)

// SessionInfo is what the far end reports in its pong: the chunk size it
// works in and the message format it speaks.
type SessionInfo struct {
	ChunkSize int
	Major     int
	Minor     int
	Micro     int
}

func (s SessionInfo) String() string {
	return fmt.Sprintf("chunk %d bytes, format %d.%d.%d", s.ChunkSize, s.Major, s.Minor, s.Micro)
}

// Chunk is one message off the channel. Size is the peer's own declared
// length, which is how variable-length chunk types are framed.
type Chunk struct {
	Type byte
	Size byte
	Raw  []byte
}

func (c Chunk) String() string {
	name, ok := inputMessageNames[inputMessageType(c.Type)]
	if !ok {
		name = fmt.Sprintf("type %d", c.Type)
	}
	return fmt.Sprintf("%-16s size=%-3d % x", name, c.Size, c.Raw)
}

// inputMessageNames is what this build recognises. An unnamed type is
// reported by number rather than as a neighbour, since the numbering is dense
// and a wrong guess reads as a plausible message.
var inputMessageNames = map[inputMessageType]string{
	msgPing: "Ping", msgPong: "Pong",
	msgKeyDown: "KeyDown", msgKeyUp: "KeyUp",
	msgMouseMove: "MouseMove", msgMouseButton: "MouseButton", msgMouseWheel: "MouseWheel",
	msgTouchBegan: "TouchBegan", msgTouchMoved: "TouchMoved", msgTouchEnded: "TouchEnded",
	msgHomeButton:        "HomeButton",
	msgHdlsPadDeviceData: "HdlsPadDeviceData", msgHdlsPadButton: "HdlsPadButton",
	msgHdlsPadStick: "HdlsPadStick", msgHdlsPadNpadID: "HdlsPadNpadId",
	msgHdlsPadRequest: "HdlsPadRequest",
}

// ReadChunk reads one message. Chunks are self-framing: the first 12 bytes
// always arrive, and byte 3 says whether more follows.
func (r *RemoteInput) ReadChunk() (Chunk, error) {
	head := make([]byte, inputChunkSize)
	if _, err := io.ReadFull(r.conn, head); err != nil {
		return Chunk{}, err
	}
	size := int(head[3])
	if size < inputChunkSize {
		// A peer that declares a chunk shorter than the minimum is not
		// speaking this protocol, and reading on would desynchronise.
		return Chunk{}, fmt.Errorf("htc: peer declared a %d byte chunk, minimum is %d", size, inputChunkSize)
	}
	raw := head
	if size > inputChunkSize {
		rest := make([]byte, size-inputChunkSize)
		if _, err := io.ReadFull(r.conn, rest); err != nil {
			return Chunk{}, err
		}
		raw = append(raw, rest...)
	}
	return Chunk{Type: head[0], Size: head[3], Raw: raw}, nil
}

// SetReadDeadline bounds the next ReadChunk.
func (r *RemoteInput) SetReadDeadline(t time.Time) error { return r.conn.SetReadDeadline(t) }

// Ping sends a keepalive and waits for the pong.
//
// This is not optional politeness. The real client pings on connect and then
// every second, and the far end reports its chunk size and message format
// version in the reply - a session that never pings never learns either, and
// is one the peer has no reason to believe is still there.
// SendPing writes a ping without waiting for the reply, for callers that are
// already reading the channel on another goroutine.
func (r *RemoteInput) SendPing() error { return r.send(msgPing, nil) }

func (r *RemoteInput) Ping(timeout time.Duration) (SessionInfo, error) {
	if err := r.send(msgPing, nil); err != nil {
		return SessionInfo{}, err
	}
	if err := r.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return SessionInfo{}, err
	}
	defer r.conn.SetReadDeadline(time.Time{})

	for {
		// Must go through ReadChunk: the peer announces its pads on connect
		// in 16- and 20-byte chunks, and reading fixed 12-byte units walks
		// straight off the message boundary and never finds the pong.
		chunk, err := r.ReadChunk()
		if err != nil {
			return SessionInfo{}, fmt.Errorf("htc: waiting for pong: %w", err)
		}
		if inputMessageType(chunk.Type) != msgPong {
			continue
		}
		return SessionInfo{
			ChunkSize: int(chunk.Raw[3]),
			Major:     int(chunk.Raw[4]),
			Minor:     int(chunk.Raw[5]),
			Micro:     int(chunk.Raw[6]),
		}, nil
	}
}

// WaitSettled reads and discards chunks until the peer stops talking for quiet,
// or until max elapses.
//
// A fresh connection is not immediately ready for input. The peer answers the
// ping straight away but goes on to enumerate its virtual devices for several
// milliseconds after that, and anything sent during the enumeration is read off
// the socket and dropped. The reference client never notices because its
// session is minutes old by the time anyone presses a button; a one-shot
// command connects and types into the gap.
//
// Quiet is the signal rather than a fixed delay, because the enumeration is as
// long as the peer's device list, which isn't known up front.
func (r *RemoteInput) WaitSettled(quiet, max time.Duration) error {
	deadline := time.Now().Add(max)
	defer r.conn.SetReadDeadline(time.Time{})
	for {
		until := time.Now().Add(quiet)
		if until.After(deadline) {
			until = deadline
		}
		if err := r.conn.SetReadDeadline(until); err != nil {
			return err
		}
		if _, err := r.ReadChunk(); err != nil {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				// Quiet for a full interval, or out of budget. Either way
				// this is as settled as it is going to get.
				return nil
			}
			return err
		}
		if !time.Now().Before(deadline) {
			return nil
		}
	}
}

// KeepAlive pings on an interval until the returned function is called. The
// reference client pings once a second for the life of the session, so a
// command that holds a button for any length of time should do the same rather
// than going silent mid-press.
//
// The reply is not read here: a caller doing this is writing input, and the
// pongs pile up in the socket buffer until it closes. That is fine for a
// short-lived command and wrong for a long-lived one, which should run its own
// reader instead.
func (r *RemoteInput) KeepAlive(every time.Duration) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := r.SendPing(); err != nil {
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// MouseButton is a bitmask of currently-held mouse buttons. Button state is
// absolute, not edge-triggered: send the full mask every time it changes.
type MouseButton uint8

const (
	MouseLeft MouseButton = 1 << iota
	MouseRight
	MouseMiddle
	MouseForward
	MouseBack
)

// RemoteInput injects HID events into a target - the same channel Target
// Manager 2's remote-input window drives when you click on the video feed.
// The wire format is a plain TCP stream of fixed-size chunks with no
// handshake and no responses: connect and start writing.
//
// Coordinates for touch events are in target screen space (1280x720 in
// handheld mode), not host window space - scale before calling.
type RemoteInput struct {
	mu   sync.Mutex
	conn net.Conn
}

// DialRemoteInput resolves the target's HID channel and opens it. serial is
// the target's serial number, the HTCS peer name.
//
// Resolution goes through the HTCS control port, which reports what the
// target is actually listening on right now.
func DialRemoteInput(ctx context.Context, serial string) (*RemoteInput, error) {
	addr, err := resolveInputAddr(ctx, serial)
	if err != nil {
		return nil, err
	}
	return DialRemoteInputAddr(ctx, addr)
}

func resolveInputAddr(ctx context.Context, serial string) (string, error) {
	return resolvePortAddr(ctx, serial, hidPortName)
}

// resolvePortAddr finds where a named target service is reachable.
func resolvePortAddr(ctx context.Context, serial, port string) (string, error) {
	entry, err := ResolvePort(ctx, serial, port)
	if err != nil {
		return "", err
	}
	return entry.Addr(), nil
}

// DialRemoteInputAddr opens an already-resolved HID channel address.
func DialRemoteInputAddr(ctx context.Context, addr string) (*RemoteInput, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("htc: dial hid channel %s: %w", addr, err)
	}
	return &RemoteInput{conn: conn}, nil
}

func (r *RemoteInput) Close() error {
	return r.conn.Close()
}

// send writes one chunk. fill gets a zeroed 12-byte buffer with the type and
// size already set, and populates the payload from offset 4 on.
func (r *RemoteInput) send(t inputMessageType, fill func(b []byte)) error {
	return r.sendSized(t, inputChunkSize, fill)
}

// sendSized is send for the message types that are longer than the minimum.
// Byte 3 carries the real length, which is how the peer knows to read on.
func (r *RemoteInput) sendSized(t inputMessageType, size int, fill func(b []byte)) error {
	b := make([]byte, size)
	b[0] = byte(t)
	b[3] = byte(size)
	if fill != nil {
		fill(b)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.conn.Write(b); err != nil {
		return fmt.Errorf("htc: send input chunk: %w", err)
	}
	return nil
}

// MouseMove moves the pointer by a relative delta. The channel carries no
// absolute-position message for the mouse, so tracking is the caller's job.
func (r *RemoteInput) MouseMove(dx, dy int16) error {
	return r.send(msgMouseMove, func(b []byte) {
		binary.BigEndian.PutUint16(b[8:], uint16(dx))
		binary.BigEndian.PutUint16(b[10:], uint16(dy))
	})
}

// MouseButtons sets the held-button mask. Pass 0 to release everything.
func (r *RemoteInput) MouseButtons(buttons MouseButton) error {
	return r.send(msgMouseButton, func(b []byte) {
		b[4] = byte(buttons)
	})
}

// MouseWheel scrolls by delta, in the usual 120-per-detent units.
func (r *RemoteInput) MouseWheel(delta int16) error {
	return r.send(msgMouseWheel, func(b []byte) {
		binary.BigEndian.PutUint16(b[6:], uint16(delta))
	})
}

// TouchBegin starts a touch contact. fingerID identifies the contact for the
// matching TouchMove/TouchEnd calls; up to 16 can be active at once.
func (r *RemoteInput) TouchBegin(fingerID uint8, x, y int16) error {
	return r.touch(msgTouchBegan, fingerID, x, y)
}

// TouchMove moves an in-progress contact.
func (r *RemoteInput) TouchMove(fingerID uint8, x, y int16) error {
	return r.touch(msgTouchMoved, fingerID, x, y)
}

// TouchEnd lifts a contact. Coordinates are ignored.
func (r *RemoteInput) TouchEnd(fingerID uint8) error {
	return r.touch(msgTouchEnded, fingerID, 0, 0)
}

func (r *RemoteInput) touch(t inputMessageType, fingerID uint8, x, y int16) error {
	return r.send(t, func(b []byte) {
		b[4] = fingerID
		binary.BigEndian.PutUint16(b[8:], uint16(x))
		binary.BigEndian.PutUint16(b[10:], uint16(y))
	})
}

// Tap is the common case: a contact that goes down and straight back up at
// one point.
func (r *RemoteInput) Tap(x, y int16) error {
	if err := r.TouchBegin(0, x, y); err != nil {
		return err
	}
	return r.TouchEnd(0)
}

// KeyDown presses a key. usageID is a USB HID usage ID (usage page 7), not a
// Windows virtual-key code. lockedKeys carries the caps/num/scroll lock
// state alongside the press.
func (r *RemoteInput) KeyDown(usageID, lockedKeys uint8) error {
	return r.key(msgKeyDown, usageID, lockedKeys)
}

// KeyUp releases a key.
func (r *RemoteInput) KeyUp(usageID, lockedKeys uint8) error {
	return r.key(msgKeyUp, usageID, lockedKeys)
}

func (r *RemoteInput) key(t inputMessageType, usageID, lockedKeys uint8) error {
	return r.send(t, func(b []byte) {
		b[4] = usageID
		b[5] = lockedKeys
	})
}

// HomeButton sets the HOME button state (1 held, 0 released).
func (r *RemoteInput) HomeButton(pressed bool) error {
	return r.send(msgHomeButton, func(b []byte) {
		if pressed {
			binary.BigEndian.PutUint32(b[4:], 1)
		}
	})
}
