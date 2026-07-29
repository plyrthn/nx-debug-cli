package htc

import (
	"context"
	"fmt"
	"net"

	"github.com/plyrthn/nx-debug-cli/internal/targetlog"
)

// LogManagerPortName is the target port its log stream comes out of.
const LogManagerPortName = "iywys@$LogManager"

// DialTargetLog opens the target's log stream and returns a reader over it.
func DialTargetLog(ctx context.Context, serial string) (*targetlog.Reader, net.Conn, error) {
	addr, err := resolvePortAddr(ctx, serial, LogManagerPortName)
	if err != nil {
		return nil, nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("htc: dial log manager %s: %w", addr, err)
	}
	return targetlog.NewReader(conn), conn, nil
}
