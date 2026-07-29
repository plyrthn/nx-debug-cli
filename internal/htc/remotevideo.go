package htc

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"
)

// VideoPortName is the HTCS port the target streams video on. Audio rides an
// identically-framed stream on its own port, which is why the frame kinds
// below cover both: the two streams share one header format and only differ
// in which kinds they actually carry. Confirmed on hardware - the audio
// stream parses under the same layout detection as video.
const VideoPortName = "iywys@$remoteVideo"

// AudioPortName is the matching audio stream.
const AudioPortName = "iywys@$remoteAudio"

// maxFramePayload bounds what a single frame may claim. A header that
// survives framing but carries a garbage size would otherwise turn one bad
// read into a multi-gigabyte allocation.
const maxFramePayload = 64 << 20

// FrameKind is the packet type in a v2 header. The numbering is the
// target's, so the values matter.
type FrameKind int

const (
	VideoFormatFrame FrameKind = 0
	AudioFormatFrame FrameKind = 1
	VideoDataFrame   FrameKind = 2
	AudioDataFrame   FrameKind = 3
)

var frameKindNames = map[FrameKind]string{
	VideoFormatFrame: "video-format",
	AudioFormatFrame: "audio-format",
	VideoDataFrame:   "video-data",
	AudioDataFrame:   "audio-data",
}

func (k FrameKind) String() string {
	if name, ok := frameKindNames[k]; ok {
		return name
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// VideoEncoding is what the target says its video payloads are. Only the
// still-image encodings can be turned into a picture without a codec.
type VideoEncoding int

const (
	EncodingH264   VideoEncoding = 0
	EncodingBitmap VideoEncoding = 1
	EncodingPNG    VideoEncoding = 2
	EncodingJPEG   VideoEncoding = 3
	EncodingRawRGB VideoEncoding = 4
)

var videoEncodingNames = map[VideoEncoding]string{
	EncodingH264:   "h264",
	EncodingBitmap: "bitmap",
	EncodingPNG:    "png",
	EncodingJPEG:   "jpeg",
	EncodingRawRGB: "raw-rgb",
}

func (e VideoEncoding) String() string {
	if name, ok := videoEncodingNames[e]; ok {
		return name
	}
	// An unrecognised encoding reports as unknown rather than as the
	// neighbouring one it happens to sit next to. Guessing here would hand
	// a caller a decoder that produces confident garbage.
	return fmt.Sprintf("encoding(%d)", int(e))
}

// SelfContained reports whether a payload in this encoding is a complete
// image on its own, with no decoder state carried between frames.
func (e VideoEncoding) SelfContained() bool {
	switch e {
	case EncodingBitmap, EncodingPNG, EncodingJPEG, EncodingRawRGB:
		return true
	}
	return false
}

// AudioEncoding is the audio stream's counterpart.
type AudioEncoding int

const (
	AudioNone AudioEncoding = 0
	AudioPCM  AudioEncoding = 1
)

var audioEncodingNames = map[AudioEncoding]string{
	AudioNone: "none",
	AudioPCM:  "pcm",
}

func (e AudioEncoding) String() string {
	if name, ok := audioEncodingNames[e]; ok {
		return name
	}
	return fmt.Sprintf("encoding(%d)", int(e))
}

// Frame is one message off a media stream. Which fields are meaningful
// depends on Kind: the format kinds carry an encoding and a descriptor
// payload, the data kinds carry LackFrameCount and the encoded media.
type Frame struct {
	Kind           FrameKind
	TimestampUs    int64
	Encoding       VideoEncoding
	Audio          AudioEncoding
	LackFrameCount uint32
	Payload        []byte
}

// Time is the frame timestamp as a duration since the target's stream epoch.
func (f Frame) Time() time.Duration { return time.Duration(f.TimestampUs) * time.Microsecond }

func (f Frame) String() string {
	switch f.Kind {
	case VideoFormatFrame:
		return fmt.Sprintf("%-13s %-8s %6d bytes", f.Kind, f.Encoding, len(f.Payload))
	case AudioFormatFrame:
		return fmt.Sprintf("%-13s %-8s %6d bytes", f.Kind, f.Audio, len(f.Payload))
	default:
		return fmt.Sprintf("%-13s %-8s %6d bytes  t=%v lack=%d", f.Kind, "", len(f.Payload), f.Time(), f.LackFrameCount)
	}
}

// A media stream is a run of fixed-size headers each followed by its payload.
// Three header layouts exist in the wild and none of them is self-identifying,
// so which one a target speaks has to be worked out by looking at the stream.
//
// The layouts are declared here as data rather than as a switch, because
// picking the wrong one produces headers that parse into plausible garbage
// rather than failing - the only safe way to choose is to try each and see
// which one actually walks the stream.
type frameLayout struct {
	Name string
	Size int
	// parse decodes one header. ok is false when the bytes cannot be this
	// layout at all, which is what makes detection possible.
	parse func(head []byte) (f Frame, payload uint32, ok bool)
}

const (
	// legacy20 is what firmware 18.1.0 sends: a leading constant word, then
	// the same field order the documented 16-byte layout uses. Confirmed
	// against hardware by walking three consecutive frames and checking the
	// timestamps advance and each payload lands exactly on the next header.
	legacy20HeaderSize = 20
	// v1 has no packet type and no format notifications: every header
	// describes a video frame, and the encoding is H.264 by definition.
	v1HeaderSize = 16
	v2HeaderSize = 60
)

// sanePayload and saneTimestamp bound what a header may claim before it is
// treated as evidence that the layout guess was wrong.
const saneTimestampUs = int64(100*365*24*3600) * 1_000_000

func sanePayload(n uint32) bool     { return n > 0 && n <= maxFramePayload }
func saneTime(us int64) bool        { return us > 0 && us < saneTimestampUs }
func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
func le64(b []byte, off int) int64  { return int64(binary.LittleEndian.Uint64(b[off:])) }

// frameLayouts is the detection order. The more constrained a layout is, the
// earlier it goes, so a stream that could be read two ways is read as the one
// that had more to prove.
var frameLayouts = []*frameLayout{
	{
		Name: "v2",
		Size: v2HeaderSize,
		parse: func(h []byte) (Frame, uint32, bool) {
			f := Frame{Kind: FrameKind(le32(h, 0)), TimestampUs: le64(h, 4)}
			if !saneTime(f.TimestampUs) {
				return f, 0, false
			}
			var size uint32
			switch f.Kind {
			case VideoFormatFrame:
				f.Encoding = VideoEncoding(le32(h, 12))
				size = le32(h, 16)
			case AudioFormatFrame:
				f.Audio = AudioEncoding(le32(h, 12))
				size = le32(h, 20)
			case VideoDataFrame, AudioDataFrame:
				f.LackFrameCount = le32(h, 12)
				size = le32(h, 16)
			default:
				return f, 0, false
			}
			return f, size, sanePayload(size)
		},
	},
	{
		Name: "legacy20",
		Size: legacy20HeaderSize,
		parse: func(h []byte) (Frame, uint32, bool) {
			// The leading word is 1 on every frame this has been seen on.
			// It is not labelled here because a constant observed on one
			// service is not enough to say whether it means "version" or
			// "packet type", and naming it either way would be a guess
			// dressed up as a fact.
			if le32(h, 0) != 1 {
				return Frame{}, 0, false
			}
			f := Frame{
				Kind:           VideoDataFrame,
				Encoding:       EncodingH264,
				LackFrameCount: le32(h, 4),
				TimestampUs:    le64(h, 8),
			}
			size := le32(h, 16)
			return f, size, saneTime(f.TimestampUs) && sanePayload(size)
		},
	},
	{
		Name: "v1",
		Size: v1HeaderSize,
		parse: func(h []byte) (Frame, uint32, bool) {
			f := Frame{
				Kind:           VideoDataFrame,
				Encoding:       EncodingH264,
				LackFrameCount: le32(h, 0),
				TimestampUs:    le64(h, 4),
			}
			size := le32(h, 12)
			return f, size, saneTime(f.TimestampUs) && sanePayload(size)
		},
	},
}

// MediaStream reads the target's video or audio stream.
//
// The target starts sending the moment the connection is accepted - there is
// no request to make and nothing to negotiate. The only thing a reader has to
// get right is which of the two header layouts it is looking at, and that is
// fixed for the life of the connection.
type MediaStream struct {
	conn   net.Conn
	r      *bufio.Reader
	layout *frameLayout
	head   [v2HeaderSize]byte
}

// Layout names the header layout this stream turned out to use.
func (m *MediaStream) Layout() string { return m.layout.Name }

// DialVideo opens the target's video stream. serial is the HTCS peer name.
func DialVideo(ctx context.Context, serial string) (*MediaStream, error) {
	return dialMedia(ctx, serial, VideoPortName)
}

// DialAudio opens the target's audio stream.
func DialAudio(ctx context.Context, serial string) (*MediaStream, error) {
	return dialMedia(ctx, serial, AudioPortName)
}

func dialMedia(ctx context.Context, serial, port string) (*MediaStream, error) {
	addr, err := resolvePortAddr(ctx, serial, port)
	if err != nil {
		return nil, err
	}
	return DialMediaAddr(ctx, addr)
}

// DialMediaAddr opens an already-resolved stream address and works out which
// header layout the peer is using.
func DialMediaAddr(ctx context.Context, addr string) (*MediaStream, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("htc: dial media stream %s: %w", addr, err)
	}
	m := &MediaStream{conn: conn, r: bufio.NewReaderSize(conn, detectBuffer)}
	// Detection reads from the socket, so it needs the same protection: a
	// peer that accepts and stays silent must not wedge the caller.
	if d, ok := ctx.Deadline(); ok {
		conn.SetReadDeadline(d)
	} else {
		conn.SetReadDeadline(time.Now().Add(detectTimeout))
	}
	if err := m.detectLayout(); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Time{})
	return m, nil
}

// detectTimeout bounds layout detection when the caller set no deadline.
const detectTimeout = 10 * time.Second

// detectBuffer has to hold a header, a whole frame and the following header,
// since that is what the chained check below looks at.
const detectBuffer = 1 << 20

// detectLayout works out which header layout the peer is using by trying each
// one against the actual bytes.
//
// A single header is not enough evidence - the layouts overlap enough that a
// wrong guess still yields a small integer here and a large one there. What
// settles it is walking: read the claimed payload length, and check that a
// valid header with a later timestamp lands exactly where that payload ends.
// Landing on a real header by accident is not something a wrong length does.
func (m *MediaStream) detectLayout() error {
	var firstErr error
	for _, l := range frameLayouts {
		head, err := m.r.Peek(l.Size)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		f, size, ok := l.parse(head)
		if !ok {
			continue
		}
		// Chain check. If the buffer can't reach the next header the single
		// header stands on its own, which is the best that can be done for a
		// frame larger than the detection window.
		span := l.Size + int(size) + l.Size
		if span <= detectBuffer {
			if next, err := m.r.Peek(span); err == nil {
				g, _, ok := l.parse(next[l.Size+int(size):])
				if !ok || g.TimestampUs < f.TimestampUs {
					continue
				}
			}
		}
		m.layout = l
		return nil
	}
	if firstErr != nil {
		return fmt.Errorf("htc: reading first media header: %w", firstErr)
	}
	return errors.New("htc: media stream matched no known header layout")
}

// NextFrame reads one frame. The payload is freshly allocated per call, so a
// caller may hold onto it.
func (m *MediaStream) NextFrame() (Frame, error) {
	head := m.head[:m.layout.Size]
	if _, err := io.ReadFull(m.r, head); err != nil {
		return Frame{}, err
	}
	f, payloadSize, ok := m.layout.parse(head)
	if !ok {
		// Framing is positional, so once a header stops making sense there
		// is no offset to resynchronise from. Reporting it beats carrying on
		// and handing back frames assembled from the middle of a payload.
		return Frame{}, fmt.Errorf("htc: %s header did not parse (% x)", m.layout.Name, head)
	}
	if payloadSize > 0 {
		f.Payload = make([]byte, payloadSize)
		if _, err := io.ReadFull(m.r, f.Payload); err != nil {
			return Frame{}, fmt.Errorf("htc: reading %d byte payload: %w", payloadSize, err)
		}
	}
	return f, nil
}

// SetReadDeadline bounds the next NextFrame.
func (m *MediaStream) SetReadDeadline(t time.Time) error { return m.conn.SetReadDeadline(t) }

func (m *MediaStream) Close() error { return m.conn.Close() }

// MediaSession keeps a media stream running across the reconnects the target
// expects.
//
// A single TCP connection is not the unit of a stream. The target closes the
// connection on its own after a short run, tears down its listening socket,
// and re-listens on a *different* port; the reference client handles this by
// looping - connect, read until it drops, resolve the port again, reconnect.
// A reader that treats the first EOF as the end of the stream sees a couple of
// hundred milliseconds of video and concludes the protocol is broken.
type MediaSession struct {
	ctx    context.Context
	serial string
	port   string

	stream *MediaStream
	// addr is the last address that worked. The target drops the connection
	// about twice a second, so redialling is the common path, not the error
	// path - going back through the control port each time would spend most
	// of each window resolving an address that hasn't changed.
	addr string
	// Reconnects counts how many times the target dropped us. It is worth
	// surfacing: a session that reconnects every frame is working but is
	// telling you something.
	Reconnects int
	closed     bool
}

// reconnectWindow bounds how long to keep trying to find the target's new
// listener before giving up. The port reappears within a few milliseconds in
// practice; this is slack for a host that is busy.
const reconnectWindow = 10 * time.Second

// DialVideoSession opens the video stream and keeps it open across reconnects.
func DialVideoSession(ctx context.Context, serial string) (*MediaSession, error) {
	return DialMediaSession(ctx, serial, VideoPortName)
}

// DialAudioSession is the same for audio.
func DialAudioSession(ctx context.Context, serial string) (*MediaSession, error) {
	return DialMediaSession(ctx, serial, AudioPortName)
}

// DialMediaSession opens a named media port and keeps it open across the
// target's reconnects.
func DialMediaSession(ctx context.Context, serial, port string) (*MediaSession, error) {
	s := &MediaSession{ctx: ctx, serial: serial, port: port}
	if err := s.connect(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *MediaSession) connect() error {
	deadline := time.Now().Add(reconnectWindow)
	var last error
	for {
		addr, err := s.addr, error(nil)
		if addr == "" {
			addr, err = resolvePortAddr(s.ctx, s.serial, s.port)
		}
		if err == nil {
			var stream *MediaStream
			stream, err = DialMediaAddr(s.ctx, addr)
			if err != nil {
				// The cached address is the first thing to doubt. Drop it so
				// the next attempt resolves properly rather than retrying a
				// port the target has genuinely moved off.
				s.addr = ""
			}
			if err == nil {
				s.addr = addr
				// Carry the context's deadline onto the socket. Without it a
				// peer that accepts and then says nothing blocks forever:
				// the context bounds dialling, not a read already in flight.
				if d, ok := s.ctx.Deadline(); ok {
					stream.SetReadDeadline(d)
				}
				s.stream = stream
				return nil
			}
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("htc: %s did not come back: %w", s.port, last)
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// NextFrame reads the next frame, reconnecting when the target drops us.
func (s *MediaSession) NextFrame() (Frame, error) {
	for {
		f, err := s.stream.NextFrame()
		if err == nil {
			return f, nil
		}
		if s.closed || !isDisconnect(err) {
			return Frame{}, err
		}
		s.stream.Close()
		s.Reconnects++
		if err := s.connect(); err != nil {
			return Frame{}, err
		}
	}
}

// Layout names the header layout of the current connection.
func (s *MediaSession) Layout() string { return s.stream.Layout() }

func (s *MediaSession) Close() error {
	s.closed = true
	return s.stream.Close()
}

// isDisconnect distinguishes the target hanging up - which is routine and
// means reconnect - from a read that failed for any other reason, which is
// not something reconnecting would fix.
func isDisconnect(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET)
}

// ErrNoStillFrame reports that the stream is running but carries an encoding
// this build cannot turn into an image on its own.
var ErrNoStillFrame = errors.New("htc: stream carries no self-contained frames")

// NextStill reads until a frame arrives that is a complete image by itself,
// and returns it with the encoding needed to interpret it.
//
// This is the screenshot path. A stream announcing an inter-frame codec fails
// immediately rather than waiting out the deadline, since no amount of
// further reading would produce a decodable frame.
func (m *MediaStream) NextStill() (VideoEncoding, []byte, error) {
	// The layouts without format notifications carry H.264 by definition,
	// so their encoding is known before a byte is read.
	encoding := VideoEncoding(-1)
	if m.layout.Name != "v2" {
		encoding = EncodingH264
	}
	for {
		f, err := m.NextFrame()
		if err != nil {
			return 0, nil, err
		}
		switch f.Kind {
		case VideoFormatFrame:
			encoding = f.Encoding
			if !encoding.SelfContained() {
				return encoding, nil, fmt.Errorf("%w: video is %s", ErrNoStillFrame, encoding)
			}
		case VideoDataFrame:
			if encoding < 0 {
				return 0, nil, errors.New("htc: video data arrived before any format notification")
			}
			if !encoding.SelfContained() {
				return encoding, nil, fmt.Errorf("%w: video is %s", ErrNoStillFrame, encoding)
			}
			if len(f.Payload) > 0 {
				return encoding, f.Payload, nil
			}
		}
	}
}
