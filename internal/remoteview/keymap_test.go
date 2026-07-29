package remoteview

import (
	"testing"

	"github.com/hajimehoshi/ebiten/v2"
)

// Two keys sharing a usage ID means one of them types the wrong character on
// the target, which is the sort of thing nobody notices until it bites.
func TestKeyUsagesAreUnique(t *testing.T) {
	seen := map[uint8]ebiten.Key{}
	for key, usage := range keyUsage {
		if usage == 0 {
			t.Errorf("key %v maps to usage 0", key)
		}
		if prev, ok := seen[usage]; ok {
			t.Errorf("usage %#x is shared by %v and %v", usage, prev, key)
		}
		seen[usage] = key
	}
}

// Spot-check the ones with a fixed, well-known value. Getting the letter
// block or the arrow block off by one shifts every key in it.
func TestKnownUsageIDs(t *testing.T) {
	cases := []struct {
		key  ebiten.Key
		want uint8
	}{
		{ebiten.KeyA, 0x04},
		{ebiten.KeyZ, 0x1d},
		{ebiten.KeyDigit1, 0x1e},
		{ebiten.KeyDigit0, 0x27},
		{ebiten.KeyEnter, 0x28},
		{ebiten.KeySpace, 0x2c},
		{ebiten.KeyF1, 0x3a},
		{ebiten.KeyF12, 0x45},
		{ebiten.KeyArrowRight, 0x4f},
		{ebiten.KeyArrowLeft, 0x50},
		{ebiten.KeyArrowDown, 0x51},
		{ebiten.KeyArrowUp, 0x52},
		{ebiten.KeyControlLeft, 0xe0},
		{ebiten.KeyShiftLeft, 0xe1},
		{ebiten.KeyMetaRight, 0xe7},
	}
	for _, tc := range cases {
		got, ok := usageFor(tc.key)
		if !ok {
			t.Errorf("%v is not mapped", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("%v = %#x, want %#x", tc.key, got, tc.want)
		}
	}
}

// The letters and digits have to be contiguous runs, since that's how the
// usage table is defined - a gap means a typo in the table.
func TestLetterAndDigitRunsAreContiguous(t *testing.T) {
	letters := []ebiten.Key{
		ebiten.KeyA, ebiten.KeyB, ebiten.KeyC, ebiten.KeyD, ebiten.KeyE, ebiten.KeyF,
		ebiten.KeyG, ebiten.KeyH, ebiten.KeyI, ebiten.KeyJ, ebiten.KeyK, ebiten.KeyL,
		ebiten.KeyM, ebiten.KeyN, ebiten.KeyO, ebiten.KeyP, ebiten.KeyQ, ebiten.KeyR,
		ebiten.KeyS, ebiten.KeyT, ebiten.KeyU, ebiten.KeyV, ebiten.KeyW, ebiten.KeyX,
		ebiten.KeyY, ebiten.KeyZ,
	}
	for i, k := range letters {
		got, ok := usageFor(k)
		if !ok {
			t.Fatalf("%v is not mapped", k)
		}
		if want := uint8(0x04 + i); got != want {
			t.Errorf("%v = %#x, want %#x", k, got, want)
		}
	}

	digits := []ebiten.Key{
		ebiten.KeyDigit1, ebiten.KeyDigit2, ebiten.KeyDigit3, ebiten.KeyDigit4,
		ebiten.KeyDigit5, ebiten.KeyDigit6, ebiten.KeyDigit7, ebiten.KeyDigit8,
		ebiten.KeyDigit9,
	}
	for i, k := range digits {
		got, _ := usageFor(k)
		if want := uint8(0x1e + i); got != want {
			t.Errorf("%v = %#x, want %#x", k, got, want)
		}
	}
	// Zero sits after nine, not before one.
	if got, _ := usageFor(ebiten.KeyDigit0); got != 0x27 {
		t.Errorf("KeyDigit0 = %#x, want 0x27", got)
	}
}

// An unmapped key is dropped rather than sent as some nearby key.
func TestUnmappedKeyIsReported(t *testing.T) {
	if _, ok := usageFor(ebiten.KeyEscape); ok {
		t.Error("Escape is mapped, but it's the window's own quit key")
	}
	if _, ok := usageFor(ebiten.KeyMax); ok {
		t.Error("KeyMax resolved to a usage ID")
	}
}

func TestLockKeyMask(t *testing.T) {
	var l lockKeys
	if l.mask() != 0 {
		t.Errorf("fresh mask = %#x, want 0", l.mask())
	}

	if !l.toggle(ebiten.KeyCapsLock) {
		t.Error("caps lock not recognised as a lock key")
	}
	if l.mask() != lockCapsLock {
		t.Errorf("mask = %#x, want %#x", l.mask(), lockCapsLock)
	}

	l.toggle(ebiten.KeyNumLock)
	l.toggle(ebiten.KeyScrollLock)
	if want := lockCapsLock | lockNumLock | lockScrollLock; l.mask() != want {
		t.Errorf("mask = %#x, want %#x", l.mask(), want)
	}

	// Pressing it again turns it back off.
	l.toggle(ebiten.KeyCapsLock)
	if l.mask()&lockCapsLock != 0 {
		t.Errorf("caps lock still set after a second press: %#x", l.mask())
	}
}

func TestLockKeyIgnoresOrdinaryKeys(t *testing.T) {
	var l lockKeys
	if l.toggle(ebiten.KeyA) {
		t.Error("A was treated as a lock key")
	}
	if l.mask() != 0 {
		t.Errorf("mask = %#x after an ordinary key, want 0", l.mask())
	}
}

func TestLockBitsAreDistinct(t *testing.T) {
	bits := []uint8{lockNumLock, lockCapsLock, lockScrollLock}
	seen := map[uint8]bool{}
	for _, b := range bits {
		if b == 0 {
			t.Error("a lock bit is zero")
		}
		if seen[b] {
			t.Errorf("duplicate lock bit %#x", b)
		}
		seen[b] = true
	}
}
