package thalovant

import "errors"

var (
	ErrIdentity   = errors.New("thalovant identity error")
	ErrConnection = errors.New("thalovant connection error")
	ErrTimeout    = errors.New("thalovant timeout")
	ErrRuntime    = errors.New("thalovant runtime error")
	ErrAPI        = errors.New("thalovant api error")
	ErrProtocol   = errors.New("thalovant unsupported protocol")
)
