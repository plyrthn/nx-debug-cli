package htclow

import (
	"strings"
	"testing"
)

// What the host sends must be what the host can read back, or the two halves
// of the handshake have drifted apart without anything noticing.
func TestReadyRoundTrip(t *testing.T) {
	got, err := ParseReady(ReadyFromHostBody(ServiceChannels))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != int(MuxVersion) {
		t.Errorf("version = %d, want %d", got.Version, MuxVersion)
	}
	if len(got.Channels) != len(ServiceChannels) {
		t.Fatalf("%d channels, want %d", len(got.Channels), len(ServiceChannels))
	}
	for i, c := range got.Channels {
		if c != ServiceChannels[i] {
			t.Errorf("channel %d = %s, want %s", i, c, ServiceChannels[i])
		}
	}
}

// The body arrives out of a read buffer larger than the packet, so the tail is
// padding.
func TestParseReadyIgnoresTrailingNULs(t *testing.T) {
	body := append(ReadyFromHostBody(ServiceChannels), make([]byte, 64)...)
	if _, err := ParseReady(body); err != nil {
		t.Fatal(err)
	}
}

func TestReadySupports(t *testing.T) {
	r := Ready{Channels: []Channel{{Module: 3, ID: 1}}}
	if !r.Supports(Channel{Module: 3, ID: 1}) {
		t.Error("a listed channel reported unsupported")
	}
	// Same numbers in the other field must not match: the two halves of a
	// channel are not interchangeable.
	if r.Supports(Channel{Module: 1, ID: 3}) {
		t.Error("module and id were treated as interchangeable")
	}
	if r.Supports(Channel{Module: 4, ID: 0}) {
		t.Error("an unlisted channel reported supported")
	}
}

// A target that lists nothing is declining, which is a different thing from
// one speaking an old protocol, so it must not be reported as a version error.
func TestParseReadyEmptyChannelList(t *testing.T) {
	r, err := ParseReady([]byte("{\r\n  \"Chan\" : [\r\n],\r\n  \"Prot\" : 0\r\n}\r\n"))
	if err != nil {
		t.Fatalf("empty list rejected: %v", err)
	}
	if len(r.Channels) != 0 {
		t.Errorf("%d channels, want none", len(r.Channels))
	}
}

func TestParseReadyRejectsOldVersion(t *testing.T) {
	_, err := ParseReady([]byte(`{"Chan":["1:0:0"],"Prot":4}`))
	if err == nil {
		t.Fatal("mux version 4 accepted")
	}
	if !strings.Contains(err.Error(), "4") {
		t.Errorf("error %q does not name the version", err)
	}
}

func TestParseChannel(t *testing.T) {
	c, err := ParseChannel("3:0:258")
	if err != nil {
		t.Fatal(err)
	}
	if c.Module != 3 || c.ID != 258 {
		t.Errorf("= %+v, want module 3 id 258", c)
	}
}

// Each of these is a shape the parser must refuse rather than coerce into a
// plausible-looking channel.
func TestParseChannelRejectsJunk(t *testing.T) {
	for _, s := range []string{
		"",
		"3",
		"3:0",
		"3:0:1:2",
		"3:1:1",   // reserved field must be zero
		"256:0:1", // module is a byte
		"3:0:65536",
		"-1:0:1",
		"x:0:1",
		"3:0:x",
	} {
		if c, err := ParseChannel(s); err == nil {
			t.Errorf("%q parsed as %s", s, c)
		}
	}
}

func TestParseReadyRejectsJunkBody(t *testing.T) {
	if _, err := ParseReady([]byte("not json")); err == nil {
		t.Error("junk body accepted")
	}
}
