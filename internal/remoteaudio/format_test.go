package remoteaudio

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		f    Format
		ok   bool
	}{
		{"what the devkit actually sends", Format{48000, 2, 16}, true},
		{"mono", Format{48000, 1, 16}, true},
		{"5.1", Format{48000, 6, 16}, true},
		{"24-bit", Format{96000, 2, 24}, true},
		{"no sample rate", Format{0, 2, 16}, false},
		{"negative sample rate", Format{-48000, 2, 16}, false},
		{"no channels", Format{48000, 0, 16}, false},
		{"12-bit is not a thing here", Format{48000, 2, 12}, false},
		{"zero depth", Format{48000, 2, 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.Validate()
			if tc.ok && err != nil {
				t.Errorf("rejected a good format: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("accepted a bad format")
			}
		})
	}
}

// A format that fails Validate must not report a plausible-looking frame
// size, or a caller that skipped Validate would size buffers off a guess.
func TestFrameSize(t *testing.T) {
	cases := []struct {
		f    Format
		want int
	}{
		{Format{48000, 2, 16}, 4},
		{Format{48000, 1, 16}, 2},
		{Format{48000, 2, 8}, 2},
		{Format{48000, 2, 24}, 6},
		{Format{48000, 6, 32}, 24},
		{Format{48000, 2, 12}, 0},
		{Format{48000, 0, 16}, 0},
	}
	for _, tc := range cases {
		if got := tc.f.FrameSize(); got != tc.want {
			t.Errorf("%s: FrameSize = %d, want %d", tc.f, got, tc.want)
		}
	}
}

func s16(samples ...int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return b
}

func TestToStereo16(t *testing.T) {
	cases := []struct {
		name string
		f    Format
		src  []byte
		want []byte
	}{
		{
			// The common case: already in the output layout, so every
			// sample has to survive unchanged.
			name: "16-bit stereo passes through",
			f:    Format{48000, 2, 16},
			src:  s16(1000, -1000, 32767, -32768),
			want: s16(1000, -1000, 32767, -32768),
		},
		{
			name: "mono is duplicated to both speakers",
			f:    Format{48000, 1, 16},
			src:  s16(1000, -2000),
			want: s16(1000, 1000, -2000, -2000),
		},
		{
			name: "extra channels past the front pair are dropped",
			f:    Format{48000, 6, 16},
			src:  s16(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12),
			want: s16(1, 2, 7, 8),
		},
		{
			// 8-bit is unsigned with silence at 128, so the midpoint has
			// to land on zero rather than on half scale.
			name: "8-bit is re-centred",
			f:    Format{48000, 2, 8},
			src:  []byte{128, 128, 255, 0},
			want: s16(0, 0, 32512, -32768),
		},
		{
			name: "24-bit keeps the top two bytes",
			f:    Format{48000, 2, 24},
			src:  []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66},
			want: s16(0x3322, 0x6655),
		},
		{
			name: "32-bit keeps the top two bytes",
			f:    Format{48000, 2, 32},
			src:  []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
			want: s16(0x4433, -30601), // 0x8877 read as signed
		},
		{
			name: "empty in, empty out",
			f:    Format{48000, 2, 16},
			src:  nil,
			want: []byte{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.ToStereo16(tc.src)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("= % x\nwant % x", got, tc.want)
			}
		})
	}
}

// A chunk that stops mid-frame must not be half-decoded: doing so puts a
// click in the output and misaligns every frame after it.
func TestToStereo16DropsPartialFrames(t *testing.T) {
	f := Format{48000, 2, 16}
	src := append(s16(100, 200), 0x01, 0x02) // one whole frame plus a stray sample
	got := f.ToStereo16(src)
	if want := s16(100, 200); !bytes.Equal(got, want) {
		t.Errorf("= % x, want % x", got, want)
	}
}

// Output length has to be exactly four bytes a frame whatever went in, or the
// ring's alignment assumption breaks.
func TestToStereo16OutputIsFrameAligned(t *testing.T) {
	for _, f := range []Format{{48000, 1, 8}, {48000, 2, 16}, {48000, 3, 24}, {48000, 6, 32}} {
		for n := range 64 {
			got := f.ToStereo16(make([]byte, n))
			if len(got)%4 != 0 {
				t.Errorf("%s with %d bytes in: output %d bytes, not a whole number of frames", f, n, len(got))
			}
			if want := n / f.FrameSize() * 4; len(got) != want {
				t.Errorf("%s with %d bytes in: output %d bytes, want %d", f, n, len(got), want)
			}
		}
	}
}

func TestToStereo16RejectsUnknownDepth(t *testing.T) {
	f := Format{48000, 2, 12}
	if got := f.ToStereo16(make([]byte, 64)); got != nil {
		t.Errorf("= % x, want nil - an unhandled depth must not be reinterpreted as some other one", got)
	}
}
