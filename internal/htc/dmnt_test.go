package htc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"testing"
	"time"
)

// dmntFake is a stand-in target for the debug monitor protocol.
type dmntFake struct {
	conn net.Conn
}

func newTestDebugMonitor(t *testing.T) (*DebugMonitor, *dmntFake) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	fake := &dmntFake{conn: server}

	done := make(chan error, 1)
	var m *DebugMonitor
	go func() {
		m = &DebugMonitor{conn: client, r: bufio.NewReader(client)}
		done <- m.handshake()
	}()

	fake.serveHandshake(t)

	if err := <-done; err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return m, fake
}

// serveHandshake plays the target's half of the connection sequence: read
// the 32-byte greeting, read the 4-byte follow-up, then send back a banner
// echoing the greeting's own token.
func (f *dmntFake) serveHandshake(t *testing.T) {
	t.Helper()
	f.conn.SetDeadline(time.Now().Add(2 * time.Second))

	greeting := make([]byte, 32)
	if _, err := io.ReadFull(f.conn, greeting); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	followUp := make([]byte, 4)
	if _, err := io.ReadFull(f.conn, followUp); err != nil {
		t.Fatalf("read follow-up: %v", err)
	}

	text := []byte("Spec: NX\nTMA: 0.0.0.0\nTMIPC: 4\nConn: Gen2\nHW: Unknown\nBCID: 96\nPS: 0\nPMS: 0\nCD: 0\n\x00")
	banner := make([]byte, 32+len(text))
	copy(banner[0:4], greeting[0:4])
	binary.LittleEndian.PutUint32(banner[12:16], uint32(len(text)))
	copy(banner[32:], text)
	if _, err := f.conn.Write(banner); err != nil {
		t.Fatalf("write banner: %v", err)
	}
}

func (f *dmntFake) readOpcodeRequest(t *testing.T, size int) []byte {
	t.Helper()
	f.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	req := make([]byte, size)
	if _, err := io.ReadFull(f.conn, req); err != nil {
		t.Fatalf("read request: %v", err)
	}
	return req
}

func (f *dmntFake) writeTokenFrame(t *testing.T, token [4]byte, msgType uint16, body []byte) {
	t.Helper()
	head := make([]byte, 16)
	copy(head[0:4], token[:])
	binary.LittleEndian.PutUint16(head[6:8], 1)
	binary.LittleEndian.PutUint16(head[8:10], msgType)
	binary.LittleEndian.PutUint32(head[12:16], uint32(len(body)))
	f.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := f.conn.Write(head); err != nil {
		t.Fatalf("write token frame header: %v", err)
	}
	if _, err := f.conn.Write(body); err != nil {
		t.Fatalf("write token frame body: %v", err)
	}
}

func TestHandshakeParsesTheBanner(t *testing.T) {
	m, _ := newTestDebugMonitor(t)
	want := DmntBanner{
		Spec: "NX", TMA: "0.0.0.0", TMIPC: "4", Conn: "Gen2", HW: "Unknown",
		BCID: "96", PS: "0", PMS: "0", CD: "0",
	}
	if m.Banner != want {
		t.Fatalf("banner = %+v, want %+v", m.Banner, want)
	}
}

func TestHandshakeRejectsAMismatchedToken(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	m := &DebugMonitor{conn: client, r: bufio.NewReader(client)}
	done := make(chan error, 1)
	go func() { done <- m.handshake() }()

	server.SetDeadline(time.Now().Add(2 * time.Second))
	greeting := make([]byte, 32)
	io.ReadFull(server, greeting)
	followUp := make([]byte, 4)
	io.ReadFull(server, followUp)

	text := []byte("x\x00")
	banner := make([]byte, 32+len(text))
	banner[0] = greeting[0] ^ 0xff // deliberately wrong token
	binary.LittleEndian.PutUint32(banner[12:16], uint32(len(text)))
	copy(banner[32:], text)
	server.Write(banner)

	if err := <-done; err == nil {
		t.Fatal("expected an error for a mismatched token, got nil")
	}
}

func TestReadMemoryReturnsTheRequestedBytes(t *testing.T) {
	m, f := newTestDebugMonitor(t)

	want := bytes.Repeat([]byte{0xAB}, 1024)
	done := make(chan struct{})
	var got []byte
	var callErr error
	go func() {
		got, callErr = m.ReadMemory(0xa1, 0x5a6a2c00, 1024)
		close(done)
	}()

	req := f.readOpcodeRequest(t, 28)
	opcode := binary.LittleEndian.Uint32(req[0:4])
	handle := binary.LittleEndian.Uint32(req[4:8])
	addr := binary.LittleEndian.Uint64(req[12:20])
	count := binary.LittleEndian.Uint32(req[20:24])
	if opcode != 7 || handle != 0xa1 || addr != 0x5a6a2c00 || count != 1024 {
		t.Fatalf("request = opcode=%d handle=%#x addr=%#x count=%d", opcode, handle, addr, count)
	}

	// The response carries a short undecoded sub-header before the actual
	// data, matching what a live capture showed (declared length = 16 +
	// requested count).
	subHeader := make([]byte, 16)
	body := append(subHeader, want...)
	f.writeTokenFrame(t, [4]byte{0xdd, 0xde, 0x66, 0x36}, 0x26, body)

	<-done
	if callErr != nil {
		t.Fatalf("ReadMemory: %v", callErr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadMemory returned %d bytes not matching what was sent", len(got))
	}
}

func TestSelectTargetDrainsTheCoalescedNotification(t *testing.T) {
	m, f := newTestDebugMonitor(t)

	done := make(chan error, 1)
	go func() { done <- m.SelectTarget(0xa1) }()

	req := f.readOpcodeRequest(t, 12)
	opcode := binary.LittleEndian.Uint32(req[0:4])
	handle := binary.LittleEndian.Uint32(req[4:8])
	if opcode != 3 || handle != 0xa1 {
		t.Fatalf("request = opcode=%d handle=%#x", opcode, handle)
	}

	// Acknowledgement, then the coalesced opcode-14 notification observed on
	// a live target, written as one Write so they land in the same read.
	ack := make([]byte, 16)
	head := make([]byte, 16)
	token := [4]byte{0xdd, 0xde, 0x66, 0x36}
	copy(head[0:4], token[:])
	binary.LittleEndian.PutUint16(head[6:8], 1)
	binary.LittleEndian.PutUint16(head[8:10], 0x26)
	binary.LittleEndian.PutUint32(head[12:16], uint32(len(ack)))

	notification := make([]byte, 16)
	binary.LittleEndian.PutUint32(notification[0:4], 14)
	binary.LittleEndian.PutUint32(notification[4:8], 0xa1)

	msg := append(append(head, ack...), notification...)
	f.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := f.conn.Write(msg); err != nil {
		t.Fatalf("write ack+notification: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}

	// Nothing should be left over for the next read to trip on.
	if buffered := m.r.Buffered(); buffered != 0 {
		t.Fatalf("%d bytes left unread after SelectTarget, next call would desync", buffered)
	}
}

func TestSelectTargetWithoutACoalescedNotificationDoesNotHang(t *testing.T) {
	m, f := newTestDebugMonitor(t)

	done := make(chan error, 1)
	go func() { done <- m.SelectTarget(0xa1) }()

	f.readOpcodeRequest(t, 12)
	f.writeTokenFrame(t, [4]byte{0xdd, 0xde, 0x66, 0x36}, 0x26, make([]byte, 16))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SelectTarget: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SelectTarget did not return; likely blocked waiting for a notification that never came")
	}
}

func TestModuleAtDecodesARealModuleRecord(t *testing.T) {
	m, f := newTestDebugMonitor(t)

	done := make(chan struct{})
	var mod DmntModule
	var callErr error
	go func() {
		mod, callErr = m.ModuleAt(0xa1, 0)
		close(done)
	}()

	req := f.readOpcodeRequest(t, 16)
	opcode := binary.LittleEndian.Uint32(req[0:4])
	handle := binary.LittleEndian.Uint32(req[4:8])
	index := binary.LittleEndian.Uint32(req[12:16])
	if opcode != 17 || handle != 0xa1 || index != 0 {
		t.Fatalf("request = opcode=%d handle=%#x index=%d", opcode, handle, index)
	}

	// A real capture of RRProto's own main binary, byte for byte.
	body, err := hex.DecodeString("000000000000000000000000000000000a000000a100000000000000000000000060a0420000000000a0001200000000026e98faa70f5ae028b74a4a1da784af00000000000000000000000000000000443a5c503452525c4d616e676f5c50726f746f5c5545345c525250726f746f5c42696e61726965735c537769")
	if err != nil {
		t.Fatal(err)
	}
	f.writeTokenFrame(t, [4]byte{0xdd, 0xde, 0x66, 0x36}, 0x26, body)

	<-done
	if callErr != nil {
		t.Fatalf("ModuleAt: %v", callErr)
	}
	if mod.Base != 0x42a06000 {
		t.Errorf("Base = %#x, want 0x42a06000", mod.Base)
	}
	if mod.Size != 0x1200a000 {
		t.Errorf("Size = %#x, want 0x1200a000", mod.Size)
	}
	wantBuildID := "026e98faa70f5ae028b74a4a1da784af00000000"
	if hex.EncodeToString(mod.BuildID[:20]) != wantBuildID {
		t.Errorf("BuildID head = %x, want %s", mod.BuildID[:20], wantBuildID)
	}
	wantPath := `D:\P4RR\Mango\Proto\UE4\RRProto\Binaries\Swi`
	if mod.Path != wantPath {
		t.Errorf("Path = %q, want %q", mod.Path, wantPath)
	}
}

func TestThreadAtDecodesARealThreadName(t *testing.T) {
	m, f := newTestDebugMonitor(t)

	done := make(chan struct{})
	var name string
	var callErr error
	go func() {
		name, callErr = m.ThreadAt(0xa1, 1)
		close(done)
	}()

	req := f.readOpcodeRequest(t, 16)
	opcode := binary.LittleEndian.Uint32(req[0:4])
	if opcode != 21 {
		t.Fatalf("request opcode = %d, want 21", opcode)
	}

	// A real capture of RRProto's GameThread, byte for byte.
	body, err := hex.DecodeString("000000000000000000000000000000000c000000a1000000000000000000000013050000000000000000000000000000000000005700000001000000ff00000010000000200000000200000047616d6554687265616400000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	f.writeTokenFrame(t, [4]byte{0xdd, 0xde, 0x66, 0x36}, 0x26, body)

	<-done
	if callErr != nil {
		t.Fatalf("ThreadAt: %v", callErr)
	}
	if name != "GameThread" {
		t.Errorf("name = %q, want %q", name, "GameThread")
	}
}

func TestRecordAtRejectsAMismatchedRecordOpcode(t *testing.T) {
	m, f := newTestDebugMonitor(t)

	done := make(chan struct{})
	var callErr error
	go func() {
		_, callErr = m.ModuleAt(0xa1, 0)
		close(done)
	}()

	f.readOpcodeRequest(t, 16)
	// A thread record (opcode 12) where a module record (opcode 10) was
	// expected.
	body := make([]byte, 32)
	binary.LittleEndian.PutUint32(body[16:20], 12)
	f.writeTokenFrame(t, [4]byte{0xdd, 0xde, 0x66, 0x36}, 0x26, body)

	<-done
	if callErr == nil {
		t.Fatal("expected an error for a mismatched record opcode, got nil")
	}
}
