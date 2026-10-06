package ports

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
)

// CheckAvailable probes wildcard IPv4 and IPv6 binds, catching loopback,
// wildcard, and interface-specific listeners. IPv6 may be disabled on the host.
// This is a point-in-time check; service startup must check again in a later milestone.
func CheckAvailable(port int) error {
	// Go enables SO_REUSEADDR by default. On macOS that can let a wildcard
	// probe coexist with an existing loopback listener, hiding a conflict.
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var socketErr error
		if err := c.Control(func(fd uintptr) {
			socketErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
		}); err != nil {
			return err
		}
		return socketErr
	}}
	for _, addr := range []struct{ network, host string }{{"tcp4", "0.0.0.0"}, {"tcp6", "::"}} {
		listener, err := lc.Listen(context.Background(), addr.network, net.JoinHostPort(addr.host, strconv.Itoa(port)))
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				return fmt.Errorf("%w: TCP %d", ErrOccupied, port)
			}
			if addr.network == "tcp6" && (errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL)) {
				continue
			}
			return err
		}
		if err := listener.Close(); err != nil {
			return err
		}
	}
	return nil
}
