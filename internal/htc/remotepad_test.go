package htc

import (
	"bytes"
	"testing"
)

func TestPadConnectChunk(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.PadConnect(2, PadJoyConRight, PadInterfaceBluetooth) })

	got := read()
	want := []byte{
		21, 0, 0, 12, // AbstractedPadDeviceData, size 12
		2,          // pad ID
		1,          // interface: bluetooth
		5,          // device: right Joy-Con
		0,          // pad
		0, 0, 0, 1, // attribute: IsConnected
	}
	if !bytes.Equal(got, want) {
		t.Errorf("chunk = % x, want % x", got, want)
	}
}

// Disconnect is the same message with the connected bit cleared, not a
// message of its own.
func TestPadDisconnectClearsTheConnectedBit(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.PadDisconnect(1) })

	got := read()
	if got[0] != 21 {
		t.Errorf("type = %d, want 21", got[0])
	}
	if got[4] != 1 {
		t.Errorf("pad ID = %d, want 1", got[4])
	}
	if !bytes.Equal(got[8:12], []byte{0, 0, 0, 0}) {
		t.Errorf("attribute = % x, want all zero", got[8:12])
	}
}

func TestPadButtonsChunk(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.PadButtons(0, PadA|PadZR|PadHome) })

	got := read()
	if got[0] != 24 {
		t.Errorf("type = %d, want 24", got[0])
	}
	// A=0x1, ZR=0x200, Home=0x40000
	want := []byte{0x00, 0x04, 0x02, 0x01}
	if !bytes.Equal(got[8:12], want) {
		t.Errorf("buttons = % x, want % x", got[8:12], want)
	}
}

// The target's game reads these bit positions by value - an off-by-one here
// means the wrong button presses.
func TestPadButtonBitValues(t *testing.T) {
	cases := []struct {
		name   string
		button PadButton
		want   uint32
	}{
		{"A", PadA, 0x1},
		{"B", PadB, 0x2},
		{"X", PadX, 0x4},
		{"Y", PadY, 0x8},
		{"stick L press", PadStickLPress, 0x10},
		{"stick R press", PadStickRPress, 0x20},
		{"L", PadL, 0x40},
		{"R", PadR, 0x80},
		{"ZL", PadZL, 0x100},
		{"ZR", PadZR, 0x200},
		{"plus", PadPlus, 0x400},
		{"minus", PadMinus, 0x800},
		{"left", PadLeft, 0x1000},
		{"up", PadUp, 0x2000},
		{"right", PadRight, 0x4000},
		{"down", PadDown, 0x8000},
		{"SL", PadSL, 0x10000},
		{"SR", PadSR, 0x20000},
		{"home", PadHome, 0x40000},
		{"capture", PadCapture, 0x80000},
		{"stick L left", PadStickLLeft, 0x100000},
		{"stick L up", PadStickLUp, 0x200000},
		{"stick L right", PadStickLRight, 0x400000},
		{"stick L down", PadStickLDown, 0x800000},
		{"stick R left", PadStickRLeft, 0x1000000},
		{"stick R up", PadStickRUp, 0x2000000},
		{"stick R right", PadStickRRight, 0x4000000},
		{"stick R down", PadStickRDown, 0x8000000},
	}
	for _, tc := range cases {
		if uint32(tc.button) != tc.want {
			t.Errorf("%s = %#x, want %#x", tc.name, uint32(tc.button), tc.want)
		}
	}
}

func TestPadStickChunk(t *testing.T) {
	cases := []struct {
		name     string
		side     PadStickSide
		x, y     int16
		wantSide byte
		wantX    []byte
		wantY    []byte
	}{
		{"left full up", PadStickLeft, 0, 32767, 0, []byte{0x00, 0x00}, []byte{0x7f, 0xff}},
		{"right full left", PadStickRight, -32768, 0, 1, []byte{0x80, 0x00}, []byte{0x00, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ri, read := newTestInput(t)
			sendAsync(t, func() error { return ri.PadStick(1, tc.side, tc.x, tc.y) })

			got := read()
			if got[0] != 25 {
				t.Errorf("type = %d, want 25", got[0])
			}
			if got[4] != 1 {
				t.Errorf("pad ID = %d, want 1", got[4])
			}
			if got[5] != tc.wantSide {
				t.Errorf("stick side = %d, want %d", got[5], tc.wantSide)
			}
			if !bytes.Equal(got[8:10], tc.wantX) {
				t.Errorf("X = % x, want % x", got[8:10], tc.wantX)
			}
			if !bytes.Equal(got[10:12], tc.wantY) {
				t.Errorf("Y = % x, want % x", got[10:12], tc.wantY)
			}
		})
	}
}

func TestPadPowerChunk(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.PadPower(3, true, false, PadBatteryMedium) })

	got := read()
	want := []byte{23, 0, 0, 12, 3, 1, 0, 3, 0, 0, 0, 0}
	if !bytes.Equal(got, want) {
		t.Errorf("chunk = % x, want % x", got, want)
	}
}

func TestPadColorChunk(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.PadColor(0, PadColorSub, 0x11, 0x22, 0x33, 0xff) })

	got := read()
	if got[0] != 22 {
		t.Errorf("type = %d, want 22", got[0])
	}
	if got[5] != 1 {
		t.Errorf("colour kind = %d, want 1 (sub)", got[5])
	}
	if !bytes.Equal(got[8:12], []byte{0x11, 0x22, 0x33, 0xff}) {
		t.Errorf("rgba = % x", got[8:12])
	}
}

// A pad has to be declared before anything else about it means anything, so
// the device-data chunk must be the first thing on the wire.
func TestPadAttachSequence(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.PadAttach(0) })

	wantTypes := []byte{21, 23, 24, 25, 25}
	for i, want := range wantTypes {
		got := read()
		if got[0] != want {
			t.Fatalf("chunk %d type = %d, want %d", i, got[0], want)
		}
		if got[3] != inputChunkSize {
			t.Errorf("chunk %d size = %d, want %d", i, got[3], inputChunkSize)
		}
		if got[4] != 0 {
			t.Errorf("chunk %d pad ID = %d, want 0", i, got[4])
		}
	}
}

// The two stick chunks in an attach have to address different sticks, or one
// of them silently overwrites the other.
func TestPadAttachCentresBothSticks(t *testing.T) {
	ri, read := newTestInput(t)
	sendAsync(t, func() error { return ri.PadAttach(0) })

	for i := 0; i < 3; i++ {
		read()
	}
	left, right := read(), read()
	if left[5] != byte(PadStickLeft) || right[5] != byte(PadStickRight) {
		t.Errorf("stick sides = %d, %d; want %d, %d", left[5], right[5], PadStickLeft, PadStickRight)
	}
	for _, c := range [][]byte{left, right} {
		if !bytes.Equal(c[8:12], []byte{0, 0, 0, 0}) {
			t.Errorf("stick not centred: % x", c[8:12])
		}
	}
}
