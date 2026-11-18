//go:build windows

package mcpserver

import (
	"errors"
	"syscall"
)

// wsaeaddrinuse is Winsock's "address already in use" (WSAEADDRINUSE).
const wsaeaddrinuse = syscall.Errno(10048)

// isAddrInUse reports whether a listen error means the port is taken.
func isAddrInUse(err error) bool {
	return errors.Is(err, wsaeaddrinuse) || errors.Is(err, syscall.EADDRINUSE)
}
