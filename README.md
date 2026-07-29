# nx-debug-cli

An independent, open source client for the Nintendo Switch devkit's own
on-target protocol - a library and CLI that drive the devkit's USB link
directly, with no other software required.

**Status: working, and validated against real hardware.** The htclow mux,
HTCS, HTCMISC, HTCFS, the command shell, the video and audio streams, the gdb
stub, the debug monitor, the target log, and `.nxdmp` crash dump reading have
all been exercised against a real devkit.

Raw HID input (touch, tap, home, and gamepad buttons) drives the target's UI
directly - `nxdbg input <serial> raw-tap`/`raw-pad`/etc. See
`nxdbg input <serial> status`.

Run `nxdbg` with no arguments for a `bubbletea` TUI; run it with any
subcommand for plain CLI/scripting output. Same binary either way, and the
same command list: both are built from one catalog (`internal/commands`), so
the TUI's `:` palette offers every command the CLI has, and `nxdbg help` is
generated rather than written out beside it.

## Live control window

```
nxdbg video <serial>          # or bare `nxdbg video` with one target connected
nxdbg shell <serial> watch    # same window, reached through the shell group
```

Opens a window showing the target's screen, driven by repeated screenshots
rather than the H.264 stream, so the picture never drifts or degrades over a
long session. Left click/drag sends touch, right click sends HOME, keyboard
and any connected gamepad are forwarded to the target, and the target's audio
plays back alongside it.

## Installing and managing applications

```
nxdbg install <serial> game.nsp [--sdcard|--builtin|--auto]
nxdbg apps <serial>
nxdbg uninstall <serial> <application-id>
```

Installing is the target's own DevMenuCommand reading the package back off
the host through HTCFS, so the file has to be inside the tree `nxdbg serve`
is rooted at (`--root`, default the working directory).

## Attach your own debugger

The devkit runs a standard GDB remote stub. This forwards it to a local port,
so any debugger that speaks the GDB remote protocol can attach.

```
nxdbg gdb <serial>
```

That binds `localhost:2159` (the IANA-registered `gdb-remote` port). Then:

| Debugger | Connect with |
| --- | --- |
| gdb | `target remote localhost:2159` |
| LLDB | `gdb-remote localhost:2159` |
| VS Code | `"type": "cppdbg"`, `"miDebuggerServerAddress": "localhost:2159"` |
| IDA | Debugger > Attach > Remote GDB debugger, `localhost:2159` |
| Ghidra | Debugger > Connect, `target remote localhost:2159` |

Verified end to end against a real devkit: a `qSupported` handshake came back
reporting `PacketSize`, `multiprocess+`, `swbreak+`, `hwbreak+`,
`ConditionalBreakpoints+` and `vContSupported+`.

In the TUI this is the `g` key.

### Scripting the gdb stub directly

`nxdbg gdbstub` is a one-shot scripting equivalent of the same stub, for use
without launching an actual debugger - attach by pid, read/write memory and
registers, set software/hardware breakpoints and watchpoints, walk a
backtrace, list loaded modules, resolve an address against a local symbol
file:

```
nxdbg gdbstub <serial> attach <pid> [symbol-file]
nxdbg gdbstub <serial> regs <pid>
nxdbg gdbstub <serial> setreg <pid> <reg> <value-hex>
nxdbg gdbstub <serial> read/write <pid> <addr-hex> ...
nxdbg gdbstub <serial> break/clear <pid> <addr-hex>
nxdbg gdbstub <serial> hwbreak/hwclear <pid> <addr-hex>
nxdbg gdbstub <serial> watch/unwatch <pid> <addr-hex> <length-hex> <write|read|access>
nxdbg gdbstub <serial> backtrace <pid> [symbol-file] [max-frames]
nxdbg gdbstub <serial> modules <pid>
```

Attach, register read, memory read/write, software breakpoint set, hardware
breakpoint set, and watchpoint set are all confirmed working live against a
real devkit, several byte-for-byte verified (a real `0xDEADBEEF` write and
read-back, a software breakpoint patched and restored). **Clearing any kind
of breakpoint or watchpoint - including the plain software `break`/`clear`
pair - fails if `clear`/`unwatch`/`hwclear` runs as a separate CLI
invocation after `break`/`watch`/`hwbreak` already detached**: whatever this
stub tracks about a breakpoint does not survive a full detach/reattach
cycle through two different connections. Set and clear in the same
persistent session (`nxdbg gdb` plus a real gdb/lldb client staying
attached throughout) rather than two one-shot `gdbstub` CLI calls if you
need the clear to actually take.

Register **write** (`setreg`) was refused live (`E01`) - tested only
against a crashed process whose thread context was already reading back as
all-zero (see the SP/FP/LR limitation above), so it's not yet known whether
that's this stub refusing register writes outright or specific to a
thread already in a broken state.

`step`/`continue` are implemented against the modern `vCont` resume packets
this stub itself advertises, but on every devkit tested here the stub never
actually completes one - it doesn't refuse, it just never replies, and
leaves the pid stuck attached until the process is terminated or the console
rebooted. Confirmed against five different processes in five different
states, including a first-party SDK sample built in the `Debug`
configuration specifically to rule out build type as the variable. Treat
step/continue on this stub as non-functional until proven otherwise on a
given firmware. `step-legacy`/`continue-legacy` send the bare pre-`vCont`
`s`/`c` packets instead - confirmed live to fail cleanly and immediately
(`ErrNotSupported`) rather than hang, so they're safe to try, but they
don't actually resume execution either; this stub has no working execution
control through either mechanism.

The optional `symbol-file` on `attach`/`step`/`continue` resolves the
reported pc against an unstripped ELF - for anything built with this SDK's
toolchain, that's the `.nss` file sitting next to the shipped, stripped
`.nso`. Matched against the live module list (`modules`) by name, so it
works regardless of where the local copy lives.

### Reading the target's own crash dumps

```
nxdbg dump read <file.nxdmp> [--all-threads]
nxdbg dump list <dir>
```

Decodes `.nxdmp` files directly - process/exception info, loaded modules,
per-thread registers, and a symbolicated stack trace - with no official
tooling or SDK install required. Some dumps genuinely carry no stack trace
or no thread flagged as the exception thread (seen live: a debugger-attach
"User Break", not a real fault); the reader says so plainly rather than
printing a misleading blank section.

### The debug monitor (`iywys@$dmnt`)

```
nxdbg debug <serial> banner
nxdbg debug <serial> select <handle-hex>
nxdbg debug <serial> read <handle-hex> <addr-hex> <count>
nxdbg debug <serial> modules/threads <handle-hex> [count]
```

A second, independent inspection path into an already-attached process (real
thread names recovered live: `MainThread`, `GameThread`,
`TaskGraphThreadNP 0`/`1`, ...). Complements the gdb stub rather than
replacing it - this one needs something to already hold a debug attach on
the target; it doesn't create one itself.

## Getting a session

`nxdbg serve` drives the devkit over USB directly: the htclow link and
handshake, the HTCS session, and a local listener for every service the
target publishes.

```
nxdbg serve          # then, in another terminal, nxdbg gdb <serial>
nxdbg usb            # inspect the USB interface directly
```

`serve` brings up all three services a target's own session needs: HTCS,
HTCMISC, and HTCFS. That last one is how a program on the target reaches the
host's filesystem, so it is bounded here:

```
nxdbg serve --root ./sandbox     # the target sees this directory as its root
nxdbg serve --read-only          # reads work, nothing on the host changes
```

The default root is the working directory. Nothing outside it is reachable:
`..`, absolute paths and drive letters all resolve back inside.

### The target's own command shell

`nxdbg shell` talks to the target's control service directly, and is the
fastest way to see what is actually on the screen:

```
nxdbg shell <serial> screenshot-fg out.png   # a real PNG, no video decoding
nxdbg shell <serial> firmware
nxdbg shell <serial> app
nxdbg shell <serial> events 30
```

The screenshot is the target compositing and handing back finished pixels, so
there is no codec in the path at all. `nxdbg help shell` lists the rest
(launch, terminate, reboot, shutdown, process lookups, the live window).

### The target log

```
nxdbg logging watch <serial>
```

Reads the target's log stream directly and decodes it. Records only appear when
something on the target is actually logging through `nn::diag`; a devkit sitting
on DevMenu produces none.

### Recording the video stream

```
nxdbg video record <serial> 10 out.h264
ffmpeg -flags2 +showall -i out.h264 frame%03d.png
```

The target sends no SPS/PPS, so a raw capture describes nothing and no decoder
will open it. `record` writes the encoder's parameter sets in front of the
stream, which makes the file valid; `--raw` writes only what arrived. There is
also no keyframe in the stream, so a decoder needs `-flags2 +showall` to emit
frames at all, and the picture builds up rather than snapping into place.

## Why

The official tools work, but they're Windows-only, closed source, and require
a full Visual Studio install for live debugging. This project is a
standalone, cross-platform, scriptable alternative, with no daemon and no
official SDK install required: full target management, live debugging
(attach, memory read/write, software and hardware breakpoints, watchpoints,
register read/write, stack backtraces), crash dump reading, and video/audio
capture all built on the devkit's own USB protocol directly.

## Layout

- `internal/htc/` - the actual client library. No CLI-specific code lives
  here, so it's usable as a Go library on its own.
- `cmd/nxdbg/` - the one binary. No args launches `internal/tui`; any
  subcommand runs the plain CLI dispatch in this package directly, with
  deliberately simple, script-friendly output (easy to wrap from a future
  MCP server or anything else without needing to touch the library).
- `internal/commands/` - the command catalog. One list of everything nxdbg
  can do, holding no handlers: the CLI checks its arguments and generates its
  help from it, and the TUI builds its palette from it. Tests check it against
  the real dispatch tables in both directions, so a command cannot exist in
  one front end and be missing from the other.
- `internal/tui/` - the `bubbletea`/`lipgloss` terminal UI, built on
  `internal/htc` directly, same as the CLI dispatch code. `:` opens the
  palette, which runs anything in the catalog.
- `internal/htclow/`, `internal/htcs/`, `internal/htcmisc/`, `internal/htcfs/` -
  the host side of the devkit link, from the USB transport up. This is what
  `serve` runs.
- `internal/usbdev/` - the raw USB transport `serve` and `usb` open directly
  (WinUSB on Windows, libusb via `gousb` elsewhere).
- `internal/targetlog/` - decoder for the target's log stream.
- `internal/remoteview/`, `internal/remoteaudio/`, `internal/remoteinput/` -
  the live control window: the `ebiten`-based display, audio playback, and
  keyboard/mouse/gamepad forwarding behind `nxdbg video` and `shell watch`.
- `internal/nxdmp/` - reads the target's own `.nxdmp` crash dump format.
- `internal/symbols/` - resolves an address to a symbol using an unstripped
  ELF (a project's own `.nss`), for the gdb stub commands.

## Safety notes (read before running against real hardware)

- `serve`, `usb` and `gdb` open the devkit's USB interface themselves. Don't
  run two of them at once.
- `serve` gives the target read and write access to the host filesystem, which
  is what HTCFS is for. It is confined to `--root` (default: the working
  directory) and `--read-only` turns writing off. Point `--root` somewhere
  deliberate rather than running it from a directory you care about.
- **Device initialization and reset commands are intentionally not
  implemented here at all.** Those are what wipe or reflash a devkit's system
  image, and this project has no use for that capability.
- Test new functionality against a devkit you can watch, one command at a
  time.

## Building

Requires Go 1.25+.

```
go build ./...
go run ./cmd/nxdbg              # no args: TUI
go run ./cmd/nxdbg serve        # any subcommand: CLI
go run ./cmd/nxdbg help         # the command list
```

`nxdbg help` leads with the everyday commands (`video`, `serve`, `gdb`,
`install`, `shell watch`) before anything else. Everything rarer is grouped,
so help stays readable: `logging`, `shell`, `input`, `video`, `htcs`, and
`config`. Run `nxdbg help <group>` for any of them.

In the TUI, `:` opens a filterable list of the same commands. It fills in the
selected target's serial, prompts for the rest of the arguments, and asks
before anything that changes target state.

The TUI reads the target list off the HTCS control port, and if nothing is
serving it at all it starts `nxdbg serve` itself over USB and stops it again
when you quit. So `nxdbg` on its own, with a devkit plugged in, is a complete
setup.

## Testing

```
go test ./...
```

Nothing here touches actual hardware. `internal/htc` has unit tests for the
client's non-trivial logic (port-map parsing, screen-image chunk reassembly,
protocol framing); `cmd/nxdbg` has unit tests for its dispatch tables and
catalog consistency.
