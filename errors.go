package thalovant

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	// ErrDeviceLoginPending reports that nobody has approved a device
	// sign-in yet. PollDeviceLogin returns it as a *DeviceLoginPendingError,
	// whose Interval says when to ask again.
	ErrDeviceLoginPending = errors.New("thalovant device sign-in pending")

	// ErrAuth matches an *APIError that signing in again is the way out of:
	// HTTP 401 (a token unknown, expired or revoked), 423 (a locked account),
	// or 403 whose detail is "Insufficient scopes".
	ErrAuth = errors.New("thalovant authentication refused")
	// ErrPlan matches an *APIError the account's plan refused: HTTP 402, or
	// 403 with code "plan_limit". Problem carries the plan's numbers.
	ErrPlan = errors.New("thalovant plan refused")
	// ErrAlreadyLinked matches an *APIError saying the hub already holds the
	// one link of its kind: HTTP 409 with code "home_assistant_already_linked".
	// LinkedClientID names the connection that holds it.
	ErrAlreadyLinked = errors.New("thalovant hub already linked")
	// ErrUnsupportedConnectionType matches an *UnsupportedConnectionTypeError:
	// the API could not make a connection of the kind asked for.
	ErrUnsupportedConnectionType = errors.New("thalovant unsupported connection type")
	// ErrHubRefused reports that a hub turned the connection's credentials
	// away. It always travels with ErrConnection. A new connection is refused
	// until its hub admits it, so a HubSession's Run treats it as "not yet"
	// for a grace period before returning it.
	ErrHubRefused = errors.New("thalovant hub refused the credentials")
	// ErrHubKeyChanged reports that a hub's Noise static key is not the one
	// pinned for it: the hub was replaced or reinstalled, or another machine
	// answers at its address. It always travels with ErrConnection. It is not
	// a refusal, and retrying cannot change it; the pin is never replaced
	// automatically (see ForgetNoisePin).
	ErrHubKeyChanged = errors.New("thalovant hub key changed")
	// ErrAPIUnreachable reports a control-plane request that never got an
	// answer: DNS, the connection, TLS, a proxy. It always travels with ErrAPI
	// and ErrConnection, and it says nothing about what the API would have
	// answered.
	ErrAPIUnreachable = errors.New("thalovant api unreachable")
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
		if q.Limit == 0 && q.Used == 0 && q.ResetAfter == 0 && q.Period == "" {
			// Refused on a quota, with none of the numbers. "All questions
			// used" would be inventing one.
			return fmt.Sprintf("%v: the hub refused %q: a quota has run out.", ErrRuntime, e.DeniedType)
		}
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
// maxCount is the largest whole number every JSON decoder carries exactly.
// Above it a decoder backed by a double can no longer tell one whole number
// from the next, so two SDKs would report different allowances for the same
// denial -- and a count nobody can agree on is worse than none.
const maxCount int64 = 1<<53 - 1

func wholeCount(raw any) int64 {
	value := signedCount(raw)
	if value < 0 || value > maxCount {
		return 0
	}
	return value
}

func signedCount(raw any) int64 {
	switch value := raw.(type) {
	case float64:
		// Whole, and inside what an int64 holds: 1e20 is neither a count a
		// policy can have meant nor a number this conversion can survive.
		// Exclusive at the top: math.MaxInt64 as a float64 rounds up to 1<<63,
		// which int64 cannot represent, and Go leaves that conversion's result
		// to the implementation.
		if value == math.Trunc(value) && !math.IsInf(value, 0) &&
			value >= math.MinInt64 && value < -float64(math.MinInt64) {
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
		// What the person said: both names carry the input, and that is what a
		// caller shows. `reason` is not on these events at all, so reading it
		// left Said empty.
		return &UnansweredError{Said: strings.TrimSpace(event.Text())}
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

// APIError is a control-plane request the API answered with an error status.
// It preserves the HTTP status while continuing to match ErrAPI.
//
// Error() prints one bounded line for display, and that line can be
// shortened, so it is never where to read what the API said. That rides
// beside it: Code to branch on, ProblemDetail for the API's whole sentence,
// and Problem for every structured field of the body.
//
//	var apiErr *thalovant.APIError
//	if errors.As(err, &apiErr) && apiErr.Code == "platform_image_required" {
//		fmt.Println(apiErr.ProblemDetail)
//		fmt.Println(apiErr.Problem["allowed_images"])
//	}
//
// An APIError built as a literal with only StatusCode and Detail reads and
// prints exactly as it always did, with the other fields empty.
type APIError struct {
	// StatusCode is the HTTP status the API answered with.
	StatusCode int
	// Detail is the single line Error() prints: the body's own message
	// fields joined, whitespace-collapsed and cut at 256 runes, or a stand-in
	// such as "(no response body)" or "(server error response omitted)". It
	// never carries a value the body echoed back from the request.
	Detail string
	// Code is the body's machine-readable code, such as
	// "platform_image_required" or "plan_limit", exactly as sent; "" when the
	// body has none. It is read from the body's "code" member, or from inside
	// a "detail" member that is itself an object (FastAPI's own envelope), and
	// a code that is not a string or is only whitespace is no code.
	Code string
	// ProblemDetail is the API's whole sentence, exactly as sent: never
	// trimmed, collapsed or shortened, unlike Detail. It is read the same way
	// as Code, from the body's "detail" member; "" when the body has none.
	ProblemDetail string
	// Problem is the whole error body decoded, when it is a JSON object: the
	// Problem+JSON document every API refusal is. A structured field is
	// reachable here without a new SDK release: refused_images,
	// allowed_images and allowed_repositories on platform_image_required;
	// resource, limit, used and plan on plan_limit. Numbers are float64, as
	// everywhere encoding/json decodes into map[string]any. nil for a body
	// that is empty, not JSON, or JSON that is not an object. It can hold
	// values the body echoed back from the request, which is why Error()
	// never prints it.
	Problem map[string]any
	// RetryAfter is how long the API asked the caller to wait before trying
	// again, when it said; 0 otherwise. It is read from the body's
	// retry_after_seconds (at the top, or inside a detail object), else from
	// the Retry-After header in seconds, else from RateLimit-Reset: the API's
	// own rate limiter answers a 429 in plain text with only that header.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%v: HTTP %d: %s", ErrAPI, e.StatusCode, e.Detail)
}
func (e *APIError) Unwrap() error { return ErrAPI }

// Is reports whether the refusal is one a caller can branch on: ErrAuth,
// ErrPlan or ErrAlreadyLinked. They are read from the status and the body, so
// every control-plane call answers them the same way, and the error stays an
// *APIError with every field it carried:
//
//	var apiErr *thalovant.APIError
//	switch {
//	case errors.Is(err, thalovant.ErrAlreadyLinked) && errors.As(err, &apiErr):
//		fmt.Println("linked by", apiErr.LinkedClientID())
//	case errors.Is(err, thalovant.ErrPlan):
//		fmt.Println("upgrade the plan")
//	case errors.Is(err, thalovant.ErrAuth):
//		fmt.Println("sign in again")
//	}
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrAuth:
		return e.refusal() == ErrAuth
	case ErrPlan:
		return e.refusal() == ErrPlan
	case ErrAlreadyLinked:
		return e.refusal() == ErrAlreadyLinked
	}
	return false
}

// refusal is the kind of refusal the answer is, or nil for none of them. The
// rules are the parity contract's (connection-kinds-vectors.json).
func (e *APIError) refusal() error {
	// An APIError built by hand may carry only the body; read it the way
	// apiErrorFromResponse does.
	bodyCode, bodyDetail := problemFields(e.Problem)
	code, detail := firstNonEmpty(e.Code, bodyCode), firstNonEmpty(e.ProblemDetail, bodyDetail)
	switch {
	case e.StatusCode == 401 || e.StatusCode == 423 || (e.StatusCode == 403 && detail == "Insufficient scopes"):
		// A token that is unknown, expired or revoked; a locked account; or a
		// token without the scope: signing in again is the way out of each.
		return ErrAuth
	case e.StatusCode == 402 || (e.StatusCode == 403 && code == "plan_limit"):
		return ErrPlan
	case e.StatusCode == 409 && code == "home_assistant_already_linked":
		return ErrAlreadyLinked
	}
	return nil
}

// LinkedClientID is the connection that already holds a hub's link, named by
// an ErrAlreadyLinked refusal; "" when the answer names none. It is read from
// the body's client_id (or existing_client_id, or connection_id), at the top
// or inside a detail that is itself an object.
func (e *APIError) LinkedClientID() string {
	if e.Problem == nil {
		return ""
	}
	nested, _ := e.Problem["detail"].(map[string]any)
	for _, source := range []map[string]any{e.Problem, nested} {
		for _, key := range []string{"client_id", "existing_client_id", "connection_id"} {
			if text, ok := source[key].(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}

// DeviceLoginPendingError is a device sign-in nobody has approved yet: poll
// again after Interval. A slow_down answer has already lengthened it, for good,
// on the DeviceAuthorization that was polled. It matches ErrDeviceLoginPending,
// and errors.As reaches the *APIError the API answered with (HTTP 400).
type DeviceLoginPendingError struct {
	// Interval is how long to wait before the next poll.
	Interval time.Duration
	// APIError is what the API answered.
	APIError *APIError
}

func (e *DeviceLoginPendingError) Error() string {
	return fmt.Sprintf("%v: nobody has approved the device sign-in yet; poll again in %s", ErrDeviceLoginPending, e.Interval)
}

// Unwrap makes a DeviceLoginPendingError match ErrDeviceLoginPending and ErrAPI.
func (e *DeviceLoginPendingError) Unwrap() []error {
	return []error{ErrDeviceLoginPending, apiErrorOrSentinel(e.APIError)}
}

// DeviceLoginDeniedError is a device sign-in the person refused in the
// browser. It matches ErrDeviceAccessDenied, and errors.As reaches the
// *APIError the API answered with.
type DeviceLoginDeniedError struct {
	APIError *APIError
}

func (e *DeviceLoginDeniedError) Error() string {
	return fmt.Sprintf("%v: the device sign-in request was denied in the browser", ErrDeviceAccessDenied)
}

// Unwrap makes a DeviceLoginDeniedError match ErrDeviceAccessDenied and ErrAPI.
func (e *DeviceLoginDeniedError) Unwrap() []error {
	return []error{ErrDeviceAccessDenied, apiErrorOrSentinel(e.APIError)}
}

// DeviceLoginExpiredError is a device sign-in code that expired before anyone
// approved it; start a new sign-in for a new code. It matches
// ErrDeviceCodeExpired, and errors.As reaches the *APIError the API answered
// with.
type DeviceLoginExpiredError struct {
	APIError *APIError
}

func (e *DeviceLoginExpiredError) Error() string {
	return fmt.Sprintf("%v: the device sign-in code expired before it was approved; start a new sign-in to get a new code", ErrDeviceCodeExpired)
}

// Unwrap makes a DeviceLoginExpiredError match ErrDeviceCodeExpired and ErrAPI.
func (e *DeviceLoginExpiredError) Unwrap() []error {
	return []error{ErrDeviceCodeExpired, apiErrorOrSentinel(e.APIError)}
}

// UnsupportedConnectionTypeError reports that the API could not make a
// connection of the kind asked for. Either it refused the kind (HTTP 422 about
// spec.connection_type; APIError carries the answer), or it made an ordinary
// connection instead, which the SDK then deleted (APIError is nil; ClientID
// and Deleted say what happened to it). It matches
// ErrUnsupportedConnectionType and ErrAPI.
type UnsupportedConnectionTypeError struct {
	// ConnectionType is the kind asked for.
	ConnectionType string
	// Answered is the kind the API made instead; "" when it named none.
	Answered string
	// ClientID is the connection the API made instead, when it made one.
	ClientID string
	// Deleted reports that the connection the API made instead is gone.
	Deleted bool
	// DeleteErr is why deleting it failed, when it did; remove it in the
	// dashboard.
	DeleteErr error
	// APIError is the API's refusal, when it refused.
	APIError *APIError
}

func (e *UnsupportedConnectionTypeError) Error() string {
	if e.APIError != nil {
		return fmt.Sprintf("%v: the Thalovant API cannot create a %q connection yet (HTTP %d: %s)",
			ErrUnsupportedConnectionType, e.ConnectionType, e.APIError.StatusCode, e.APIError.Detail)
	}
	answered := e.Answered
	if answered == "" {
		answered = "no type"
	}
	message := fmt.Sprintf("%v: the Thalovant API did not make a %q connection (it answered %q)",
		ErrUnsupportedConnectionType, e.ConnectionType, answered)
	if e.ClientID != "" && !e.Deleted {
		message += fmt.Sprintf("; deleting the connection it made instead (%s) failed, so remove it in the dashboard", e.ClientID)
	}
	return message
}

// Unwrap makes an UnsupportedConnectionTypeError match
// ErrUnsupportedConnectionType, and ErrAPI through the API's own answer when
// there is one.
func (e *UnsupportedConnectionTypeError) Unwrap() []error {
	return []error{ErrUnsupportedConnectionType, apiErrorOrSentinel(e.APIError)}
}

// AdmissionTimeoutError reports that a new connection was not admitted by its
// hub within the wait. It is a connection error and a timeout at once --
// errors.Is matches both ErrConnection and ErrTimeout, and Timeout reports
// true -- because the connection may still be admitted after it.
type AdmissionTimeoutError struct {
	// Wait is how long the wait lasted.
	Wait time.Duration
	// OperationID is the operation that was followed.
	OperationID string
}

func (e *AdmissionTimeoutError) Error() string {
	return fmt.Sprintf("%v: %v: the hub did not admit the connection within %s; it may still admit it later.", ErrConnection, ErrTimeout, e.Wait)
}

// Unwrap makes an AdmissionTimeoutError match ErrConnection and ErrTimeout.
func (e *AdmissionTimeoutError) Unwrap() []error { return []error{ErrConnection, ErrTimeout} }

// Timeout reports true, the way a net.Error that timed out does.
func (e *AdmissionTimeoutError) Timeout() bool { return true }

// AdmissionFailedError reports that the hub could not admit a new connection:
// the operation carrying it ended failed or timed_out on the platform, or the
// API refused the wait itself (for any reason but authentication, which
// WaitForAdmission returns as the *APIError it is). It matches ErrConnection;
// when the API refused the wait, errors.As also reaches its *APIError, with
// the status, code and detail it answered.
type AdmissionFailedError struct {
	// OperationID is the operation that was followed.
	OperationID string
	// Status is the operation's final status, when it reached one.
	Status OperationStatus
	// ErrorCode is the operation's own code, such as "gitops_push_rejected";
	// "" when it had none, and always "" when the API refused the wait (its
	// code is on the *APIError in Err).
	ErrorCode string
	// ErrorMessage is the operation's own explanation, when it gave one.
	ErrorMessage string
	// Err is the API's refusal of the wait, when that is what ended it.
	Err error
}

func (e *AdmissionFailedError) Error() string {
	detail := e.ErrorMessage
	if detail == "" {
		detail = e.ErrorCode
	}
	if detail == "" && e.Err != nil {
		detail = e.Err.Error()
	}
	if detail == "" {
		detail = "no detail"
	}
	if e.Status != "" {
		return fmt.Sprintf("%v: the hub could not admit the connection: operation %s ended %s: %s", ErrConnection, e.OperationID, e.Status, detail)
	}
	return fmt.Sprintf("%v: the hub could not admit the connection: %s", ErrConnection, detail)
}

// Unwrap makes an AdmissionFailedError match ErrConnection, and whatever the
// API answered when that is what ended the wait.
func (e *AdmissionFailedError) Unwrap() []error {
	if e.Err != nil {
		return []error{ErrConnection, e.Err}
	}
	return []error{ErrConnection}
}

// apiUnreachableError is a control-plane request that never got an answer. Its
// message is the one this SDK has always given; it matches ErrAPI as it always
// did, and ErrAPIUnreachable and ErrConnection besides.
type apiUnreachableError struct{}

func (apiUnreachableError) Error() string {
	return fmt.Sprintf("%v: control request failed", ErrAPI)
}

func (apiUnreachableError) Unwrap() []error {
	return []error{ErrAPI, ErrAPIUnreachable, ErrConnection}
}

// errNothingSent marks a send withdrawn before any of it was encrypted or
// written: the transport is still sound, so nothing is torn down for it.
var errNothingSent = errors.New("nothing was sent")

// apiErrorOrSentinel is the API's answer when there is one, and ErrAPI when
// there is not, so a typed error always matches ErrAPI without ever
// unwrapping to a nil *APIError inside a non-nil interface.
func apiErrorOrSentinel(apiErr *APIError) error {
	if apiErr == nil {
		return ErrAPI
	}
	return apiErr
}

// problemFields reads the code and the sentence out of an API error body.
//
// Read from the body's own members first. When "detail" is itself an object,
// it is FastAPI's envelope around a structured refusal -- what the API sends
// when its Problem+JSON handler has not lifted that object's members to the
// top -- so the code and the sentence are read from inside it. Nothing is
// trimmed or shortened: the detail is the whole sentence.
func problemFields(problem map[string]any) (code string, detail string) {
	if problem == nil {
		return "", ""
	}
	// Indexing a nil map reads nothing, so a detail that is not an object
	// simply has no nested members.
	nested, _ := problem["detail"].(map[string]any)
	code = problemText(problem["code"])
	if code == "" {
		code = problemText(nested["code"])
	}
	detail = problemText(problem["detail"])
	if detail == "" {
		detail = problemText(nested["detail"])
	}
	return code, detail
}

// problemText is a string with something in it, exactly as sent; anything
// else -- a number, an object, a blank string -- is absent, which Go spells "".
func problemText(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	for _, r := range text {
		if !problemBlank(r) {
			return text
		}
	}
	return ""
}

// problemBlank is the whitespace the reference's str.strip() removes:
// unicode.IsSpace, plus the four information separators U+001C..U+001F that
// Python counts as whitespace and Go does not. A code made of those alone is
// no code in the reference, so it is none here either.
func problemBlank(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}
