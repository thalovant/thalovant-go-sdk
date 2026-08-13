package thalovant

import "errors"

var (
	ErrIdentity   = errors.New("thalovant identity error")
	ErrConnection = errors.New("thalovant connection error")
	ErrTimeout    = errors.New("thalovant timeout")
	ErrRuntime    = errors.New("thalovant runtime error")
	ErrAPI        = errors.New("thalovant api error")
	ErrProtocol   = errors.New("thalovant unsupported protocol")

	// ErrDeviceAccessDenied reports that the browser device sign-in request
	// was denied by the user.
	ErrDeviceAccessDenied = errors.New("thalovant device sign-in denied")
	// ErrDeviceCodeExpired reports that the device sign-in code expired
	// before it was approved.
	ErrDeviceCodeExpired = errors.New("thalovant device sign-in code expired")
)
