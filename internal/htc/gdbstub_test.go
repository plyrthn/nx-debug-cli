package htc

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestGdbRLEDecodeExpandsRepeatedBytes(t *testing.T) {
	// "0*," means '0' followed by a repeat count of ',' - 29 = 15 more
	// copies, so 16 '0' characters in total - the exact shape a real
	// register dump used for its zeroed FPU registers.
	got := gdbRLEDecode("ab0*,cd")
	want := "ab0000000000000000cd"
	if got != want {
		t.Fatalf("gdbRLEDecode(%q) = %q, want %q", "ab0*,cd", got, want)
	}
}

func TestGdbRLEDecodePassesThroughPlainText(t *testing.T) {
	got := gdbRLEDecode("deadbeef")
	if got != "deadbeef" {
		t.Fatalf("gdbRLEDecode passed plain text through as %q", got)
	}
}

func TestGdbLEHexRoundTrips(t *testing.T) {
	want := uint64(0x123456789abcdef0)
	hexStr := gdbUint64ToLEHex(want, 8)
	got := gdbLEHexToUint64(hexStr)
	if got != want {
		t.Fatalf("round trip got %#x, want %#x (hex %q)", got, want, hexStr)
	}
}

func TestParseStopReplyDecodesExitStatus(t *testing.T) {
	sr, err := parseStopReply("W00")
	if err != nil {
		t.Fatalf("parseStopReply: %v", err)
	}
	if !sr.Exited || sr.ExitCode != 0 {
		t.Fatalf("got %+v, want Exited=true ExitCode=0", sr)
	}
}

func TestParseStopReplyDecodesRegistersAndThread(t *testing.T) {
	// A real reply captured live: signal 5 (SIGTRAP), three inline
	// registers (29=fp, 31=sp, 32=pc in this stub's numbering) and a
	// thread id.
	sr, err := parseStopReply("T051d:d0dc69010f000000;1f:b0dc69010f000000;20:0094c58453000000;thread:290;")
	if err != nil {
		t.Fatalf("parseStopReply: %v", err)
	}
	if sr.Signal != 5 {
		t.Fatalf("signal = %d, want 5", sr.Signal)
	}
	if sr.ThreadID != 0x290 {
		t.Fatalf("thread = %#x, want 0x290", sr.ThreadID)
	}
	pc, ok := sr.Registers[0x20]
	if !ok || pc != 0x5384c59400 {
		t.Fatalf("register 0x20 (pc) = %#x, ok=%v, want 0x5384c59400", pc, ok)
	}
}

func TestParseStopReplyRejectsEmptyAsNotSupported(t *testing.T) {
	_, err := parseStopReply("")
	if err != ErrNotSupported {
		t.Fatalf("got err %v, want ErrNotSupported", err)
	}
}

func TestParseStopReplyDecodesBreakReasons(t *testing.T) {
	// Real formats this stub sends: a hardware instruction breakpoint hit
	// reports hwbreak with no value attached, just a marker, and a software
	// (patched-instruction) one reports swbreak the same way.
	cases := []struct {
		reply string
		want  StopReason
	}{
		{"T05thread:29f;hwbreak:;", StopHardwareBreak},
		{"T05thread:29f;swbreak:;", StopSoftwareBreak},
	}
	for _, tc := range cases {
		sr, err := parseStopReply(tc.reply)
		if err != nil {
			t.Fatalf("parseStopReply(%q): %v", tc.reply, err)
		}
		if sr.Reason != tc.want {
			t.Fatalf("parseStopReply(%q).Reason = %q, want %q", tc.reply, sr.Reason, tc.want)
		}
		if sr.ThreadID != 0x29f {
			t.Fatalf("parseStopReply(%q).ThreadID = %#x, want 0x29f", tc.reply, sr.ThreadID)
		}
	}
}

func TestParseStopReplyDecodesWatchpointAddress(t *testing.T) {
	// The address here is plain hex, not the little-endian byte-pair
	// encoding the register fields use elsewhere in the same packet.
	cases := []struct {
		reply string
		want  StopReason
	}{
		{"T05thread:29f;watch:7fff1234;", StopWatch},
		{"T05thread:29f;rwatch:7fff1234;", StopRWatch},
		{"T05thread:29f;awatch:7fff1234;", StopAWatch},
	}
	for _, tc := range cases {
		sr, err := parseStopReply(tc.reply)
		if err != nil {
			t.Fatalf("parseStopReply(%q): %v", tc.reply, err)
		}
		if sr.Reason != tc.want || sr.WatchAddr != 0x7fff1234 {
			t.Fatalf("parseStopReply(%q) = reason %q addr %#x, want %q 0x7fff1234", tc.reply, sr.Reason, sr.WatchAddr, tc.want)
		}
	}
}

// gdbFake is a minimal stand-in for the target's GDB stub: it acknowledges
// every request with "+" then answers with a scripted reply, the same
// two-phase shape the real stub used live (ack, then the actual packet).
type gdbFake struct {
	conn net.Conn
	r    *bufio.Reader
}

func newTestGdbStub(t *testing.T) (*GdbStub, *gdbFake) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return &GdbStub{conn: client, r: bufio.NewReader(client)}, &gdbFake{conn: server, r: bufio.NewReader(server)}
}

// expectAndReply reads one "$<payload>#cc" request (discarding the
// checksum), then answers with the given reply and reads back the client's
// final ack, matching the client's own roundTrip shape.
//
// This always runs on a goroutine spawned by the test, so it reports
// failures with t.Errorf rather than t.Fatalf: FailNow (which Fatalf calls)
// is only safe to call from the goroutine running the test function itself.
func (f *gdbFake) expectAndReply(t *testing.T, reply string) string {
	t.Helper()
	dollar, err := f.r.ReadByte()
	if err != nil || dollar != '$' {
		t.Errorf("expected '$', got %q, err=%v", dollar, err)
		return ""
	}
	raw, err := f.r.ReadString('#')
	if err != nil {
		t.Errorf("read request body: %v", err)
		return ""
	}
	raw = raw[:len(raw)-1]
	checksum := make([]byte, 2)
	if _, err := f.r.Read(checksum); err != nil {
		t.Errorf("read request checksum: %v", err)
		return ""
	}

	sum := 0
	for i := 0; i < len(reply); i++ {
		sum += int(reply[i])
	}
	if _, err := fmt.Fprintf(f.conn, "+$%s#%02x", reply, sum%256); err != nil {
		t.Errorf("send reply: %v", err)
		return ""
	}
	ack := make([]byte, 1)
	if _, err := f.r.Read(ack); err != nil || ack[0] != '+' {
		t.Errorf("expected client ack, got %q, err=%v", ack, err)
		return ""
	}
	return raw
}

// expect starts one exchange on a goroutine, concurrently with the
// client's blocking round trip, and returns a function that waits for it
// to genuinely finish - including whatever check does. Every call's wait
// must run before the next call to expect on the same fake: expectAndReply
// reads through the fake's single shared bufio.Reader, which is not safe
// for two goroutines to touch at once, so starting a second exchange
// before the first has actually finished races on it. Waiting also has to
// happen before the test function returns regardless - a goroutine still
// running (and possibly still about to call t.Errorf) after that point
// panics rather than failing the test cleanly.
func (f *gdbFake) expect(t *testing.T, reply string, check func(req string)) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := f.expectAndReply(t, reply)
		if check != nil {
			check(req)
		}
	}()
	return func() { <-done }
}

func TestAttachSendsVAttachAndDecodesStop(t *testing.T) {
	g, f := newTestGdbStub(t)
	wait := f.expect(t, "T0520:0094c58453000000;thread:29f;", func(req string) {
		if req != "vAttach;a1" {
			t.Errorf("request = %q, want vAttach;a1", req)
		}
	})
	// Attach does two round trips when the stop reply names a thread: the
	// vAttach itself, then Hg to select that thread for later register and
	// memory ops (see selectGeneralThread). It has to run on its own
	// goroutine so the fake can answer both in order without deadlocking on
	// net.Pipe's unbuffered, synchronous writes.
	done := make(chan struct{})
	var stop StopReply
	var err error
	go func() {
		defer close(done)
		stop, err = g.Attach(0xa1)
	}()
	wait()
	waitHg := f.expect(t, "OK", func(req string) {
		if req != "Hg29f" {
			t.Errorf("request = %q, want Hg29f", req)
		}
	})
	waitHg()
	<-done
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if stop.Signal != 5 || stop.ThreadID != 0x29f {
		t.Fatalf("got %+v", stop)
	}
}

func TestAttachRefusalReportsTheErrorCode(t *testing.T) {
	g, f := newTestGdbStub(t)
	wait := f.expect(t, "E01", nil)
	_, err := g.Attach(1)
	wait()
	if err == nil {
		t.Fatal("expected an error for a refused attach")
	}
}

func TestReadRegistersDecodesCoreAArch64Layout(t *testing.T) {
	g, f := newTestGdbStub(t)
	// x0=1, x1..x30=0, sp=0x1000, pc=0x2000, cpsr=0x60000000.
	body := gdbUint64ToLEHex(1, 8)
	for i := 0; i < 29; i++ {
		body += gdbUint64ToLEHex(0, 8)
	}
	body += gdbUint64ToLEHex(0, 8) // x30
	body += gdbUint64ToLEHex(0x1000, 8)
	body += gdbUint64ToLEHex(0x2000, 8)
	body += gdbUint64ToLEHex(0x60000000, 4)
	wait := f.expect(t, body, nil)

	regs, err := g.ReadRegisters()
	wait()
	if err != nil {
		t.Fatalf("ReadRegisters: %v", err)
	}
	if regs.X[0] != 1 || regs.SP != 0x1000 || regs.PC != 0x2000 || regs.CPSR != 0x60000000 {
		t.Fatalf("got %+v", regs)
	}
}

func TestReadMemorySendsAddrAndLength(t *testing.T) {
	g, f := newTestGdbStub(t)
	wait := f.expect(t, "deadbeef", func(req string) {
		if req != "m1000,4" {
			t.Errorf("request = %q, want m1000,4", req)
		}
	})
	data, err := g.ReadMemory(0x1000, 4)
	wait()
	if err != nil {
		t.Fatalf("ReadMemory: %v", err)
	}
	want := []byte{0xde, 0xad, 0xbe, 0xef}
	if len(data) != 4 || data[0] != want[0] || data[3] != want[3] {
		t.Fatalf("got %x, want %x", data, want)
	}
}

func TestWriteMemorySendsHexEncodedPayload(t *testing.T) {
	g, f := newTestGdbStub(t)
	wait := f.expect(t, "OK", func(req string) {
		if req != "M1000,4:deadbeef" {
			t.Errorf("request = %q, want M1000,4:deadbeef", req)
		}
	})
	err := g.WriteMemory(0x1000, []byte{0xde, 0xad, 0xbe, 0xef})
	wait()
	if err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
}

func TestSetAndClearBreakpointUseZPackets(t *testing.T) {
	g, f := newTestGdbStub(t)
	waitSet := f.expect(t, "OK", func(req string) {
		if req != "Z0,1000,4" {
			t.Errorf("set request = %q, want Z0,1000,4", req)
		}
	})
	err := g.SetBreakpoint(0x1000)
	waitSet()
	if err != nil {
		t.Fatalf("SetBreakpoint: %v", err)
	}

	waitClear := f.expect(t, "OK", func(req string) {
		if req != "z0,1000,4" {
			t.Errorf("clear request = %q, want z0,1000,4", req)
		}
	})
	err = g.ClearBreakpoint(0x1000)
	waitClear()
	if err != nil {
		t.Fatalf("ClearBreakpoint: %v", err)
	}
}

func TestSetAndClearHardwareBreakpointUseZ1Packets(t *testing.T) {
	g, f := newTestGdbStub(t)
	waitSet := f.expect(t, "OK", func(req string) {
		if req != "Z1,1000,4" {
			t.Errorf("set request = %q, want Z1,1000,4", req)
		}
	})
	err := g.SetHardwareBreakpoint(0x1000)
	waitSet()
	if err != nil {
		t.Fatalf("SetHardwareBreakpoint: %v", err)
	}

	waitClear := f.expect(t, "OK", func(req string) {
		if req != "z1,1000,4" {
			t.Errorf("clear request = %q, want z1,1000,4", req)
		}
	})
	err = g.ClearHardwareBreakpoint(0x1000)
	waitClear()
	if err != nil {
		t.Fatalf("ClearHardwareBreakpoint: %v", err)
	}
}

func TestSetHardwareBreakpointReportsNotSupportedOnAnEmptyReply(t *testing.T) {
	g, f := newTestGdbStub(t)
	wait := f.expect(t, "", nil)
	err := g.SetHardwareBreakpoint(0x1000)
	wait()
	if err == nil {
		t.Fatal("expected an error for an empty (unsupported) reply")
	}
}

func TestSetAndClearWatchpointUseZ2Z3Z4Packets(t *testing.T) {
	cases := []struct {
		kind   WatchpointKind
		setPkt string
		clrPkt string
	}{
		{WatchpointWrite, "Z2,2000,8", "z2,2000,8"},
		{WatchpointRead, "Z3,2000,8", "z3,2000,8"},
		{WatchpointAccess, "Z4,2000,8", "z4,2000,8"},
	}
	for _, tc := range cases {
		t.Run(tc.kind.String(), func(t *testing.T) {
			g, f := newTestGdbStub(t)
			waitSet := f.expect(t, "OK", func(req string) {
				if req != tc.setPkt {
					t.Errorf("set request = %q, want %q", req, tc.setPkt)
				}
			})
			if err := g.SetWatchpoint(0x2000, 8, tc.kind); err != nil {
				t.Fatalf("SetWatchpoint: %v", err)
			}
			waitSet()

			waitClear := f.expect(t, "OK", func(req string) {
				if req != tc.clrPkt {
					t.Errorf("clear request = %q, want %q", req, tc.clrPkt)
				}
			})
			if err := g.ClearWatchpoint(0x2000, 8, tc.kind); err != nil {
				t.Fatalf("ClearWatchpoint: %v", err)
			}
			waitClear()
		})
	}
}

func TestSetWatchpointReportsNotSupportedOnAnEmptyReply(t *testing.T) {
	g, f := newTestGdbStub(t)
	wait := f.expect(t, "", nil)
	err := g.SetWatchpoint(0x2000, 8, WatchpointWrite)
	wait()
	if err == nil {
		t.Fatal("expected an error for an empty (unsupported) reply")
	}
}

// TestSetWatchpointRejectsBadRangesWithoutARoundTrip pins the same
// alignment rules the server enforces before it will even try to program a
// debug register. The server gates Z2/Z3/Z4 on this exact check and, on
// failure, answers with the same empty reply a genuinely unsupported
// watchpoint gets - so these have to be rejected locally, with no packet
// sent at all, or a bad range would misleadingly look like this stub has no
// watchpoint support.
func TestSetWatchpointRejectsBadRangesWithoutARoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		addr   uint64
		length uint64
	}{
		{"zero length", 0x1000, 0},
		{"short range crosses a double-word", 0x1005, 8},
		{"long length not a power of two", 0x1000, 24},
		{"long range misaligned to its length", 0x1004, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newTestGdbStub(t)
			// No f.expect set up - a call that tries a real round trip
			// would block on net.Pipe's unbuffered write and time out the
			// test, which is exactly the failure this guards against.
			if err := g.SetWatchpoint(tc.addr, tc.length, WatchpointWrite); err == nil {
				t.Fatalf("SetWatchpoint(%#x, %#x): expected a validation error", tc.addr, tc.length)
			}
		})
	}
}

// TestSetWatchpointAcceptsValidLongRange pins the other half of
// validateWatchpointRange: a power-of-two length over 8 bytes with an
// aligned address is legal and must still reach the wire.
func TestSetWatchpointAcceptsValidLongRange(t *testing.T) {
	g, f := newTestGdbStub(t)
	waitSet := f.expect(t, "OK", func(req string) {
		if req != "Z2,4000,10" {
			t.Errorf("request = %q, want Z2,4000,10", req)
		}
	})
	if err := g.SetWatchpoint(0x4000, 0x10, WatchpointWrite); err != nil {
		t.Fatalf("SetWatchpoint: %v", err)
	}
	waitSet()
}

func TestWriteRegisterEncodesPPacketByWidth(t *testing.T) {
	g, f := newTestGdbStub(t)
	// x3 is a plain 8-byte GPR: little-endian hex of 0x12345678.
	wait := f.expect(t, "OK", func(req string) {
		if req != "P3=7856341200000000" {
			t.Errorf("request = %q, want P3=7856341200000000", req)
		}
	})
	err := g.WriteRegister(3, 0x12345678)
	wait()
	if err != nil {
		t.Fatalf("WriteRegister: %v", err)
	}

	// cpsr (33) is 4 bytes wide.
	waitCpsr := f.expect(t, "OK", func(req string) {
		if req != "P21=00000060" {
			t.Errorf("request = %q, want P21=00000060", req)
		}
	})
	err = g.WriteRegister(gdbRegCPSR, 0x60000000)
	waitCpsr()
	if err != nil {
		t.Fatalf("WriteRegister(cpsr): %v", err)
	}
}

func TestLegacyStepSelectsThreadViaHcThenSendsBareS(t *testing.T) {
	g, f := newTestGdbStub(t)
	g.lastThread = 0x29f

	waitHc := f.expect(t, "OK", func(req string) {
		if req != "Hc29f" {
			t.Errorf("request = %q, want Hc29f", req)
		}
	})
	done := make(chan struct{})
	var sr StopReply
	var err error
	go func() {
		defer close(done)
		sr, err = g.StepLegacy()
	}()
	waitHc()
	waitS := f.expect(t, "T0520:0094c58453000000;thread:29f;", func(req string) {
		if req != "s" {
			t.Errorf("request = %q, want bare s", req)
		}
	})
	waitS()
	// The stop reply names a thread, so legacyResume also re-selects it via
	// Hg for any following register/memory ops - same as Attach/vContResume.
	waitHg := f.expect(t, "OK", func(req string) {
		if req != "Hg29f" {
			t.Errorf("request = %q, want Hg29f", req)
		}
	})
	waitHg()
	<-done
	if err != nil {
		t.Fatalf("StepLegacy: %v", err)
	}
	if sr.Signal != 5 || sr.ThreadID != 0x29f {
		t.Fatalf("got %+v", sr)
	}
}

// TestLegacyResumeSendsBareActionEvenWhenHcIsRefused pins a real finding
// from live hardware: this project's own devkit refuses "Hc" outright
// (E01), and the fix for that (legacyResume falling through to the resume
// packet anyway) has to keep working, or a stub with no Hc support at all
// would make StepLegacy/ContinueLegacy permanently fail before ever
// actually trying to resume.
func TestLegacyResumeSendsBareActionEvenWhenHcIsRefused(t *testing.T) {
	g, f := newTestGdbStub(t)
	g.lastThread = 0x29f

	waitHc := f.expect(t, "E01", func(req string) {
		if req != "Hc29f" {
			t.Errorf("request = %q, want Hc29f", req)
		}
	})
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = g.StepLegacy()
	}()
	waitHc()
	waitS := f.expect(t, "", func(req string) {
		if req != "s" {
			t.Errorf("request = %q, want bare s", req)
		}
	})
	waitS()
	<-done
	if err != ErrNotSupported {
		t.Fatalf("StepLegacy after Hc refusal: got %v, want ErrNotSupported", err)
	}
	if !g.hcUnsupported {
		t.Error("hcUnsupported not set after an E-code Hc refusal")
	}

	// A second call on the same connection should skip Hc entirely now.
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		_, err = g.StepLegacy()
	}()
	waitS2 := f.expect(t, "", func(req string) {
		if req != "s" {
			t.Errorf("second request = %q, want bare s with no Hc first", req)
		}
	})
	waitS2()
	<-done2
	if err != ErrNotSupported {
		t.Fatalf("second StepLegacy: got %v, want ErrNotSupported", err)
	}
}

func TestStepReportsNotSupportedOnAnEmptyReply(t *testing.T) {
	g, f := newTestGdbStub(t)
	wait := f.expect(t, "", nil)
	_, err := g.Step()
	wait()
	if err != ErrNotSupported {
		t.Fatalf("got err %v, want ErrNotSupported", err)
	}
}

func TestDetachSendsDPacket(t *testing.T) {
	g, f := newTestGdbStub(t)
	wait := f.expect(t, "OK", func(req string) {
		if req != "D" {
			t.Errorf("request = %q, want D", req)
		}
	})
	err := g.Detach()
	wait()
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
}

func TestThreadIDsParsesTheFirstReplyAndConfirmsNoMoreFollow(t *testing.T) {
	g, f := newTestGdbStub(t)
	waitF := f.expect(t, "m1,29f,3e8", func(req string) {
		if req != "qfThreadInfo" {
			t.Errorf("request = %q, want qfThreadInfo", req)
		}
	})
	done := make(chan struct{})
	var ids []uint64
	var err error
	go func() {
		defer close(done)
		ids, err = g.ThreadIDs()
	}()
	waitF()
	// This stub always answers qfThreadInfo with the whole list, but a
	// correct client still asks qsThreadInfo to confirm "l" (no more)
	// rather than assuming - see ThreadIDs' own doc comment.
	waitS := f.expect(t, "l", func(req string) {
		if req != "qsThreadInfo" {
			t.Errorf("request = %q, want qsThreadInfo", req)
		}
	})
	waitS()
	<-done
	if err != nil {
		t.Fatalf("ThreadIDs: %v", err)
	}
	want := []uint64{1, 0x29f, 0x3e8}
	if len(ids) != len(want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got %v, want %v", ids, want)
		}
	}
}

func TestThreadIDsHandlesAnEmptyList(t *testing.T) {
	g, f := newTestGdbStub(t)
	wait := f.expect(t, "l", func(req string) {
		if req != "qfThreadInfo" {
			t.Errorf("request = %q, want qfThreadInfo", req)
		}
	})
	ids, err := g.ThreadIDs()
	wait()
	if err != nil {
		t.Fatalf("ThreadIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("got %v, want empty", ids)
	}
}

func TestReadReplyTimesOutRatherThanHanging(t *testing.T) {
	g, _ := newTestGdbStub(t)
	start := time.Now()
	_, err := g.readReply(100 * time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v, want well under 1s", elapsed)
	}
}
