package htcfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// Server answers the target's filesystem requests.
//
// Every path the target sends is resolved underneath Root and cannot leave it.
// The reference host does not do this - it resolves against the target's
// configured working directory and hands back whatever the host filesystem
// says, so a target program can read and write anywhere the daemon can. That
// is a lot of authority to give a devkit by default, and containing it costs
// nothing: a program that stays inside its own directory sees no difference.
type Server struct {
	rw io.ReadWriter

	// Root is the directory the target's filesystem is rooted at. It must be
	// set before Serve; NewServer defaults it to the process's own working
	// directory, which is what the reference falls back to when a target has no
	// working directory configured.
	Root string

	// Log, when set, receives a line per request. Trace additionally receives
	// the decoded packets.
	Log   func(string)
	Trace func(string)

	// ReadOnly refuses every request that would modify the host filesystem.
	// Reads still work, so a target reading its data files is unaffected.
	ReadOnly bool

	// Bulk opens the second channel a large transfer needs. Left nil, the
	// three Large operations are refused exactly as they were before this
	// existed - a caller that has not wired up a link to open channels on
	// (a test, or code with no htclow underneath at all) still gets an
	// honest refusal instead of a nil pointer panic.
	Bulk BulkChannel

	mu      sync.Mutex
	version int16
	files   map[int32]*openFile
	dirs    map[int32]*openDir

	done chan struct{}
	err  error
	once sync.Once
}

// BulkChannel is what a large transfer needs beyond the request/response
// channel itself: a way to open a second channel by id and move raw bytes on
// it. ReadFileLarge, WriteFileLarge and ReadDirectoryLarge all answer this
// way instead of in their own response body, which is the entire reason a
// target picks the "Large" form over the plain one - the payload would not
// fit a single reply.
//
// *htclow.Link satisfies this without htcfs importing htclow: the channel id
// is just a number on the wire, and htclow already knows how to raise one on
// demand - the same Data/MaxData exchange the four service channels use,
// just started after the link is up instead of during its handshake.
type BulkChannel interface {
	OpenBulkChannel(id uint16) (BulkStream, error)
}

// BulkStream is one bulk channel for the life of a single transfer. Unlike
// the request channel there is no framing: a send transfers exactly the
// bytes given, and a receive collects exactly the byte count asked for. Flow
// control is not part of it either - the whole size is agreed on the request
// channel before this is opened, which is why a target uses this form at all
// rather than the ordinary one.
type BulkStream interface {
	SendBulk(p []byte) error
	ReceiveBulk(w io.Writer, n int64) error
	Close() error
}

// openFile is one file handle the target holds.
type openFile struct {
	f        *os.File
	priority int32
}

// openDir is one directory handle. Listing is incremental, because the target
// reads a directory in batches and expects to pick up where it left off.
type openDir struct {
	f        *os.File
	path     string
	mode     int32
	priority int32
	entries  []fs.DirEntry
	next     int
	loaded   bool
}

// maxHandles is the target's own limit, per kind.
const maxHandles = 256

// NewServer wraps a channel stream.
func NewServer(rw io.ReadWriter) *Server {
	root, err := os.Getwd()
	if err != nil {
		root = "."
	}
	return &Server{
		rw:      rw,
		Root:    root,
		version: MaxVersion,
		files:   make(map[int32]*openFile),
		dirs:    make(map[int32]*openDir),
		done:    make(chan struct{}),
	}
}

func (s *Server) Done() <-chan struct{} { return s.done }

func (s *Server) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Server) stop(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		for h, f := range s.files {
			f.f.Close()
			delete(s.files, h)
		}
		for h, d := range s.dirs {
			d.f.Close()
			delete(s.dirs, h)
		}
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, args...))
	}
}

// Serve reads requests until the channel fails.
func (s *Server) Serve() {
	head := make([]byte, HeaderSize)
	for {
		if _, err := io.ReadFull(s.rw, head); err != nil {
			s.stop(err)
			return
		}
		p, bodySize, err := ParseHeader(head)
		if err != nil {
			s.stop(err)
			return
		}
		if p.Category != Request {
			s.stop(fmt.Errorf("htcfs: got a %s, this channel only carries requests", p.Category))
			return
		}
		if err := s.handle(p, bodySize); err != nil {
			s.stop(err)
			return
		}
	}
}

// handler answers one request. It is given the declared body size rather than
// the body, because how much to read depends on the operation: most carry a
// path, WriteFile carries file data, and RenameFile carries two paths whose
// lengths are in the parameters rather than in BodySize.
type handler func(*Server, Packet, int64) error

// handlers maps each operation to what answers it. A request with no entry is
// refused explicitly, so an unimplemented operation shows up on the target as a
// refusal instead of a channel that stopped answering.
var handlers = map[Type]handler{
	GetMaxProtocolVersion:   (*Server).handleGetMaxProtocolVersion,
	SetProtocolVersion:      (*Server).handleSetProtocolVersion,
	GetEntryType:            (*Server).handleGetEntryType,
	OpenFile:                (*Server).handleOpenFile,
	CloseFile:               (*Server).handleCloseFile,
	GetPriorityForFile:      (*Server).handleGetPriorityForFile,
	SetPriorityForFile:      (*Server).handleSetPriorityForFile,
	CreateFile:              (*Server).handleCreateFile,
	DeleteFile:              (*Server).handleDeleteFile,
	RenameFile:              (*Server).handleRenameFile,
	FileExists:              (*Server).handleFileExists,
	ReadFile:                (*Server).handleReadFile,
	WriteFile:               (*Server).handleWriteFile,
	FlushFile:               (*Server).handleFlushFile,
	GetFileTimeStamp:        (*Server).handleGetFileTimeStamp,
	GetFileSize:             (*Server).handleGetFileSize,
	SetFileSize:             (*Server).handleSetFileSize,
	ReadFileLarge:           (*Server).handleReadFileLarge,
	WriteFileLarge:          (*Server).handleWriteFileLarge,
	OpenDirectory:           (*Server).handleOpenDirectory,
	CloseDirectory:          (*Server).handleCloseDirectory,
	GetPriorityForDirectory: (*Server).handleGetPriorityForDirectory,
	SetPriorityForDirectory: (*Server).handleSetPriorityForDirectory,
	CreateDirectory:         (*Server).handleCreateDirectory,
	DeleteDirectory:         (*Server).handleDeleteDirectory,
	RenameDirectory:         (*Server).handleRenameDirectory,
	DirectoryExists:         (*Server).handleDirectoryExists,
	ReadDirectory:           (*Server).handleReadDirectory,
	GetEntryCount:           (*Server).handleGetEntryCount,
	GetWorkingDirectory:     (*Server).handleGetWorkingDirectory,
	GetWorkingDirectorySize: (*Server).handleGetWorkingDirectorySize,
	GetCaseSensitivePath:    (*Server).handleGetCaseSensitivePath,
	GetDiskFreeSpace:        (*Server).handleGetDiskFreeSpace,
	ReadDirectoryLarge:      (*Server).handleReadDirectoryLarge,
	GetFileSystemAttribute:  (*Server).handleGetFileSystemAttribute,
}

// writers are the operations that change the host filesystem, refused when
// ReadOnly is set. Keeping this as a set rather than a check inside each
// handler means a new mutating operation is one line away from being covered,
// and the completeness test can see it.
var writers = map[Type]bool{
	OpenFile:        true, // only when the mode asks for write; checked there
	CreateFile:      true,
	DeleteFile:      true,
	RenameFile:      true,
	WriteFile:       true,
	FlushFile:       true,
	SetFileSize:     true,
	WriteFileLarge:  true,
	CreateDirectory: true,
	DeleteDirectory: true,
	RenameDirectory: true,
}

func (s *Server) handle(p Packet, bodySize int64) error {
	if s.Trace != nil {
		s.Trace("<- " + p.String())
	}
	h, ok := handlers[p.Type]
	if !ok {
		s.logf("unhandled %s", p.Type)
		if err := s.discard(bodySize); err != nil {
			return err
		}
		return s.reply(p.respond(InvalidRequest))
	}
	// OpenFile is only a write when the mode says so, so it checks for itself.
	if s.ReadOnly && writers[p.Type] && p.Type != OpenFile {
		s.logf("read-only: refused %s", p.Type)
		if err := s.discard(bodySize); err != nil {
			return err
		}
		return s.reply(p.respond(Success, fsTargetLocked))
	}
	return h(s, p, bodySize)
}

func (s *Server) reply(p Packet) error {
	if s.Trace != nil {
		s.Trace("-> " + p.String())
	}
	_, err := s.rw.Write(p.Encode())
	return err
}

// replyBody answers with a body attached. The header and body go out in one
// write, because two writes on a mux channel can be split across packets and
// the target reads them as one unit.
func (s *Server) replyBody(p Packet, body []byte) error {
	p.Body = body
	if s.Trace != nil {
		s.Trace("-> " + p.String())
	}
	_, err := s.rw.Write(p.Encode())
	return err
}

// readBody pulls a declared body off the channel.
func (s *Server) readBody(size int64) ([]byte, error) {
	if size < 0 || size > maxBody {
		return nil, fmt.Errorf("htcfs: body size %d is out of range", size)
	}
	if size == 0 {
		return nil, nil
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(s.rw, b); err != nil {
		return nil, err
	}
	return b, nil
}

// discard drains a body this side is not going to use. Skipping it would leave
// the next header misaligned with the stream.
func (s *Server) discard(size int64) error {
	if size <= 0 {
		return nil
	}
	_, err := io.CopyN(io.Discard, s.rw, size)
	return err
}

// readPath pulls a path body and rejects one longer than the target's own
// limit.
func (s *Server) readPath(size int64) ([]byte, error) {
	if size < 0 || size > MaxPathLength {
		return nil, fmt.Errorf("htcfs: path length %d is out of range", size)
	}
	return s.readBody(size)
}

// resolve maps a path from the target onto a host path under Root.
//
// Containment is done by construction rather than by checking afterwards: the
// path is cleaned as a rooted POSIX path first, so "..", a leading slash and a
// drive letter all collapse before anything touches the host filesystem.
//
// The one exception is a path that already names a real location inside Root,
// which is taken as it stands. Programs on the target are handed host paths by
// whoever launched them - DevMenuCommand gets the nsp to install that way - so
// collapsing those would leave them looking for the file in the wrong place.
// That exception cannot widen what is reachable, because withinRoot refuses
// anything Root does not contain.
func (s *Server) resolve(raw []byte) (string, int64) {
	p := strings.TrimRight(string(raw), "\x00")
	if strings.ContainsRune(p, '\x00') {
		return "", fsInvalidCharacter
	}
	if host, ok := s.withinRoot(p); ok {
		return host, fsSuccess
	}
	p = strings.ReplaceAll(p, "\\", "/")
	// A drive letter is not meaningful to the target and must not be allowed to
	// select a host volume.
	if len(p) >= 2 && p[1] == ':' {
		p = p[2:]
	}
	cleaned := path.Clean("/" + p)
	if cleaned == "/" {
		return s.Root, fsSuccess
	}
	return filepath.Join(s.Root, filepath.FromSlash(cleaned[1:])), fsSuccess
}

// withinRoot reports whether p is a host path that Root contains, returning it
// cleaned.
//
// This is the only place an absolute host path is accepted, so the containment
// check is explicit here rather than by construction: anything that leaves Root
// by any route, ".." included, is refused and falls back to the rooted form.
func (s *Server) withinRoot(p string) (string, bool) {
	if p == "" || !filepath.IsAbs(p) {
		return "", false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(pathKey(root), pathKey(abs))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

// pathKey normalises a path for comparison. Windows compares paths without
// regard to case, so a root and a path below it can differ in case and still
// be the same place.
func pathKey(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// hostResult translates a host error into the filesystem result the target
// expects. Anything unrecognised becomes fsUnexpected rather than being
// reported as success, since a wrong-but-plausible result is worse than an
// obviously unknown one.
func hostResult(err error) int64 {
	switch {
	case err == nil:
		return fsSuccess
	case errors.Is(err, fs.ErrNotExist):
		return fsPathNotFound
	case errors.Is(err, fs.ErrExist):
		return fsPathAlreadyExists
	case errors.Is(err, fs.ErrPermission):
		return fsTargetLocked
	case errors.Is(err, syscall.ENOTEMPTY):
		return fsDirectoryNotEmpty
	case errors.Is(err, syscall.ENOSPC):
		return fsUsableSpaceNotEnough
	}
	// Windows reports a non-empty directory and a sharing violation through
	// codes that do not map onto the syscall constants above.
	var perr *os.PathError
	if errors.As(err, &perr) {
		if errno, ok := perr.Err.(syscall.Errno); ok {
			switch uintptr(errno) {
			case 145: // ERROR_DIR_NOT_EMPTY
				return fsDirectoryNotEmpty
			case 32, 33: // ERROR_SHARING_VIOLATION, ERROR_LOCK_VIOLATION
				return fsTargetLocked
			case 39, 112: // ERROR_HANDLE_DISK_FULL, ERROR_DISK_FULL
				return fsUsableSpaceNotEnough
			}
		}
	}
	return fsUnexpected
}

// allocate picks the lowest free handle. The numbers go back to the target, so
// staying inside its 256-handle expectation matters.
func allocate[T any](table map[int32]T) (int32, bool) {
	for h := int32(0); h < maxHandles; h++ {
		if _, taken := table[h]; !taken {
			return h, true
		}
	}
	return 0, false
}

func (s *Server) file(h int32) (*openFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[h]
	return f, ok
}

func (s *Server) dir(h int32) (*openDir, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.dirs[h]
	return d, ok
}

// --- version ---

func (s *Server) handleGetMaxProtocolVersion(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	s.logf("max protocol version -> %d", MaxVersion)
	return s.reply(p.respond(Success, int64(MaxVersion)))
}

func (s *Server) handleSetProtocolVersion(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	switch {
	case p.Param0 < 0:
		return s.reply(p.respond(InvalidRequest))
	case p.Param0 > int64(MaxVersion):
		return s.reply(p.respond(UnsupportedVer))
	}
	s.mu.Lock()
	s.version = int16(p.Param0)
	s.mu.Unlock()
	s.logf("protocol version set to %d", p.Param0)
	return s.reply(p.respond(Success))
}

// --- entries ---

func (s *Server) handleGetEntryType(p Packet, bodySize int64) error {
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	fi, err := os.Stat(host)
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	kind := int64(EntryFile)
	if fi.IsDir() {
		kind = EntryDirectory
	}
	return s.reply(p.respond(Success, fsSuccess, kind))
}

func (s *Server) handleFileExists(p Packet, bodySize int64) error {
	return s.exists(p, bodySize, false)
}

func (s *Server) handleDirectoryExists(p Packet, bodySize int64) error {
	return s.exists(p, bodySize, true)
}

// exists answers with a boolean in Param1, not a filesystem result. The two
// look identical on the wire and mean opposite things: 1 here is "yes", where
// 1 as a result would be a failure.
func (s *Server) exists(p Packet, bodySize int64, wantDir bool) error {
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	fi, err := os.Stat(host)
	yes := int64(0)
	if err == nil && fi.IsDir() == wantDir {
		yes = 1
	}
	return s.reply(p.respond(Success, yes))
}

// --- files ---

func (s *Server) handleOpenFile(p Packet, bodySize int64) error {
	mode := int32(p.Param0)
	prefetch := p.Param2
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	if s.ReadOnly && mode&OpenWrite != 0 {
		s.logf("read-only: refused write open of %s", host)
		return s.reply(p.respond(Success, fsTargetLocked))
	}

	flag := os.O_RDONLY
	switch {
	case mode&OpenRead != 0 && mode&OpenWrite != 0:
		flag = os.O_RDWR
	case mode&OpenWrite != 0:
		flag = os.O_WRONLY
	}
	f, err := os.OpenFile(host, flag, 0o644)
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}

	s.mu.Lock()
	h, ok := allocate(s.files)
	if ok {
		s.files[h] = &openFile{f: f}
	}
	s.mu.Unlock()
	if !ok {
		f.Close()
		return s.reply(p.respond(OutOfHandle))
	}
	s.logf("open %s -> handle %d", host, h)

	// The target can ask for the first slice of the file along with the handle,
	// which saves a round trip on the read it was about to make anyway.
	if prefetch > 0 {
		if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
			n := prefetch
			if fi.Size() < n {
				n = fi.Size()
			}
			buf := make([]byte, n)
			if got, err := f.ReadAt(buf, 0); err == nil && int64(got) == n {
				return s.replyBody(p.respond(Success, fsSuccess, int64(h), 1, fi.Size()), buf)
			}
		}
	}
	return s.reply(p.respond(Success, fsSuccess, int64(h)))
}

func (s *Server) handleCloseFile(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	h := int32(p.Param0)
	s.mu.Lock()
	f, ok := s.files[h]
	if ok {
		delete(s.files, h)
	}
	s.mu.Unlock()
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	f.f.Close()
	return s.reply(p.respond(Success))
}

func (s *Server) handleGetPriorityForFile(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	f, ok := s.file(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	return s.reply(p.respond(Success, int64(f.priority)))
}

func (s *Server) handleSetPriorityForFile(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	f, ok := s.file(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	s.mu.Lock()
	f.priority = int32(p.Param1)
	s.mu.Unlock()
	return s.reply(p.respond(Success))
}

func (s *Server) handleCreateFile(p Packet, bodySize int64) error {
	size := p.Param0
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	if size < 0 {
		return s.reply(p.respond(InvalidRequest))
	}
	// Creating over an existing file is an error here, not a truncation: the
	// target has a separate call for changing a file's size.
	f, err := os.OpenFile(host, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			f.Close()
			os.Remove(host)
			return s.reply(p.respond(Success, hostResult(err)))
		}
	}
	f.Close()
	s.logf("create %s (%d bytes)", host, size)
	return s.reply(p.respond(Success, fsSuccess))
}

func (s *Server) handleDeleteFile(p Packet, bodySize int64) error {
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	// Refuse to take a directory down a call meant for files, which would
	// otherwise remove an empty one silently.
	if fi, err := os.Stat(host); err == nil && fi.IsDir() {
		return s.reply(p.respond(Success, fsPathNotFound))
	}
	s.logf("delete %s", host)
	return s.reply(p.respond(Success, hostResult(os.Remove(host))))
}

func (s *Server) handleRenameFile(p Packet, bodySize int64) error {
	return s.rename(p, bodySize, false)
}

func (s *Server) handleRenameDirectory(p Packet, bodySize int64) error {
	return s.rename(p, bodySize, true)
}

// rename reads two paths whose lengths are in the parameters. BodySize is not
// the total here, which is the one place in this protocol where it does not
// describe what follows.
func (s *Server) rename(p Packet, _ int64, wantDir bool) error {
	fromRaw, err := s.readPath(p.Param0)
	if err != nil {
		return err
	}
	toRaw, err := s.readPath(p.Param1)
	if err != nil {
		return err
	}
	from, res := s.resolve(fromRaw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	to, res := s.resolve(toRaw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	if fi, err := os.Stat(from); err == nil && fi.IsDir() != wantDir {
		return s.reply(p.respond(Success, fsPathNotFound))
	}
	s.logf("rename %s -> %s", from, to)
	return s.reply(p.respond(Success, hostResult(os.Rename(from, to))))
}

func (s *Server) handleReadFile(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	offset, size := p.Param1, p.Param2
	f, ok := s.file(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	if offset < 0 || size < 0 || size > maxBody {
		return s.reply(p.respond(Success, fsOutOfRange))
	}
	buf := make([]byte, size)
	n, err := f.f.ReadAt(buf, offset)
	// A short read at the end of the file is a normal outcome, not a failure:
	// the target sees the count it actually got.
	if err != nil && !errors.Is(err, io.EOF) {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	return s.replyBody(p.respond(Success, fsSuccess), buf[:n])
}

func (s *Server) handleWriteFile(p Packet, bodySize int64) error {
	data, err := s.readBody(bodySize)
	if err != nil {
		return err
	}
	option, offset := p.Param1, p.Param2
	f, ok := s.file(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	if offset < 0 {
		return s.reply(p.respond(Success, fsOutOfRange))
	}
	if _, err := f.f.WriteAt(data, offset); err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	if option&WriteOptionFlush != 0 {
		if err := f.f.Sync(); err != nil {
			return s.reply(p.respond(Success, hostResult(err)))
		}
	}
	return s.reply(p.respond(Success, fsSuccess))
}

func (s *Server) handleFlushFile(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	f, ok := s.file(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	return s.reply(p.respond(Success, hostResult(f.f.Sync())))
}

func (s *Server) handleGetFileTimeStamp(p Packet, bodySize int64) error {
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	fi, err := os.Stat(host)
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	// Only the modification time is portable. Reporting it for all three is
	// honest in a way that inventing a creation time from the platform's
	// stat block would not be, and the target only compares them for change.
	t := fi.ModTime().Unix()
	return s.reply(p.respond(Success, fsSuccess, t, t, t))
}

func (s *Server) handleGetFileSize(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	f, ok := s.file(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	fi, err := f.f.Stat()
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	return s.reply(p.respond(Success, fsSuccess, fi.Size()))
}

func (s *Server) handleSetFileSize(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	if p.Param1 < 0 {
		return s.reply(p.respond(InvalidRequest))
	}
	f, ok := s.file(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	return s.reply(p.respond(Success, hostResult(f.f.Truncate(p.Param1))))
}

// handleReadFileLarge is ReadFile's bulk form: the target names a channel to
// receive the data on rather than waiting for it in this request's own
// reply, which is how it asks for more than a single response body can hold.
//
// The small reply here still carries the real byte count, exactly as
// ReadFile's does, and it goes out before a single byte moves on the bulk
// channel - a caller finds out how much it is getting the same way either
// form answers, just ahead of the transfer instead of concluding it.
func (s *Server) handleReadFileLarge(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	if s.Bulk == nil {
		s.logf("%s needs a bulk data channel, refusing (none configured)", p.Type)
		return s.reply(p.respond(InvalidRequest))
	}
	offset, size, channelID := p.Param1, p.Param2, uint16(p.Param3)
	f, ok := s.file(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	if offset < 0 || size < 0 {
		return s.reply(p.respond(Success, fsOutOfRange))
	}

	fi, err := f.f.Stat()
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	total := fi.Size() - offset
	if total < 0 {
		total = 0
	}
	if total > size {
		total = size
	}
	buf := make([]byte, total)
	n, err := f.f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	buf = buf[:n]

	if err := s.reply(p.respond(Success, fsSuccess, int64(len(buf)))); err != nil {
		return err
	}
	return s.sendBulk(p.Type, channelID, buf)
}

// handleWriteFileLarge is WriteFile's bulk form. Unlike the read side, the
// small reply here comes twice: Ready before the transfer, once the bulk
// channel is open and this side is actually listening on it, and the real
// result after the write completes. A target that only got one reply would
// not know whether "ready" also meant "done".
func (s *Server) handleWriteFileLarge(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	if s.Bulk == nil {
		s.logf("%s needs a bulk data channel, refusing (none configured)", p.Type)
		return s.reply(p.respond(InvalidRequest))
	}
	option, offset, size, channelID := p.Param1, p.Param2, p.Param3, uint16(p.Param4)
	f, ok := s.file(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	if offset < 0 || size < 0 {
		return s.reply(p.respond(Success, fsOutOfRange))
	}

	ch, err := s.Bulk.OpenBulkChannel(channelID)
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	defer ch.Close()

	if err := s.reply(p.respond(Ready)); err != nil {
		return err
	}

	if err := ch.ReceiveBulk(&offsetWriter{f: f.f, offset: offset}, size); err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	if option&WriteOptionFlush != 0 {
		if err := f.f.Sync(); err != nil {
			return s.reply(p.respond(Success, hostResult(err)))
		}
	}
	return s.reply(p.respond(Success, fsSuccess))
}

// handleReadDirectoryLarge is ReadDirectory's bulk form: same entry encoding
// as the plain one, just batched onto the bulk channel instead of bounded by
// how many DirectoryEntry structs fit in one reply body.
func (s *Server) handleReadDirectoryLarge(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	if s.Bulk == nil {
		s.logf("%s needs a bulk data channel, refusing (none configured)", p.Type)
		return s.reply(p.respond(InvalidRequest))
	}
	want := p.Param1
	channelID := uint16(p.Param2)
	d, ok := s.dir(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	if want < 0 {
		return s.reply(p.respond(InvalidRequest))
	}

	s.mu.Lock()
	entries, err := d.listing()
	if err != nil {
		s.mu.Unlock()
		return s.reply(p.respond(Success, hostResult(err)))
	}
	n := int64(len(entries) - d.next)
	if n > want {
		n = want
	}
	batch := entries[d.next : d.next+int(n)]
	d.next += int(n)
	s.mu.Unlock()

	body := make([]byte, DirectoryEntrySize*n)
	for i, e := range batch {
		encodeDirectoryEntry(body[i*DirectoryEntrySize:], e)
	}

	if err := s.reply(p.respond(Success, fsSuccess, n)); err != nil {
		return err
	}
	return s.sendBulk(p.Type, channelID, body)
}

// sendBulk opens a bulk channel, sends the whole payload, and closes it.
// Errors here are logged rather than turned into a protocol response,
// because the response for this request already went out - the small reply
// is what carries success or failure at the application level, and by the
// time this runs the target has already been told which one it got.
func (s *Server) sendBulk(t Type, channelID uint16, buf []byte) error {
	ch, err := s.Bulk.OpenBulkChannel(channelID)
	if err != nil {
		s.logf("%s: bulk channel %d: %v", t, channelID, err)
		return nil
	}
	defer ch.Close()
	if err := ch.SendBulk(buf); err != nil {
		s.logf("%s: bulk channel %d: %v", t, channelID, err)
	}
	return nil
}

// offsetWriter adapts a file handle to io.Writer for ReceiveBulk, which only
// knows how to write sequentially - WriteFileLarge's data starts partway
// through the file, at the offset the request named, not at its beginning.
type offsetWriter struct {
	f      *os.File
	offset int64
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.offset)
	w.offset += int64(n)
	return n, err
}

// --- directories ---

func (s *Server) handleOpenDirectory(p Packet, bodySize int64) error {
	mode := int32(p.Param0)
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	f, err := os.Open(host)
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	if fi, err := f.Stat(); err != nil || !fi.IsDir() {
		f.Close()
		return s.reply(p.respond(Success, fsPathNotFound))
	}

	s.mu.Lock()
	h, ok := allocate(s.dirs)
	if ok {
		s.dirs[h] = &openDir{f: f, path: host, mode: mode}
	}
	s.mu.Unlock()
	if !ok {
		f.Close()
		return s.reply(p.respond(OutOfHandle))
	}
	s.logf("open dir %s -> handle %d", host, h)
	return s.reply(p.respond(Success, fsSuccess, int64(h)))
}

func (s *Server) handleCloseDirectory(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	h := int32(p.Param0)
	s.mu.Lock()
	d, ok := s.dirs[h]
	if ok {
		delete(s.dirs, h)
	}
	s.mu.Unlock()
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	d.f.Close()
	return s.reply(p.respond(Success))
}

func (s *Server) handleGetPriorityForDirectory(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	d, ok := s.dir(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	return s.reply(p.respond(Success, int64(d.priority)))
}

func (s *Server) handleSetPriorityForDirectory(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	d, ok := s.dir(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	s.mu.Lock()
	d.priority = int32(p.Param1)
	s.mu.Unlock()
	return s.reply(p.respond(Success))
}

func (s *Server) handleCreateDirectory(p Packet, bodySize int64) error {
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	s.logf("mkdir %s", host)
	return s.reply(p.respond(Success, hostResult(os.Mkdir(host, 0o755))))
}

func (s *Server) handleDeleteDirectory(p Packet, bodySize int64) error {
	recursive := p.Param0 != 0
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	// Never let a recursive delete land on the root itself. The target asking
	// for it would be a bug on its side, and honouring it would empty the
	// directory this server was pointed at.
	if filepath.Clean(host) == filepath.Clean(s.Root) {
		return s.reply(p.respond(Success, fsTargetLocked))
	}
	if fi, err := os.Stat(host); err != nil || !fi.IsDir() {
		return s.reply(p.respond(Success, fsPathNotFound))
	}
	s.logf("rmdir %s (recursive=%v)", host, recursive)
	if recursive {
		return s.reply(p.respond(Success, hostResult(os.RemoveAll(host))))
	}
	return s.reply(p.respond(Success, hostResult(os.Remove(host))))
}

// listing loads a directory's entries once and filters them by the mode the
// handle was opened with, so successive reads walk one stable list.
func (d *openDir) listing() ([]fs.DirEntry, error) {
	if d.loaded {
		return d.entries, nil
	}
	all, err := os.ReadDir(d.path)
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name() < all[j].Name() })
	for _, e := range all {
		if e.IsDir() && d.mode&ListDirectories == 0 {
			continue
		}
		if !e.IsDir() && d.mode&ListFiles == 0 {
			continue
		}
		d.entries = append(d.entries, e)
	}
	d.loaded = true
	return d.entries, nil
}

func (s *Server) handleReadDirectory(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	want := p.Param1
	d, ok := s.dir(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	if want < 0 || want > maxBody/DirectoryEntrySize {
		return s.reply(p.respond(InvalidRequest))
	}

	s.mu.Lock()
	entries, err := d.listing()
	if err != nil {
		s.mu.Unlock()
		return s.reply(p.respond(Success, hostResult(err)))
	}
	n := int64(len(entries) - d.next)
	if n > want {
		n = want
	}
	batch := entries[d.next : d.next+int(n)]
	d.next += int(n)
	s.mu.Unlock()

	body := make([]byte, DirectoryEntrySize*n)
	for i, e := range batch {
		encodeDirectoryEntry(body[i*DirectoryEntrySize:], e)
	}
	return s.replyBody(p.respond(Success, fsSuccess, n), body)
}

// encodeDirectoryEntry writes one nn::fs::DirectoryEntry. A name too long for
// the field is truncated rather than dropped, since the alternative is failing
// a whole listing over one entry.
func encodeDirectoryEntry(out []byte, e fs.DirEntry) {
	name := []byte(e.Name())
	if len(name) > entryNameSize-1 {
		name = name[:entryNameSize-1]
	}
	copy(out, name)
	if e.IsDir() {
		out[entryOffType] = EntryDirectory
		return
	}
	out[entryOffType] = EntryFile
	if fi, err := e.Info(); err == nil {
		binary.LittleEndian.PutUint64(out[entryOffSize:], uint64(fi.Size()))
	}
}

func (s *Server) handleGetEntryCount(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	d, ok := s.dir(int32(p.Param0))
	if !ok {
		return s.reply(p.respond(InvalidHandle))
	}
	s.mu.Lock()
	entries, err := d.listing()
	s.mu.Unlock()
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	return s.reply(p.respond(Success, fsSuccess, int64(len(entries))))
}

// --- host information ---

func (s *Server) handleGetWorkingDirectory(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	dir := []byte(filepath.ToSlash(s.Root))
	// Param0 is the target's buffer size, and zero means it did not say.
	if p.Param0 != 0 && p.Param0 < int64(len(dir)) {
		return s.reply(p.respond(NotEnoughBuffer))
	}
	return s.replyBody(p.respond(Success), dir)
}

func (s *Server) handleGetWorkingDirectorySize(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	return s.reply(p.respond(Success, int64(len(filepath.ToSlash(s.Root)))))
}

// handleGetCaseSensitivePath answers with the path as the host actually spells
// it. Resolving the real casing needs a directory walk on a case-insensitive
// filesystem; the cleaned path is returned instead, which is correct wherever
// the target already sent the right case and is what a case-sensitive host
// would answer anyway.
func (s *Server) handleGetCaseSensitivePath(p Packet, bodySize int64) error {
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	out := append([]byte(filepath.ToSlash(host)), 0)
	return s.replyBody(p.respond(Success, fsSuccess), out)
}

func (s *Server) handleGetDiskFreeSpace(p Packet, bodySize int64) error {
	raw, err := s.readPath(bodySize)
	if err != nil {
		return err
	}
	host, res := s.resolve(raw)
	if res != fsSuccess {
		return s.reply(p.respond(Success, res))
	}
	free, total, err := diskFreeSpace(host)
	if err != nil {
		return s.reply(p.respond(Success, hostResult(err)))
	}
	// The third value is the volume's total free space, which differs from the
	// first only where per-user quotas apply.
	return s.reply(p.respond(Success, fsSuccess, free, total, free))
}

// handleGetFileSystemAttribute reports that the host imposes none of the
// length limits the target asks about.
//
// Every value comes with its own "has value" flag, so leaving them all unset is
// a real answer: the target then uses its own limits instead of a number this
// side would have had to invent.
func (s *Server) handleGetFileSystemAttribute(p Packet, bodySize int64) error {
	if err := s.discard(bodySize); err != nil {
		return err
	}
	return s.replyBody(p.respond(Success, fsSuccess), make([]byte, fileSystemAttributeSize))
}

// fileSystemAttributeSize is thirteen limits followed by thirteen flags, as
// int32s.
const fileSystemAttributeSize = 26 * 4
