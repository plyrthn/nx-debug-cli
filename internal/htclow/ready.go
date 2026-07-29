package htclow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Ready is what each side declares to finish the handshake: the service
// channels it supports and the mux version it speaks.
type Ready struct {
	Channels []Channel
	Version  int
}

// Supports reports whether the peer listed a channel. The peer's list is
// authoritative - a channel the host asked for and the target left out is
// unsupported, and opening it anyway is a protocol error rather than a
// channel that quietly does nothing.
func (r Ready) Supports(c Channel) bool {
	for _, have := range r.Channels {
		if have == c {
			return true
		}
	}
	return false
}

// ParseReady decodes a ready body. The body is JSON with CRLF line endings,
// which encoding/json treats as ordinary whitespace, so the awkward hand-built
// form on the sending side doesn't need a hand-built parser to match.
func ParseReady(body []byte) (Ready, error) {
	var raw struct {
		Chan []string
		Prot int
	}
	// The read buffer is bigger than the packet, so anything past the body is
	// padding rather than content.
	if err := json.Unmarshal(bytes.TrimRight(body, "\x00"), &raw); err != nil {
		return Ready{}, fmt.Errorf("htclow: parsing ready body: %w", err)
	}
	r := Ready{Version: raw.Prot}
	for _, s := range raw.Chan {
		c, err := ParseChannel(s)
		if err != nil {
			return Ready{}, err
		}
		r.Channels = append(r.Channels, c)
	}
	// Only a listed channel set carries a version worth checking; an empty
	// list means the peer is declining, not speaking an older protocol.
	if len(r.Channels) > 0 && r.Version < int(MuxVersion) {
		return r, fmt.Errorf("htclow: target speaks mux version %d, this needs %d", r.Version, MuxVersion)
	}
	return r, nil
}

// parseTargetInfo decodes the identity JSON in the connect reply. The body is
// NUL-padded out of a read buffer, so the tail is trimmed before parsing.
func parseTargetInfo(body []byte, out *TargetInfo) error {
	trimmed := bytes.TrimRight(body, "\x00")
	if len(trimmed) == 0 {
		return fmt.Errorf("htclow: target sent no identity")
	}
	if err := json.Unmarshal(trimmed, out); err != nil {
		return fmt.Errorf("htclow: parsing target identity: %w", err)
	}
	return nil
}

// ParseChannel decodes the "module:0:id" form the ready body uses. The middle
// field is reserved: a non-zero one means a layout this code doesn't
// understand, so it's reported rather than dropped.
func ParseChannel(s string) (Channel, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return Channel{}, fmt.Errorf("htclow: %q is not a channel", s)
	}
	module, err := strconv.ParseUint(parts[0], 10, 8)
	if err != nil {
		return Channel{}, fmt.Errorf("htclow: channel %q has a bad module: %w", s, err)
	}
	if parts[1] != "0" {
		return Channel{}, fmt.Errorf("htclow: channel %q has a non-zero reserved field", s)
	}
	id, err := strconv.ParseUint(parts[2], 10, 16)
	if err != nil {
		return Channel{}, fmt.Errorf("htclow: channel %q has a bad id: %w", s, err)
	}
	return Channel{Module: uint8(module), ID: uint16(id)}, nil
}
