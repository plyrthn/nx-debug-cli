# Help wanted

This project has been built and tested against one devkit by one person.
That's enough to get everything here working end to end, but not enough to
know which quirks are real protocol behavior and which are specific to this
one unit, its firmware version, or the one title it's been tested against.
This file is the detail behind the short version in the README.

If you can help with any of this, opening an issue with what you tried and
what you saw is the most useful thing - even a "tried it, same result" on an
existing report narrows things down.

## Tools for digging in

Everything below is either already in this project or a standard, publicly
available tool - none of it needs the official SDK or a daemon running.

- `nxdbg gdbstub <serial> ...` is a one-shot scriptable client for the
  target's GDB remote stub (attach, registers, memory, breakpoints,
  watchpoints, backtrace) without needing a real debugger attached. Good for
  poking at the step/continue hang below without a full gdb/lldb session in
  the way.
- `nxdbg gdb <serial>` holds the same stub open behind a local port instead,
  for an actual debugger (gdb, LLDB, IDA, Ghidra, VS Code) to attach to.
  Anything a real debugger can do through that port is fair game to try.
- `nxdbg video record <serial> <seconds> out.h264` (and `dump`/`dump-audio`)
  writes the raw stream to a file. Feeding a capture straight into
  `ffmpeg`/`ffprobe`, bypassing this project's own decode path entirely, is
  how the video bugs above were actually tracked down, and is the fastest
  way to confirm or rule out a report independent of anything this
  project's code might be doing.
- `nxdbg dump read <file.nxdmp>` decodes a crash dump with no official
  tooling installed, if a devkit produces one worth cross-checking.
- Wireshark with its USBPcap capture driver (a standard Windows USB sniffer,
  nothing specific to this project) can capture the raw USB traffic between
  host and devkit, for anything that needs chasing below the level this
  project's own code even sees.
- `go test ./...` runs the existing test suite. A lot of what's pinned down
  above (the parameter set bytes, the gamepad button mapping, the wire
  formats) is covered by tests that read back what was written, so a
  regression fails loudly instead of needing to be spotted by eye.

## Video

**The big one: the target never sends a real keyframe, at all.** In
practice that means parts of the picture sit gray, and playback can skip
and stutter instead of running smooth, worst right after the stream
reconnects (which it does on its own roughly every 500ms). This is
confirmed H.264 (see the README), just never carrying an actual reference
frame - `internal/videodecode` shells out to `ffmpeg` and forces it to
build a picture anyway. Three real bugs in that path were found and fixed:

1. A decoder that had (incorrectly) flagged a real reference frame as seen
   stopped resyncing entirely and just drifted forever.
2. Seeding a fresh decoder from an encoded screenshot broke every real slice
   after it, because the screenshot's own encoder wrote its own parameter
   sets rather than the ones the target's real slices need.
3. `gaps_in_frame_num_value_allowed_flag` was set to forbid gaps, but the
   target's real `frame_num` cycles through a small range and starts over
   rather than climbing steadily, so every cycle read as a forbidden gap and
   roughly 93% of real frames were silently dropped before ever reaching the
   screen.

Those three took it from barely watchable to roughly 25 frames a second
measured over a 20-second window of continuous, real controller input, with
real picture detail, not just gray. That number is an average, not a
steady rate - the delivery is bursty (below), so what it actually looks
like in the moment is closer to "gray, then a burst of motion" than a
constant 30fps. Whether that's fixable client-side or is just what this
stream is, given it has no real keyframe to anchor to, is the open
question. What hasn't been confirmed at all: an actual person playing with
an actual controller in hand, and saying whether it feels good - stutters,
input lag, and general "does this feel like a game" are judgment calls no
amount of scripted measurement substitutes for.

Two things are believed to be structural limits of this specific stream
rather than bugs still to find, but more hardware would help confirm that:

- **Regions of the screen that rarely change stay gray indefinitely.** The
  stream has no real reference frame ever, so a fresh decoder only builds up
  a picture from whatever real slice data happens to touch each area.
  Confirmed stable (not decaying further) over a full 30 real seconds of
  gameplay, so this reads as a ceiling, not ongoing decay - but only tested
  on one title.
- **The stream's own delivery is bursty**, tied to the target closing and
  rebinding its listening socket roughly every 500ms. A resync sometimes
  shows a burst of quickly-flushed frames right after reconnecting,
  settling into whatever pace the target delivers afterward. Whether this
  pacing is a fundamental property of the stream or something environment-
  specific (a particular firmware version, a particular title, USB
  controller/host chipset) is genuinely unknown from a sample size of one.
- **Whether the target ever sends a real keyframe under any condition is
  still an open question**, not a settled "never." Every capture taken so
  far reconnected to a stream that was already running (DevMenu or a game
  already up); nobody has yet checked whether the very first client to
  connect right after the target boots gets a real IDR that later
  reconnects don't.

If you try this: which title, what the picture actually looked like
(screenshot or short recording helps a lot), and whether it held up during
real movement versus standing still, are the details that matter most.

## A second devkit

Several things in this project are pinned to values observed on one unit:

- The video stream's parameter sets (`internal/htc/h264params.go`,
  `NXVideoConfig`) - profile, level, entropy coding mode, and now
  `frame_num`'s 14-frame cycle - were all determined by trying candidates
  against a live capture until one decoded cleanly. Whether the exact cycle
  length (or the fact that there's a short cycle at all, rather than a
  steady climb) holds on a different firmware version or title is unknown.
- The GDB stub's `step`/`continue` hang (next section) and the SP/FP/LR
  registers reading back as zero on every thread state tested - both could
  be firmware-specific, kernel-specific, or universal to this generation of
  devkit. There's no way to tell from one unit.

## GDB stub step/continue

The stub advertises the modern `vCont` resume packets in its own
`qSupported` response, but on this hardware neither `vCont;s` nor `vCont;c`
ever actually resumes - no error, no reply at all, and the process stays
stuck attached until it's terminated or the console reboots. Confirmed
against five different processes in five different states (a crash handler,
a parked thread, a frozen post-abort thread, two healthy running games, and
a first-party SDK sample built in a debug configuration specifically to
rule out build type as the variable). The legacy pre-`vCont` `s`/`c`
packets fail fast and cleanly instead of hanging (`step-legacy`/
`continue-legacy`), which at least means there's a safe way to try it, but
they don't resume execution either.

If this behaves differently on a different firmware version, that would be
worth knowing - it would mean this is a firmware regression or
target-specific limitation rather than something wrong in how this project
talks to the stub.

## Linux, on real hardware

The USB transport has three implementations: WinUSB on Windows
(`internal/usbdev`, no cgo, no third-party library beyond the Windows SDK
headers), and `github.com/google/gousb` (a libusb binding) on Linux and
macOS. The macOS path is hardware-confirmed - handshake, link, `nxdbg
serve`, and a screenshot all matched Windows byte for byte on real
hardware. The Linux path is structurally identical but has only ever been
build-tested (`go build`/`go vet`), never run against a real devkit. If you
have one and can spare the time, even just confirming the handshake and a
screenshot succeed would close a real gap.
