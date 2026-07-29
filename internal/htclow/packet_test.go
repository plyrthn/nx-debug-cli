package htclow

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

// The header the devkit actually accepted, captured off the wire. If this
// ever changes shape the target stops answering, so it's pinned byte for
// byte rather than checked field by field.
func TestConnectFromHostMatchesTheWire(t *testing.T) {
	got, err := CtrlPacket(ConnectFromHost, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString(
		"37568278" + // signature 0x78825637, little endian
			"01000000" + // sequence 1
			"00000000" + // reserved
			"00000000" + // body size 0
			"0100" + // version 1
			"1000" + // packet type 16, ConnectFromHost
			"00000000" +
			"00000000" +
			"00000000")
	if !bytes.Equal(got, want) {
		t.Errorf("got  % x\nwant % x", got, want)
	}
	if len(got) != HeaderSize {
		t.Errorf("header is %d bytes, want %d", len(got), HeaderSize)
	}
}

func TestCtrlPacketBody(t *testing.T) {
	body := []byte("hello")
	pkt, err := CtrlPacket(ReadyFromHost, 0x11223344, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkt) != HeaderSize+len(body) {
		t.Fatalf("packet is %d bytes, want %d", len(pkt), HeaderSize+len(body))
	}
	h, err := ParseHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Ctrl() {
		t.Error("round trip lost the ctrl signature")
	}
	if h.Mux() {
		t.Error("a ctrl packet must not also read as mux")
	}
	if h.Word1 != 0x11223344 {
		t.Errorf("sequence = %#x, want 0x11223344", h.Word1)
	}
	if h.BodySize != uint32(len(body)) {
		t.Errorf("body size = %d, want %d", h.BodySize, len(body))
	}
	if CtrlType(h.Type) != ReadyFromHost {
		t.Errorf("type = %s, want %s", CtrlType(h.Type), ReadyFromHost)
	}
	if !bytes.Equal(pkt[HeaderSize:], body) {
		t.Errorf("body = %q, want %q", pkt[HeaderSize:], body)
	}
}

func TestMuxPacketFields(t *testing.T) {
	body := []byte("payload")
	pkt, err := MuxPacket(MuxData, Channel{Module: 3, ID: 7}, 0xdeadbeef, 42, body)
	if err != nil {
		t.Fatal(err)
	}
	h, err := ParseHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Mux() || h.Ctrl() {
		t.Fatal("signature did not round trip as mux")
	}
	if h.ChannelID != 7 {
		t.Errorf("channel = %d, want 7", h.ChannelID)
	}
	if h.ModuleID != 3 {
		t.Errorf("module = %d, want 3", h.ModuleID)
	}
	if h.Word1 != 0xdeadbeef {
		t.Errorf("offset = %#x, want 0xdeadbeef", h.Word1)
	}
	if h.Share != 42 {
		t.Errorf("share = %d, want 42", h.Share)
	}
	if h.Version != MuxVersion {
		t.Errorf("version = %d, want %d", h.Version, MuxVersion)
	}
}

// The two families must never be confused for each other: a mux packet read
// as ctrl would decode its stream offset as a sequence number.
func TestSignaturesAreDistinct(t *testing.T) {
	if CtrlSignature == MuxSignature {
		t.Fatal("the two signatures are equal")
	}
	ctrl, _ := CtrlPacket(ConnectFromHost, 1, nil)
	mux, _ := MuxPacket(MuxData, Channel{}, 0, 0, nil)

	ch, _ := ParseHeader(ctrl)
	mh, _ := ParseHeader(mux)
	if !ch.Ctrl() || ch.Mux() {
		t.Error("ctrl packet misidentified")
	}
	if !mh.Mux() || mh.Ctrl() {
		t.Error("mux packet misidentified")
	}
}

// An unrecognised packet must say so rather than being reported as some
// known type, which is the difference between "the devkit sent something
// unexpected" and a silently wrong decode.
func TestUnknownSignatureIsNotGuessed(t *testing.T) {
	buf := make([]byte, HeaderSize)
	buf[0] = 0xff
	h, err := ParseHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if h.Ctrl() || h.Mux() {
		t.Fatal("a junk signature was claimed by a family")
	}
	if !strings.Contains(h.String(), "unrecognised") {
		t.Errorf("String() = %q, want it to say the packet is unrecognised", h)
	}
	if !strings.Contains(h.TypeName(), "unknown family") {
		t.Errorf("TypeName() = %q, want it to disclaim the family", h.TypeName())
	}
}

func TestParseHeaderRejectsShortBuffers(t *testing.T) {
	for _, n := range []int{0, 1, HeaderSize - 1} {
		if _, err := ParseHeader(make([]byte, n)); err == nil {
			t.Errorf("%d bytes parsed as a header", n)
		}
	}
}

func TestBodySizeLimits(t *testing.T) {
	if _, err := CtrlPacket(ReadyFromHost, 0, make([]byte, CtrlMaxBodySize+1)); err == nil {
		t.Error("oversized ctrl body accepted")
	}
	if _, err := CtrlPacket(ReadyFromHost, 0, make([]byte, CtrlMaxBodySize)); err != nil {
		t.Errorf("body at the limit rejected: %v", err)
	}
	if _, err := MuxPacket(MuxData, Channel{}, 0, 0, make([]byte, MuxMaxBodySize+1)); err == nil {
		t.Error("oversized mux body accepted")
	}
}

// The target parses this, so the punctuation and the CRLFs are contract, not
// formatting preference.
func TestReadyFromHostBody(t *testing.T) {
	empty := string(ReadyFromHostBody(nil))
	if want := "{\r\n  \"Chan\" : [\r\n],\r\n  \"Prot\" : 5\r\n}\r\n"; empty != want {
		t.Errorf("empty body =\n%q\nwant\n%q", empty, want)
	}

	// Two channels, pinned in full: the comma goes before the newline of the
	// following entry, not after the preceding one, and the last entry has
	// no trailing comma.
	two := string(ReadyFromHostBody([]Channel{{Module: 1, ID: 2}, {Module: 3, ID: 4}}))
	want := "{\r\n  \"Chan\" : [\r\n \"1:0:2\",\r\n \"3:0:4\"\r\n],\r\n  \"Prot\" : 5\r\n}\r\n"
	if two != want {
		t.Errorf("body =\n%q\nwant\n%q", two, want)
	}
}

func TestTypeNames(t *testing.T) {
	if got := ConnectFromHost.String(); got != "ConnectFromHost" {
		t.Errorf("= %q", got)
	}
	if got := MuxData.String(); got != "Data" {
		t.Errorf("= %q", got)
	}
	// An unnamed value reports its number instead of a neighbouring name.
	if got := CtrlType(999).String(); !strings.Contains(got, "999") {
		t.Errorf("= %q, want the number", got)
	}
	if got := MuxType(999).String(); !strings.Contains(got, "999") {
		t.Errorf("= %q, want the number", got)
	}
}

// Share is 64 bits at offset 24 and ChannelId 16 bits at offset 20. Both
// were originally written too narrow, which corrupts a stream offset or a
// flow-control window without any error anywhere, so the widths are pinned.
func TestMuxFieldWidths(t *testing.T) {
	pkt, err := MuxPacket(MuxData, Channel{Module: 0xAB, ID: 0x1234}, 0, 0x1122334455667788, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(pkt[24:]); got != 0x1122334455667788 {
		t.Errorf("share on the wire = %#x, want the full 64 bits", got)
	}
	if got := binary.LittleEndian.Uint16(pkt[20:]); got != 0x1234 {
		t.Errorf("channel on the wire = %#x, want 0x1234", got)
	}
	if pkt[23] != 0xAB {
		t.Errorf("module on the wire = %#x, want 0xab", pkt[23])
	}
	h, err := ParseHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if h.Share != 0x1122334455667788 {
		t.Errorf("parsed share = %#x", h.Share)
	}
	if h.ChannelID != 0x1234 {
		t.Errorf("parsed channel = %#x", h.ChannelID)
	}
}

// MaxData is a bare header: its whole meaning is the window in Share, so a
// body would be wrong and the counter is unused.
func TestMaxDataPacket(t *testing.T) {
	pkt, err := MaxDataPacket(Channel{Module: 3, ID: 1}, 57344)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkt) != HeaderSize {
		t.Errorf("MaxData is %d bytes, want a bare %d byte header", len(pkt), HeaderSize)
	}
	h, _ := ParseHeader(pkt)
	if MuxType(h.Type) != MuxMaxData {
		t.Errorf("type = %s, want MaxData", MuxType(h.Type))
	}
	if h.Share != 57344 {
		t.Errorf("window = %d, want 57344", h.Share)
	}
	if h.Word1 != 0 {
		t.Errorf("counter = %d, want 0", h.Word1)
	}
}

// The counter and the share must not be swapped: they sit in different
// fields and mean entirely different things.
func TestDataPacketKeepsCounterAndShareApart(t *testing.T) {
	pkt, err := DataPacket(Channel{Module: 4, ID: 0}, 7, 9000, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := ParseHeader(pkt)
	if h.Word1 != 7 {
		t.Errorf("counter = %d, want 7", h.Word1)
	}
	if h.Share != 9000 {
		t.Errorf("share = %d, want 9000", h.Share)
	}
}

// The four service channels are what the daemon advertises. If this list
// drifts the target sees channels the host never opened, which is what took
// the link down once already.
func TestServiceChannels(t *testing.T) {
	want := []string{"1:0:0", "3:0:1", "3:0:2", "4:0:0"}
	if len(ServiceChannels) != len(want) {
		t.Fatalf("%d channels, want %d", len(ServiceChannels), len(want))
	}
	for i, c := range ServiceChannels {
		if c.String() != want[i] {
			t.Errorf("channel %d = %s, want %s", i, c, want[i])
		}
	}
}
