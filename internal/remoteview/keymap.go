package remoteview

import "github.com/hajimehoshi/ebiten/v2"

// The target takes USB HID usage IDs from usage page 7, not platform key
// codes, so host keys have to be translated. The table is the only place
// that mapping is written down; anything not in it is simply not sent
// rather than being approximated by a nearby key.

// keyUsage maps a host key to its USB HID usage ID.
var keyUsage = map[ebiten.Key]uint8{
	ebiten.KeyA: 0x04, ebiten.KeyB: 0x05, ebiten.KeyC: 0x06, ebiten.KeyD: 0x07,
	ebiten.KeyE: 0x08, ebiten.KeyF: 0x09, ebiten.KeyG: 0x0a, ebiten.KeyH: 0x0b,
	ebiten.KeyI: 0x0c, ebiten.KeyJ: 0x0d, ebiten.KeyK: 0x0e, ebiten.KeyL: 0x0f,
	ebiten.KeyM: 0x10, ebiten.KeyN: 0x11, ebiten.KeyO: 0x12, ebiten.KeyP: 0x13,
	ebiten.KeyQ: 0x14, ebiten.KeyR: 0x15, ebiten.KeyS: 0x16, ebiten.KeyT: 0x17,
	ebiten.KeyU: 0x18, ebiten.KeyV: 0x19, ebiten.KeyW: 0x1a, ebiten.KeyX: 0x1b,
	ebiten.KeyY: 0x1c, ebiten.KeyZ: 0x1d,

	ebiten.KeyDigit1: 0x1e, ebiten.KeyDigit2: 0x1f, ebiten.KeyDigit3: 0x20,
	ebiten.KeyDigit4: 0x21, ebiten.KeyDigit5: 0x22, ebiten.KeyDigit6: 0x23,
	ebiten.KeyDigit7: 0x24, ebiten.KeyDigit8: 0x25, ebiten.KeyDigit9: 0x26,
	ebiten.KeyDigit0: 0x27,

	ebiten.KeyEnter: 0x28, ebiten.KeyBackspace: 0x2a, ebiten.KeyTab: 0x2b,
	ebiten.KeySpace: 0x2c, ebiten.KeyMinus: 0x2d, ebiten.KeyEqual: 0x2e,
	ebiten.KeyBracketLeft: 0x2f, ebiten.KeyBracketRight: 0x30,
	ebiten.KeyBackslash: 0x31, ebiten.KeySemicolon: 0x33,
	ebiten.KeyQuote: 0x34, ebiten.KeyBackquote: 0x35,
	ebiten.KeyComma: 0x36, ebiten.KeyPeriod: 0x37, ebiten.KeySlash: 0x38,
	ebiten.KeyCapsLock: 0x39,

	ebiten.KeyF1: 0x3a, ebiten.KeyF2: 0x3b, ebiten.KeyF3: 0x3c, ebiten.KeyF4: 0x3d,
	ebiten.KeyF5: 0x3e, ebiten.KeyF6: 0x3f, ebiten.KeyF7: 0x40, ebiten.KeyF8: 0x41,
	ebiten.KeyF9: 0x42, ebiten.KeyF10: 0x43, ebiten.KeyF11: 0x44, ebiten.KeyF12: 0x45,

	ebiten.KeyPrintScreen: 0x46, ebiten.KeyScrollLock: 0x47, ebiten.KeyPause: 0x48,
	ebiten.KeyInsert: 0x49, ebiten.KeyHome: 0x4a, ebiten.KeyPageUp: 0x4b,
	ebiten.KeyDelete: 0x4c, ebiten.KeyEnd: 0x4d, ebiten.KeyPageDown: 0x4e,

	ebiten.KeyArrowRight: 0x4f, ebiten.KeyArrowLeft: 0x50,
	ebiten.KeyArrowDown: 0x51, ebiten.KeyArrowUp: 0x52,

	ebiten.KeyNumLock: 0x53, ebiten.KeyNumpadDivide: 0x54,
	ebiten.KeyNumpadMultiply: 0x55, ebiten.KeyNumpadSubtract: 0x56,
	ebiten.KeyNumpadAdd: 0x57, ebiten.KeyNumpadEnter: 0x58,
	ebiten.KeyNumpad1: 0x59, ebiten.KeyNumpad2: 0x5a, ebiten.KeyNumpad3: 0x5b,
	ebiten.KeyNumpad4: 0x5c, ebiten.KeyNumpad5: 0x5d, ebiten.KeyNumpad6: 0x5e,
	ebiten.KeyNumpad7: 0x5f, ebiten.KeyNumpad8: 0x60, ebiten.KeyNumpad9: 0x61,
	ebiten.KeyNumpad0: 0x62, ebiten.KeyNumpadDecimal: 0x63,

	ebiten.KeyControlLeft: 0xe0, ebiten.KeyShiftLeft: 0xe1,
	ebiten.KeyAltLeft: 0xe2, ebiten.KeyMetaLeft: 0xe3,
	ebiten.KeyControlRight: 0xe4, ebiten.KeyShiftRight: 0xe5,
	ebiten.KeyAltRight: 0xe6, ebiten.KeyMetaRight: 0xe7,
}

// Lock-key bits ride alongside every key event so the target knows the
// keyboard's LED state.
const (
	lockNumLock uint8 = 1 << iota
	lockCapsLock
	lockScrollLock
)

// lockKeys is the current lock state as the target expects it. Ebiten
// reports these as ordinary keys, so the state has to be tracked rather than
// read.
type lockKeys struct {
	num, caps, scroll bool
}

func (l lockKeys) mask() uint8 {
	var m uint8
	if l.num {
		m |= lockNumLock
	}
	if l.caps {
		m |= lockCapsLock
	}
	if l.scroll {
		m |= lockScrollLock
	}
	return m
}

// toggle flips the lock that a key press corresponds to, and reports whether
// it was one.
func (l *lockKeys) toggle(k ebiten.Key) bool {
	switch k {
	case ebiten.KeyNumLock:
		l.num = !l.num
	case ebiten.KeyCapsLock:
		l.caps = !l.caps
	case ebiten.KeyScrollLock:
		l.scroll = !l.scroll
	default:
		return false
	}
	return true
}

// usageFor translates a host key. An unmapped key reports false and is
// dropped: sending a wrong usage ID would type the wrong character on the
// target, which is worse than the key doing nothing.
func usageFor(k ebiten.Key) (uint8, bool) {
	u, ok := keyUsage[k]
	return u, ok
}
