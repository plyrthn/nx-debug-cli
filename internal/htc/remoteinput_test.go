package htc

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// newTestInput wires a RemoteInput to one end of an in-memory pipe and
// returns a read function for whatever it wrote. Every chunk is fixed-size,
// so the reader can just pull exactly that many bytes.
func newTestInput(t *testing.T) (*RemoteInput, func() []byte) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	ri := &RemoteInput{conn: client}
	read := func() []byte {
		buf := make([]byte, inputChunkSize)
		server.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := server.Read(buf)
		if err != nil {
			t.Fatalf("read chunk: %v", err)
		}
		if n != inputChunkSize {
			t.Fatalf("chunk length = %d, want %d", n, inputChunkSize)
		}
		return buf
	}
	return ri, read
}

// sendAsync runs a write on its own goroutine - net.Pipe is unbuffered, so a
// direct call would block until the test reads.
func sendAsync(t *testing.T, fn func() error) {
	t.Helper()
	go func() {
		if err := fn(); err != nil {
			t.Errorf("send: %v", err)
		}
	}()
}

func TestMouseMoveChunk(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.MouseMove(-2, 300) })

	got := read()
	want := []byte{
		7, 0, 0, 12, // MouseMove, size 12
		0, 0, // buttons, pad
		0, 0, // wheel delta
		0xff, 0xfe, // deltaX = -2
		0x01, 0x2c, // deltaY = 300
	}
	if !bytes.Equal(got, want) {
		t.Errorf("chunk = % x, want % x", got, want)
	}
}

func TestMouseButtonsChunk(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.MouseButtons(MouseLeft | MouseMiddle) })

	got := read()
	if got[0] != 8 {
		t.Errorf("type = %d, want 8 (MouseButton)", got[0])
	}
	if got[3] != inputChunkSize {
		t.Errorf("size = %d, want %d", got[3], inputChunkSize)
	}
	if got[4] != 0x05 {
		t.Errorf("buttons = %#x, want 0x05 (left|middle)", got[4])
	}
}

func TestMouseWheelChunk(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.MouseWheel(-120) })

	got := read()
	if got[0] != 9 {
		t.Errorf("type = %d, want 9 (MouseWheel)", got[0])
	}
	// wheel delta lives at offset 6, not 4 - the buttons byte sits at 4.
	if got[6] != 0xff || got[7] != 0x88 {
		t.Errorf("wheel delta bytes = % x, want ff 88 (-120)", got[6:8])
	}
}

func TestTouchChunks(t *testing.T) {
	cases := []struct {
		name     string
		send     func(*RemoteInput) error
		wantType byte
		wantX    []byte
		wantY    []byte
	}{
		{
			name:     "begin",
			send:     func(r *RemoteInput) error { return r.TouchBegin(1, 640, 360) },
			wantType: 13,
			wantX:    []byte{0x02, 0x80},
			wantY:    []byte{0x01, 0x68},
		},
		{
			name:     "move",
			send:     func(r *RemoteInput) error { return r.TouchMove(1, 100, 200) },
			wantType: 14,
			wantX:    []byte{0x00, 0x64},
			wantY:    []byte{0x00, 0xc8},
		},
		{
			name:     "end",
			send:     func(r *RemoteInput) error { return r.TouchEnd(1) },
			wantType: 15,
			wantX:    []byte{0x00, 0x00},
			wantY:    []byte{0x00, 0x00},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ri, read := newTestInput(t)
			sendAsync(t, func() error { return tc.send(ri) })

			got := read()
			if got[0] != tc.wantType {
				t.Errorf("type = %d, want %d", got[0], tc.wantType)
			}
			if got[4] != 1 {
				t.Errorf("finger ID = %d, want 1", got[4])
			}
			if !bytes.Equal(got[8:10], tc.wantX) {
				t.Errorf("X bytes = % x, want % x", got[8:10], tc.wantX)
			}
			if !bytes.Equal(got[10:12], tc.wantY) {
				t.Errorf("Y bytes = % x, want % x", got[10:12], tc.wantY)
			}
		})
	}
}

func TestTapSendsBeginThenEnd(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.Tap(500, 400) })

	begin := read()
	if begin[0] != 13 {
		t.Errorf("first chunk type = %d, want 13 (TouchBegan)", begin[0])
	}
	end := read()
	if end[0] != 15 {
		t.Errorf("second chunk type = %d, want 15 (TouchEnded)", end[0])
	}
	if begin[4] != end[4] {
		t.Errorf("finger IDs differ: begin %d, end %d", begin[4], end[4])
	}
}

func TestKeyChunks(t *testing.T) {
	ri, read := newTestInput(t)
	// 0x04 is USB HID usage "a"; 0x02 in the lock mask is caps lock.
	sendAsync(t, func() error { return ri.KeyDown(0x04, 0x02) })

	down := read()
	if down[0] != 5 {
		t.Errorf("type = %d, want 5 (KeyDown)", down[0])
	}
	if down[4] != 0x04 {
		t.Errorf("usage ID = %#x, want 0x04", down[4])
	}
	if down[5] != 0x02 {
		t.Errorf("locked keys = %#x, want 0x02", down[5])
	}

	sendAsync(t, func() error { return ri.KeyUp(0x04, 0) })
	up := read()
	if up[0] != 6 {
		t.Errorf("type = %d, want 6 (KeyUp)", up[0])
	}
}

func TestHomeButtonChunk(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.HomeButton(true) })

	pressed := read()
	if pressed[0] != 16 {
		t.Errorf("type = %d, want 16 (HomeButton)", pressed[0])
	}
	if pressed[7] != 1 {
		t.Errorf("buttons = %d, want 1", pressed[7])
	}

	sendAsync(t, func() error { return ri.HomeButton(false) })
	released := read()
	if released[7] != 0 {
		t.Errorf("buttons = %d, want 0", released[7])
	}
}

// Every chunk carries its own length at byte 3; a wrong value there would
// desync the target's reader, so pin it across all message types.
func TestAllChunksCarrySize(t *testing.T) {
	ri, read := newTestInput(t)
	sends := []func() error{
		func() error { return ri.MouseMove(1, 1) },
		func() error { return ri.MouseButtons(MouseRight) },
		func() error { return ri.MouseWheel(120) },
		func() error { return ri.TouchBegin(0, 1, 1) },
		func() error { return ri.TouchMove(0, 2, 2) },
		func() error { return ri.TouchEnd(0) },
		func() error { return ri.KeyDown(0x04, 0) },
		func() error { return ri.KeyUp(0x04, 0) },
		func() error { return ri.HomeButton(true) },
	}
	for i, send := range sends {
		sendAsync(t, send)
		got := read()
		if got[3] != inputChunkSize {
			t.Errorf("send %d: size byte = %d, want %d", i, got[3], inputChunkSize)
		}
		if got[1] != 0 || got[2] != 0 {
			t.Errorf("send %d: bytes 1-2 = % x, want zero padding", i, got[1:3])
		}
	}
}
