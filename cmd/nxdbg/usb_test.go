package main

import (
	"errors"
	"testing"

	"github.com/plyrthn/nx-debug-cli/internal/htclow"
)

// fakePipe records every transfer separately, which is the whole point: the
// boundaries are what's under test, not the concatenated bytes.
type fakePipe struct {
	writes [][]byte
	err    error
	failOn int // index of the write that fails, -1 for none
}

func (f *fakePipe) Write(pipe uint8, b []byte) (int, error) {
	if f.failOn == len(f.writes) {
		return 0, f.err
	}
	f.writes = append(f.writes, append([]byte(nil), b...))
	return len(b), nil
}

func newFakePipe() *fakePipe { return &fakePipe{failOn: -1} }

// A packet with a body goes out as two transfers. Combining them overruns the
// target's 32-byte header read and stalls the endpoint, which costs a trip to
// the hardware to clear - so this is pinned rather than left to review.
func TestWritePacketSplitsHeaderFromBody(t *testing.T) {
	pkt, err := htclow.CtrlPacket(htclow.ReadyFromHost, 1, htclow.ReadyFromHostBody(htclow.ServiceChannels))
	if err != nil {
		t.Fatal(err)
	}
	pipe := newFakePipe()
	if err := usbWritePacket(pipe, pkt); err != nil {
		t.Fatal(err)
	}
	if len(pipe.writes) != 2 {
		t.Fatalf("%d transfers, want the header and the body separately", len(pipe.writes))
	}
	if len(pipe.writes[0]) != htclow.HeaderSize {
		t.Errorf("first transfer is %d bytes, want %d", len(pipe.writes[0]), htclow.HeaderSize)
	}
	if got, want := len(pipe.writes[1]), len(pkt)-htclow.HeaderSize; got != want {
		t.Errorf("second transfer is %d bytes, want %d", got, want)
	}
	if string(pipe.writes[1]) != string(pkt[htclow.HeaderSize:]) {
		t.Error("body transfer does not match the packet body")
	}
}

// A bare header is one transfer, not one plus an empty one - a zero-length
// bulk write is a protocol event of its own.
func TestWritePacketWithNoBodyIsOneTransfer(t *testing.T) {
	pkt, err := htclow.CtrlPacket(htclow.ConnectFromHost, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	pipe := newFakePipe()
	if err := usbWritePacket(pipe, pkt); err != nil {
		t.Fatal(err)
	}
	if len(pipe.writes) != 1 {
		t.Fatalf("%d transfers, want 1", len(pipe.writes))
	}
	if len(pipe.writes[0]) != htclow.HeaderSize {
		t.Errorf("transfer is %d bytes, want %d", len(pipe.writes[0]), htclow.HeaderSize)
	}
}

// A failed header write must not be followed by a body write: that would put
// an orphan body on a pipe the target is reading as a header.
func TestWritePacketStopsAfterAFailedHeader(t *testing.T) {
	pkt, _ := htclow.CtrlPacket(htclow.ReadyFromHost, 1, []byte("body"))
	pipe := &fakePipe{failOn: 0, err: errors.New("stalled")}
	if err := usbWritePacket(pipe, pkt); err == nil {
		t.Fatal("failed header reported success")
	}
	if len(pipe.writes) != 0 {
		t.Errorf("%d transfers after a failed header, want none", len(pipe.writes))
	}
}

func TestWritePacketRejectsShortBuffers(t *testing.T) {
	pipe := newFakePipe()
	if err := usbWritePacket(pipe, make([]byte, htclow.HeaderSize-1)); err == nil {
		t.Fatal("a buffer shorter than a header was accepted")
	}
	if len(pipe.writes) != 0 {
		t.Error("a short buffer still reached the pipe")
	}
}
