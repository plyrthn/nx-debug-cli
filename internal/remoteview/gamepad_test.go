package remoteview

import (
	"math"
	"testing"

	"github.com/hajimehoshi/ebiten/v2"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// Two host buttons mapped to the same target button, or one host button
// mapped twice, would make part of the pad unusable in a way that's hard to
// spot by playing.
func TestPadBindingsAreOneToOne(t *testing.T) {
	seenHost := map[ebiten.StandardGamepadButton]bool{}
	seenTarget := map[htc.PadButton]bool{}
	for _, b := range padBindings {
		if seenHost[b.host] {
			t.Errorf("host button %v bound twice", b.host)
		}
		if seenTarget[b.target] {
			t.Errorf("target button %#x bound twice", uint32(b.target))
		}
		if b.target == 0 {
			t.Errorf("host button %v maps to no target button", b.host)
		}
		seenHost[b.host] = true
		seenTarget[b.target] = true
	}
}

// Face buttons are mapped by position, not by letter. An Xbox pad's bottom
// button has to be the Switch's bottom button (B), not the Switch's A.
func TestFaceButtonsMapByPosition(t *testing.T) {
	want := map[ebiten.StandardGamepadButton]htc.PadButton{
		ebiten.StandardGamepadButtonRightBottom: htc.PadB,
		ebiten.StandardGamepadButtonRightRight:  htc.PadA,
		ebiten.StandardGamepadButtonRightLeft:   htc.PadY,
		ebiten.StandardGamepadButtonRightTop:    htc.PadX,
	}
	got := map[ebiten.StandardGamepadButton]htc.PadButton{}
	for _, b := range padBindings {
		if _, interesting := want[b.host]; interesting {
			got[b.host] = b.target
		}
	}
	for host, target := range want {
		if got[host] != target {
			t.Errorf("host %v maps to %#x, want %#x", host, uint32(got[host]), uint32(target))
		}
	}
}

// Every button a player can reach should do something; a missing binding is
// a button that silently does nothing on the target.
func TestEveryStandardButtonIsBound(t *testing.T) {
	bound := map[ebiten.StandardGamepadButton]bool{}
	for _, b := range padBindings {
		bound[b.host] = true
	}
	for _, b := range []ebiten.StandardGamepadButton{
		ebiten.StandardGamepadButtonRightBottom, ebiten.StandardGamepadButtonRightRight,
		ebiten.StandardGamepadButtonRightLeft, ebiten.StandardGamepadButtonRightTop,
		ebiten.StandardGamepadButtonFrontTopLeft, ebiten.StandardGamepadButtonFrontTopRight,
		ebiten.StandardGamepadButtonFrontBottomLeft, ebiten.StandardGamepadButtonFrontBottomRight,
		ebiten.StandardGamepadButtonCenterLeft, ebiten.StandardGamepadButtonCenterRight,
		ebiten.StandardGamepadButtonLeftStick, ebiten.StandardGamepadButtonRightStick,
		ebiten.StandardGamepadButtonLeftTop, ebiten.StandardGamepadButtonLeftBottom,
		ebiten.StandardGamepadButtonLeftLeft, ebiten.StandardGamepadButtonLeftRight,
		ebiten.StandardGamepadButtonCenterCenter,
	} {
		if !bound[b] {
			t.Errorf("standard button %v has no binding", b)
		}
	}
}

func TestAxisToStick(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want int16
	}{
		{"centred", 0, 0},
		{"inside deadzone", 0.1, 0},
		{"inside deadzone negative", -0.1, 0},
		{"full right", 1, math.MaxInt16},
		{"full left", -1, -math.MaxInt16},
		{"beyond range clamps", 1.5, math.MaxInt16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := axisToStick(tc.in); got != tc.want {
				t.Errorf("axisToStick(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// Movement just past the deadzone must start from near zero, not jump to a
// large value - otherwise a small nudge reads as a hard push.
func TestAxisToStickDoesNotJumpAtTheDeadzone(t *testing.T) {
	justInside := axisToStick(stickDeadzone - 0.001)
	justOutside := axisToStick(stickDeadzone + 0.001)
	if justInside != 0 {
		t.Errorf("just inside the deadzone = %d, want 0", justInside)
	}
	if justOutside > math.MaxInt16/100 {
		t.Errorf("just outside the deadzone = %d, want a small value", justOutside)
	}
	// And it has to be monotonic.
	prev := int16(0)
	for v := stickDeadzone; v <= 1; v += 0.05 {
		got := axisToStick(v)
		if got < prev {
			t.Fatalf("axisToStick(%v) = %d went backwards from %d", v, got, prev)
		}
		prev = got
	}
}

func TestDigitalDirections(t *testing.T) {
	left := stickAxes[0]
	cases := []struct {
		name string
		x, y float64
		want htc.PadButton
	}{
		{"centred", 0, 0, 0},
		{"small nudge", 0.2, 0.2, 0},
		{"pushed right", 1, 0, htc.PadStickLRight},
		{"pushed left", -1, 0, htc.PadStickLLeft},
		// Host vertical axes are positive-down, so -1 is up.
		{"pushed up", 0, -1, htc.PadStickLUp},
		{"pushed down", 0, 1, htc.PadStickLDown},
		{"diagonal", 1, 1, htc.PadStickLRight | htc.PadStickLDown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := digitalDirections(left, tc.x, tc.y); got != tc.want {
				t.Errorf("digitalDirections(%v, %v) = %#x, want %#x", tc.x, tc.y, uint32(got), uint32(tc.want))
			}
		})
	}
}

// The two stick entries must address different sticks and different
// direction bits, or the right stick would overwrite the left.
func TestStickAxesAreDistinct(t *testing.T) {
	if len(stickAxes) != 2 {
		t.Fatalf("stickAxes has %d entries, want 2", len(stickAxes))
	}
	l, r := stickAxes[0], stickAxes[1]
	if l.side == r.side {
		t.Error("both entries address the same stick")
	}
	if l.x == r.x || l.y == r.y {
		t.Error("both entries read the same host axes")
	}
	if l.left|l.right|l.up|l.down == r.left|r.right|r.up|r.down {
		t.Error("both entries set the same direction bits")
	}
}
