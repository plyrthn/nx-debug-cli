package remoteview

import (
	"bytes"
	"testing"
)

func TestRGB24ToRGBA(t *testing.T) {
	// two pixels: red, then green
	src := []byte{0xff, 0x00, 0x00, 0x00, 0xff, 0x00}
	got := rgb24ToRGBA(src, 2, 1)
	want := []byte{0xff, 0x00, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff}
	if !bytes.Equal(got, want) {
		t.Errorf("rgba = % x, want % x", got, want)
	}
}

func TestRGB24ToRGBALength(t *testing.T) {
	got := rgb24ToRGBA(make([]byte, 4*3), 2, 2)
	if len(got) != 2*2*4 {
		t.Errorf("len = %d, want %d", len(got), 2*2*4)
	}
}

// A short source buffer must not panic - the target briefly returns partial
// frames around resolution changes.
func TestRGB24ToRGBAShortSource(t *testing.T) {
	got := rgb24ToRGBA([]byte{1, 2, 3}, 4, 4)
	if len(got) != 4*4*4 {
		t.Errorf("len = %d, want %d", len(got), 4*4*4)
	}
}

func TestFitLetterboxes(t *testing.T) {
	v := &view{opts: Options{Width: 1280, Height: 720}}

	// Exact aspect: fills the window, no offset.
	scale, ox, oy := v.fit(1280, 720)
	if scale != 1 || ox != 0 || oy != 0 {
		t.Errorf("exact fit = %v,%d,%d; want 1,0,0", scale, ox, oy)
	}

	// Too wide: height-limited, bars on the left and right.
	scale, ox, oy = v.fit(2560, 720)
	if scale != 1 || ox != 640 || oy != 0 {
		t.Errorf("wide fit = %v,%d,%d; want 1,640,0", scale, ox, oy)
	}

	// Too tall: width-limited, bars top and bottom.
	scale, ox, oy = v.fit(1280, 1440)
	if scale != 1 || ox != 0 || oy != 360 {
		t.Errorf("tall fit = %v,%d,%d; want 1,0,360", scale, ox, oy)
	}
}

func TestMapPointer(t *testing.T) {
	v := &view{opts: Options{Width: 1280, Height: 720}}

	cases := []struct {
		name           string
		cx, cy, ww, wh int
		wantX, wantY   int16
		wantIn         bool
	}{
		{"origin 1:1", 0, 0, 1280, 720, 0, 0, true},
		{"centre 1:1", 640, 360, 1280, 720, 640, 360, true},
		{"half scale", 320, 180, 640, 360, 640, 360, true},
		{"double scale", 1280, 720, 2560, 1440, 640, 360, true},
		// Letterboxed: 640px of bar each side, so the target's x=0 sits at
		// window x=640.
		{"letterbox left edge", 640, 0, 2560, 720, 0, 0, true},
		{"inside left bar", 100, 100, 2560, 720, 0, 0, false},
		{"past right edge", 2559, 100, 2560, 720, 0, 0, false},
		{"negative", -5, -5, 1280, 720, 0, 0, false},
		{"degenerate window", 10, 10, 0, 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, y, in := v.mapPointer(tc.cx, tc.cy, tc.ww, tc.wh)
			if in != tc.wantIn {
				t.Fatalf("inBounds = %v, want %v", in, tc.wantIn)
			}
			if !in {
				return
			}
			if x != tc.wantX || y != tc.wantY {
				t.Errorf("mapped = (%d,%d), want (%d,%d)", x, y, tc.wantX, tc.wantY)
			}
		})
	}
}

// Whatever the window size, a mapped point must always be a legal target
// coordinate - the touch chunk has no room for out-of-range values.
func TestMapPointerStaysInTargetSpace(t *testing.T) {
	v := &view{opts: Options{Width: 1280, Height: 720}}
	for _, ww := range []int{200, 640, 1280, 3000} {
		for _, wh := range []int{200, 360, 720, 1600} {
			for _, cx := range []int{0, ww / 3, ww - 1} {
				for _, cy := range []int{0, wh / 3, wh - 1} {
					x, y, in := v.mapPointer(cx, cy, ww, wh)
					if !in {
						continue
					}
					if x < 0 || y < 0 || int(x) >= v.opts.Width || int(y) >= v.opts.Height {
						t.Fatalf("window %dx%d cursor (%d,%d) -> out of range (%d,%d)", ww, wh, cx, cy, x, y)
					}
				}
			}
		}
	}
}
