package htcfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHeaderRoundTrip(t *testing.T) {
	p := Packet{
		Version:  1,
		Category: Request,
		Type:     OpenFile,
		Param0:   1,
		Param1:   -2,
		Param2:   3,
		Param3:   4,
		Param4:   5,
		Body:     []byte("data/file.bin"),
	}
	raw := p.Encode()
	got, bodySize, err := ParseHeader(raw[:HeaderSize])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bodySize != int64(len(p.Body)) {
		t.Errorf("body size = %d, want %d", bodySize, len(p.Body))
	}
	if got.Type != p.Type || got.Param0 != p.Param0 || got.Param1 != p.Param1 ||
		got.Param2 != p.Param2 || got.Param3 != p.Param3 || got.Param4 != p.Param4 {
		t.Errorf("round trip = %+v, want %+v", got, p)
	}
}

// This protocol has no task id, so its parameters sit eight bytes earlier than
// the ones in HTCS and HTCMISC. Reusing those offsets here would put every
// parameter in the wrong place while still parsing cleanly.
func TestHeaderFieldOffsets(t *testing.T) {
	raw := Packet{Version: 1, Category: Response, Type: ReadFile, Param0: 0x11, Param1: 0x22}.Encode()
	cases := []struct {
		name   string
		offset int
		size   int
		want   uint64
	}{
		{"protocol", 0, 2, uint64(Protocol)},
		{"version", 2, 2, 1},
		{"category", 4, 2, uint64(Response)},
		{"type", 6, 2, uint64(ReadFile)},
		{"body size", 8, 8, 0},
		{"param0", 16, 8, 0x11},
		{"param1", 24, 8, 0x22},
	}
	for _, c := range cases {
		var got uint64
		switch c.size {
		case 2:
			got = uint64(binary.LittleEndian.Uint16(raw[c.offset:]))
		case 8:
			got = binary.LittleEndian.Uint64(raw[c.offset:])
		}
		if got != c.want {
			t.Errorf("%s at %d = %d, want %d", c.name, c.offset, got, c.want)
		}
	}
}

func TestParseHeaderRejectsBadPackets(t *testing.T) {
	good := Packet{Type: ReadFile}.Encode()

	t.Run("short", func(t *testing.T) {
		if _, _, err := ParseHeader(good[:HeaderSize-1]); err == nil {
			t.Error("a short header was accepted")
		}
	})
	t.Run("wrong protocol", func(t *testing.T) {
		raw := append([]byte(nil), good...)
		binary.LittleEndian.PutUint16(raw[0:], uint16(Protocol+1))
		if _, _, err := ParseHeader(raw); err == nil {
			t.Error("another protocol's header was accepted")
		}
	})
	t.Run("future version", func(t *testing.T) {
		raw := append([]byte(nil), good...)
		binary.LittleEndian.PutUint16(raw[2:], uint16(MaxVersion+1))
		if _, _, err := ParseHeader(raw); err == nil {
			t.Error("a version this build cannot speak was accepted")
		}
	})
	t.Run("oversized body", func(t *testing.T) {
		raw := append([]byte(nil), good...)
		binary.LittleEndian.PutUint64(raw[8:], maxBody+1)
		if _, _, err := ParseHeader(raw); err == nil {
			t.Error("an oversized body was accepted")
		}
	})
}

// The fs results go back to the target as-is, so their packed values are worth
// pinning against the module and description each one is built from.
func TestFilesystemResultValues(t *testing.T) {
	cases := []struct {
		name        string
		value       int64
		module      int64
		description int64
	}{
		{"PathNotFound", fsPathNotFound, 2, 1},
		{"PathAlreadyExists", fsPathAlreadyExists, 2, 2},
		{"TargetLocked", fsTargetLocked, 2, 7},
		{"DirectoryNotEmpty", fsDirectoryNotEmpty, 2, 8},
		{"UsableSpaceNotEnough", fsUsableSpaceNotEnough, 2, 30},
		{"OutOfRange", fsOutOfRange, 2, 3005},
		{"InvalidCharacter", fsInvalidCharacter, 2, 6004},
	}
	for _, c := range cases {
		if got := c.value & 0x1ff; got != c.module {
			t.Errorf("%s: module = %d, want %d", c.name, got, c.module)
		}
		if got := (c.value >> 9) & 0x1fff; got != c.description {
			t.Errorf("%s: description = %d, want %d", c.name, got, c.description)
		}
	}
	// This one is also worth pinning as a literal, as a direct check on the
	// packing.
	if fsInvalidCharacter != 3074050 {
		t.Errorf("InvalidCharacter = %d, want 3074050", fsInvalidCharacter)
	}
}

// newTestServer runs a Server over a pipe against a temporary root, and
// returns a round trip plus the root path.
func newTestServer(t *testing.T, configure func(*Server)) (func(Packet, ...[]byte) Packet, string) {
	t.Helper()
	root := t.TempDir()
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	s := NewServer(server)
	s.Root = root
	if configure != nil {
		configure(s)
	}
	go s.Serve()

	call := func(p Packet, bodies ...[]byte) Packet {
		t.Helper()
		p.Category = Request
		if len(bodies) > 0 {
			p.Body = bodies[0]
		}
		client.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := client.Write(p.Encode()); err != nil {
			t.Fatalf("write request: %v", err)
		}
		// A second body follows the first for the rename operations, whose
		// lengths live in the parameters rather than in BodySize.
		if len(bodies) > 1 {
			for _, extra := range bodies[1:] {
				if _, err := client.Write(extra); err != nil {
					t.Fatalf("write extra body: %v", err)
				}
			}
		}
		head := make([]byte, HeaderSize)
		if _, err := io.ReadFull(client, head); err != nil {
			t.Fatalf("read response: %v", err)
		}
		reply, bodySize, err := ParseHeader(head)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if bodySize > 0 {
			reply.Body = make([]byte, bodySize)
			if _, err := io.ReadFull(client, reply.Body); err != nil {
				t.Fatalf("read response body: %v", err)
			}
		}
		return reply
	}
	return call, root
}

func TestVersionExchange(t *testing.T) {
	call, _ := newTestServer(t, nil)

	r := call(Packet{Type: GetMaxProtocolVersion})
	if Result(r.Param0) != Success || r.Param1 != int64(MaxVersion) {
		t.Errorf("max version = %s/%d, want Success/%d", Result(r.Param0), r.Param1, MaxVersion)
	}

	r = call(Packet{Type: SetProtocolVersion, Param0: 1})
	if Result(r.Param0) != Success {
		t.Errorf("set version 1 = %s, want Success", Result(r.Param0))
	}

	r = call(Packet{Type: SetProtocolVersion, Param0: 99})
	if Result(r.Param0) != UnsupportedVer {
		t.Errorf("set version 99 = %s, want UnsupportedVersion", Result(r.Param0))
	}

	r = call(Packet{Type: SetProtocolVersion, Param0: -1})
	if Result(r.Param0) != InvalidRequest {
		t.Errorf("set version -1 = %s, want InvalidRequest", Result(r.Param0))
	}
}

func TestReadAFile(t *testing.T) {
	call, root := newTestServer(t, nil)
	if err := os.WriteFile(filepath.Join(root, "data.bin"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := call(Packet{Type: OpenFile, Param0: OpenRead}, []byte("/data.bin"))
	if Result(r.Param0) != Success || r.Param1 != fsSuccess {
		t.Fatalf("open = %s/%d, want Success/0", Result(r.Param0), r.Param1)
	}
	handle := r.Param2

	r = call(Packet{Type: GetFileSize, Param0: handle})
	if r.Param2 != 11 {
		t.Errorf("size = %d, want 11", r.Param2)
	}

	r = call(Packet{Type: ReadFile, Param0: handle, Param1: 6, Param2: 5})
	if Result(r.Param0) != Success || string(r.Body) != "world" {
		t.Errorf("read = %s/%q, want Success/%q", Result(r.Param0), r.Body, "world")
	}

	// Reading past the end is a short read, not a failure.
	r = call(Packet{Type: ReadFile, Param0: handle, Param1: 8, Param2: 100})
	if Result(r.Param0) != Success || string(r.Body) != "rld" {
		t.Errorf("short read = %s/%q, want Success/%q", Result(r.Param0), r.Body, "rld")
	}

	r = call(Packet{Type: CloseFile, Param0: handle})
	if Result(r.Param0) != Success {
		t.Errorf("close = %s, want Success", Result(r.Param0))
	}

	r = call(Packet{Type: ReadFile, Param0: handle, Param2: 1})
	if Result(r.Param0) != InvalidHandle {
		t.Errorf("read after close = %s, want InvalidHandle", Result(r.Param0))
	}
}

// The target can ask for the start of the file along with the handle. The
// extra parameters say the prefetch happened, so a caller that ignores them
// would read the body as something else.
func TestOpenFileWithPrefetch(t *testing.T) {
	call, root := newTestServer(t, nil)
	if err := os.WriteFile(filepath.Join(root, "data.bin"), []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := call(Packet{Type: OpenFile, Param0: OpenRead, Param2: 4}, []byte("data.bin"))
	if Result(r.Param0) != Success {
		t.Fatalf("open = %s, want Success", Result(r.Param0))
	}
	if r.Param3 != 1 {
		t.Errorf("prefetch flag = %d, want 1", r.Param3)
	}
	if r.Param4 != 6 {
		t.Errorf("reported file size = %d, want 6", r.Param4)
	}
	if string(r.Body) != "abcd" {
		t.Errorf("prefetched %q, want %q", r.Body, "abcd")
	}
}

func TestWriteAndCreate(t *testing.T) {
	call, root := newTestServer(t, nil)

	r := call(Packet{Type: CreateFile, Param0: 0}, []byte("new.txt"))
	if Result(r.Param0) != Success || r.Param1 != fsSuccess {
		t.Fatalf("create = %s/%d, want Success/0", Result(r.Param0), r.Param1)
	}

	// Creating the same path twice must report the collision, not silently
	// truncate what is there.
	r = call(Packet{Type: CreateFile}, []byte("new.txt"))
	if r.Param1 != fsPathAlreadyExists {
		t.Errorf("second create = %d, want fsPathAlreadyExists (%d)", r.Param1, fsPathAlreadyExists)
	}

	r = call(Packet{Type: OpenFile, Param0: OpenRead | OpenWrite}, []byte("new.txt"))
	handle := r.Param2
	r = call(Packet{Type: WriteFile, Param0: handle, Param1: WriteOptionFlush, Param2: 0}, []byte("payload"))
	if Result(r.Param0) != Success || r.Param1 != fsSuccess {
		t.Fatalf("write = %s/%d, want Success/0", Result(r.Param0), r.Param1)
	}
	call(Packet{Type: CloseFile, Param0: handle})

	got, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("file contains %q, want %q", got, "payload")
	}
}

func TestExistsAnswersBooleansNotResults(t *testing.T) {
	call, root := newTestServer(t, nil)
	os.WriteFile(filepath.Join(root, "f.txt"), nil, 0o644)
	os.Mkdir(filepath.Join(root, "d"), 0o755)

	cases := []struct {
		typ  Type
		path string
		want int64
	}{
		{FileExists, "f.txt", 1},
		{FileExists, "d", 0},
		{FileExists, "missing", 0},
		{DirectoryExists, "d", 1},
		{DirectoryExists, "f.txt", 0},
	}
	for _, c := range cases {
		r := call(Packet{Type: c.typ}, []byte(c.path))
		if Result(r.Param0) != Success {
			t.Errorf("%s %q: %s, want Success", c.typ, c.path, Result(r.Param0))
			continue
		}
		if r.Param1 != c.want {
			t.Errorf("%s %q = %d, want %d", c.typ, c.path, r.Param1, c.want)
		}
	}
}

func TestGetEntryType(t *testing.T) {
	call, root := newTestServer(t, nil)
	os.WriteFile(filepath.Join(root, "f.txt"), nil, 0o644)
	os.Mkdir(filepath.Join(root, "d"), 0o755)

	r := call(Packet{Type: GetEntryType}, []byte("f.txt"))
	if r.Param1 != fsSuccess || r.Param2 != EntryFile {
		t.Errorf("file: result=%d type=%d, want 0/%d", r.Param1, r.Param2, EntryFile)
	}
	r = call(Packet{Type: GetEntryType}, []byte("d"))
	if r.Param1 != fsSuccess || r.Param2 != EntryDirectory {
		t.Errorf("dir: result=%d type=%d, want 0/%d", r.Param1, r.Param2, EntryDirectory)
	}
	r = call(Packet{Type: GetEntryType}, []byte("missing"))
	if r.Param1 != fsPathNotFound {
		t.Errorf("missing: result=%d, want %d", r.Param1, fsPathNotFound)
	}
}

func TestDirectoryListing(t *testing.T) {
	call, root := newTestServer(t, nil)
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("12345"), 0o644)
	os.WriteFile(filepath.Join(root, "sub", "b.txt"), nil, 0o644)
	os.Mkdir(filepath.Join(root, "sub", "inner"), 0o755)

	r := call(Packet{Type: OpenDirectory, Param0: ListAll}, []byte("sub"))
	if Result(r.Param0) != Success || r.Param1 != fsSuccess {
		t.Fatalf("open dir = %s/%d, want Success/0", Result(r.Param0), r.Param1)
	}
	handle := r.Param2

	r = call(Packet{Type: GetEntryCount, Param0: handle})
	if r.Param2 != 3 {
		t.Errorf("entry count = %d, want 3", r.Param2)
	}

	// Reading in batches has to pick up where the last one stopped.
	r = call(Packet{Type: ReadDirectory, Param0: handle, Param1: 2})
	if r.Param2 != 2 || len(r.Body) != 2*DirectoryEntrySize {
		t.Fatalf("first batch = %d entries / %d bytes, want 2 / %d", r.Param2, len(r.Body), 2*DirectoryEntrySize)
	}
	names := []string{entryName(r.Body, 0), entryName(r.Body, 1)}
	if names[0] != "a.txt" || names[1] != "b.txt" {
		t.Errorf("first batch = %v, want [a.txt b.txt]", names)
	}
	if got := r.Body[entryOffType]; got != EntryFile {
		t.Errorf("a.txt type = %d, want %d", got, EntryFile)
	}
	if got := binary.LittleEndian.Uint64(r.Body[entryOffSize:]); got != 5 {
		t.Errorf("a.txt size = %d, want 5", got)
	}

	r = call(Packet{Type: ReadDirectory, Param0: handle, Param1: 2})
	if r.Param2 != 1 || entryName(r.Body, 0) != "inner" {
		t.Fatalf("second batch = %d entries, first %q, want 1 / inner", r.Param2, entryName(r.Body, 0))
	}
	if got := r.Body[entryOffType]; got != EntryDirectory {
		t.Errorf("inner type = %d, want %d", got, EntryDirectory)
	}

	r = call(Packet{Type: ReadDirectory, Param0: handle, Param1: 2})
	if r.Param2 != 0 {
		t.Errorf("third batch = %d entries, want 0", r.Param2)
	}

	if r = call(Packet{Type: CloseDirectory, Param0: handle}); Result(r.Param0) != Success {
		t.Errorf("close dir = %s, want Success", Result(r.Param0))
	}
}

func TestDirectoryListingRespectsMode(t *testing.T) {
	call, root := newTestServer(t, nil)
	os.WriteFile(filepath.Join(root, "f.txt"), nil, 0o644)
	os.Mkdir(filepath.Join(root, "d"), 0o755)

	for _, c := range []struct {
		mode int64
		want int64
	}{
		{ListAll, 2},
		{ListFiles, 1},
		{ListDirectories, 1},
	} {
		r := call(Packet{Type: OpenDirectory, Param0: c.mode}, []byte("/"))
		handle := r.Param2
		r = call(Packet{Type: GetEntryCount, Param0: handle})
		if r.Param2 != c.want {
			t.Errorf("mode %d: %d entries, want %d", c.mode, r.Param2, c.want)
		}
		call(Packet{Type: CloseDirectory, Param0: handle})
	}
}

func entryName(body []byte, i int) string {
	b := body[i*DirectoryEntrySize : i*DirectoryEntrySize+entryNameSize]
	if n := bytes.IndexByte(b, 0); n >= 0 {
		b = b[:n]
	}
	return string(b)
}

// Nothing the target sends may reach outside the root. The reference host does
// not contain paths at all, so this is the check that matters most here.
func TestPathsCannotEscapeTheRoot(t *testing.T) {
	call, root := newTestServer(t, nil)
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	for _, p := range []string{
		"../outside.txt",
		"/../outside.txt",
		"sub/../../outside.txt",
		"..\\outside.txt",
		"C:\\Windows\\win.ini",
		"/etc/passwd",
	} {
		r := call(Packet{Type: FileExists}, []byte(p))
		if Result(r.Param0) != Success {
			t.Errorf("%q: %s", p, Result(r.Param0))
			continue
		}
		if r.Param1 != 0 {
			t.Errorf("%q resolved to something real outside the root", p)
		}
	}
}

func TestResolveContainsPaths(t *testing.T) {
	s := &Server{Root: filepath.FromSlash("/srv/root")}
	cases := []struct {
		in   string
		want string
	}{
		{"file.txt", "/srv/root/file.txt"},
		{"/file.txt", "/srv/root/file.txt"},
		{"a/b/../c.txt", "/srv/root/a/c.txt"},
		{"../../etc/passwd", "/srv/root/etc/passwd"},
		{"\\windows\\system32", "/srv/root/windows/system32"},
		{"D:/data/x", "/srv/root/data/x"},
		{"/", "/srv/root"},
		{"file.txt\x00", "/srv/root/file.txt"},
	}
	for _, c := range cases {
		got, res := s.resolve([]byte(c.in))
		if res != fsSuccess {
			t.Errorf("%q: result %d", c.in, res)
			continue
		}
		if filepath.ToSlash(got) != c.want {
			t.Errorf("%q -> %q, want %q", c.in, filepath.ToSlash(got), c.want)
		}
	}
}

// A program on the target is handed host paths by whoever launched it, so a
// path that already names a real place inside the root has to resolve to that
// place rather than being folded underneath the root a second time.
func TestResolveAcceptsHostPathsInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	s := &Server{Root: root}

	inside := filepath.Join(root, "sub", "package.nsp")
	if got, res := s.resolve([]byte(inside)); res != fsSuccess || got != inside {
		t.Errorf("resolve(%q) = %q (%d), want it unchanged", inside, got, res)
	}
}

// Accepting host paths must not widen what the target can reach: one that the
// root does not contain still gets folded underneath it.
func TestResolveStillContainsHostPathsOutsideTheRoot(t *testing.T) {
	root := t.TempDir()
	s := &Server{Root: root}

	outside := filepath.Join(filepath.Dir(root), "elsewhere", "secret.txt")
	got, res := s.resolve([]byte(outside))
	if res != fsSuccess {
		t.Fatalf("resolve(%q): result %d", outside, res)
	}
	if got == outside {
		t.Fatalf("resolve(%q) escaped the root", outside)
	}
	rel, err := filepath.Rel(root, got)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("resolve(%q) = %q, which is not under %q", outside, got, root)
	}
}

func TestReadOnlyRefusesWrites(t *testing.T) {
	call, root := newTestServer(t, func(s *Server) { s.ReadOnly = true })
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("keep"), 0o644)

	for _, c := range []struct {
		typ  Type
		body []byte
	}{
		{CreateFile, []byte("new.txt")},
		{DeleteFile, []byte("f.txt")},
		{CreateDirectory, []byte("d")},
		{DeleteDirectory, []byte("f.txt")},
	} {
		r := call(Packet{Type: c.typ}, c.body)
		if r.Param1 != fsTargetLocked {
			t.Errorf("%s = %d, want fsTargetLocked (%d)", c.typ, r.Param1, fsTargetLocked)
		}
	}

	// Opening for write is refused; opening for read still works.
	r := call(Packet{Type: OpenFile, Param0: OpenWrite}, []byte("f.txt"))
	if r.Param1 != fsTargetLocked {
		t.Errorf("write open = %d, want fsTargetLocked", r.Param1)
	}
	r = call(Packet{Type: OpenFile, Param0: OpenRead}, []byte("f.txt"))
	if r.Param1 != fsSuccess {
		t.Errorf("read open = %d, want success", r.Param1)
	}

	if got, _ := os.ReadFile(filepath.Join(root, "f.txt")); string(got) != "keep" {
		t.Errorf("file changed to %q under read-only", got)
	}
}

func TestDeleteAndRename(t *testing.T) {
	call, root := newTestServer(t, nil)
	os.WriteFile(filepath.Join(root, "old.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(root, "dir"), 0o755)

	r := call(Packet{Type: RenameFile, Param0: 7, Param1: 7}, []byte("old.txt"), []byte("new.txt"))
	if r.Param1 != fsSuccess {
		t.Fatalf("rename = %d, want 0", r.Param1)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); err != nil {
		t.Errorf("renamed file is missing: %v", err)
	}

	// A file call must not act on a directory, or a stray delete removes one.
	r = call(Packet{Type: DeleteFile}, []byte("dir"))
	if r.Param1 != fsPathNotFound {
		t.Errorf("delete of a directory via DeleteFile = %d, want fsPathNotFound", r.Param1)
	}
	if _, err := os.Stat(filepath.Join(root, "dir")); err != nil {
		t.Error("DeleteFile removed a directory")
	}

	r = call(Packet{Type: DeleteDirectory, Param0: 0}, []byte("dir"))
	if r.Param1 != fsSuccess {
		t.Errorf("rmdir = %d, want 0", r.Param1)
	}

	r = call(Packet{Type: DeleteFile}, []byte("new.txt"))
	if r.Param1 != fsSuccess {
		t.Errorf("delete = %d, want 0", r.Param1)
	}
}

// A recursive delete aimed at the root would empty the directory this server
// was pointed at, so it is refused however it is spelled.
func TestRecursiveDeleteCannotTakeTheRoot(t *testing.T) {
	call, root := newTestServer(t, nil)
	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("x"), 0o644)

	for _, p := range []string{"/", "", ".", "sub/.."} {
		r := call(Packet{Type: DeleteDirectory, Param0: 1}, []byte(p))
		if r.Param1 != fsTargetLocked {
			t.Errorf("recursive delete of %q = %d, want fsTargetLocked", p, r.Param1)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Fatal("the root was emptied")
	}
}

func TestWorkingDirectory(t *testing.T) {
	call, root := newTestServer(t, nil)

	r := call(Packet{Type: GetWorkingDirectorySize})
	if int(r.Param1) != len(filepath.ToSlash(root)) {
		t.Errorf("size = %d, want %d", r.Param1, len(filepath.ToSlash(root)))
	}

	r = call(Packet{Type: GetWorkingDirectory})
	if string(r.Body) != filepath.ToSlash(root) {
		t.Errorf("dir = %q, want %q", r.Body, filepath.ToSlash(root))
	}

	// A buffer the target says is too small has to be reported, not overrun.
	r = call(Packet{Type: GetWorkingDirectory, Param0: 2})
	if Result(r.Param0) != NotEnoughBuffer {
		t.Errorf("small buffer = %s, want NotEnoughBuffer", Result(r.Param0))
	}
}

func TestDiskFreeSpace(t *testing.T) {
	call, _ := newTestServer(t, nil)

	r := call(Packet{Type: GetDiskFreeSpace}, []byte("/"))
	if Result(r.Param0) != Success || r.Param1 != fsSuccess {
		t.Fatalf("free space = %s/%d, want Success/0", Result(r.Param0), r.Param1)
	}
	if r.Param2 <= 0 || r.Param3 <= 0 {
		t.Errorf("free=%d total=%d, both should be positive", r.Param2, r.Param3)
	}
	if r.Param2 > r.Param3 {
		t.Errorf("free (%d) is larger than total (%d)", r.Param2, r.Param3)
	}
}

func TestFileSystemAttributeReportsNoLimits(t *testing.T) {
	call, _ := newTestServer(t, nil)

	r := call(Packet{Type: GetFileSystemAttribute})
	if Result(r.Param0) != Success || r.Param1 != fsSuccess {
		t.Fatalf("attribute = %s/%d, want Success/0", Result(r.Param0), r.Param1)
	}
	if len(r.Body) != fileSystemAttributeSize {
		t.Fatalf("body is %d bytes, want %d", len(r.Body), fileSystemAttributeSize)
	}
	// Every "has value" flag must be clear, or the target reads a limit of
	// zero as a real one.
	for i := 13; i < 26; i++ {
		if got := binary.LittleEndian.Uint32(r.Body[i*4:]); got != 0 {
			t.Errorf("flag %d = %d, want 0", i-13, got)
		}
	}
}

func TestGetCaseSensitivePath(t *testing.T) {
	call, root := newTestServer(t, nil)
	os.WriteFile(filepath.Join(root, "File.TXT"), nil, 0o644)

	r := call(Packet{Type: GetCaseSensitivePath}, []byte("File.TXT"))
	if Result(r.Param0) != Success || r.Param1 != fsSuccess {
		t.Fatalf("case sensitive path = %s/%d", Result(r.Param0), r.Param1)
	}
	got := strings.TrimRight(string(r.Body), "\x00")
	if got != filepath.ToSlash(filepath.Join(root, "File.TXT")) {
		t.Errorf("path = %q, want %q", got, filepath.ToSlash(filepath.Join(root, "File.TXT")))
	}
}

// The bulk forms need a second channel to move their payload on. A server
// nobody has wired one into (Bulk left nil, as newTestServer's callers who
// don't ask for one get) has to refuse rather than leave the target waiting
// on a reply that never comes.
func TestLargeTransfersAreRefusedNotIgnored(t *testing.T) {
	call, _ := newTestServer(t, nil)

	for _, typ := range []Type{ReadFileLarge, WriteFileLarge, ReadDirectoryLarge} {
		r := call(Packet{Type: typ})
		if Result(r.Param0) != InvalidRequest {
			t.Errorf("%s = %s, want InvalidRequest", typ, Result(r.Param0))
		}
		if r.Type != typ {
			t.Errorf("%s answered as %s", typ, r.Type)
		}
	}
}

// fakeBulk is a BulkChannel for tests: it never touches a wire, it just
// records what a send handed it and hands a receive whatever was queued for
// that channel id, so a handler's use of Bulk can be checked without a real
// htclow link underneath.
type fakeBulk struct {
	mu     sync.Mutex
	sent   map[uint16][]byte
	toRecv map[uint16][]byte
	opened []uint16
}

func newFakeBulk() *fakeBulk {
	return &fakeBulk{sent: map[uint16][]byte{}, toRecv: map[uint16][]byte{}}
}

func (b *fakeBulk) OpenBulkChannel(id uint16) (BulkStream, error) {
	b.mu.Lock()
	b.opened = append(b.opened, id)
	b.mu.Unlock()
	return &fakeBulkStream{bulk: b, id: id}, nil
}

type fakeBulkStream struct {
	bulk *fakeBulk
	id   uint16
}

func (s *fakeBulkStream) SendBulk(p []byte) error {
	s.bulk.mu.Lock()
	defer s.bulk.mu.Unlock()
	s.bulk.sent[s.id] = append([]byte(nil), p...)
	return nil
}

func (s *fakeBulkStream) ReceiveBulk(w io.Writer, n int64) error {
	s.bulk.mu.Lock()
	data := s.bulk.toRecv[s.id]
	s.bulk.mu.Unlock()
	if int64(len(data)) != n {
		return fmt.Errorf("fakeBulk: asked to receive %d bytes, %d were queued", n, len(data))
	}
	_, err := w.Write(data)
	return err
}

func (s *fakeBulkStream) Close() error { return nil }

func TestReadFileLargeSendsOverTheBulkChannel(t *testing.T) {
	bulk := newFakeBulk()
	call, root := newTestServer(t, func(s *Server) { s.Bulk = bulk })
	os.WriteFile(filepath.Join(root, "data.bin"), []byte("hello world"), 0o644)

	r := call(Packet{Type: OpenFile, Param0: OpenRead}, []byte("data.bin"))
	if Result(r.Param0) != Success || r.Param1 != fsSuccess {
		t.Fatalf("open = %s/%d", Result(r.Param0), r.Param1)
	}
	handle := r.Param2

	r = call(Packet{Type: ReadFileLarge, Param0: handle, Param1: 6, Param2: 5, Param3: 42})
	if Result(r.Param0) != Success || r.Param1 != fsSuccess || r.Param2 != 5 {
		t.Fatalf("read large = %s params=%d,%d, want Success/0/5", Result(r.Param0), r.Param1, r.Param2)
	}
	// The bulk send happens on the server after this reply, inside the same
	// handler call that produced it. Serve() answers one request at a time,
	// so a second round trip only completes once that handler has returned -
	// which is what makes it safe to check bulk.sent afterward instead of
	// racing the send.
	call(Packet{Type: GetFileSize, Param0: handle})
	if got := string(bulk.sent[42]); got != "world" {
		t.Errorf("sent on channel 42 = %q, want %q", got, "world")
	}

	// A read past EOF reports the short count truthfully rather than padding
	// or failing outright, same as the ordinary ReadFile.
	r = call(Packet{Type: ReadFileLarge, Param0: handle, Param1: 6, Param2: 50, Param3: 43})
	if r.Param2 != 5 {
		t.Errorf("short read reported %d bytes, want 5", r.Param2)
	}
	call(Packet{Type: GetFileSize, Param0: handle})
	if got := string(bulk.sent[43]); got != "world" {
		t.Errorf("sent on channel 43 = %q, want %q", got, "world")
	}
}

// WriteFileLarge answers twice: Ready once the bulk channel is open, then
// the real result once the transfer and the write to disk are done. Reading
// only the first would race the file write, since that happens after it -
// so this test drives the pipe directly rather than through the call()
// helper, which only ever reads one reply per request.
func TestWriteFileLargeReceivesFromTheBulkChannel(t *testing.T) {
	root := t.TempDir()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	bulk := newFakeBulk()
	bulk.toRecv[7] = []byte("payload")
	s := NewServer(server)
	s.Root = root
	s.Bulk = bulk
	go s.Serve()
	os.WriteFile(filepath.Join(root, "new.txt"), nil, 0o644)

	client.SetDeadline(time.Now().Add(3 * time.Second))
	send := func(p Packet, body []byte) Packet {
		t.Helper()
		p.Category = Request
		p.Body = body
		if _, err := client.Write(p.Encode()); err != nil {
			t.Fatalf("write request: %v", err)
		}
		head := make([]byte, HeaderSize)
		if _, err := io.ReadFull(client, head); err != nil {
			t.Fatalf("read reply: %v", err)
		}
		reply, bodySize, err := ParseHeader(head)
		if err != nil {
			t.Fatalf("parse reply: %v", err)
		}
		if bodySize > 0 {
			reply.Body = make([]byte, bodySize)
			if _, err := io.ReadFull(client, reply.Body); err != nil {
				t.Fatalf("read reply body: %v", err)
			}
		}
		return reply
	}
	recvOnly := func() Packet {
		t.Helper()
		head := make([]byte, HeaderSize)
		if _, err := io.ReadFull(client, head); err != nil {
			t.Fatalf("read reply: %v", err)
		}
		reply, _, err := ParseHeader(head)
		if err != nil {
			t.Fatalf("parse reply: %v", err)
		}
		return reply
	}

	r := send(Packet{Type: OpenFile, Param0: OpenRead | OpenWrite}, []byte("new.txt"))
	if Result(r.Param0) != Success {
		t.Fatalf("open = %s", Result(r.Param0))
	}
	handle := r.Param2

	r = send(Packet{Type: WriteFileLarge, Param0: handle, Param1: WriteOptionFlush, Param2: 0, Param3: 7, Param4: 7}, nil)
	if Result(r.Param0) != Ready {
		t.Fatalf("first reply = %s, want Ready", Result(r.Param0))
	}
	r = recvOnly()
	if Result(r.Param0) != Success || r.Param1 != fsSuccess {
		t.Fatalf("second reply = %s/%d, want Success/0", Result(r.Param0), r.Param1)
	}

	got, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("file contains %q, want %q", got, "payload")
	}
}

func TestReadDirectoryLargeSendsEncodedEntries(t *testing.T) {
	bulk := newFakeBulk()
	call, root := newTestServer(t, func(s *Server) { s.Bulk = bulk })
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("yy"), 0o644)

	r := call(Packet{Type: OpenDirectory, Param0: ListAll}, []byte("/"))
	if Result(r.Param0) != Success {
		t.Fatalf("open dir = %s", Result(r.Param0))
	}
	handle := r.Param2

	r = call(Packet{Type: ReadDirectoryLarge, Param0: handle, Param1: 10, Param2: 99})
	if Result(r.Param0) != Success || r.Param1 != fsSuccess || r.Param2 != 2 {
		t.Fatalf("read directory large = %s params=%d,%d, want Success/0/2", Result(r.Param0), r.Param1, r.Param2)
	}
	// See TestReadFileLargeSendsOverTheBulkChannel: a second round trip is
	// the barrier that guarantees the bulk send has actually happened.
	call(Packet{Type: GetEntryCount, Param0: handle})
	body := bulk.sent[99]
	if len(body) != DirectoryEntrySize*2 {
		t.Fatalf("sent %d bytes, want %d", len(body), DirectoryEntrySize*2)
	}
	name0 := strings.TrimRight(string(body[:entryNameSize]), "\x00")
	name1 := strings.TrimRight(string(body[DirectoryEntrySize:DirectoryEntrySize+entryNameSize]), "\x00")
	if name0 != "a.txt" || name1 != "b.txt" {
		t.Errorf("entry names = %q, %q, want a.txt, b.txt", name0, name1)
	}
}

func TestUnknownTypeIsRefused(t *testing.T) {
	call, _ := newTestServer(t, nil)

	r := call(Packet{Type: Type(999)})
	if Result(r.Param0) != InvalidRequest {
		t.Errorf("unknown type = %s, want InvalidRequest", Result(r.Param0))
	}
}

// An operation added to the enum without a handler would answer InvalidRequest
// forever and look like a target-side problem. Catching it here is the point.
func TestEveryTypeHasAHandlerAndAName(t *testing.T) {
	for typ := range typeNames {
		if _, ok := handlers[typ]; !ok {
			t.Errorf("%s has no handler", typ)
		}
	}
	for typ := range handlers {
		if _, ok := typeNames[typ]; !ok {
			t.Errorf("type %d has a handler but no name", int16(typ))
		}
	}
	for typ := range writers {
		if _, ok := handlers[typ]; !ok {
			t.Errorf("%s is listed as a writer but has no handler", typ)
		}
	}
}

// Every handler must consume exactly what its request declared. A handler that
// skips its body leaves the next header misaligned, which shows up much later
// as a protocol error on an unrelated operation.
func TestHandlersConsumeTheirBodies(t *testing.T) {
	call, _ := newTestServer(t, nil)

	// A body on an operation that takes none, then a normal request. If the
	// first left anything behind, the second cannot parse.
	call(Packet{Type: GetMaxProtocolVersion}, []byte("unexpected"))
	r := call(Packet{Type: GetMaxProtocolVersion})
	if Result(r.Param0) != Success || r.Param1 != int64(MaxVersion) {
		t.Fatalf("the stream desynchronised: %s/%d", Result(r.Param0), r.Param1)
	}

	call(Packet{Type: Type(999)}, []byte("unexpected"))
	r = call(Packet{Type: GetMaxProtocolVersion})
	if Result(r.Param0) != Success {
		t.Fatalf("an unknown type left its body on the stream: %s", Result(r.Param0))
	}
}

func TestServerStopsOnAResponse(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	s := NewServer(server)
	s.Root = t.TempDir()
	go s.Serve()

	client.SetDeadline(time.Now().Add(2 * time.Second))
	client.Write(Packet{Category: Response, Type: ReadFile}.Encode())

	select {
	case <-s.Done():
		if s.Err() == nil {
			t.Error("stopped without an error")
		}
	case <-time.After(2 * time.Second):
		t.Error("a response on the request channel was accepted")
	}
}

func TestHandlesAreExhaustible(t *testing.T) {
	call, root := newTestServer(t, nil)
	os.WriteFile(filepath.Join(root, "f.txt"), nil, 0o644)

	for i := 0; i < maxHandles; i++ {
		r := call(Packet{Type: OpenFile, Param0: OpenRead}, []byte("f.txt"))
		if Result(r.Param0) != Success || r.Param1 != fsSuccess {
			t.Fatalf("open %d = %s/%d", i, Result(r.Param0), r.Param1)
		}
		if r.Param2 != int64(i) {
			t.Fatalf("open %d got handle %d, want the lowest free one", i, r.Param2)
		}
	}
	r := call(Packet{Type: OpenFile, Param0: OpenRead}, []byte("f.txt"))
	if Result(r.Param0) != OutOfHandle {
		t.Errorf("open past the limit = %s, want OutOfHandle", Result(r.Param0))
	}

	// Freeing one must make that number available again.
	call(Packet{Type: CloseFile, Param0: 7})
	r = call(Packet{Type: OpenFile, Param0: OpenRead}, []byte("f.txt"))
	if r.Param2 != 7 {
		t.Errorf("reopened handle = %d, want the freed 7", r.Param2)
	}
}
