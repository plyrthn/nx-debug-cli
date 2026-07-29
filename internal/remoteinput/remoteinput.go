// Package remoteinput defines the interface the remote-view window sends
// input through.
//
// This used to adapt two backends onto that interface: the daemon's Input
// Director gRPC API, preferred on the theory that matching Target Manager
// 2's own code path would match its behaviour, and the raw HTCS channel as
// a fallback. Once the raw channel's own byte-order bug got fixed, raw
// input drove the UI exactly like the gRPC path did, so the gRPC backend
// was removed - it bought nothing over raw and cost a daemon dependency.
// See CLAUDE.md's "SOLVED: raw touch/tap input" and "SOLVED: raw gamepad
// buttons" sections for how that got proven.
package remoteinput

import (
	"fmt"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// Sink is what the window sends input through.
type Sink interface {
	TouchBegin(fingerID uint8, x, y int16) error
	TouchMove(fingerID uint8, x, y int16) error
	TouchEnd(fingerID uint8) error
	HomeButton(pressed bool) error
	KeyDown(usageID, lockedKeys uint8) error
	KeyUp(usageID, lockedKeys uint8) error
	PadAttach(id uint8) error
	PadDisconnect(id uint8) error
	PadButtons(id uint8, buttons htc.PadButton) error
	PadStick(id uint8, side htc.PadStickSide, x, y int16) error
}

var _ Sink = (*htc.RemoteInput)(nil)

// Unavailable reports that input can't be delivered right now. The window
// keeps running on it - losing the picture because a button didn't land
// would be the wrong trade.
type Unavailable struct{ Err error }

func (e *Unavailable) Error() string { return fmt.Sprintf("remote input unavailable: %v", e.Err) }
func (e *Unavailable) Unwrap() error { return e.Err }
