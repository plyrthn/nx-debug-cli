package remoteview

import (
	"math"

	"github.com/hajimehoshi/ebiten/v2"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// Host gamepads are mapped positionally, not by label. An Xbox pad's bottom
// face button is "A" and a Switch pad's bottom face button is "B", so
// matching on the letter would swap every confirm and cancel. Matching on
// position is what the player's thumb actually does.

// padBinding ties one host button to one target button. Adding a binding is
// an entry here and nothing else.
type padBinding struct {
	host   ebiten.StandardGamepadButton
	target htc.PadButton
}

var padBindings = []padBinding{
	// Face buttons, by position.
	{ebiten.StandardGamepadButtonRightBottom, htc.PadB},
	{ebiten.StandardGamepadButtonRightRight, htc.PadA},
	{ebiten.StandardGamepadButtonRightLeft, htc.PadY},
	{ebiten.StandardGamepadButtonRightTop, htc.PadX},

	// Shoulders and triggers.
	{ebiten.StandardGamepadButtonFrontTopLeft, htc.PadL},
	{ebiten.StandardGamepadButtonFrontTopRight, htc.PadR},
	{ebiten.StandardGamepadButtonFrontBottomLeft, htc.PadZL},
	{ebiten.StandardGamepadButtonFrontBottomRight, htc.PadZR},

	// D-pad.
	{ebiten.StandardGamepadButtonLeftTop, htc.PadUp},
	{ebiten.StandardGamepadButtonLeftBottom, htc.PadDown},
	{ebiten.StandardGamepadButtonLeftLeft, htc.PadLeft},
	{ebiten.StandardGamepadButtonLeftRight, htc.PadRight},

	// Centre cluster. Select/Start sit either side of the guide button.
	{ebiten.StandardGamepadButtonCenterLeft, htc.PadMinus},
	{ebiten.StandardGamepadButtonCenterRight, htc.PadPlus},
	{ebiten.StandardGamepadButtonCenterCenter, htc.PadHome},

	// Stick clicks.
	{ebiten.StandardGamepadButtonLeftStick, htc.PadStickLPress},
	{ebiten.StandardGamepadButtonRightStick, htc.PadStickRPress},
}

// stickDeadzone ignores the slack a resting stick reports. Without it a pad
// streams a change every frame and a menu drifts on its own.
const stickDeadzone = 0.12

// digitalStickThreshold is how far a stick has to go before it also counts
// as a digital direction press. Some target UI only reads the digital bits.
const digitalStickThreshold = 0.5

// stickAxis pairs the host axes for one stick with the target's digital
// direction bits for the same stick.
type stickAxis struct {
	side        htc.PadStickSide
	x, y        ebiten.StandardGamepadAxis
	left, right htc.PadButton
	up, down    htc.PadButton
}

var stickAxes = []stickAxis{
	{
		side:  htc.PadStickLeft,
		x:     ebiten.StandardGamepadAxisLeftStickHorizontal,
		y:     ebiten.StandardGamepadAxisLeftStickVertical,
		left:  htc.PadStickLLeft,
		right: htc.PadStickLRight,
		up:    htc.PadStickLUp,
		down:  htc.PadStickLDown,
	},
	{
		side:  htc.PadStickRight,
		x:     ebiten.StandardGamepadAxisRightStickHorizontal,
		y:     ebiten.StandardGamepadAxisRightStickVertical,
		left:  htc.PadStickRLeft,
		right: htc.PadStickRRight,
		up:    htc.PadStickRUp,
		down:  htc.PadStickRDown,
	},
}

// padState is what was last sent for a pad, so only real changes go on the
// wire. The channel takes absolute state, so resending unchanged values
// would just be traffic.
type padState struct {
	attached bool
	buttons  htc.PadButton
	stickX   [2]int16
	stickY   [2]int16
}

// axisToStick converts a host axis reading to the target's int16 range,
// applying the deadzone. Ebiten reports vertical axes positive-down and the
// target expects positive-up, so the caller negates y.
func axisToStick(v float64) int16 {
	if math.Abs(v) < stickDeadzone {
		return 0
	}
	// Rescale so the first movement past the deadzone starts from zero
	// rather than jumping.
	sign := 1.0
	if v < 0 {
		sign = -1
	}
	scaled := (math.Abs(v) - stickDeadzone) / (1 - stickDeadzone)
	if scaled > 1 {
		scaled = 1
	}
	return int16(sign * scaled * math.MaxInt16)
}

// digitalDirections turns a stick position into the direction bits that go
// alongside it.
func digitalDirections(s stickAxis, x, y float64) htc.PadButton {
	var b htc.PadButton
	if x <= -digitalStickThreshold {
		b |= s.left
	}
	if x >= digitalStickThreshold {
		b |= s.right
	}
	// Host vertical axes are positive-down.
	if y <= -digitalStickThreshold {
		b |= s.up
	}
	if y >= digitalStickThreshold {
		b |= s.down
	}
	return b
}

// firstStandardGamepad returns a connected pad that reports the standard
// layout. A pad without a layout mapping is skipped rather than read with
// guessed axis numbers.
func firstStandardGamepad() (ebiten.GamepadID, bool) {
	for _, id := range ebiten.AppendGamepadIDs(nil) {
		if ebiten.IsStandardGamepadLayoutAvailable(id) {
			return id, true
		}
	}
	return 0, false
}
