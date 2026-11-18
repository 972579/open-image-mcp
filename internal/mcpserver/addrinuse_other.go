//go:build !windows

package mcpserver

import (
	"errors"
	"syscall"
)

// isAddrInUse reports whether a listen error means the port is taken.
func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
