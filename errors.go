package thalovant

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
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

// The hub's codes for the three kinds of refusal that arrive as
// hive.policy.denied, each needing something different said about it.
const (
	// PolicyCodeACL is an allow-list refusal: ask whoever manages the
	// connection to allow the type; Allowed lists what it may send.
	PolicyCodeACL = "acl_disallowed_type"
	// PolicyCodeQuotaExceeded is a spent allowance: wait, or raise the
	// limit; Quota carries the numbers.
	PolicyCodeQuotaExceeded = "intent_quota_exceeded"
	// PolicyCodeBackendUnavailable is a hub whose agent bus is down, which
	// nothing the caller does will fix.
	PolicyCodeBackendUnavailable = "backend_unavailable"
)

// Quota is the numbers behind a refusal that is a spent allowance, not a
// policy. The intent-quota policy sends which counter ran out ("daily",
// "monthly"), what it allows, how much was used, and how many seconds until
// it resets. Without them a caller can only say "refused", which is what an
// app showed somebody who had simply used up the day.
type Quota struct {
	Period string
	Limit  int64
	Used   int64
	// ResetAfter is seconds until the counter resets, or 0 when the hub did
	// not say.
	ResetAfter int64
}

// PolicyDeniedError reports that the hub refused a message, the instant it
// did. The hub sends hive.policy.denied as soon as it refuses; returning this
// saves the caller a timeout and says which kind of refusal it was.
//
// It wraps ErrRuntime, so errors.Is(err, ErrRuntime) holds, and it is
// retrieved with errors.As:
//
//	var denied *thalovant.PolicyDeniedError
//	if errors.As(err, &denied) && denied.Quota != nil {
//		fmt.Println(denied.Quota.Used, "of", denied.Quota.Limit)
//	}
type PolicyDeniedError struct {
	// DeniedType is the message type the hub refused, such as
	// "recognizer_loop:utterance".
	DeniedType string
	// Code is the hub's refusal code: PolicyCodeACL,
	// PolicyCodeQuotaExceeded or PolicyCodeBackendUnavailable.
	Code string
	// Reason is the hub's human-readable explanation, when it gave one.
	Reason string
	// Allowed lists the message types the connection may publish, when the
	// hub said.
	Allowed []string
	// Quota carries the numbers behind a spent allowance; nil for any other
	// refusal.
	Quota *Quota
}

func (e *PolicyDeniedError) Error() string {
	// Advice follows the kind of refusal. Telling somebody who used up their
	// day to "allow this connection to publish recognizer_loop:utterance"
	// sent them to a settings page that could not help.
	if q := e.Quota; q != nil {
		used := "all"
		if q.Limit > 0 {
			used = fmt.Sprintf("%d of %d", q.Used, q.Limit)
		}
		period := ""
		if q.Period != "" {
			period = " " + q.Period
		}
		resets := ""
		if q.ResetAfter > 0 {
			resets = fmt.Sprintf("; it resets in %ds", q.ResetAfter)
		}
		return fmt.Sprintf("%v: the hub refused %q: %s%s questions used%s.", ErrRuntime, e.DeniedType, used, period, resets)
	}
	if e.Code == PolicyCodeBackendUnavailable {
		detail := ""
		if e.Reason != "" {
			detail = ": " + e.Reason
		}
		return fmt.Sprintf("%v: the hub could not reach its assistant%s. Try again shortly.", ErrRuntime, detail)
	}
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

// UnansweredError reports that the hub understood a question and has nothing
// for it. ovos.intent.unmatched (complete_intent_failure from older hubs) is
// neither a refusal nor a fault; as a bare runtime error a caller could only
// report that something failed. It wraps ErrRuntime.
type UnansweredError struct {
	// Said is the hub's own words, when it sent any.
	Said string
}

func (e *UnansweredError) Error() string {
	if e.Said != "" {
		return fmt.Sprintf("%v: %s", ErrRuntime, e.Said)
	}
	return fmt.Sprintf("%v: the hub has no skill that answers this", ErrRuntime)
}

// Unwrap makes an UnansweredError match ErrRuntime under errors.Is.
func (e *UnansweredError) Unwrap() error { return ErrRuntime }

// policyDeniedFromEvent reads a hive.policy.denied event. The policy's own
// detail rides nested under data.data (hivemind-core _send_policy_denied:
// "data": verdict.data):
//
//	{denied_type, code, reason, data: {allowed | period, limit, used, reset_after}}
func policyDeniedFromEvent(event Event) *PolicyDeniedError {
	inner := mapValue(event.Data["data"])
	// Only non-blank strings, trimmed: a number, a null or a blank in the list
	// is not a message type an operator can allow.
	var allowed []string
	for _, item := range anySlice(inner["allowed"]) {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			allowed = append(allowed, strings.TrimSpace(text))
		}
	}
	code := stringValue(event.Data["code"])
	var quota *Quota
	if code == PolicyCodeQuotaExceeded {
		period, _ := inner["period"].(string)
		quota = &Quota{
			Period:     period,
			Limit:      wholeCount(inner["limit"]),
			Used:       wholeCount(inner["used"]),
			ResetAfter: wholeCount(inner["reset_after"]),
		}
	}
	return &PolicyDeniedError{
		DeniedType: stringValue(event.Data["denied_type"]),
		Code:       code,
		Reason:     stringValue(event.Data["reason"]),
		Allowed:    allowed,
		Quota:      quota,
	}
}

// wholeCount reads a whole, non-negative count from the wire, or 0: never a
// bool, never a guess. encoding/json decodes every number in a map[string]any
// as float64, so a whole float is a count and a fractional one is not. A
// negative limit, usage or reset time is not something a policy can mean, and
// passing one through would have an app say "-1 of -5 questions used".
func wholeCount(raw any) int64 {
	return max(signedCount(raw), 0)
}

func signedCount(raw any) int64 {
	switch value := raw.(type) {
	case float64:
		if value == math.Trunc(value) && !math.IsInf(value, 0) {
			return int64(value)
		}
	case int:
		return int64(value)
	case int64:
		return value
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return n
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// failureError is the typed error an ask returns for the failure event it
// ended on: a refusal, a question the hub has nothing for, and a fault need
// three different sentences, and a bare runtime error allowed only one.
func failureError(event Event) error {
	switch event.Name {
	case EventPolicyDenied:
		return policyDeniedFromEvent(event)
	case EventIntentUnmatched, EventIntentFailure:
		said := stringValue(event.Data["reason"])
		if said == "" {
			said = stringValue(event.Data["error"])
		}
		return &UnansweredError{Said: strings.TrimSpace(said)}
	}
	return fmt.Errorf("%w: %s", ErrRuntime, event.Name)
}

// refusalBelongsToAsk decides whether a hive.policy.denied is this ask's to
// return. A denial carrying a request id is judged by it, like any reply. The
// hub builds its denials with source and destination context only, so the
// usual one carries none and names the refused type instead: enough when this
// ask is the only utterance the client has out, a guess otherwise -- and a
// wrong guess ends a question the hub never refused. sendsInFlight counts
// fire-and-forget utterances still inside untrackedUtteranceGrace: they have
// nothing to wait on, but a refusal of one could land while this ask waits.
// The shared refusal vectors pin every case.
func refusalBelongsToAsk(requestID, ownRequestID, deniedType string, asksInFlight, queriesInFlight, sendsInFlight int) bool {
	if requestID != "" {
		return requestID == ownRequestID
	}
	return deniedType == EventRecognizerLoopUtterance && asksInFlight == 1 && queriesInFlight == 0 && sendsInFlight == 0
}

// untrackedUtteranceGrace is how long a fire-and-forget utterance counts as
// possibly still being refused. Denials come back as fast as the hub admits a
// message -- milliseconds -- so this is generous on purpose: a wrong "in
// flight" only costs an ask the deadline it always had, where a wrong "not in
// flight" ends a question the hub never refused. The shared refusal vectors
// name it (untracked_grace_seconds), so every SDK uses the same window.
const untrackedUtteranceGrace = 10 * time.Second

// APIError preserves the HTTP status while continuing to match ErrAPI.
type APIError struct {
	StatusCode int
	Detail     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%v: HTTP %d: %s", ErrAPI, e.StatusCode, e.Detail)
}
func (e *APIError) Unwrap() error { return ErrAPI }
