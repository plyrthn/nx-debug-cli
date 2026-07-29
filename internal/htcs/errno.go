package htcs

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// Errno is the target's own errno space. It is not the host's: the numbers
// are HTCS's own and only coincidentally resemble anything else, so they are
// never derived from a host error value by arithmetic.
type Errno int64

const (
	ENONE         Errno = 0
	EACCES        Errno = 2
	EADDRINUSE    Errno = 3
	EADDRNOTAVAIL Errno = 4
	EAGAIN        Errno = 6
	EALREADY      Errno = 7
	EBADF         Errno = 8
	EBUSY         Errno = 10
	ECONNABORTED  Errno = 13
	ECONNREFUSED  Errno = 14
	ECONNRESET    Errno = 15
	EDESTADDRREQ  Errno = 17
	EFAULT        Errno = 21
	EINPROGRESS   Errno = 26
	EINTR         Errno = 27
	EINVAL        Errno = 28
	EIO           Errno = 29
	EISCONN       Errno = 30
	EMFILE        Errno = 33
	EMSGSIZE      Errno = 35
	ENETDOWN      Errno = 38
	ENETRESET     Errno = 39
	ENOBUFS       Errno = 42
	ENOMEM        Errno = 49
	ENOTCONN      Errno = 56
	ETIMEDOUT     Errno = 76
	EUNKNOWN      Errno = 79
)

var errnoNames = map[Errno]string{
	ENONE: "ENONE", EACCES: "EACCES", EADDRINUSE: "EADDRINUSE",
	EADDRNOTAVAIL: "EADDRNOTAVAIL", EAGAIN: "EAGAIN", EALREADY: "EALREADY",
	EBADF: "EBADF", EBUSY: "EBUSY", ECONNABORTED: "ECONNABORTED",
	ECONNREFUSED: "ECONNREFUSED", ECONNRESET: "ECONNRESET",
	EDESTADDRREQ: "EDESTADDRREQ", EFAULT: "EFAULT", EINPROGRESS: "EINPROGRESS",
	EINTR: "EINTR", EINVAL: "EINVAL", EIO: "EIO", EISCONN: "EISCONN",
	EMFILE: "EMFILE", EMSGSIZE: "EMSGSIZE", ENETDOWN: "ENETDOWN",
	ENETRESET: "ENETRESET", ENOBUFS: "ENOBUFS", ENOMEM: "ENOMEM",
	ENOTCONN: "ENOTCONN", ETIMEDOUT: "ETIMEDOUT", EUNKNOWN: "EUNKNOWN",
}

func (e Errno) String() string {
	if n, ok := errnoNames[e]; ok {
		return n
	}
	return fmt.Sprintf("errno %d", int64(e))
}

// hostErrnos maps the host errors worth distinguishing onto the target's
// space. Anything not listed becomes EUNKNOWN rather than being guessed at:
// a wrong-but-plausible errno sends the target down the wrong recovery path,
// which is harder to diagnose than an honest "something failed".
var hostErrnos = []struct {
	match func(error) bool
	errno Errno
}{
	{func(err error) bool { return errors.Is(err, syscall.ECONNRESET) }, ECONNRESET},
	{func(err error) bool { return errors.Is(err, syscall.ECONNABORTED) }, ECONNABORTED},
	{func(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }, ECONNREFUSED},
	{func(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }, EADDRINUSE},
	{func(err error) bool { return errors.Is(err, syscall.EADDRNOTAVAIL) }, EADDRNOTAVAIL},
	{func(err error) bool { return errors.Is(err, os.ErrDeadlineExceeded) }, EAGAIN},
	{func(err error) bool { return errors.Is(err, net.ErrClosed) }, EINTR},
	{func(err error) bool {
		var ne net.Error
		return errors.As(err, &ne) && ne.Timeout()
	}, ETIMEDOUT},
}

// toErrno translates a host error. nil is ENONE.
func toErrno(err error) Errno {
	if err == nil {
		return ENONE
	}
	for _, m := range hostErrnos {
		if m.match(err) {
			return m.errno
		}
	}
	return EUNKNOWN
}
