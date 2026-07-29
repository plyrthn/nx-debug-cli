package htcmisc

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Client makes HTCMISC requests of the target on the client channel.
//
// The version exchange here is the part that matters on connect: the host asks
// what the target can speak and then tells it which version the two will use.
// Until that happens the target has a host it can talk to but no agreed
// protocol with it.
type Client struct {
	rw io.ReadWriter

	Trace func(string)

	mu       sync.Mutex
	nextTask uint32
	version  int16
}

// NewClient wraps a channel stream.
func NewClient(rw io.ReadWriter) *Client {
	return &Client{rw: rw, version: MaxVersion}
}

// Version is the protocol version agreed with the target, valid after
// Handshake.
func (c *Client) Version() int16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// SetTargetName renames the target.
//
// This is the only request the reference host ever makes on this channel. The
// version types in the enum are never sent here; the channel exists, stays
// open, and is otherwise silent until someone renames the target. Opening it
// still matters, because a channel the target agreed to and the host never
// reads is one whose receive window fills up.
func (c *Client) SetTargetName(name string) error {
	reply, err := c.call(Packet{Type: SetTargetName, Body: []byte(name)})
	if err != nil {
		return err
	}
	if Result(reply.Param0) != Success {
		return fmt.Errorf("htcmisc: set target name: %s", Result(reply.Param0))
	}
	return nil
}

// call sends one request and reads its response.
//
// This channel is strictly one request at a time: the host is the only thing
// making requests on it, and the target answers in order, so a mutex over the
// whole exchange is both correct and simpler than matching task ids.
func (c *Client) call(p Packet) (Packet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	p.TaskID = c.nextTask
	c.nextTask++
	p.Category = Request
	p.Version = c.version
	if c.Trace != nil {
		c.Trace("-> " + p.String())
	}
	if _, err := c.rw.Write(p.Encode()); err != nil {
		return Packet{}, err
	}

	head := make([]byte, HeaderSize)
	if _, err := io.ReadFull(c.rw, head); err != nil {
		return Packet{}, err
	}
	reply, bodySize, err := ParseHeader(head)
	if err != nil {
		return Packet{}, err
	}
	if bodySize > 0 {
		reply.Body = make([]byte, bodySize)
		if _, err := io.ReadFull(c.rw, reply.Body); err != nil {
			return Packet{}, err
		}
	}
	if c.Trace != nil {
		c.Trace("<- " + reply.String())
	}
	if reply.Category != Response {
		return Packet{}, fmt.Errorf("htcmisc: expected a response, got %s", reply.Category)
	}
	if reply.Type != p.Type {
		return Packet{}, fmt.Errorf("htcmisc: asked %s, got %s", p.Type, reply.Type)
	}
	return reply, nil
}

// int64Body reads the 8-byte integer some responses carry.
func int64Body(b []byte) (int64, bool) {
	if len(b) < 8 {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(b)), true
}
