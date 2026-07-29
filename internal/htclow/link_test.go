package htclow

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeTransport is a loopback wire with a scripted target on the far end.
type fakeTransport struct {
	mu      sync.Mutex
	cond    *sync.Cond
	toHost  []byte
	written [][]byte
	closed  bool

	// onWrite lets a test answer a packet the moment it goes out.
	onWrite func(*fakeTransport, []byte)
}

func newFakeTransport() *fakeTransport {
	f := &fakeTransport{}
	f.cond = sync.NewCond(&f.mu)
	return f
}

func (f *fakeTransport) WritePacket(pkt []byte) error {
	f.mu.Lock()
	f.written = append(f.written, append([]byte(nil), pkt...))
	onWrite := f.onWrite
	f.mu.Unlock()
	if onWrite != nil {
		onWrite(f, pkt)
	}
	return nil
}

func (f *fakeTransport) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.toHost) == 0 && !f.closed {
		f.cond.Wait()
	}
	if len(f.toHost) == 0 {
		return 0, io.EOF
	}
	n := copy(p, f.toHost)
	f.toHost = f.toHost[n:]
	return n, nil
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.cond.Broadcast()
	return nil
}

// deliver queues bytes for the host to read.
func (f *fakeTransport) deliver(b []byte) {
	f.mu.Lock()
	f.toHost = append(f.toHost, b...)
	f.mu.Unlock()
	f.cond.Broadcast()
}

func (f *fakeTransport) sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.written))
	copy(out, f.written)
	return out
}

const targetInfoJSON = `{"Spec":"NX","Conn":"USB-gen2","HW":"EDEV_00_03_00_00","Name":"","SN":"SERIAL","FW":"fw","Prot":5}`

// scriptTarget answers the handshake the way a real devkit does.
func scriptTarget(f *fakeTransport) {
	f.onWrite = func(f *fakeTransport, pkt []byte) {
		h, err := ParseHeader(pkt)
		if err != nil || !h.Ctrl() {
			return
		}
		switch CtrlType(h.Type) {
		case ConnectFromHost:
			reply, _ := CtrlPacket(ConnectFromTarget, 1, []byte(targetInfoJSON))
			f.deliver(reply)
		case ReadyFromHost:
			reply, _ := CtrlPacket(ReadyFromTarget, 2, ReadyFromHostBody(ServiceChannels))
			f.deliver(reply)
		}
	}
}

func dialTest(t *testing.T) (*Link, *fakeTransport) {
	t.Helper()
	f := newFakeTransport()
	scriptTarget(f)
	link, err := Dial(f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { link.Close() })
	return link, f
}

func TestDialCompletesTheHandshake(t *testing.T) {
	link, f := dialTest(t)

	if link.Info.SN != "SERIAL" || link.Info.Prot != 5 {
		t.Errorf("target info = %+v", link.Info)
	}
	if got := len(link.Channels()); got != len(ServiceChannels) {
		t.Errorf("%d channels open, want %d", got, len(ServiceChannels))
	}
	for _, ch := range ServiceChannels {
		if _, ok := link.Stream(ch); !ok {
			t.Errorf("channel %s not open", ch)
		}
	}

	// The order is the whole protocol: connect, then ready, and only then
	// anything on the mux. A MaxData before the ready exchange stalls the
	// target's endpoint, which costs a physical replug to clear.
	var order []string
	for _, pkt := range f.sent() {
		h, err := ParseHeader(pkt)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, h.TypeName())
	}
	if len(order) < 3 {
		t.Fatalf("only %d packets sent: %v", len(order), order)
	}
	if order[0] != "ConnectFromHost" {
		t.Errorf("first packet = %s, want ConnectFromHost", order[0])
	}
	if order[1] != "ReadyFromHost" {
		t.Errorf("second packet = %s, want ReadyFromHost - nothing may precede it on the mux", order[1])
	}
	for _, name := range order[2:] {
		if name != "MaxData" {
			t.Errorf("packet after ready = %s, want only MaxData", name)
		}
	}
}

// A channel the target left out must not be opened: it never agreed to it,
// and a mux packet on one is what takes the link down.
func TestDialOnlyOpensAgreedChannels(t *testing.T) {
	f := newFakeTransport()
	only := []Channel{{Module: 4, ID: 0}}
	f.onWrite = func(f *fakeTransport, pkt []byte) {
		h, _ := ParseHeader(pkt)
		if !h.Ctrl() {
			return
		}
		switch CtrlType(h.Type) {
		case ConnectFromHost:
			reply, _ := CtrlPacket(ConnectFromTarget, 1, []byte(targetInfoJSON))
			f.deliver(reply)
		case ReadyFromHost:
			reply, _ := CtrlPacket(ReadyFromTarget, 2, ReadyFromHostBody(only))
			f.deliver(reply)
		}
	}
	link, err := Dial(f)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	if got := link.Channels(); len(got) != 1 || got[0] != only[0] {
		t.Errorf("channels = %v, want just %v", got, only)
	}
	if _, ok := link.Stream(Channel{Module: 1, ID: 0}); ok {
		t.Error("opened a channel the target never listed")
	}
}

func TestDialRejectsATargetThatAgreesToNothing(t *testing.T) {
	f := newFakeTransport()
	f.onWrite = func(f *fakeTransport, pkt []byte) {
		h, _ := ParseHeader(pkt)
		if !h.Ctrl() {
			return
		}
		switch CtrlType(h.Type) {
		case ConnectFromHost:
			reply, _ := CtrlPacket(ConnectFromTarget, 1, []byte(targetInfoJSON))
			f.deliver(reply)
		case ReadyFromHost:
			reply, _ := CtrlPacket(ReadyFromTarget, 2, ReadyFromHostBody(nil))
			f.deliver(reply)
		}
	}
	if _, err := Dial(f); err == nil {
		t.Fatal("a target that listed no channels was accepted")
	}
}

// The initial MaxData has to carry a real window. Advertising zero credit
// means the target may never send, which looks like a dead service.
func TestInitialMaxDataCarriesTheReceiveWindow(t *testing.T) {
	_, f := dialTest(t)
	found := 0
	for _, pkt := range f.sent() {
		h, _ := ParseHeader(pkt)
		if h.Mux() && MuxType(h.Type) == MuxMaxData {
			found++
			if h.Share != DefaultReceiveBuffer {
				t.Errorf("MaxData window = %d, want %d", h.Share, DefaultReceiveBuffer)
			}
		}
	}
	if found != len(ServiceChannels) {
		t.Errorf("%d MaxData packets, want one per channel (%d)", found, len(ServiceChannels))
	}
}

func TestStreamReadDeliversData(t *testing.T) {
	link, f := dialTest(t)
	s, _ := link.Stream(Channel{Module: 4, ID: 0})

	pkt, err := DataPacket(s.Channel(), 0, 1<<20, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	f.deliver(pkt)

	buf := make([]byte, 16)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "hello" {
		t.Errorf("read %q, want %q", got, "hello")
	}
}

// The stream position is checked, not trusted. A gap means bytes were lost,
// and carrying on would hand the layer above a corrupted stream.
func TestStreamRejectsAnOutOfOrderOffset(t *testing.T) {
	link, f := dialTest(t)
	s, _ := link.Stream(Channel{Module: 4, ID: 0})

	pkt, _ := DataPacket(s.Channel(), 99, 1<<20, []byte("x"))
	f.deliver(pkt)

	select {
	case <-link.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("link carried on after a stream-offset gap")
	}
	if link.Err() == nil {
		t.Error("link stopped without saying why")
	}
}

// Credit is cumulative and may only go up. A window that went backwards means
// the two sides disagree about what has been sent.
func TestStreamRejectsCreditGoingBackwards(t *testing.T) {
	link, f := dialTest(t)
	s, _ := link.Stream(Channel{Module: 4, ID: 0})

	grant, _ := MaxDataPacket(s.Channel(), 5000)
	f.deliver(grant)
	// Let the first grant land before contradicting it.
	time.Sleep(50 * time.Millisecond)
	regress, _ := MaxDataPacket(s.Channel(), 10)
	f.deliver(regress)

	select {
	case <-link.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("link carried on after credit went backwards")
	}
}

// Write must not put a byte on the wire before the peer has granted room for
// it, and must split at the negotiated body size.
func TestStreamWriteRespectsCredit(t *testing.T) {
	link, f := dialTest(t)
	s, _ := link.Stream(Channel{Module: 4, ID: 0})

	before := len(f.sent())
	done := make(chan error, 1)
	go func() {
		_, err := s.Write([]byte("payload"))
		done <- err
	}()

	select {
	case <-done:
		t.Fatal("wrote with no credit granted")
	case <-time.After(150 * time.Millisecond):
	}

	grant, _ := MaxDataPacket(s.Channel(), 4096)
	f.deliver(grant)

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write never completed after credit arrived")
	}

	var data []byte
	for _, pkt := range f.sent()[before:] {
		h, _ := ParseHeader(pkt)
		if h.Mux() && MuxType(h.Type) == MuxData {
			data = pkt[HeaderSize:]
		}
	}
	if !bytes.Equal(data, []byte("payload")) {
		t.Errorf("data packet body = %q, want %q", data, "payload")
	}
}

// Reading has to renew credit, or the peer runs out and stops sending for
// good. The renewal is cumulative: drained bytes are added to the window.
func TestReadRenewsCredit(t *testing.T) {
	link, f := dialTest(t)
	s, _ := link.Stream(Channel{Module: 4, ID: 0})

	// Push more than half the buffer through so a renewal is due.
	chunk := make([]byte, MuxDefaultBody)
	var offset uint32
	for sent := 0; sent < DefaultReceiveBuffer/2+len(chunk); sent += len(chunk) {
		pkt, _ := DataPacket(s.Channel(), offset, 1<<30, chunk)
		f.deliver(pkt)
		offset += uint32(len(chunk))
	}

	buf := make([]byte, 32*1024)
	read := 0
	for read < DefaultReceiveBuffer/2+len(chunk) {
		n, err := s.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		read += n
	}

	var last uint64
	for _, pkt := range f.sent() {
		h, _ := ParseHeader(pkt)
		if h.Mux() && MuxType(h.Type) == MuxMaxData {
			last = h.Share
		}
	}
	if last <= DefaultReceiveBuffer {
		t.Errorf("window never grew past its initial %d (last %d), so the peer would stall",
			DefaultReceiveBuffer, last)
	}
}

// Closing tells the target the host is gone. Skipping it is what makes the
// next attempt come back as a disconnect instead of a fresh handshake.
func TestCloseSendsDisconnect(t *testing.T) {
	link, f := dialTest(t)
	link.Close()

	for _, pkt := range f.sent() {
		h, _ := ParseHeader(pkt)
		if h.Ctrl() && CtrlType(h.Type) == DisconnectFromHost {
			return
		}
	}
	t.Error("Close did not send DisconnectFromHost")
}

// A blocked reader has to find out the link died rather than waiting forever.
func TestLinkFailureUnblocksReaders(t *testing.T) {
	link, f := dialTest(t)
	s, _ := link.Stream(Channel{Module: 4, ID: 0})

	done := make(chan struct{})
	go func() {
		s.Read(make([]byte, 16))
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	f.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked Read outlived the link")
	}
}

func TestReaderFramesExactLengths(t *testing.T) {
	f := newFakeTransport()
	r := newReader(f, 128)
	f.deliver([]byte("abcdefghij"))

	got, err := r.readFull(4)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcd" {
		t.Errorf("= %q, want abcd", got)
	}
	got, err = r.readFull(6)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "efghij" {
		t.Errorf("= %q, want efghij", got)
	}
}

// A read that arrives in pieces must still frame correctly - on USB a header
// and its body can land as separate transfers or as one.
func TestReaderJoinsSplitReads(t *testing.T) {
	f := newFakeTransport()
	r := newReader(f, 128)

	go func() {
		f.deliver([]byte("abc"))
		time.Sleep(20 * time.Millisecond)
		f.deliver([]byte("def"))
	}()

	got, err := r.readFull(6)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdef" {
		t.Errorf("= %q, want abcdef", got)
	}
}

func TestReaderRefusesMoreThanItsBuffer(t *testing.T) {
	r := newReader(newFakeTransport(), 32)
	if _, err := r.readFull(64); err == nil {
		t.Error("a read larger than the buffer was accepted")
	}
}

// OpenChannel is how a bulk transfer's second channel comes up: after the
// handshake, on a module:id the peer named in a request rather than one
// ServiceChannels listed.
func TestOpenChannelRaisesAChannelOutsideTheHandshake(t *testing.T) {
	link, _ := dialTest(t)
	ch := Channel{Module: 1, ID: 900}

	if _, ok := link.Stream(ch); ok {
		t.Fatal("bulk channel already open before OpenChannel")
	}
	s, err := link.OpenChannel(ch, DefaultReceiveBuffer)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := link.Stream(ch); !ok || got != s {
		t.Error("OpenChannel did not register the stream on the link")
	}
	if _, err := link.OpenChannel(ch, DefaultReceiveBuffer); err == nil {
		t.Error("opening the same channel id twice was accepted")
	}
}

// Bulk channels skip the handshake by agreement (EnableHandshake is false on
// both sides in the reference), so OpenChannel must not send anything -
// unlike the four service channels, which get an immediate MaxData.
func TestOpenChannelSendsNothing(t *testing.T) {
	link, f := dialTest(t)
	before := len(f.sent())

	if _, err := link.OpenChannel(Channel{Module: 1, ID: 900}, DefaultReceiveBuffer); err != nil {
		t.Fatal(err)
	}
	if got := len(f.sent()); got != before {
		t.Errorf("%d packets went out from OpenChannel alone, want 0", got-before)
	}
}

// SendBulk is the reference's MakeBulkSendConfig behaviour: no credit wait,
// every packet's share field carries the whole transfer's length rather
// than a running grant, and a payload bigger than one packet's limit splits
// across several with a contiguous offset.
func TestSendBulkChunksAndDeclaresTheWholeTotal(t *testing.T) {
	link, f := dialTest(t)
	s, err := link.OpenChannel(Channel{Module: 1, ID: 900}, DefaultReceiveBuffer)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.sent())

	payload := bytes.Repeat([]byte{0xAB}, MuxMaxBodySize+100)
	if err := s.SendBulk(payload); err != nil {
		t.Fatal(err)
	}

	sent := f.sent()[before:]
	if len(sent) != 2 {
		t.Fatalf("%d packets sent, want 2 (one full, one remainder)", len(sent))
	}
	var reassembled []byte
	for i, pkt := range sent {
		h, err := ParseHeader(pkt)
		if err != nil {
			t.Fatal(err)
		}
		if !h.Mux() || MuxType(h.Type) != MuxData {
			t.Fatalf("packet %d = %s, want Data", i, h.TypeName())
		}
		if h.Share != uint64(len(payload)) {
			t.Errorf("packet %d share = %d, want the whole total %d, not a running grant", i, h.Share, len(payload))
		}
		if int(h.BodySize) != len(pkt)-HeaderSize {
			t.Fatal("body size header did not match")
		}
		reassembled = append(reassembled, pkt[HeaderSize:]...)
	}
	if !bytes.Equal(reassembled, payload) {
		t.Error("reassembled payload does not match what was sent")
	}
}

// ReceiveBulk is the read-side mirror: it must not send a credit renewal the
// way Read does, since a bulk channel's peer already committed to the whole
// size up front and a renewal is traffic the wire protocol has no use for
// here.
func TestReceiveBulkCollectsExactlyNAndSendsNothing(t *testing.T) {
	link, f := dialTest(t)
	s, err := link.OpenChannel(Channel{Module: 1, ID: 900}, DefaultReceiveBuffer)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.sent())

	payload := bytes.Repeat([]byte{0xCD}, 300000)
	go func() {
		sent := 0
		for sent < len(payload) {
			n := len(payload) - sent
			if n > MuxMaxBodySize {
				n = MuxMaxBodySize
			}
			pkt, err := DataPacket(s.Channel(), uint32(sent), uint64(len(payload)), payload[sent:sent+n])
			if err != nil {
				t.Error(err)
				return
			}
			f.deliver(pkt)
			sent += n
		}
	}()

	var out bytes.Buffer
	if err := s.ReceiveBulk(&out, int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Error("received payload does not match what was sent")
	}
	if got := len(f.sent()); got != before {
		t.Errorf("ReceiveBulk sent %d packets, want 0 (no credit renewal on a bulk channel)", got-before)
	}
}

// Close on a channel opened with OpenChannel must free its id: a bulk
// channel is only good for one transfer, and a stale entry would either
// refuse the next OpenChannel for the same id or, worse, route a late
// straggler packet to a finished stream.
func TestCloseChannelFreesTheIDForReuse(t *testing.T) {
	link, _ := dialTest(t)
	ch := Channel{Module: 1, ID: 900}
	s, err := link.OpenChannel(ch, DefaultReceiveBuffer)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := link.Stream(ch); ok {
		t.Error("channel still registered after Close")
	}
	if _, err := link.OpenChannel(ch, DefaultReceiveBuffer); err != nil {
		t.Errorf("could not reopen a closed channel's id: %v", err)
	}
}
