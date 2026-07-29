// Package remoteaudio plays the target's live audio stream on the host - the
// sound half of what Target Manager 2's remote video window does.
//
// The target streams raw PCM in whatever layout its mixer happens to be
// running, while the playback side wants signed 16-bit little-endian stereo,
// so every chunk goes through one conversion on the way in.
package remoteaudio

import (
	"encoding/binary"
	"fmt"
)

// Format is the target's PCM layout, as reported by the video service.
type Format struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
}

// sampleConv decodes one sample of a given width down to a signed 16-bit
// value. Holding these in a table means an unhandled depth is a missing key,
// which Validate reports by name, rather than a switch default that quietly
// plays noise.
type sampleConv struct {
	bytes  int
	decode func(b []byte) int16
}

var sampleConvs = map[int]sampleConv{
	// 8-bit PCM is unsigned, with silence at 128; every wider depth is
	// signed, so only this one needs re-centring.
	8:  {1, func(b []byte) int16 { return int16(int(b[0])-128) << 8 }},
	16: {2, func(b []byte) int16 { return int16(binary.LittleEndian.Uint16(b)) }},
	// Wider samples are truncated to their top 16 bits. The low bits are
	// below the noise floor of anything this is used for.
	24: {3, func(b []byte) int16 { return int16(binary.LittleEndian.Uint16(b[1:])) }},
	32: {4, func(b []byte) int16 { return int16(binary.LittleEndian.Uint16(b[2:])) }},
}

func (f Format) Validate() error {
	if f.SampleRate <= 0 {
		return fmt.Errorf("remoteaudio: bad sample rate %d", f.SampleRate)
	}
	if f.Channels < 1 {
		return fmt.Errorf("remoteaudio: bad channel count %d", f.Channels)
	}
	if _, ok := sampleConvs[f.BitsPerSample]; !ok {
		return fmt.Errorf("remoteaudio: unsupported sample depth %d-bit", f.BitsPerSample)
	}
	return nil
}

// FrameSize is how many bytes one sample across every channel takes up in
// this format. It is zero for a format Validate rejects.
func (f Format) FrameSize() int {
	conv, ok := sampleConvs[f.BitsPerSample]
	if !ok || f.Channels < 1 {
		return 0
	}
	return conv.bytes * f.Channels
}

func (f Format) String() string {
	return fmt.Sprintf("%dHz %dch %d-bit", f.SampleRate, f.Channels, f.BitsPerSample)
}

// ToStereo16 converts a chunk of PCM in this format to the signed 16-bit
// little-endian stereo the player consumes. Mono is duplicated to both
// speakers and anything past the first two channels is dropped, which is the
// front pair in every standard layout. A trailing partial frame is discarded
// rather than half-decoded - decoding it would put a click in the output and
// leave the stream misaligned from there on.
func (f Format) ToStereo16(src []byte) []byte {
	conv, ok := sampleConvs[f.BitsPerSample]
	if !ok || f.Channels < 1 {
		return nil
	}
	frame := conv.bytes * f.Channels
	frames := len(src) / frame

	dst := make([]byte, frames*4)
	for i := range frames {
		off := i * frame
		left := conv.decode(src[off:])
		right := left
		if f.Channels > 1 {
			right = conv.decode(src[off+conv.bytes:])
		}
		binary.LittleEndian.PutUint16(dst[i*4:], uint16(left))
		binary.LittleEndian.PutUint16(dst[i*4+2:], uint16(right))
	}
	return dst
}
