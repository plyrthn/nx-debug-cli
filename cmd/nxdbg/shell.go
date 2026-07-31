package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/plyrthn/nx-debug-cli/internal/commands"
	"github.com/plyrthn/nx-debug-cli/internal/htc"
	"github.com/plyrthn/nx-debug-cli/internal/remoteaudio"
	"github.com/plyrthn/nx-debug-cli/internal/remoteview"
	"github.com/plyrthn/nx-debug-cli/internal/videodecode"
)

// The command shell is the target's own control service. Everything here works
// with no daemon, because it resolves through the HTCS control port and talks
// to the target directly - which also means `nxdbg shell screenshot` produces a
// real screenshot without decoding the video stream.

// shellSub is one command-shell verb. It has the same shape as the other
// groups so `nxdbg help shell` and the dispatcher stay in step, but it is
// given the open connection rather than a daemon address. Its usage line and
// argument count come from the catalog, same as every other group.
type shellSub struct {
	name string
	run  func(ctx context.Context, s *htc.CommandShell, rest []string) error
}

var shellSubs = []shellSub{
	{"screenshot", shellScreenshot},
	{"screenshot-fg", shellScreenshotFg},
	{"firmware", shellFirmware},
	{"app", shellApp},
	{"title", shellTitle},
	{"program-id", shellProgramID},
	{"launch", shellLaunch},
	{"launch-system", shellLaunchSystem},
	{"terminate", shellTerminate},
	{"terminate-all", shellTerminateAll},
	{"reboot", shellReboot},
	{"shutdown", shellShutdown},
	{"events", shellEvents},
	{"devmenu", shellDevMenu},
	{"watch", shellWatch},
}

// shellDevMenu runs one of the target's own DevMenu commands. The remaining
// arguments are joined, so a command line can be written naturally rather than
// quoted as one string. Output is read back off the target log and printed as
// it arrives, so a query (`debug get-memory-mode`, `application occupied-size`,
// ...) actually shows its answer instead of just confirming the process ran.
func shellDevMenu(ctx context.Context, s *htc.CommandShell, rest []string) error {
	args := strings.Join(rest[1:], " ")
	_, err := htc.RunDevMenu(ctx, s.Serial, args, func(line string) {
		fmt.Println(line)
	})
	return err
}

func findShellSub(name string) (shellSub, bool) {
	for _, s := range shellSubs {
		if s.name == name {
			return s, true
		}
	}
	return shellSub{}, false
}

func runShell(ctx context.Context, serial string, rest []string) error {
	sub, ok := findShellSub(rest[0])
	if !ok {
		return fmt.Errorf("unknown shell subcommand: %s (try `nxdbg help shell`)", rest[0])
	}
	spec, ok := commands.Find("shell " + sub.name)
	if !ok {
		return fmt.Errorf("shell %s is dispatched but missing from the command catalog", sub.name)
	}
	// The serial is already consumed by the time this runs, so the target
	// placeholder is not part of what's left to check.
	if len(rest) < spec.MinArgs()-1 {
		return fmt.Errorf("usage: %s", spec.Usage())
	}
	if spec.Long {
		// A long-running subcommand (watch, events) blocks until interrupted,
		// not under the default per-command timeout.
		sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		ctx = sigCtx
	}
	s, err := htc.DialCommandShell(ctx, serial)
	if err != nil {
		return err
	}
	defer s.Close()
	return sub.run(ctx, s, rest)
}

func shellScreenshot(ctx context.Context, s *htc.CommandShell, rest []string) error {
	img, err := s.Screenshot(ctx)
	if err != nil {
		return err
	}
	return writeShellScreenshot(img, rest, "screenshot")
}

func shellScreenshotFg(ctx context.Context, s *htc.CommandShell, rest []string) error {
	img, err := s.ForegroundScreenshot(ctx)
	if err != nil {
		return err
	}
	return writeShellScreenshot(img, rest, "foreground")
}

// writeShellScreenshot saves to an explicit file, or names one inside a
// directory. The default is the working directory, matching the rest of the
// CLI, and the format is PNG because the target hands back raw pixels.
func writeShellScreenshot(img image.Image, rest []string, kind string) error {
	out := "."
	if len(rest) > 1 {
		out = rest[1]
	}
	if fi, err := os.Stat(out); err == nil && fi.IsDir() {
		out = filepath.Join(out, fmt.Sprintf("%s-%d.png", kind, time.Now().Unix()))
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	b := img.Bounds()
	fmt.Printf("wrote %s (%dx%d)\n", out, b.Dx(), b.Dy())
	return nil
}

func shellFirmware(ctx context.Context, s *htc.CommandShell, _ []string) error {
	fw, err := s.Firmware(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("version        %s\n", fw.Version)
	fmt.Printf("display        %s (%s)\n", fw.DisplayVersion, fw.DisplayName)
	fmt.Printf("platform       %s\n", fw.Platform)
	fmt.Printf("revision       %s\n", fw.Revision)
	fmt.Printf("relstep        %d.%d\n", fw.MajorRelstep, fw.MinorRelstep)
	return nil
}

func shellApp(ctx context.Context, s *htc.CommandShell, _ []string) error {
	app, err := s.Application(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("process index %d, process id %d\n", app.ProcessIndex, app.ProcessID)
	programID, err := s.ProgramID(ctx, app.ProcessIndex)
	if err != nil {
		return err
	}
	fmt.Printf("program id 0x%016x\n", programID)
	return nil
}

func shellTitle(ctx context.Context, s *htc.CommandShell, rest []string) error {
	index, err := shellProcessIndex(rest)
	if err != nil {
		return err
	}
	name, err := s.TitleName(ctx, index)
	if err != nil {
		return err
	}
	fmt.Println(name)
	return nil
}

func shellProgramID(ctx context.Context, s *htc.CommandShell, rest []string) error {
	index, err := shellProcessIndex(rest)
	if err != nil {
		return err
	}
	id, err := s.ProgramID(ctx, index)
	if err != nil {
		return err
	}
	fmt.Printf("0x%016x\n", id)
	return nil
}

func shellProcessIndex(rest []string) (uint64, error) {
	if len(rest) < 2 {
		return 0, nil
	}
	n, err := strconv.ParseUint(rest[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid process index %q: %w", rest[1], err)
	}
	return n, nil
}

func shellLaunch(ctx context.Context, s *htc.CommandShell, rest []string) error {
	id, err := strconv.ParseUint(trimHexPrefix(rest[1]), 16, 64)
	if err != nil {
		return fmt.Errorf("invalid program id %q: %w", rest[1], err)
	}
	var args string
	if len(rest) > 2 {
		args = rest[2]
	}
	index, err := s.LaunchApplication(ctx, id, args, "", 0)
	if err != nil {
		return err
	}
	fmt.Printf("launched 0x%016x (process index %d)\n", id, index)
	return nil
}

// shellLaunchSystem launches a system program rather than an application. It
// is the same command the target uses to run its own DevMenu commands, so it
// is also the general form of `devmenu`.
func shellLaunchSystem(ctx context.Context, s *htc.CommandShell, rest []string) error {
	id, err := strconv.ParseUint(trimHexPrefix(rest[1]), 16, 64)
	if err != nil {
		return fmt.Errorf("invalid program id %q: %w", rest[1], err)
	}
	index, err := s.LaunchSystemProgram(ctx, id, strings.Join(rest[2:], " "))
	if err != nil {
		return err
	}
	fmt.Printf("launched 0x%016x (process index %d)\n", id, index)
	return nil
}

func shellTerminate(ctx context.Context, s *htc.CommandShell, _ []string) error {
	if err := s.TerminateApplication(ctx); err != nil {
		return err
	}
	fmt.Println("✓ application terminated")
	return nil
}

func shellTerminateAll(ctx context.Context, s *htc.CommandShell, _ []string) error {
	if err := s.TerminateProcesses(ctx); err != nil {
		return err
	}
	fmt.Println("✓ all processes terminated")
	return nil
}

func shellReboot(ctx context.Context, s *htc.CommandShell, _ []string) error {
	// A target that is rebooting stops answering, so the reply never arrives.
	// Losing the connection here is the expected outcome, not a failure.
	if err := s.Reboot(ctx); err != nil && !shellWentAway(err) {
		return err
	}
	fmt.Println("✓ reboot requested")
	return nil
}

func shellShutdown(ctx context.Context, s *htc.CommandShell, _ []string) error {
	if err := s.Shutdown(ctx); err != nil && !shellWentAway(err) {
		return err
	}
	fmt.Println("✓ shutdown requested")
	return nil
}

func shellEvents(ctx context.Context, s *htc.CommandShell, rest []string) error {
	seconds := 30
	if len(rest) > 1 {
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			return fmt.Errorf("invalid seconds %q: %w", rest[1], err)
		}
		seconds = n
	}
	if err := s.SubscribeProcessEvents(ctx, true); err != nil {
		return err
	}
	fmt.Printf("watching for %ds\n", seconds)
	deadline := time.After(time.Duration(seconds) * time.Second)
	for {
		select {
		case <-deadline:
			return nil
		case <-ctx.Done():
			return nil
		case e, ok := <-s.Events:
			if !ok {
				return nil
			}
			fmt.Printf("  %-10s process %d\n", e.Kind, e.ProcessIndex)
		}
	}
}

// shellWatchMaxFPS caps how often the screenshot-polling window asks for a
// fresh frame. Each one is an uncompressed RGBA framebuffer (about 3.7MB at
// 1280x720) pulled synchronously over the command shell, so this only
// matters if the transport can actually keep up with it - remoteview's own
// pump already adapts to however long a poll actually takes, sleeping the
// remainder of the interval rather than the whole thing.
const shellWatchMaxFPS = 60

// runScreenshotWindow drives the interactive window: it polls
// CommandShell.BestScreenshot() in a loop rather than any video stream. The
// target composites a fresh, complete framebuffer on every request, so
// there is nothing to drift the way an H.264 stream would (see CLAUDE.md) -
// and, discovered the hard way, a stream like that doesn't just drift, it
// can fail to produce a picture at all for genuinely dynamic content like a
// running game, since it never sends an IDR to anchor a decoder on.
// BestScreenshot rather than plain Screenshot because sitting at DevMenu
// with no application running is the normal case, not a fault, and the
// whole-screen capture refuses in exactly that state.
// extra is appended to the printed banner.
func runScreenshotWindow(ctx context.Context, s *htc.CommandShell, windowTitle, extra string, input remoteview.Input) error {
	first, err := s.BestScreenshot(ctx)
	if err != nil {
		return fmt.Errorf("initial screenshot: %w", err)
	}
	b := first.Bounds()
	width, height := b.Dx(), b.Dy()

	next := func(ctx context.Context) ([]byte, error) {
		img, err := s.BestScreenshot(ctx)
		if err != nil {
			return nil, err
		}
		return imageToRGB24(img), nil
	}
	return runInteractiveWindow(ctx, s.Serial, windowTitle, extra, width, height, next, input)
}

// runInteractiveWindow prints the window banner and blocks in the window
// itself, whatever the picture source turns out to be.
func runInteractiveWindow(ctx context.Context, serial, windowTitle, extra string, width, height int, next remoteview.FrameFunc, input remoteview.Input) error {
	fmt.Printf("%s (%dx%d)%s\n", serial, width, height, extra)
	fmt.Println("  left click/drag  touch      right click  HOME      Esc  quit")
	fmt.Println("  keyboard and any connected gamepad are forwarded to the target")
	return remoteview.Run(ctx, remoteview.Options{
		Title:  windowTitle,
		Width:  width,
		Height: height,
		MaxFPS: shellWatchMaxFPS,
	}, next, input)
}

// shellWatch drives the interactive window off repeated screenshots: no
// codec, no drift, an exact copy of the target's screen every frame. Input
// and audio both resolve straight over HTCS, the same way the screenshots
// do.
func shellWatch(ctx context.Context, s *htc.CommandShell, _ []string) error {
	input := &lazyInput{serial: s.Serial, ctx: ctx}
	defer input.Close()

	stopAudio, audioDesc := startAudioDaemonFree(ctx, s.Serial)
	if stopAudio != nil {
		defer stopAudio()
	}

	return runScreenshotWindow(ctx, s, fmt.Sprintf("nxdbg - %s", s.Serial),
		fmt.Sprintf(", fresh screenshot each frame, never drifts, audio: %s", audioDesc), input)
}

// runVideoWindow drives `nxdbg video`'s window. It decodes the target's raw
// H.264 stream when it can, since that is what actually keeps pace with the
// target (nxdbg video dump sustains close to 30fps once MediaSession
// reconnects promptly - see CLAUDE.md), and falls back to shellWatch's
// screenshot picture (a couple of frames a second on this hardware) if
// decoding can't be started at all, so the window still opens either way.
func runVideoWindow(ctx context.Context, s *htc.CommandShell) error {
	input := &lazyInput{serial: s.Serial, ctx: ctx}
	defer input.Close()

	stopAudio, audioDesc := startAudioDaemonFree(ctx, s.Serial)
	if stopAudio != nil {
		defer stopAudio()
	}

	windowTitle := fmt.Sprintf("nxdbg - %s", s.Serial)
	next, width, height, stopVideo, videoDesc := startVideoDecodeDaemonFree(ctx, s.Serial)
	if next == nil {
		return runScreenshotWindow(ctx, s, windowTitle,
			fmt.Sprintf(", %s, audio: %s", videoDesc, audioDesc), input)
	}
	defer stopVideo()

	return runInteractiveWindow(ctx, s.Serial, windowTitle,
		fmt.Sprintf(", %s, audio: %s", videoDesc, audioDesc), width, height, next, input)
}

// startVideoDecodeDaemonFree turns on real H.264 decode for the video
// window: it dials the raw stream directly (no daemon) and feeds it through
// ffmpeg, forced to emit frames despite the target never sending a keyframe
// - see internal/videodecode and NXVideoConfig in internal/htc for why. A
// nil FrameFunc and a message explaining why is returned rather than an
// error - decoding is what makes the window fast, not what makes it work at
// all, so a machine without ffmpeg on PATH still gets a working window via
// the screenshot fallback instead of nothing.
func startVideoDecodeDaemonFree(ctx context.Context, serial string) (next remoteview.FrameFunc, width, height int, stop func(), desc string) {
	session, err := htc.DialVideoSession(ctx, serial)
	if err != nil {
		return nil, 0, 0, nil, fmt.Sprintf("H.264 decode unavailable (%v), falling back to screenshots", err)
	}

	// Seeding this from a screenshot was tried and reverted: a screenshot
	// encoded fresh via libx264 carries libx264's own SPS/PPS (its own
	// reference-frame count, its own picture-order-count scheme), not this
	// stream's - see NXVideoConfig's doc for why the real one is exactly
	// this and nothing else. Once that foreign parameter set is in effect,
	// every real slice from the target after it decodes as garbage
	// ("reference count overflow", confirmed against a live capture). The
	// target's own slices only ever parse cleanly against NXVideoConfig's
	// own parameter sets, so that is what every decoder has to start from.
	cfg := htc.NXVideoConfig
	seed := cfg.ParameterSets()

	dec, err := videodecode.StartSession(ctx, cfg.Width, cfg.Height, videodecode.StaticParams(seed))
	if err != nil {
		session.Close()
		return nil, 0, 0, nil, fmt.Sprintf("H.264 decode unavailable (%v), falling back to screenshots", err)
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-dec.Events():
				if !ok {
					return
				}
				log.Printf("video decode: %s", msg)
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := session.NextFrame()
			if err != nil {
				// The raw stream is gone for good (MediaSession already
				// retries every ordinary reconnect internally, so reaching
				// here means that gave up too). Nothing after this point
				// ever writes to dec again, so its picture freezes on
				// whatever it last decoded - log it so a frozen window has
				// an actual reason attached instead of going silent.
				log.Printf("video decode: raw stream ended (%v), picture will stop updating", err)
				return
			}
			if f.Kind != htc.VideoDataFrame || len(f.Payload) == 0 {
				continue
			}
			// A write failing here is expected whenever it lands in the
			// brief window where Session is mid-resync (the old decoder's
			// stdin just closed, the new one isn't wired in yet) - that
			// payload is lost, which is fine at ~30 payloads/sec, but the
			// raw stream itself (session.NextFrame above) is still alive
			// and has to keep being drained regardless, or the target's
			// own reconnect cycle backs up behind it. Only NextFrame
			// failing means the stream itself is actually gone.
			dec.Write(f.Payload)
		}
	}()

	stop = func() {
		session.Close()
		<-done
		dec.Close()
	}

	return dec.Frame, cfg.Width, cfg.Height, stop, "decoded H.264 video, live"
}

// daemonFreeAudioFormat is what the target's audio stream actually carries,
// fixed rather than queried. This hardware's stream uses the legacy20
// header layout (see CLAUDE.md), which carries no format frame at all - the
// same reason the video side's resolution and frame rate are fixed too -
// and the daemon's own RemoteAudioFormat RPC reports exactly this same
// 48kHz stereo 16-bit on every connection, so it isn't a guess a daemon-free
// client couldn't make just as well.
var daemonFreeAudioFormat = remoteaudio.Format{SampleRate: 48000, Channels: 2, BitsPerSample: 16}

// startAudioDaemonFree plays the target's audio with no daemon at all, over
// iywys@$remoteAudio directly through HTCS - the raw stream the daemon's own
// audio RPCs relay, reached the same way nxdbg serve reaches every other
// service. A nil stop and a message explaining why is returned rather than
// an error, matching startAudio's own "sound is a bonus" handling: nothing
// here should keep the picture from opening.
func startAudioDaemonFree(ctx context.Context, serial string) (stop func(), desc string) {
	session, err := htc.DialAudioSession(ctx, serial)
	if err != nil {
		return nil, "unavailable (" + err.Error() + ")"
	}
	player, err := remoteaudio.NewPlayer(daemonFreeAudioFormat)
	if err != nil {
		session.Close()
		return nil, "unavailable (" + err.Error() + ")"
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := session.NextFrame()
			if err != nil {
				return
			}
			if len(f.Payload) > 0 {
				player.Write(f.Payload)
			}
		}
	}()

	stop = func() {
		session.Close()
		<-done
		player.Close()
	}
	desc = fmt.Sprintf("%dHz %dch %d-bit (daemon-free, fixed format)",
		daemonFreeAudioFormat.SampleRate, daemonFreeAudioFormat.Channels, daemonFreeAudioFormat.BitsPerSample)
	return stop, desc
}

// imageToRGB24 packs an image down to the raw top-row-first RGB24 remoteview
// wants. Screenshots come back as *image.RGBA, so the fast path below is
// what actually runs; the generic fallback just keeps this correct for any
// image.Image rather than panicking on a type it doesn't expect.
func imageToRGB24(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, w*h*3)
	if rgba, ok := img.(*image.RGBA); ok {
		for y := 0; y < h; y++ {
			si := y * rgba.Stride
			di := y * w * 3
			for x := 0; x < w; x++ {
				out[di+0] = rgba.Pix[si+0]
				out[di+1] = rgba.Pix[si+1]
				out[di+2] = rgba.Pix[si+2]
				si += 4
				di += 3
			}
		}
		return out
	}
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			out[i+0] = byte(r >> 8)
			out[i+1] = byte(g >> 8)
			out[i+2] = byte(bl >> 8)
			i += 3
		}
	}
	return out
}

// shellWentAway reports whether an error is the connection dropping or the
// wait timing out, which is the normal outcome of asking the target to reboot
// or power off: it stops answering before it can acknowledge.
func shellWentAway(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}
