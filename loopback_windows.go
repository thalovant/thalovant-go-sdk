//go:build windows

package thalovant

import "syscall"

// soExclusiveAddrUse is SO_EXCLUSIVEADDRUSE, defined in winsock2.h as
// ((int)(~SO_REUSEADDR)).
const soExclusiveAddrUse = ^syscall.SO_REUSEADDR

// loopbackListenControl makes the browser sign-in's socket exclusive before
// it is bound, so that no other process can bind the same port and take the
// callback. Windows otherwise lets a socket with SO_REUSEADDR share a port
// in use.
func loopbackListenControl(_, _ string, raw syscall.RawConn) error {
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
	}); err != nil {
		return err
	}
	return sockErr
}
