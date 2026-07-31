# Help wanted

This project has been built and tested against one devkit by one person.
That's enough to get everything here working end to end, but not enough to
know which quirks are real protocol behavior and which are specific to this
one unit, its firmware version, or the one title it's been tested against.
This file is the detail behind the short version in the README.

If you can help with any of this, opening an issue with what you tried and
what you saw is the most useful thing - even a "tried it, same result" on an
existing report narrows things down.

## Video

`internal/videodecode` turns the target's raw H.264 stream into a live
picture by shelling out to `ffmpeg`, forced to emit frames even though the
stream never contains a keyframe. Three real bugs were found and fixed this
way:

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

What's confirmed and measured after those fixes: real, continuous,
scripted controller input (holding a stick direction through the same input
path a physical controller drives) decodes at roughly 25 frames a second
over a 20-second window, with real picture detail, not just gray. What
hasn't been confirmed: an actual person playing with an actual controller
in hand, and saying whether it feels good - stutters, input lag, and
general "does this feel like a game" are judgment calls no amount of
scripted measurement substitutes for.

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
