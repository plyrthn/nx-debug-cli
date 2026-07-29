package htc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"net"
	"testing"
	"time"
)

// csFake is a stand-in target: it reads one command off the wire and answers
// with whatever the test says. Every reply carries the command's own id back,
// because that is what the client matches on.
type csFake struct {
	conn net.Conn
}

func newTestShell(t *testing.T) (*CommandShell, *csFake) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	s := &CommandShell{
		conn:    client,
		waiting: make(map[int64]chan csReply),
		done:    make(chan struct{}),
		Events:  make(chan CsEvent, 16),
	}
	go s.readLoop()
	return s, &csFake{conn: server}
}

// read pulls one command, returning its id, number and body.
func (f *csFake) read(t *testing.T) (int64, int32, []byte) {
	t.Helper()
	f.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	head := make([]byte, csHeaderSize)
	if _, err := io.ReadFull(f.conn, head); err != nil {
		t.Fatalf("read header: %v", err)
	}
	id := int64(binary.LittleEndian.Uint64(head[0:]))
	cmd := int32(binary.LittleEndian.Uint32(head[8:]))
	body := make([]byte, binary.LittleEndian.Uint32(head[12:]))
	if len(body) > 0 {
		if _, err := io.ReadFull(f.conn, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
	}
	return id, cmd, body
}

func (f *csFake) write(t *testing.T, id int64, kind int32, body []byte) {
	t.Helper()
	msg := make([]byte, csHeaderSize+len(body))
	binary.LittleEndian.PutUint64(msg[0:], uint64(id))
	binary.LittleEndian.PutUint32(msg[8:], uint32(kind))
	binary.LittleEndian.PutUint32(msg[12:], uint32(len(body)))
	copy(msg[csHeaderSize:], body)
	f.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := f.conn.Write(msg); err != nil {
		t.Fatalf("write reply: %v", err)
	}
}

// answer reads one command and replies to it, which is the shape almost every
// test here needs.
func (f *csFake) answer(t *testing.T, kind int32, body []byte) (int32, []byte) {
	t.Helper()
	id, cmd, req := f.read(t)
	f.write(t, id, kind, body)
	return cmd, req
}

func TestCallSendsAHeaderAndMatchesTheReply(t *testing.T) {
	s, fake := newTestShell(t)

	var gotCmd int32
	var gotBody []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		gotCmd, gotBody = fake.answer(t, csRespSuccess, nil)
	}()

	if _, err := s.call(context.Background(), csTerminateApplication, []byte("hi")); err != nil {
		t.Fatalf("call: %v", err)
	}
	<-done
	if gotCmd != csTerminateApplication {
		t.Errorf("command = %d, want %d", gotCmd, csTerminateApplication)
	}
	if string(gotBody) != "hi" {
		t.Errorf("body = %q, want %q", gotBody, "hi")
	}
}

// The target answers whichever command it finishes first, so a reply that
// arrives out of order has to reach the caller that asked for it.
func TestRepliesAreMatchedById(t *testing.T) {
	s, fake := newTestShell(t)

	first := make(chan string, 1)
	second := make(chan string, 1)

	go func() {
		r, err := s.call(context.Background(), csGetTitleName, []byte("first"))
		if err != nil {
			t.Errorf("first call: %v", err)
			first <- ""
			return
		}
		first <- string(r.body)
	}()
	id1, _, _ := fake.read(t)

	go func() {
		r, err := s.call(context.Background(), csGetTitleName, []byte("second"))
		if err != nil {
			t.Errorf("second call: %v", err)
			second <- ""
			return
		}
		second <- string(r.body)
	}()
	id2, _, _ := fake.read(t)

	if id1 == id2 {
		t.Fatalf("both commands used id %d", id1)
	}

	fake.write(t, id2, csRespTitleName, []byte("two"))
	fake.write(t, id1, csRespTitleName, []byte("one"))

	if got := <-first; got != "one" {
		t.Errorf("first reply = %q, want %q", got, "one")
	}
	if got := <-second; got != "two" {
		t.Errorf("second reply = %q, want %q", got, "two")
	}
}

func TestErrorResponseBecomesACsError(t *testing.T) {
	s, fake := newTestShell(t)

	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, 57550)
	go fake.answer(t, csRespError, body)

	_, err := s.call(context.Background(), csTakeScreenShot, nil)
	var cse *CsError
	if !errors.As(err, &cse) {
		t.Fatalf("error = %v, want a *CsError", err)
	}
	if cse.Result != 57550 {
		t.Errorf("result = %d, want 57550", cse.Result)
	}
	if cse.Command != csTakeScreenShot {
		t.Errorf("command = %d, want %d", cse.Command, csTakeScreenShot)
	}
}

// A result is a packed module and description, and the 2XXX-YYYY form is the
// only one that can be looked up.
func TestCsErrorCodeDecodesModuleAndDescription(t *testing.T) {
	cases := []struct {
		result      uint32
		module      uint32
		description uint32
		text        string
	}{
		{57550, 206, 112, "no screenshot target (2206-0112, 0xe0ce)"},
		{3600, 16, 7, "application not running (2016-0007, 0xe10)"},
		{716, 204, 1, "the command shell rejected the request (2204-0001, 0x2cc)"},
		{0x410, 16, 2, "2016-0002, 0x410"},
	}
	for _, c := range cases {
		e := &CsError{Result: c.result}
		if got := e.Module(); got != c.module {
			t.Errorf("%d: module = %d, want %d", c.result, got, c.module)
		}
		if got := e.Description(); got != c.description {
			t.Errorf("%d: description = %d, want %d", c.result, got, c.description)
		}
		if got := e.Error(); !contains(got, c.text) {
			t.Errorf("%d: error = %q, want it to contain %q", c.result, got, c.text)
		}
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

func TestScreenshotDecodesPixels(t *testing.T) {
	s, fake := newTestShell(t)

	const w, h = 2, 2
	body := make([]byte, 12+w*h*4)
	binary.LittleEndian.PutUint32(body[0:], uint32(w*h*4))
	binary.LittleEndian.PutUint32(body[4:], w)
	binary.LittleEndian.PutUint32(body[8:], h)
	for i := 0; i < w*h; i++ {
		body[12+i*4+0] = byte(i + 1)
		body[12+i*4+1] = byte(i + 2)
		body[12+i*4+2] = byte(i + 3)
		// Alpha arrives as zero and must not be trusted.
		body[12+i*4+3] = 0
	}
	go fake.answer(t, csRespScreenShot, body)

	img, err := s.Screenshot(context.Background())
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if got := img.Bounds(); got != image.Rect(0, 0, w, h) {
		t.Fatalf("bounds = %v, want %v", got, image.Rect(0, 0, w, h))
	}
	rgba, ok := img.(*image.RGBA)
	if !ok {
		t.Fatalf("image is %T, want *image.RGBA", img)
	}
	for i := 0; i < w*h; i++ {
		if rgba.Pix[i*4+0] != byte(i+1) || rgba.Pix[i*4+1] != byte(i+2) || rgba.Pix[i*4+2] != byte(i+3) {
			t.Errorf("pixel %d = % x, want % x", i, rgba.Pix[i*4:i*4+3], []byte{byte(i + 1), byte(i + 2), byte(i + 3)})
		}
		if rgba.Pix[i*4+3] != 0xff {
			t.Errorf("pixel %d alpha = %d, want 255", i, rgba.Pix[i*4+3])
		}
	}
}

func TestScreenshotRejectsAShortBody(t *testing.T) {
	s, fake := newTestShell(t)

	body := make([]byte, 12+8) // claims 4x4 but carries two pixels
	binary.LittleEndian.PutUint32(body[4:], 4)
	binary.LittleEndian.PutUint32(body[8:], 4)
	go fake.answer(t, csRespScreenShot, body)

	if _, err := s.Screenshot(context.Background()); err == nil {
		t.Fatal("short screenshot body was accepted")
	}
}

func TestFirmwareFieldOffsets(t *testing.T) {
	s, fake := newTestShell(t)

	body := make([]byte, 8+32+64+24+128)
	body[0], body[1], body[2] = 18, 1, 0
	body[4], body[5] = 3, 4
	copy(body[8:], "NX")
	copy(body[40:], "rev-abc")
	copy(body[104:], "18.1.0")
	copy(body[128:], "SDEV")
	go fake.answer(t, csRespFirmwareVersion, body)

	fw, err := s.Firmware(context.Background())
	if err != nil {
		t.Fatalf("firmware: %v", err)
	}
	want := CsFirmware{
		Version:        "18.1.0",
		Platform:       "NX",
		Revision:       "rev-abc",
		DisplayVersion: "18.1.0",
		DisplayName:    "SDEV",
		MajorRelstep:   3,
		MinorRelstep:   4,
	}
	if fw != want {
		t.Errorf("firmware = %+v, want %+v", fw, want)
	}
}

func TestFirmwareRejectsAShortBody(t *testing.T) {
	s, fake := newTestShell(t)
	go fake.answer(t, csRespFirmwareVersion, make([]byte, 16))

	if _, err := s.Firmware(context.Background()); err == nil {
		t.Fatal("short firmware body was accepted")
	}
}

// GetApplicationInformation's reply is two little-endian uint64s: the
// process index and the real Horizon process id, not a program id - the
// opcode's name is misleading (see CsApplication's doc comment).
func TestApplicationDecodesProcessIndexAndID(t *testing.T) {
	s, fake := newTestShell(t)

	body := make([]byte, 16)
	binary.LittleEndian.PutUint64(body[0:], 3)
	binary.LittleEndian.PutUint64(body[8:], 154)
	go fake.answer(t, csRespApplicationInformation, body)

	app, err := s.Application(context.Background())
	if err != nil {
		t.Fatalf("application: %v", err)
	}
	want := CsApplication{ProcessIndex: 3, ProcessID: 154}
	if app != want {
		t.Errorf("application = %+v, want %+v", app, want)
	}
}

func TestApplicationRejectsAShortBody(t *testing.T) {
	s, fake := newTestShell(t)
	go fake.answer(t, csRespApplicationInformation, make([]byte, 8))

	if _, err := s.Application(context.Background()); err == nil {
		t.Fatal("short application body was accepted")
	}
}

// The program id is in the header, but the target also parses it out of the
// argument string, and a system program carries a "system:" prefix on it.
func TestLaunchArgumentsCarryTheProgramSpec(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		want   string
	}{
		{"application", "", "0x0100000000002101 arg"},
		{"system program", "system:", "system:0x0100000000002101 arg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := csArguments(c.prefix, DevMenuCommandProgramID, "arg")
			if got != c.want {
				t.Errorf("arguments = %q, want %q", got, c.want)
			}
		})
	}
}

func TestLaunchSystemProgramBody(t *testing.T) {
	s, fake := newTestShell(t)

	type result struct {
		cmd  int32
		body []byte
	}
	got := make(chan result, 1)
	go func() {
		cmd, body := fake.answer(t, csRespSuccess, nil)
		got <- result{cmd, body}
	}()

	if _, err := s.DevMenuCommand(context.Background(), "run"); err != nil {
		t.Fatalf("devmenu: %v", err)
	}
	r := <-got
	if r.cmd != csLaunchInstalledSystemProgram {
		t.Errorf("command = %d, want %d", r.cmd, csLaunchInstalledSystemProgram)
	}
	if len(r.body) < 16 {
		t.Fatalf("body is %d bytes, want at least 16", len(r.body))
	}
	if id := binary.LittleEndian.Uint64(r.body); id != DevMenuCommandProgramID {
		t.Errorf("program id = 0x%x, want 0x%x", id, DevMenuCommandProgramID)
	}
	args := string(r.body[16:])
	if n := binary.LittleEndian.Uint32(r.body[8:]); int(n) != len(args) {
		t.Errorf("argument size = %d, want %d", n, len(args))
	}
	if want := "system:0x0100000000002101 run"; args != want {
		t.Errorf("arguments = %q, want %q", args, want)
	}
}

func TestLaunchApplicationBody(t *testing.T) {
	s, fake := newTestShell(t)

	got := make(chan []byte, 1)
	go func() {
		_, body := fake.answer(t, csRespSuccess, nil)
		got <- body
	}()

	if _, err := s.LaunchApplication(context.Background(), 0x0100000000002065, "a", "env", 7); err != nil {
		t.Fatalf("launch: %v", err)
	}
	body := <-got
	if len(body) < 24 {
		t.Fatalf("body is %d bytes, want at least 24", len(body))
	}
	argSize := int(binary.LittleEndian.Uint32(body[8:]))
	flags := binary.LittleEndian.Uint32(body[12:])
	envSize := int(binary.LittleEndian.Uint32(body[16:]))
	if flags != 7 {
		t.Errorf("flags = %d, want 7", flags)
	}
	if want := "0x0100000000002065 a"; string(body[24:24+argSize]) != want {
		t.Errorf("arguments = %q, want %q", body[24:24+argSize], want)
	}
	if got := string(body[24+argSize : 24+argSize+envSize]); got != "env" {
		t.Errorf("env = %q, want %q", got, "env")
	}
}

// A lifecycle notification arrives unprompted and must not be handed to
// whatever command happens to be waiting.
func TestUnsolicitedEventsGoToEvents(t *testing.T) {
	s, fake := newTestShell(t)

	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, 42)
	fake.write(t, 999, csRespProgramExited, body)

	select {
	case e := <-s.Events:
		if e.Kind != "exited" || e.ProcessIndex != 42 {
			t.Errorf("event = %+v, want exited/42", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived")
	}
}

func TestCallFailsOnceTheConnectionDrops(t *testing.T) {
	s, fake := newTestShell(t)
	fake.conn.Close()

	// The read loop has to notice the close before a call can report it.
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("read loop did not stop")
	}
	if _, err := s.call(context.Background(), csGetFirmwareVersion, nil); err == nil {
		t.Fatal("call succeeded on a closed connection")
	}
}

func TestCallRespectsContextCancellation(t *testing.T) {
	s, fake := newTestShell(t)
	// Take the command off the pipe but never answer it. This deliberately
	// avoids the t-aware reader: a failure reported from here would be on a
	// goroutine the test does not wait for.
	go io.ReadFull(fake.conn, make([]byte, csHeaderSize))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := s.call(ctx, csGetFirmwareVersion, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want %v", err, context.DeadlineExceeded)
	}

	// The waiter must be gone, or an unanswered command leaks one per call.
	s.mu.Lock()
	n := len(s.waiting)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d waiters left behind", n)
	}
}

func TestOversizedBodyStopsTheReadLoop(t *testing.T) {
	s, fake := newTestShell(t)

	head := make([]byte, csHeaderSize)
	binary.LittleEndian.PutUint32(head[8:], uint32(csRespSuccess))
	binary.LittleEndian.PutUint32(head[12:], csMaxBody+1)
	fake.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	fake.conn.Write(head)

	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("read loop accepted an oversized body")
	}
}
