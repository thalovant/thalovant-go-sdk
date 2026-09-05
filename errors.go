package thalovant

import (
	"errors"
	"fmt"
)

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

// PolicyDeniedError reports that the hub refused a message type this
// connection may not publish. The hub answers hive.policy.denied at once,
// naming the type and the list it does allow; returning this error saves the
// caller a timeout and tells the operator exactly what to add to the
// connection's allow-list.
//
// It wraps ErrRuntime, so errors.Is(err, ErrRuntime) holds, and it is
// retrieved with errors.As:
//
//	var denied *thalovant.PolicyDeniedError
//	if errors.As(err, &denied) {
//		fmt.Println(denied.DeniedType, denied.Allowed)
//	}
type PolicyDeniedError struct {
	// DeniedType is the message type the hub refused, such as
	// "ovos.intent.list".
	DeniedType string
	// Code is the hub's refusal code, "acl_disallowed_type" for a type outside
	// the connection's allow-list.
	Code string
	// Reason is the hub's human-readable explanation, when it gave one.
	Reason string
	// Allowed lists the message types the connection may publish, when the
	// hub said.
	Allowed []string
}

func (e *PolicyDeniedError) Error() string {
	detail := e.Reason
	if detail == "" {
		detail = e.Code
	}
	if detail == "" {
		detail = "refused by the hub's policy"
	}
	return fmt.Sprintf(
		"%v: the hub refused %q: %s. Allow this connection to publish %q in the dashboard's connection settings.",
		ErrRuntime, e.DeniedType, detail, e.DeniedType,
	)
}

// Unwrap makes a PolicyDeniedError match ErrRuntime under errors.Is, the same
// way the hub's other refusals do.
func (e *PolicyDeniedError) Unwrap() error {
	return ErrRuntime
}

// policyDeniedFromEvent reads a hive.policy.denied event:
//
//	{denied_type, code, reason, data: {msg_type, allowed}}
func policyDeniedFromEvent(event Event) *PolicyDeniedError {
	inner := mapValue(event.Data["data"])
	var allowed []string
	for _, item := range anySlice(inner["allowed"]) {
		if text, ok := item.(string); ok {
			allowed = append(allowed, text)
		}
	}
	return &PolicyDeniedError{
		DeniedType: stringValue(event.Data["denied_type"]),
		Code:       stringValue(event.Data["code"]),
		Reason:     stringValue(event.Data["reason"]),
		Allowed:    allowed,
	}
}
