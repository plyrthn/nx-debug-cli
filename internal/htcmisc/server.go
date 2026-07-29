package htcmisc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Server answers the target's HTCMISC requests on the server channel.
//
// Everything here is a question about the *host*: what is in its environment,
// what its clock says, where its working directory is. The target asks because
// a program running on it was built expecting a development host to be there,
// and getting no answer is not the same as getting a refusal.
type Server struct {
	rw io.ReadWriter

	// Log, when set, receives a line per request. Trace additionally receives
	// the decoded packets.
	Log   func(string)
	Trace func(string)

	// Status is called when the target reports a status change. The target
	// pushes these unprompted, and it is the only place the host learns the
	// target's own view of the connection.
	Status func(int64)

	// Env resolves an environment variable for the target. Returning false
	// means "no such variable", which is a real answer and different from an
	// error. Defaults to the host's own environment.
	Env func(name string) (string, bool)

	// WorkingDir is what the target is told the host's working directory is.
	// Defaults to the process's own.
	WorkingDir func() (string, error)

	mu   sync.Mutex
	done chan struct{}
	err  error
	once sync.Once
}

// NewServer wraps a channel stream.
func NewServer(rw io.ReadWriter) *Server {
	return &Server{rw: rw, done: make(chan struct{})}
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
		if bodySize > 0 {
			p.Body = make([]byte, bodySize)
			if _, err := io.ReadFull(s.rw, p.Body); err != nil {
				s.stop(err)
				return
			}
		}
		if s.Trace != nil {
			s.Trace("<- " + p.String())
		}
		if p.Category != Request {
			// Only the client channel expects responses. One arriving here is
			// a protocol error rather than something to quietly skip.
			s.stop(fmt.Errorf("htcmisc: server channel got a %s", p.Category))
			return
		}
		if err := s.handle(p); err != nil {
			s.stop(err)
			return
		}
	}
}

func (s *Server) reply(p Packet) error {
	if s.Trace != nil {
		s.Trace("-> " + p.String())
	}
	_, err := s.rw.Write(p.Encode())
	return err
}

// handlers maps each request to what answers it. A request with no entry gets
// an explicit InvalidRequest rather than silence, so an unimplemented
// operation shows up as a refusal on the target instead of a hang.
var handlers = map[Type]func(*Server, Packet) error{
	GetEnvironmentVariable:       (*Server).handleGetEnv,
	GetEnvironmentVariableLength: (*Server).handleGetEnvLength,
	SetTargetStatus:              (*Server).handleSetTargetStatus,
	SyncTime:                     (*Server).handleSyncTime,
	GetWorkingDirectory:          (*Server).handleGetWorkingDir,
	GetWorkingDirectorySize:      (*Server).handleGetWorkingDirSize,
	RunOnHost:                    (*Server).handleRunOnHost,
}

func (s *Server) handle(p Packet) error {
	h, ok := handlers[p.Type]
	if !ok {
		s.logf("unhandled %s", p.Type)
		return s.reply(p.respond(InvalidRequest, nil))
	}
	return h(s, p)
}

// env resolves a variable through the override, or the host's environment.
func (s *Server) env(name string) (string, bool) {
	if s.Env != nil {
		return s.Env(name)
	}
	return os.LookupEnv(name)
}

func (s *Server) handleGetEnv(p Packet) error {
	name := string(p.Body)
	value, ok := s.env(name)
	if !ok {
		s.logf("env %q not set", name)
		return s.reply(p.respond(InvalidRequest, nil))
	}
	s.logf("env %q", name)
	return s.reply(p.respond(Success, []byte(value)))
}

func (s *Server) handleGetEnvLength(p Packet) error {
	name := string(p.Body)
	value, ok := s.env(name)
	if !ok {
		return s.reply(p.respond(InvalidRequest, nil))
	}
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, uint64(len(value)))
	return s.reply(p.respond(Success, body))
}

func (s *Server) handleSetTargetStatus(p Packet) error {
	s.logf("target status %d", p.Param0)
	if s.Status != nil {
		s.Status(p.Param0)
	}
	return s.reply(p.respond(Success, nil))
}

func (s *Server) handleSyncTime(p Packet) error {
	switch p.Param0 {
	case SyncByResponse:
		// The host's clock, as the target's own formatter expects it.
		now := time.Now().UTC().Format("20060102150405")
		s.logf("sync time -> %s", now)
		return s.reply(p.respond(Success, []byte(now)))
	case SyncByDevMenuCommand:
		// The other method asks the host to set the clock by running a
		// command on the target through the command shell. Reporting failure
		// is honest: the target then knows the clock was not set, rather than
		// believing a sync that never happened.
		s.logf("sync time by devmenu command, not supported")
		return s.reply(p.respond(UnknownError, nil))
	default:
		return s.reply(p.respond(InvalidRequest, nil))
	}
}

func (s *Server) workingDir() (string, error) {
	if s.WorkingDir != nil {
		return s.WorkingDir()
	}
	return os.Getwd()
}

func (s *Server) handleGetWorkingDir(p Packet) error {
	dir, err := s.workingDir()
	if err != nil {
		return s.reply(p.respond(UnknownError, nil))
	}
	return s.reply(p.respond(Success, []byte(dir)))
}

func (s *Server) handleGetWorkingDirSize(p Packet) error {
	dir, err := s.workingDir()
	if err != nil {
		return s.reply(p.respond(UnknownError, nil))
	}
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, uint64(len(dir)))
	return s.reply(p.respond(Success, body))
}

// handleRunOnHost refuses. The target is asking to execute a command line on
// this machine, and doing that silently on behalf of whatever is running on a
// devkit is not something this build does by default. The exit code goes in
// Param0, so a non-zero one is a well-formed "it failed".
func (s *Server) handleRunOnHost(p Packet) error {
	s.logf("run-on-host refused: %q", string(p.Body))
	return s.reply(Packet{
		Version:  p.Version,
		Category: Response,
		Type:     p.Type,
		TaskID:   p.TaskID,
		Param0:   1,
	})
}

// ErrClosed is returned by Err when the channel shut down cleanly.
var ErrClosed = errors.New("htcmisc: channel closed")
