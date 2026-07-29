package htc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"sync"
	"time"
)

// The command shell is the target's own control service: launching and
// terminating programs, screenshots, reboot, firmware. The official tools reach
// it through the daemon, but it is an ordinary HTCS port and nothing about it
// needs one, so everything here works against `nxdbg serve` as well.
//
// It is worth knowing this is a different route to the same outcomes as several
// daemon RPCs. A screenshot taken here is the target compositing and handing
// back a finished image, with no video stream and no decoder in the path.

// CsPortName is the well-known HTCS port the command shell listens on.
const CsPortName = "iywys@$cs"

// Command numbers. The values are the target's, so they are fixed and the gaps
// (17, 18) are real.
const (
	csLaunchProgramFromHost        int32 = 1
	csTerminateProcesses           int32 = 2
	csGetFirmwareVersion           int32 = 3
	csReboot                       int32 = 4
	csSetSafeMode                  int32 = 5
	csRegisterTenvDefinitionFile   int32 = 6
	csTerminateApplication         int32 = 7
	csShutdown                     int32 = 8
	csSubscribeProcessEvent        int32 = 9
	csGetTitleName                 int32 = 10
	csControlVirtualTemperature    int32 = 11
	csLaunchInstalledApplication   int32 = 12
	csLaunchGameCardApplication    int32 = 13
	csLaunchInstalledSystemProgram int32 = 14
	csTakeScreenShot               int32 = 15
	csTakeForegroundScreenShot     int32 = 16
	csDumpRunningApplication       int32 = 19
	csGetProgramID                 int32 = 20
	csGetApplicationInformation    int32 = 21
	csControlVirtualBatteryLevel   int32 = 22
)

// Response numbers.
const (
	csRespSuccess                int32 = 1
	csRespError                  int32 = 2
	csRespProgramExited          int32 = 3
	csRespFirmwareVersion        int32 = 4
	csRespJitDebug               int32 = 5
	csRespProgramLaunched        int32 = 6
	csRespTitleName              int32 = 7
	csRespScreenShot             int32 = 8
	csRespProgramID              int32 = 9
	csRespApplicationInformation int32 = 10
)

// csHeaderSize is the fixed header on every message in both directions:
// an 8-byte id, a 4-byte command or response number, and a 4-byte body length.
const csHeaderSize = 16

// csMaxBody caps a declared body length. A 1080p screenshot is about 8MB, so
// this leaves room for one while still refusing a length that can only be a
// desynchronised stream.
const csMaxBody = 64 << 20

// CsError is a failure reported by the target rather than by the transport.
type CsError struct {
	Command int32
	Result  uint32
}

func (e *CsError) Error() string {
	if name, ok := csResultNames[e.Result]; ok {
		return fmt.Sprintf("htc: command shell: %s (%s)", name, e.Code())
	}
	return fmt.Sprintf("htc: command shell failed with %s", e.Code())
}

// Code renders the result the way the target itself reports errors, as
// 2<module>-<description>.
//
// A result is not an opaque number: it packs a module in its low 9 bits and a
// description above them. Printing only the hex leaves the one form that can
// actually be looked up unavailable, and it is also what says which subsystem
// refused - a launch that fails in fs is a different problem from one that
// fails in ns.
func (e *CsError) Code() string {
	return fmt.Sprintf("2%03d-%04d, 0x%x", e.Module(), e.Description(), e.Result)
}

// Module is the subsystem that produced the result.
func (e *CsError) Module() uint32 { return e.Result & 0x1ff }

// Description is the module's own error number.
func (e *CsError) Description() uint32 { return (e.Result >> 9) & 0x1fff }

// csResultNames covers the results identified so far. An unlisted code is
// reported as a 2<module>-<description> pair rather than guessed at.
//
// Module 204 is the command shell's own, so a result there is the shell
// refusing the request rather than a subsystem failing to carry it out.
var csResultNames = map[uint32]string{
	2321922: "application verification failed",
	3303938: "unsupported NCA key generation",
	2569:    "invalid NSO",
	4617:    "invalid program id",
	3600:    "application not running",
	1764:    "application not running (pgl)",
	35856:   "application content not found",
	40976:   "main application not found",
	76816:   "content meta not found (ns)",
	87056:   "system update required",
	317968:  "application update required (required version)",
	319504:  "application update required (compacted application)",
	465936:  "application is running",
	506896:  "unexpected application version",
	527376:  "application resource not available",
	1049616: "required add-on content set not satisfied",
	52381:   "application not registered",
	4300:    "not a launchable content meta type",
	57550:   "no screenshot target",
	3300:    "content meta not found (pgl)",
	716:     "the command shell rejected the request",
	1228:    "process not found",
}

// csReply is one response off the wire.
type csReply struct {
	kind int32
	body []byte
}

// CommandShell is a connection to the target's command shell.
//
// Replies are matched to commands by the id in the header, because the target
// is free to interleave unsolicited events (a program exiting, a JIT debug
// notification) with the answer to whatever was asked. Reading the socket
// inline after a write would treat the first of those as the answer.
type CommandShell struct {
	conn net.Conn

	// Serial is the target this connection was resolved for. Set by
	// DialCommandShell; empty when opened directly through
	// DialCommandShellAddr, which has no serial to record.
	Serial string

	mu      sync.Mutex
	nextID  int64
	waiting map[int64]chan csReply

	closeOnce sync.Once
	done      chan struct{}
	readErr   error

	// Events is closed when the shell shuts down. Unsolicited program
	// lifecycle notifications are delivered here, and dropped if nobody is
	// reading, so a caller that does not care costs nothing.
	Events chan CsEvent
}

// CsEvent is an unsolicited notification from the target.
type CsEvent struct {
	Kind         string
	ProcessIndex uint64
}

// DialCommandShell resolves the target's command shell and opens it.
func DialCommandShell(ctx context.Context, serial string) (*CommandShell, error) {
	addr, err := resolvePortAddr(ctx, serial, CsPortName)
	if err != nil {
		return nil, err
	}
	s, err := DialCommandShellAddr(ctx, addr)
	if err != nil {
		return nil, err
	}
	s.Serial = serial
	return s, nil
}

// DialCommandShellAddr opens an already-resolved command shell address.
func DialCommandShellAddr(ctx context.Context, addr string) (*CommandShell, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("htc: dial command shell %s: %w", addr, err)
	}
	s := &CommandShell{
		conn:    conn,
		waiting: make(map[int64]chan csReply),
		done:    make(chan struct{}),
		Events:  make(chan CsEvent, 16),
	}
	go s.readLoop()
	return s, nil
}

func (s *CommandShell) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	return s.conn.Close()
}

// readLoop demultiplexes responses onto whoever is waiting for them.
func (s *CommandShell) readLoop() {
	defer close(s.Events)
	for {
		head := make([]byte, csHeaderSize)
		if _, err := io.ReadFull(s.conn, head); err != nil {
			s.fail(err)
			return
		}
		id := int64(binary.LittleEndian.Uint64(head[0:]))
		kind := int32(binary.LittleEndian.Uint32(head[8:]))
		size := binary.LittleEndian.Uint32(head[12:])
		if size > csMaxBody {
			s.fail(fmt.Errorf("htc: command shell declared a %d byte body, over the %d cap", size, csMaxBody))
			return
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(s.conn, body); err != nil {
			s.fail(err)
			return
		}

		// Lifecycle notifications carry the id of the subscription, not of a
		// pending command, so they are delivered separately rather than being
		// matched and dropped.
		switch kind {
		case csRespProgramExited, csRespProgramLaunched, csRespJitDebug:
			if s.deliverEvent(kind, body) {
				continue
			}
		}

		s.mu.Lock()
		ch, ok := s.waiting[id]
		if ok {
			delete(s.waiting, id)
		}
		s.mu.Unlock()
		if ok {
			ch <- csReply{kind: kind, body: body}
		}
	}
}

// deliverEvent reports a lifecycle notification, and says whether it was one.
// A full channel drops the event rather than stalling the read loop, since
// blocking here would wedge every command on the connection.
func (s *CommandShell) deliverEvent(kind int32, body []byte) bool {
	e := CsEvent{}
	switch kind {
	case csRespProgramExited:
		e.Kind = "exited"
	case csRespProgramLaunched:
		e.Kind = "launched"
	case csRespJitDebug:
		e.Kind = "jit-debug"
	default:
		return false
	}
	if len(body) >= 8 {
		e.ProcessIndex = binary.LittleEndian.Uint64(body)
	}
	select {
	case s.Events <- e:
	default:
	}
	// A launch or exit is also what a caller waiting on that command wants, so
	// it is not consumed here unless nothing is waiting for it.
	return kind == csRespJitDebug
}

func (s *CommandShell) fail(err error) {
	s.mu.Lock()
	if s.readErr == nil {
		s.readErr = err
	}
	for id, ch := range s.waiting {
		close(ch)
		delete(s.waiting, id)
	}
	s.mu.Unlock()
	s.closeOnce.Do(func() { close(s.done) })
}

// call sends a command and waits for its reply.
func (s *CommandShell) call(ctx context.Context, cmd int32, body []byte) (csReply, error) {
	s.mu.Lock()
	if s.readErr != nil {
		err := s.readErr
		s.mu.Unlock()
		return csReply{}, err
	}
	id := s.nextID
	s.nextID++
	ch := make(chan csReply, 1)
	s.waiting[id] = ch
	s.mu.Unlock()

	msg := make([]byte, csHeaderSize+len(body))
	binary.LittleEndian.PutUint64(msg[0:], uint64(id))
	binary.LittleEndian.PutUint32(msg[8:], uint32(cmd))
	binary.LittleEndian.PutUint32(msg[12:], uint32(len(body)))
	copy(msg[csHeaderSize:], body)

	if deadline, ok := ctx.Deadline(); ok {
		s.conn.SetWriteDeadline(deadline)
		defer s.conn.SetWriteDeadline(time.Time{})
	}
	if _, err := s.conn.Write(msg); err != nil {
		s.mu.Lock()
		delete(s.waiting, id)
		s.mu.Unlock()
		return csReply{}, fmt.Errorf("htc: command shell send: %w", err)
	}

	select {
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.waiting, id)
		s.mu.Unlock()
		return csReply{}, ctx.Err()
	case r, ok := <-ch:
		if !ok {
			s.mu.Lock()
			err := s.readErr
			s.mu.Unlock()
			if err == nil {
				err = errors.New("htc: command shell closed")
			}
			return csReply{}, err
		}
		if r.kind == csRespError {
			var result uint32
			if len(r.body) >= 4 {
				result = binary.LittleEndian.Uint32(r.body)
			}
			return csReply{}, &CsError{Command: cmd, Result: result}
		}
		return r, nil
	}
}

// Screenshot asks the target to capture the screen and hand back the pixels.
//
// This is a different mechanism to the video stream: the target composites and
// returns a finished image, so it needs no decoder and no parameter sets. It is
// what makes screenshots work with no daemon.
func (s *CommandShell) Screenshot(ctx context.Context) (image.Image, error) {
	return s.screenshot(ctx, csTakeScreenShot)
}

// ForegroundScreenshot captures only the foreground application, leaving out
// any system overlay.
func (s *CommandShell) ForegroundScreenshot(ctx context.Context) (image.Image, error) {
	return s.screenshot(ctx, csTakeForegroundScreenShot)
}

func (s *CommandShell) screenshot(ctx context.Context, cmd int32) (image.Image, error) {
	r, err := s.call(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	if r.kind != csRespScreenShot {
		return nil, fmt.Errorf("htc: expected a screenshot, got response %d", r.kind)
	}
	if len(r.body) < 12 {
		return nil, fmt.Errorf("htc: screenshot header is %d bytes, need 12", len(r.body))
	}
	width := int(int32(binary.LittleEndian.Uint32(r.body[4:])))
	height := int(int32(binary.LittleEndian.Uint32(r.body[8:])))
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("htc: screenshot reports %dx%d", width, height)
	}
	pixels := r.body[12:]
	if want := width * height * 4; len(pixels) < want {
		return nil, fmt.Errorf("htc: screenshot is %d bytes, need %d for %dx%d", len(pixels), want, width, height)
	}

	// The wire carries RGBA with an alpha byte that isn't meaningful; the
	// official client forces it opaque and so does this. Copying rather than
	// aliasing keeps the returned image independent of the reply buffer.
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < width*height; i++ {
		img.Pix[i*4+0] = pixels[i*4+0]
		img.Pix[i*4+1] = pixels[i*4+1]
		img.Pix[i*4+2] = pixels[i*4+2]
		img.Pix[i*4+3] = 0xff
	}
	return img, nil
}

// CsFirmware is what the target reports about its own system version.
type CsFirmware struct {
	Version        string
	Platform       string
	Revision       string
	DisplayVersion string
	DisplayName    string
	MajorRelstep   uint8
	MinorRelstep   uint8
}

// Firmware reads the target's firmware version over the command shell.
func (s *CommandShell) Firmware(ctx context.Context) (CsFirmware, error) {
	r, err := s.call(ctx, csGetFirmwareVersion, nil)
	if err != nil {
		return CsFirmware{}, err
	}
	if r.kind != csRespFirmwareVersion {
		return CsFirmware{}, fmt.Errorf("htc: expected a firmware version, got response %d", r.kind)
	}
	// Fixed-width fields, in order: version bytes and relsteps, then four
	// null-padded strings of 32, 64, 24 and 128 bytes.
	const want = 8 + 32 + 64 + 24 + 128
	if len(r.body) < want {
		return CsFirmware{}, fmt.Errorf("htc: firmware body is %d bytes, need %d", len(r.body), want)
	}
	return CsFirmware{
		Version:        fmt.Sprintf("%d.%d.%d", r.body[0], r.body[1], r.body[2]),
		MajorRelstep:   r.body[4],
		MinorRelstep:   r.body[5],
		Platform:       cstring(r.body[8:40]),
		Revision:       cstring(r.body[40:104]),
		DisplayVersion: cstring(r.body[104:128]),
		DisplayName:    cstring(r.body[128:256]),
	}, nil
}

// TitleName reads the display title of a running process.
func (s *CommandShell) TitleName(ctx context.Context, processIndex uint64) (string, error) {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, processIndex)
	r, err := s.call(ctx, csGetTitleName, body)
	if err != nil {
		return "", err
	}
	if r.kind != csRespTitleName {
		return "", fmt.Errorf("htc: expected a title name, got response %d", r.kind)
	}
	if len(r.body) < 4 {
		return "", fmt.Errorf("htc: title body is %d bytes, need at least 4", len(r.body))
	}
	// The leading length is redundant with the body size and the official
	// client ignores it, so the rest of the body is the name.
	return cstring(r.body[4:]), nil
}

// ProgramID reads the program id of a running process.
func (s *CommandShell) ProgramID(ctx context.Context, processIndex uint64) (uint64, error) {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, processIndex)
	r, err := s.call(ctx, csGetProgramID, body)
	if err != nil {
		return 0, err
	}
	if r.kind != csRespProgramID || len(r.body) < 8 {
		return 0, fmt.Errorf("htc: expected a program id, got response %d with %d bytes", r.kind, len(r.body))
	}
	return binary.LittleEndian.Uint64(r.body), nil
}

// CsApplication is what the target reports about the running application: its
// CS-level process slot and its real Horizon kernel process id. Despite the
// opcode's name (GetApplicationInformation), this is not the program id - for
// that, pass ProcessIndex to ProgramID.
type CsApplication struct {
	ProcessIndex uint64
	ProcessID    uint64
}

// Application reads the running application's process index and process id.
func (s *CommandShell) Application(ctx context.Context) (CsApplication, error) {
	r, err := s.call(ctx, csGetApplicationInformation, nil)
	if err != nil {
		return CsApplication{}, err
	}
	if r.kind != csRespApplicationInformation || len(r.body) < 16 {
		return CsApplication{}, fmt.Errorf("htc: expected application information, got response %d with %d bytes", r.kind, len(r.body))
	}
	return CsApplication{
		ProcessIndex: binary.LittleEndian.Uint64(r.body[0:]),
		ProcessID:    binary.LittleEndian.Uint64(r.body[8:]),
	}, nil
}

// csArguments builds the argument string a launch command carries.
//
// The program id is in the header already, but the target also expects it as
// the first token of the arguments, and system programs carry a "system:"
// prefix on it. This is not decoration: the target parses that token, so an
// argument string without it is rejected before the command is even considered.
func csArguments(prefix string, programID uint64, arguments string) string {
	return fmt.Sprintf("%s0x%016x %s", prefix, programID, arguments)
}

// LaunchApplication starts an installed application by program id. flags and
// the argument and environment strings match the daemon's launch RPC.
func (s *CommandShell) LaunchApplication(ctx context.Context, programID uint64, arguments, envPath string, flags uint32) (uint64, error) {
	args := []byte(csArguments("", programID, arguments))
	env := []byte(envPath)
	body := make([]byte, 24+len(args)+len(env))
	binary.LittleEndian.PutUint64(body[0:], programID)
	binary.LittleEndian.PutUint32(body[8:], uint32(len(args)))
	binary.LittleEndian.PutUint32(body[12:], flags)
	binary.LittleEndian.PutUint32(body[16:], uint32(len(env)))
	copy(body[24:], args)
	copy(body[24+len(args):], env)

	r, err := s.call(ctx, csLaunchInstalledApplication, body)
	if err != nil {
		return 0, err
	}
	if r.kind == csRespProgramLaunched && len(r.body) >= 8 {
		return binary.LittleEndian.Uint64(r.body), nil
	}
	return 0, nil
}

// DevMenuCommandProgramID is the system program that runs DevMenu commands.
// Launching it with an argument string is how the official tools drive the
// target's own menu, which is the same mechanism behind the daemon's clock
// sync.
//
// Not to be confused with 0x0100000000002065, which is the DevMenu
// application - the on-screen menu, not this.
const DevMenuCommandProgramID uint64 = 0x0100000000002101

// LaunchSystemProgram starts an installed system program by id.
func (s *CommandShell) LaunchSystemProgram(ctx context.Context, programID uint64, arguments string) (uint64, error) {
	args := []byte(csArguments("system:", programID, arguments))
	body := make([]byte, 16+len(args))
	binary.LittleEndian.PutUint64(body[0:], programID)
	binary.LittleEndian.PutUint32(body[8:], uint32(len(args)))
	copy(body[16:], args)

	r, err := s.call(ctx, csLaunchInstalledSystemProgram, body)
	if err != nil {
		return 0, err
	}
	if r.kind == csRespProgramLaunched && len(r.body) >= 8 {
		return binary.LittleEndian.Uint64(r.body), nil
	}
	return 0, nil
}

// DevMenuCommand runs one of the target's own DevMenu commands.
func (s *CommandShell) DevMenuCommand(ctx context.Context, arguments string) (uint64, error) {
	return s.LaunchSystemProgram(ctx, DevMenuCommandProgramID, arguments)
}

// TerminateApplication stops the running application.
func (s *CommandShell) TerminateApplication(ctx context.Context) error {
	_, err := s.call(ctx, csTerminateApplication, nil)
	return err
}

// TerminateProcesses stops every process, not just the foreground one.
func (s *CommandShell) TerminateProcesses(ctx context.Context) error {
	_, err := s.call(ctx, csTerminateProcesses, nil)
	return err
}

// Reboot restarts the target.
func (s *CommandShell) Reboot(ctx context.Context) error {
	_, err := s.call(ctx, csReboot, nil)
	return err
}

// Shutdown powers the target off.
func (s *CommandShell) Shutdown(ctx context.Context) error {
	_, err := s.call(ctx, csShutdown, nil)
	return err
}

// SubscribeProcessEvents asks the target to report program launches, exits and
// JIT debug notifications on Events until unsubscribed.
func (s *CommandShell) SubscribeProcessEvents(ctx context.Context, on bool) error {
	body := make([]byte, 4)
	if on {
		body[0] = 1
	}
	_, err := s.call(ctx, csSubscribeProcessEvent, body)
	return err
}

// cstring reads a null-padded fixed-width string.
func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
