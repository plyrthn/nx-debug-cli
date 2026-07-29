// Package remoteview is the interactive remote-screen window: it shows the
// target's live screen and forwards mouse input back to it as touch events,
// the same pairing Target Manager 2's remote video window provides.
//
// Frames come in already decoded (raw RGB24 straight off the target), so
// there's no codec and no external decoder process involved - the window is
// pure Go and behaves the same on every platform Ebiten supports.
package remoteview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// Options configures a window. Width/Height are the target's screen
// dimensions, not the window's - the window is resizable and the view scales
// to fit, with input mapped back into this space.
type Options struct {
	Title  string
	Width  int
	Height int

	// MaxFPS caps how often frames are pulled. The source can usually go
	// much faster than anything can be seen, and each pull is a round trip,
	// so there's no point running flat out. Zero picks a sane default.
	MaxFPS int
}

// Input is the subset of htc.RemoteInput the window drives. Keeping it an
// interface means the window can be exercised without a live devkit.
type Input interface {
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

// FrameFunc returns the target's current screen as raw RGB24, three bytes
// per pixel, top row first - exactly Width*Height*3 bytes.
type FrameFunc func(ctx context.Context) ([]byte, error)

// asyncInput runs every call to an Input on its own goroutine instead of the
// caller's, so a slow transport round trip never blocks Update() - which
// also draws the frame, so a synchronous send would stall the picture along
// with every later input for as long as the round trip took. Calls run in
// the order they were made, one at a time, so the underlying Input never
// sees concurrent access and a TouchBegin still lands before its TouchEnd.
//
// The queue is large enough that a real transport never fills it: even a
// slow round trip of tens of milliseconds would need several seconds of
// unbroken input to back up 256 deep. If it ever does fill, that means the
// transport genuinely cannot keep up, and blocking briefly is the honest
// outcome - dropping a touch or key event here can strand it half-sent (a
// TouchBegin with no TouchEnd holds a finger down on the target forever).
type asyncInput struct {
	underlying Input
	calls      chan func() error
}

func newAsyncInput(ctx context.Context, underlying Input) *asyncInput {
	a := &asyncInput{underlying: underlying, calls: make(chan func() error, 256)}
	go a.run(ctx)
	return a
}

func (a *asyncInput) run(ctx context.Context) {
	for {
		select {
		case fn := <-a.calls:
			if err := fn(); err != nil {
				fmt.Fprintln(os.Stderr, "input:", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (a *asyncInput) TouchBegin(id uint8, x, y int16) error {
	a.calls <- func() error { return a.underlying.TouchBegin(id, x, y) }
	return nil
}

func (a *asyncInput) TouchMove(id uint8, x, y int16) error {
	a.calls <- func() error { return a.underlying.TouchMove(id, x, y) }
	return nil
}

func (a *asyncInput) TouchEnd(id uint8) error {
	a.calls <- func() error { return a.underlying.TouchEnd(id) }
	return nil
}

func (a *asyncInput) HomeButton(pressed bool) error {
	a.calls <- func() error { return a.underlying.HomeButton(pressed) }
	return nil
}

func (a *asyncInput) KeyDown(usageID, lockedKeys uint8) error {
	a.calls <- func() error { return a.underlying.KeyDown(usageID, lockedKeys) }
	return nil
}

func (a *asyncInput) KeyUp(usageID, lockedKeys uint8) error {
	a.calls <- func() error { return a.underlying.KeyUp(usageID, lockedKeys) }
	return nil
}

func (a *asyncInput) PadAttach(id uint8) error {
	a.calls <- func() error { return a.underlying.PadAttach(id) }
	return nil
}

func (a *asyncInput) PadDisconnect(id uint8) error {
	a.calls <- func() error { return a.underlying.PadDisconnect(id) }
	return nil
}

func (a *asyncInput) PadButtons(id uint8, buttons htc.PadButton) error {
	a.calls <- func() error { return a.underlying.PadButtons(id, buttons) }
	return nil
}

func (a *asyncInput) PadStick(id uint8, side htc.PadStickSide, x, y int16) error {
	a.calls <- func() error { return a.underlying.PadStick(id, side, x, y) }
	return nil
}

const defaultMaxFPS = 30

// inputTPS is how often input is sampled and forwarded. Ebiten's default 60
// leaves up to 16ms of avoidable lag on a button press; the loop is cheap
// enough that doubling it costs nothing worth measuring.
const inputTPS = 120

// padID is the virtual controller this window drives. One window, one pad.
const padID = 0

type view struct {
	opts  Options
	input Input

	mu      sync.Mutex
	latest  []byte // most recent frame, converted to RGBA
	frameNo uint64
	srcErr  error

	img     *ebiten.Image
	drawnNo uint64

	touching bool
	locks    lockKeys
	heldKeys []ebiten.Key
	pad      padState
}

// Run opens the window and blocks until it's closed. It returns nil on a
// normal close (window closed or Escape pressed).
func Run(ctx context.Context, opts Options, next FrameFunc, input Input) error {
	if opts.Width <= 0 || opts.Height <= 0 {
		return fmt.Errorf("remoteview: bad target size %dx%d", opts.Width, opts.Height)
	}
	if opts.MaxFPS <= 0 {
		opts.MaxFPS = defaultMaxFPS
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	v := &view{opts: opts, input: newAsyncInput(ctx, input)}
	v.img = ebiten.NewImage(opts.Width, opts.Height)

	go v.pump(ctx, next)

	ebiten.SetWindowTitle(opts.Title)
	ebiten.SetWindowSize(opts.Width, opts.Height)
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	// Input should keep working while the window is in the background.
	ebiten.SetRunnableOnUnfocused(true)
	ebiten.SetTPS(inputTPS)

	if err := ebiten.RunGame(v); err != nil && !errors.Is(err, ebiten.Termination) {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.srcErr
}

// pump pulls frames until the context is cancelled. A single failed pull
// isn't fatal - the target drops the screen stream briefly on things like
// resolution changes - so it backs off and retries instead of tearing the
// window down.
func (v *view) pump(ctx context.Context, next FrameFunc) {
	interval := time.Second / time.Duration(v.opts.MaxFPS)
	want := v.opts.Width * v.opts.Height * 3
	fails := 0
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		raw, err := next(ctx)
		switch {
		case err != nil:
			fails++
			if fails > 100 {
				v.mu.Lock()
				v.srcErr = err
				v.mu.Unlock()
				return
			}
		case len(raw) < want:
			fails++
		default:
			fails = 0
			rgba := rgb24ToRGBA(raw, v.opts.Width, v.opts.Height)
			v.mu.Lock()
			v.latest = rgba
			v.frameNo++
			v.mu.Unlock()
		}
		if d := interval - time.Since(start); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return
			}
		}
	}
}

// rgb24ToRGBA widens packed RGB to the RGBA layout Ebiten uploads, filling
// in an opaque alpha channel.
func rgb24ToRGBA(src []byte, w, h int) []byte {
	dst := make([]byte, w*h*4)
	for i, j := 0, 0; i+2 < len(src) && j+3 < len(dst); i, j = i+3, j+4 {
		dst[j] = src[i]
		dst[j+1] = src[i+1]
		dst[j+2] = src[i+2]
		dst[j+3] = 0xff
	}
	return dst
}

func (v *view) Update() error {
	// Escape is the window's own quit key, so it's checked before the
	// keyboard forwarder and never reaches the target.
	if ebiten.IsKeyPressed(ebiten.KeyEscape) {
		return ebiten.Termination
	}
	if err := v.updatePointer(); err != nil {
		return err
	}
	if err := v.updateKeyboard(); err != nil {
		return err
	}
	return v.updateGamepad()
}

func (v *view) updatePointer() error {
	// Right-click stands in for the HOME button; there's nowhere else to
	// put it in a bare window.
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonRight) {
		if err := v.input.HomeButton(true); err != nil {
			return err
		}
	}
	if inpututil.IsMouseButtonJustReleased(ebiten.MouseButtonRight) {
		if err := v.input.HomeButton(false); err != nil {
			return err
		}
	}

	x, y, inBounds := v.pointer()
	switch {
	case inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) && inBounds:
		v.touching = true
		return v.input.TouchBegin(0, x, y)
	case v.touching && inpututil.IsMouseButtonJustReleased(ebiten.MouseButtonLeft):
		v.touching = false
		return v.input.TouchEnd(0)
	case v.touching && ebiten.IsMouseButtonPressed(ebiten.MouseButtonLeft):
		return v.input.TouchMove(0, x, y)
	}
	return nil
}

// updateKeyboard forwards key presses and releases. The lock-key state has
// to be updated before the press is sent, so the target sees the new LED
// state on the event that caused it.
func (v *view) updateKeyboard() error {
	v.heldKeys = inpututil.AppendJustPressedKeys(v.heldKeys[:0])
	for _, k := range v.heldKeys {
		usage, ok := usageFor(k)
		if !ok {
			continue
		}
		v.locks.toggle(k)
		if err := v.input.KeyDown(usage, v.locks.mask()); err != nil {
			return err
		}
	}

	v.heldKeys = inpututil.AppendJustReleasedKeys(v.heldKeys[:0])
	for _, k := range v.heldKeys {
		usage, ok := usageFor(k)
		if !ok {
			continue
		}
		if err := v.input.KeyUp(usage, v.locks.mask()); err != nil {
			return err
		}
	}
	return nil
}

// updateGamepad mirrors the first standard-layout host pad onto a virtual
// controller. Only changes are sent: the channel carries absolute state, so
// resending an unchanged mask every frame would be pure traffic.
func (v *view) updateGamepad() error {
	id, ok := firstStandardGamepad()
	if !ok {
		if v.pad.attached {
			v.pad = padState{}
			return v.input.PadDisconnect(padID)
		}
		return nil
	}
	if !v.pad.attached {
		if err := v.input.PadAttach(padID); err != nil {
			return err
		}
		v.pad = padState{attached: true}
	}

	var buttons htc.PadButton
	for _, b := range padBindings {
		if ebiten.IsStandardGamepadButtonPressed(id, b.host) {
			buttons |= b.target
		}
	}

	for i, s := range stickAxes {
		x := ebiten.StandardGamepadAxisValue(id, s.x)
		y := ebiten.StandardGamepadAxisValue(id, s.y)
		buttons |= digitalDirections(s, x, y)

		// The target's y axis points up, the host's points down.
		sx, sy := axisToStick(x), axisToStick(-y)
		if sx != v.pad.stickX[i] || sy != v.pad.stickY[i] {
			if err := v.input.PadStick(padID, s.side, sx, sy); err != nil {
				return err
			}
			v.pad.stickX[i], v.pad.stickY[i] = sx, sy
		}
	}

	if buttons != v.pad.buttons {
		if err := v.input.PadButtons(padID, buttons); err != nil {
			return err
		}
		v.pad.buttons = buttons
	}
	return nil
}

// pointer maps the cursor from window space into the target's screen space,
// accounting for the letterboxing the draw path applies.
func (v *view) pointer() (int16, int16, bool) {
	cx, cy := ebiten.CursorPosition()
	ww, wh := ebiten.WindowSize()
	return v.mapPointer(cx, cy, ww, wh)
}

func (v *view) mapPointer(cx, cy, ww, wh int) (int16, int16, bool) {
	if ww <= 0 || wh <= 0 {
		return 0, 0, false
	}
	scale, ox, oy := v.fit(ww, wh)
	tx := float64(cx-ox) / scale
	ty := float64(cy-oy) / scale
	if tx < 0 || ty < 0 || tx >= float64(v.opts.Width) || ty >= float64(v.opts.Height) {
		return 0, 0, false
	}
	return int16(tx), int16(ty), true
}

// fit returns the scale and offsets that centre the target's screen inside a
// window of the given size without distorting it.
func (v *view) fit(ww, wh int) (scale float64, ox, oy int) {
	sx := float64(ww) / float64(v.opts.Width)
	sy := float64(wh) / float64(v.opts.Height)
	scale = sx
	if sy < sx {
		scale = sy
	}
	ox = (ww - int(float64(v.opts.Width)*scale)) / 2
	oy = (wh - int(float64(v.opts.Height)*scale)) / 2
	return scale, ox, oy
}

func (v *view) Draw(screen *ebiten.Image) {
	v.mu.Lock()
	frame, no := v.latest, v.frameNo
	v.mu.Unlock()

	if frame != nil && no != v.drawnNo {
		v.img.WritePixels(frame)
		v.drawnNo = no
	}

	ww, wh := screen.Bounds().Dx(), screen.Bounds().Dy()
	scale, ox, oy := v.fit(ww, wh)
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Scale(scale, scale)
	op.GeoM.Translate(float64(ox), float64(oy))
	op.Filter = ebiten.FilterLinear
	screen.DrawImage(v.img, op)
}

func (v *view) Layout(int, int) (int, int) {
	// Unused: LayoutF drives sizing so the view tracks the real window size.
	return v.opts.Width, v.opts.Height
}

func (v *view) LayoutF(outsideWidth, outsideHeight float64) (float64, float64) {
	return outsideWidth, outsideHeight
}
