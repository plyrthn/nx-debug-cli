package htc

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"

	"github.com/plyrthn/nx-debug-cli/internal/targetlog"
)

// readLogLines is what turns the log socket into something a run can be read
// from, so it has to cope with the log arriving in whatever pieces the network
// hands over rather than a line at a time.
func TestReadLogLinesSplitsOnLinesNotPackets(t *testing.T) {
	a, b := net.Pipe()
	lines := make(chan string, 8)
	go readLogLines(a, lines)

	go func() {
		b.Write([]byte("Preparing to inst"))
		b.Write([]byte("all package.nsp...\r\n1234 / 1234\n[SUCC"))
		b.Write([]byte("ESS]\n"))
		b.Close()
	}()

	var got []string
	for l := range lines {
		got = append(got, l)
	}
	want := []string{"Preparing to install package.nsp...", "1234 / 1234", "[SUCCESS]"}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A failed run's reason is in the output and nowhere else, so it has to reach
// the error message.
func TestDevMenuErrorReportsTheReason(t *testing.T) {
	err := &DevMenuError{
		Args: "application install x.nsp",
		Lines: []string{
			"Preparing to install x.nsp...",
			"Same or higher version nsp is already installed",
			"To overwrite, please use --force option",
			"[FAILURE]",
		},
	}
	if got := err.Error(); !strings.Contains(got, "To overwrite, please use --force option") {
		t.Errorf("Error() = %q, want it to carry the last line before [FAILURE]", got)
	}
}

// targetLogPacket builds one complete wire packet (head and tail both set,
// little-endian) carrying a process name and a text chunk, matching the shape
// a real devkit sends.
func targetLogPacket(process, text string) []byte {
	body := append(chunk(chunkProcessName, []byte(process)), chunk(chunkTextLog, []byte(text))...)
	b := make([]byte, 24+len(body))
	binary.LittleEndian.PutUint32(b[20:], uint32(len(body)))
	b[16] = 1 | 2 | 4 // head | tail | little-endian
	copy(b[24:], body)
	return b
}

const (
	chunkTextLog     = 2
	chunkProcessName = 10
)

func chunk(key int, value []byte) []byte {
	b := uleb(uint32(key))
	b = append(b, uleb(uint32(len(value)))...)
	return append(b, value...)
}

func uleb(v uint32) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if v == 0 {
			return b
		}
	}
}

// readDevMenuRecords is the fallback used when @Log is not published (no
// daemon): it decodes the target's own log directly and has to both
// reassemble DevMenuCommand's lines out of however many records they arrived
// in and ignore everything else on a log every process shares.
func TestReadDevMenuRecordsReassemblesLinesAndFiltersOtherProcesses(t *testing.T) {
	a, b := net.Pipe()
	lines := make(chan string, 8)
	go readDevMenuRecords(targetlog.NewReader(a), lines)

	go func() {
		b.Write(targetLogPacket("SomeOtherApp", "noise\n"))
		b.Write(targetLogPacket("DevMenuCommand", "Free space\t"))
		b.Write(targetLogPacket("DevMenuCommand", "345160155136 B\n"))
		b.Write(targetLogPacket("DevMenuCommand", "[SUCCESS]\n"))
		b.Close()
	}()

	var got []string
	for l := range lines {
		got = append(got, l)
	}
	want := []string{"Free space\t345160155136 B", "[SUCCESS]"}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLastMeaningfulLineSkipsMarkersAndBlanks(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"skips the marker", []string{"the reason", "[FAILURE]"}, "the reason"},
		{"skips blanks after it", []string{"the reason", "[FAILURE]", "", "  "}, "the reason"},
		{"nothing to report", []string{"[FAILURE]", ""}, ""},
		{"no lines at all", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastMeaningfulLine(c.lines); got != c.want {
				t.Errorf("= %q, want %q", got, c.want)
			}
		})
	}
}
