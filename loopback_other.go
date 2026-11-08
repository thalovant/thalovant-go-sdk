//go:build !windows

package thalovant

import "syscall"

// loopbackListenControl is nil here: outside Windows a bound port is never
// shared with another process's socket unless both ask for SO_REUSEPORT,
// which the browser sign-in's socket does not.
var loopbackListenControl func(network, address string, raw syscall.RawConn) error
