package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// cmdVideoStream talks to the target's media streams directly, with no daemon
// in the path. Resolution goes through the HTCS control port, so this works
// against whoever is driving the link.
func cmdVideoStream(ctx context.Context, sub, serial string, rest []string) error {
	switch sub {
	case "dump":
		return videoDump(ctx, serial, rest, "video")
	case "dump-audio":
		return videoDump(ctx, serial, rest, "audio")
	case "grab":
		return videoGrab(ctx, serial, rest)
	case "record":
		return videoRecord(ctx, serial, rest)
	case "raw":
		return videoRaw(ctx, serial, rest, "video")
	case "raw-audio":
		return videoRaw(ctx, serial, rest, "audio")
	default:
		return fmt.Errorf("unknown video subcommand: %s", sub)
	}
}

// videoDump prints the frame headers coming off a media stream. The stream is
// self-describing, so this is what tells you which encoding a given firmware
// actually sends before any decoding is attempted.
func videoDump(ctx context.Context, serial string, rest []string, port string) error {
	seconds := 5
	if len(rest) > 0 {
		n, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("invalid seconds %q: %w", rest[0], err)
		}
		seconds = n
	}
	// Every frame, not a sample. Frame size tracks how much of the picture
	// changed, so a full listing is a usable proxy for "did anything happen
	// on screen" without decoding a single frame.
	every := len(rest) > 1 && rest[1] == "all"
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()

	name := htc.VideoPortName
	if port == "audio" {
		name = htc.AudioPortName
	}
	stream, err := htc.DialMediaSession(ctx, serial, name)
	if err != nil {
		return err
	}
	defer stream.Close()

	fmt.Printf("%s, %s framing, listening %ds\n", name, stream.Layout(), seconds)
	for n := 0; ; n++ {
		f, err := stream.NextFrame()
		if err != nil {
			fmt.Printf("  (%v after %d frames, %d reconnects)\n", err, n, stream.Reconnects)
			return nil
		}
		// Data frames arrive at frame rate and all look alike, so print the
		// format notifications in full and thin the rest out.
		if f.Kind == htc.VideoFormatFrame || f.Kind == htc.AudioFormatFrame {
			fmt.Printf("  <- %s  % x\n", f, f.Payload)
			continue
		}
		if every || n < 8 || n%30 == 0 {
			fmt.Printf("  <- %s\n", f)
		}
	}
}

// videoRecord writes the video payloads to a file as an Annex B elementary
// stream, with the encoder's parameter sets in front of them.
//
// The target never sends its own: the stream is nothing but non-IDR slices, so
// on its own it describes nothing and no decoder will open it. Writing the
// parameter sets here is what makes the file usable. `--raw` leaves them out,
// for looking at what the target actually sent.
func videoRecord(ctx context.Context, serial string, rest []string) error {
	raw := false
	args := make([]string, 0, len(rest))
	for _, a := range rest {
		if a == "--raw" {
			raw = true
			continue
		}
		args = append(args, a)
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: nxdbg video record <serial> <seconds> <file> [--raw]")
	}
	seconds, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid seconds %q: %w", args[0], err)
	}
	out := args[1]

	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()

	stream, err := htc.DialMediaSession(ctx, serial, htc.VideoPortName)
	if err != nil {
		return err
	}
	defer stream.Close()

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()

	if !raw {
		if _, err := f.Write(htc.NXVideoConfig.ParameterSets()); err != nil {
			return err
		}
	}

	// NAL types seen, so the answer to "are the parameter sets in here" comes
	// out of the recording rather than needing a second pass with another
	// tool.
	seen := map[byte]int{}
	frames, total := 0, 0
	for {
		fr, err := stream.NextFrame()
		if err != nil {
			break
		}
		if fr.Kind != htc.VideoDataFrame || len(fr.Payload) == 0 {
			continue
		}
		if _, err := f.Write(fr.Payload); err != nil {
			return err
		}
		frames++
		total += len(fr.Payload)
		for _, t := range annexBNalTypes(fr.Payload) {
			seen[t]++
		}
	}

	fmt.Printf("wrote %s: %d frames, %d bytes, %d reconnects\n", out, frames, total, stream.Reconnects)
	types := make([]int, 0, len(seen))
	for t := range seen {
		types = append(types, int(t))
	}
	sort.Ints(types)
	fmt.Println("NAL units:")
	for _, t := range types {
		fmt.Printf("  type %-2d %-24s x%d\n", t, nalTypeNames[byte(t)], seen[byte(t)])
	}
	if seen[7] == 0 || seen[8] == 0 {
		if raw {
			fmt.Println("\nthe target sent no SPS/PPS, and --raw left them out, so this file")
			fmt.Println("will not decode. Record without --raw to get a usable one.")
			return nil
		}
		fmt.Printf("\nthe target sent no SPS/PPS, so %dx%d High@%.1f parameter sets were written\n",
			htc.NXVideoConfig.Width, htc.NXVideoConfig.Height, float64(htc.NXVideoConfig.LevelIDC)/10)
		fmt.Println("in front of the stream. There is also no IDR frame in it, so a decoder has")
		fmt.Println("no reference picture to start from and holds its output back until it finds")
		fmt.Println("one. To see the frames anyway:")
		fmt.Printf("    ffmpeg -flags2 +showall -i %s frame%%03d.png\n", out)
	}
	return nil
}

// nalTypeNames covers the H.264 unit types this stream can plausibly carry.
// An unlisted type is reported by number rather than guessed at.
var nalTypeNames = map[byte]string{
	1: "non-IDR slice", 2: "slice data A", 3: "slice data B", 4: "slice data C",
	5: "IDR slice", 6: "SEI", 7: "SPS", 8: "PPS", 9: "access unit delimiter",
	10: "end of sequence", 11: "end of stream", 12: "filler",
	13: "SPS extension", 19: "auxiliary slice",
}

// annexBNalTypes lists the NAL unit types in an Annex B buffer. Start codes
// are three or four bytes and both occur in the same stream, so both are
// matched rather than assuming the longer one.
func annexBNalTypes(b []byte) []byte {
	var out []byte
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 {
			continue
		}
		switch {
		case b[i+2] == 1:
			out = append(out, b[i+3]&0x1f)
			i += 3
		case b[i+2] == 0 && b[i+3] == 1 && i+4 < len(b):
			out = append(out, b[i+4]&0x1f)
			i += 4
		}
	}
	return out
}

// videoRaw hexdumps the head of a media stream with no framing applied. When
// a header doesn't parse, the parser's own account of what it saw is the
// least trustworthy evidence available - this is the unfiltered version.
func videoRaw(ctx context.Context, serial string, rest []string, port string) error {
	n := 256
	if len(rest) > 0 {
		v, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("invalid byte count %q: %w", rest[0], err)
		}
		n = v
	}
	entry, err := htc.ResolvePort(ctx, serial, port)
	if err != nil {
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", entry.Addr())
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Printf("%s at %s, first %d bytes\n", port, entry.Addr(), n)

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, n)
	read, err := io.ReadFull(conn, buf)
	if read > 0 {
		fmt.Print(hex.Dump(buf[:read]))
	}
	if err != nil {
		fmt.Printf("(%v after %d bytes)\n", err, read)
	}
	return nil
}

// videoGrab saves one frame off the video stream. It only works when the
// stream carries a self-contained encoding - an inter-frame codec needs a
// decoder holding state across frames, which is a different job.
func videoGrab(ctx context.Context, serial string, rest []string) error {
	out := "."
	if len(rest) > 0 {
		out = rest[0]
	}
	entry, err := htc.ResolvePort(ctx, serial, "video")
	if err != nil {
		return err
	}
	stream, err := htc.DialMediaAddr(ctx, entry.Addr())
	if err != nil {
		return err
	}
	defer stream.Close()

	stream.SetReadDeadline(time.Now().Add(15 * time.Second))
	encoding, payload, err := stream.NextStill()
	if err != nil {
		return err
	}

	if fi, statErr := os.Stat(out); statErr == nil && fi.IsDir() {
		out = filepath.Join(out, fmt.Sprintf("%s-%d.%s", serial, time.Now().Unix(), stillExtensions[encoding]))
	}
	if err := os.WriteFile(out, payload, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%s, %d bytes)\n", out, encoding, len(payload))
	return nil
}

// stillExtensions names the file a self-contained frame should be written to.
// Only the encodings that are complete images appear here; anything missing
// is caught by NextStill before it gets this far.
var stillExtensions = map[htc.VideoEncoding]string{
	htc.EncodingBitmap: "bmp",
	htc.EncodingPNG:    "png",
	htc.EncodingJPEG:   "jpg",
	htc.EncodingRawRGB: "rgb",
}
