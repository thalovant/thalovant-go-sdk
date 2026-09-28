package thalovant

// The Home Assistant link, against the four vector files every SDK shares:
// contracts/conformance/{device-login,connection-kinds,connection-admission,
// home-link}-vectors.json in the Python SDK, vendored here unchanged under
// testdata/ and pinned by the parity contract.
//
// device-login, connection-kinds and connection-admission are HTTP exchanges:
// each case's answers are served by a loopback httptest server, in order,
// through the public ControlPlane, and every request the SDK sends is checked
// against the one the case names. What the SDK produced is recorded before it
// is compared, shaped exactly as the reference's runner
// (tests/test_home_link_vectors.py) shapes it, since the digest is over that
// shape. home-link holds the reply routing and the request/response rules.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func loadHomeVectors(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var vectors map[string]any
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(anySlice(vectors["cases"])) == 0 {
		t.Fatalf("%s has no cases", name)
	}
	return vectors
}

// scriptedAPI serves a case's exchanges in order and checks each request
// against its own. An exchange marked repeat answers every request from then on.
type scriptedAPI struct {
	t          *testing.T
	mu         sync.Mutex
	exchanges  []any
	index      int
	sent       []string
	mismatches []string
	server     *httptest.Server
}

func newScriptedAPI(t *testing.T, exchanges []any) *scriptedAPI {
	t.Helper()
	api := &scriptedAPI{t: t, exchanges: exchanges}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	t.Cleanup(api.server.Close)
	return api
}

func (a *scriptedAPI) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	ifMatch := r.Header.Get("If-Match")
	line := r.Method + " " + r.URL.Path
	if ifMatch != "" {
		line += " If-Match=" + ifMatch
	}
	a.sent = append(a.sent, line)
	if a.index >= len(a.exchanges) {
		a.mismatches = append(a.mismatches, "unexpected "+r.Method+" "+r.URL.Path)
		w.WriteHeader(599)
		_, _ = w.Write([]byte("{}"))
		return
	}
	exchange := mapValue(a.exchanges[a.index])
	if repeat, _ := exchange["repeat"].(bool); !repeat {
		a.index++
	}
	expected := mapValue(exchange["request"])
	if r.Method != expected["method"] || r.URL.Path != expected["path"] {
		a.mismatches = append(a.mismatches, fmt.Sprintf("%s %s != %v %v", r.Method, r.URL.Path, expected["method"], expected["path"]))
	}
	var body any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			a.mismatches = append(a.mismatches, "body is not JSON")
		}
	}
	if want, ok := expected["json"]; ok && !reflect.DeepEqual(body, want) {
		a.mismatches = append(a.mismatches, fmt.Sprintf("body %v != %v", body, want))
	}
	if want, ok := expected["json_subset"]; ok && !jsonContains(body, want) {
		a.mismatches = append(a.mismatches, fmt.Sprintf("body %v lacks %v", body, want))
	}
	if want, ok := expected["if_match"]; ok && ifMatch != want {
		a.mismatches = append(a.mismatches, fmt.Sprintf("If-Match %q != %v", ifMatch, want))
	}
	if want, ok := expected["authorization"]; ok && r.Header.Get("Authorization") != want {
		a.mismatches = append(a.mismatches, "wrong Authorization header")
	}
	response := mapValue(exchange["response"])
	answer, _ := response["body"].(string)
	if answer != "" {
		w.Header().Set("Content-Type", fmt.Sprint(response["content_type"]))
	}
	status, _ := response["status"].(float64)
	w.WriteHeader(int(status))
	_, _ = w.Write([]byte(answer))
}

func (a *scriptedAPI) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.sent...)
}

func (a *scriptedAPI) check(t *testing.T, everyExchange bool) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.mismatches) != 0 {
		t.Errorf("requests did not match the case: %v", a.mismatches)
	}
	if everyExchange && a.index != len(a.exchanges) {
		t.Errorf("used %d of %d exchanges", a.index, len(a.exchanges))
	}
}

func jsonContains(value, subset any) bool {
	if want, ok := subset.(map[string]any); ok {
		have, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for key, item := range want {
			present, ok := have[key]
			if !ok || !jsonContains(present, item) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(value, subset)
}

// assertProduced compares what the SDK produced with what the case expects,
// both as JSON, so a Go int and a decoded float64 of the same number agree.
func assertProduced(t *testing.T, produced, expect any) {
	t.Helper()
	have, err := json.Marshal(produced)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(have, &normalized); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized, expect) {
		want, _ := json.Marshal(expect)
		t.Errorf("produced %s\nwant     %s", have, want)
	}
}

// assertExcluded checks that no secret a case names reaches any printed form
// of the error.
func assertExcluded(t *testing.T, err error, vectors map[string]any) {
	t.Helper()
	for form, text := range displayForms(err) {
		for _, secret := range anySlice(vectors["message_excludes"]) {
			if strings.Contains(text, fmt.Sprint(secret)) {
				t.Errorf("%s of %T carries a secret the case excludes", form, err)
			}
		}
	}
}

// milliseconds reads a vector's duration: every one is whole milliseconds,
// since only a whole number reads the same in every language.
func milliseconds(value any) time.Duration {
	number, _ := value.(float64)
	return time.Duration(number) * time.Millisecond
}

// -- device login --------------------------------------------------------------

func TestDeviceLoginVectors(t *testing.T) {
	vectors := loadHomeVectors(t, "device-login-vectors.json")
	if got, want := HomeAssistantScopes(), anySlice(vectors["home_assistant_scopes"]); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("HomeAssistantScopes() = %v, want %v", got, want)
	}
	for _, raw := range anySlice(vectors["cases"]) {
		spec := mapValue(raw)
		name := fmt.Sprint(spec["name"])
		t.Run(name, func(t *testing.T) {
			api := newScriptedAPI(t, anySlice(spec["exchanges"]))
			produced := runDeviceCase(t, api, mapValue(spec["call"]), vectors)
			recordConformance(t, "device-login-vectors.json", name, produced)
			api.check(t, true)
			assertProduced(t, produced, spec["expect"])
		})
	}
}

func runDeviceCase(t *testing.T, api *scriptedAPI, call map[string]any, vectors map[string]any) []any {
	t.Helper()
	plane := NewControlPlane(api.server.URL, "")
	ctx := context.Background()
	if call["op"] == "begin" {
		var scopes []string
		for _, scope := range anySlice(call["scopes"]) {
			scopes = append(scopes, fmt.Sprint(scope))
		}
		clientName, _ := call["client_name"].(string)
		grant, err := plane.BeginDeviceLogin(ctx, scopes, clientName)
		if err != nil {
			assertExcluded(t, err, vectors)
			// The reference records only the status for a refused start.
			failed := deviceErrorOutcome(err)
			return []any{map[string]any{"outcome": "error", "status": failed["status"]}}
		}
		if strings.Contains(fmt.Sprint(grant), grant.DeviceCode) || strings.Contains(fmt.Sprintf("%#v", grant), grant.DeviceCode) {
			t.Error("a printed DeviceAuthorization carries the device code")
		}
		return []any{map[string]any{
			"outcome":                   "started",
			"user_code":                 grant.UserCode,
			"verification_uri":          grant.VerificationURI,
			"verification_uri_complete": absentIfEmpty(grant.VerificationURIComplete),
			"interval":                  grant.Interval.Seconds(),
			"expires_in":                grant.ExpiresIn.Seconds(),
		}}
	}
	authorization := mapValue(call["authorization"])
	grant := &DeviceAuthorization{
		DeviceCode:      fmt.Sprint(authorization["device_code"]),
		VerificationURI: "https://x",
		// The API's own field, in seconds on the wire (RFC 8628).
		Interval:  milliseconds(authorization["interval"]) * 1000,
		ExpiresIn: 900 * time.Second,
	}
	times := 1
	if count, ok := call["times"].(float64); ok {
		times = int(count)
	}
	produced := []any{}
	for range times {
		produced = append(produced, pollOnce(t, plane, grant, vectors))
	}
	if call["op"] == "revoke" {
		if err := plane.RevokeAPIToken(ctx, ""); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if plane.AccessToken != "" || plane.TokenID != "" {
			t.Error("the revoked token is still held")
		}
		produced = []any{map[string]any{"outcome": "revoked"}}
		// Idempotent: revoking again sends nothing and succeeds. The scripted
		// API flags any request past the case's exchanges.
		if err := plane.RevokeAPIToken(ctx, ""); err != nil {
			t.Fatalf("revoking again: %v", err)
		}
	}
	return produced
}

func pollOnce(t *testing.T, plane *ControlPlane, grant *DeviceAuthorization, vectors map[string]any) map[string]any {
	t.Helper()
	token, err := plane.PollDeviceLogin(context.Background(), grant)
	var (
		pending *DeviceLoginPendingError
		expired *DeviceLoginExpiredError
		denied  *DeviceLoginDeniedError
	)
	switch {
	case err == nil:
		if plane.AccessToken != token.AccessToken || plane.TokenID != token.TokenID {
			t.Error("the approved token was not kept")
		}
		if strings.Contains(fmt.Sprint(token), token.AccessToken) || strings.Contains(fmt.Sprintf("%#v", token), token.AccessToken) {
			t.Error("a printed APIToken carries the access token")
		}
		var expires any
		if !token.ExpiresAt.IsZero() {
			expires = token.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return map[string]any{
			"outcome":    "approved",
			"token_type": token.TokenType,
			"scopes":     token.Scopes,
			"expires_at": expires,
			"token_id":   absentIfEmpty(token.TokenID),
		}
	case errors.As(err, &pending):
		if !errors.Is(err, ErrDeviceLoginPending) || !errors.Is(err, ErrAPI) {
			t.Errorf("pending does not match its sentinels: %v", err)
		}
		if grant.Interval != pending.Interval {
			t.Errorf("the authorization kept %s, the error said %s", grant.Interval, pending.Interval)
		}
		return map[string]any{"outcome": "pending", "interval": pending.Interval.Seconds()}
	case errors.As(err, &expired):
		assertExcluded(t, err, vectors)
		if !errors.Is(err, ErrDeviceCodeExpired) || !errors.Is(err, ErrAPI) {
			t.Errorf("expired does not match its sentinels: %v", err)
		}
		return map[string]any{"outcome": "expired", "status": expired.APIError.StatusCode}
	case errors.As(err, &denied):
		assertExcluded(t, err, vectors)
		if !errors.Is(err, ErrDeviceAccessDenied) || !errors.Is(err, ErrAPI) {
			t.Errorf("denied does not match its sentinels: %v", err)
		}
		return map[string]any{"outcome": "denied", "status": denied.APIError.StatusCode}
	}
	assertExcluded(t, err, vectors)
	return deviceErrorOutcome(err)
}

// deviceErrorOutcome is an error as the vectors spell it: the api-errors
// fields when the API answered, and a null status when it never did.
func deviceErrorOutcome(err error) map[string]any {
	if !errors.Is(err, ErrAPI) {
		panic(fmt.Sprintf("device login returned an error outside ErrAPI: %T %v", err, err))
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return map[string]any{"outcome": "error", "status": nil}
	}
	return map[string]any{
		"outcome": "error",
		"status":  apiErr.StatusCode,
		"code":    absentIfEmpty(apiErr.Code),
		"detail":  absentIfEmpty(apiErr.ProblemDetail),
	}
}

// -- connection kinds ------------------------------------------------------------

func TestConnectionKindsVectors(t *testing.T) {
	vectors := loadHomeVectors(t, "connection-kinds-vectors.json")
	for _, raw := range anySlice(vectors["cases"]) {
		spec := mapValue(raw)
		name := fmt.Sprint(spec["name"])
		t.Run(name, func(t *testing.T) {
			api := newScriptedAPI(t, anySlice(spec["exchanges"]))
			produced := runKindsCase(t, api, mapValue(spec["call"]), vectors)
			recordConformance(t, "connection-kinds-vectors.json", name, produced)
			api.check(t, false)
			assertProduced(t, produced, spec["expect"])
		})
	}
}

func runKindsCase(t *testing.T, api *scriptedAPI, call map[string]any, vectors map[string]any) map[string]any {
	t.Helper()
	plane := NewControlPlane(api.server.URL, "synthetic-token")
	ctx := context.Background()
	var produced map[string]any
	if call["op"] == "create" {
		result, err := plane.CreateClientIdentity(ctx, mapValue(call["hub"]), BootstrapIdentityOptions{
			Name:           fmt.Sprint(call["name"]),
			ConnectionType: fmt.Sprint(call["connection_type"]),
		})
		var unsupported *UnsupportedConnectionTypeError
		switch {
		case err == nil:
			var operationID any
			if result.Operation != nil {
				operationID = result.Operation.ID
			}
			produced = map[string]any{
				"outcome":         "created",
				"client_id":       result.ClientID(),
				"connection_type": result.ConnectionType(),
				"operation_id":    operationID,
			}
		case errors.As(err, &unsupported):
			assertExcluded(t, err, vectors)
			if !errors.Is(err, ErrUnsupportedConnectionType) || !errors.Is(err, ErrAPI) {
				t.Errorf("unsupported does not match its sentinels: %v", err)
			}
			produced = map[string]any{"outcome": "unsupported"}
			if unsupported.APIError != nil {
				for key, value := range apiFields(unsupported.APIError) {
					produced[key] = value
				}
			} else {
				deleted := false
				for _, line := range api.requests() {
					deleted = deleted || strings.HasPrefix(line, "DELETE ")
				}
				if deleted != unsupported.Deleted {
					t.Errorf("Deleted = %v, but the API saw a DELETE: %v", unsupported.Deleted, deleted)
				}
				produced["deleted"] = deleted
			}
		default:
			assertExcluded(t, err, vectors)
			produced = kindOutcome(err, true)
		}
	} else {
		etag, _ := call["etag"].(string)
		if err := plane.DeleteClient(ctx, fmt.Sprint(call["client_id"]), etag); err != nil {
			produced = kindOutcome(err, false)
		} else {
			produced = map[string]any{"outcome": "deleted"}
		}
	}
	produced["requests"] = api.requests()
	return produced
}

func apiFields(apiErr *APIError) map[string]any {
	return map[string]any{
		"status": apiErr.StatusCode,
		"code":   absentIfEmpty(apiErr.Code),
		"detail": absentIfEmpty(apiErr.ProblemDetail),
	}
}

// kindOutcome is a refusal as the vectors spell it. The reference names the
// linked client for a create only.
func kindOutcome(err error, create bool) map[string]any {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return map[string]any{"outcome": "error", "status": nil, "code": nil, "detail": nil}
	}
	produced := apiFields(apiErr)
	switch {
	case errors.Is(err, ErrPlan):
		produced["outcome"] = "plan"
	case errors.Is(err, ErrAlreadyLinked):
		produced["outcome"] = "already_linked"
		if create {
			produced["client_id"] = absentIfEmpty(apiErr.LinkedClientID())
		}
	case errors.Is(err, ErrAuth):
		produced["outcome"] = "auth"
	default:
		produced["outcome"] = "error"
	}
	return produced
}

// -- admission ---------------------------------------------------------------------

func TestConnectionAdmissionVectors(t *testing.T) {
	vectors := loadHomeVectors(t, "connection-admission-vectors.json")
	for _, raw := range anySlice(vectors["cases"]) {
		spec := mapValue(raw)
		name := fmt.Sprint(spec["name"])
		t.Run(name, func(t *testing.T) {
			api := newScriptedAPI(t, anySlice(spec["exchanges"]))
			produced := runAdmissionCase(t, api, mapValue(spec["call"]), mapValue(spec["expect"]))
			recordConformance(t, "connection-admission-vectors.json", name, produced)
			api.check(t, false)
			assertProduced(t, produced, spec["expect"])
		})
	}
}

func runAdmissionCase(t *testing.T, api *scriptedAPI, call, expect map[string]any) map[string]any {
	t.Helper()
	plane := NewControlPlane(api.server.URL, "synthetic-token")
	operation := operationFromAny(call["operation"])
	if call["operation"] != nil && operation == nil {
		t.Fatal("the case's operation did not parse")
	}
	started := time.Now()
	err := plane.WaitForAdmission(context.Background(), operation, AdmissionOptions{
		Timeout:      milliseconds(call["timeout_ms"]),
		PollInterval: milliseconds(call["poll_interval_ms"]),
	})
	waited := time.Since(started)
	polls := len(api.requests())
	var (
		timeout  *AdmissionTimeoutError
		failed   *AdmissionFailedError
		produced map[string]any
	)
	switch {
	case err == nil:
		produced = map[string]any{"outcome": "admitted", "polls": polls}
	case errors.As(err, &timeout):
		if !errors.Is(err, ErrConnection) || !errors.Is(err, ErrTimeout) || !timeout.Timeout() {
			t.Errorf("an admission timeout must be a connection error and a timeout: %v", err)
		}
		produced = map[string]any{"outcome": "timeout"}
		if _, counted := expect["polls"]; counted {
			produced["polls"] = polls
		}
	case errors.As(err, &failed):
		if !errors.Is(err, ErrConnection) {
			t.Errorf("a failed admission must be a connection error: %v", err)
		}
		produced = map[string]any{"outcome": "failed", "error_code": absentIfEmpty(failed.ErrorCode), "polls": polls}
	case errors.Is(err, ErrAPI):
		produced = map[string]any{"outcome": "error", "polls": polls}
	default:
		t.Fatalf("unexpected admission error %T %v", err, err)
	}
	if bound, present := expect["waited_at_least_ms"]; present {
		// Recorded as the bound it met, so every SDK records the same value.
		if waited >= milliseconds(bound) {
			produced["waited_at_least_ms"] = bound
		} else {
			produced["waited_at_least_ms"] = waited.Milliseconds()
		}
	}
	return produced
}

// -- the home link ------------------------------------------------------------------

type sentReply struct {
	event   Event
	msgType string
	data    Data
}

type capturingReplier struct {
	mu   sync.Mutex
	sent []sentReply
}

func (r *capturingReplier) Reply(_ context.Context, event Event, msgType string, data Data, _ Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, sentReply{event, msgType, data})
	return nil
}

func vectorHandler(spec map[string]any) HomeHandler {
	return func(_ context.Context, _ HomeRequest) (HomeAnswer, error) {
		if raises, _ := spec["raises"].(bool); raises {
			return HomeAnswer{}, errors.New("the conversation agent is gone")
		}
		if wait := milliseconds(spec["sleep_ms"]); wait > 0 {
			// Deliberately deaf to its context: the answer must still go out
			// on time.
			time.Sleep(wait)
		}
		answer := HomeAnswer{ResponseType: HomeActionDone}
		answer.Speech, _ = spec["speech"].(string)
		if kind, ok := spec["response_type"].(string); ok {
			answer.ResponseType = kind
		}
		answer.ErrorCode, _ = spec["error_code"].(string)
		answer.ContinueConversation, _ = spec["continue_conversation"].(bool)
		return answer, nil
	}
}

func TestHomeLinkVectors(t *testing.T) {
	vectors := loadHomeVectors(t, "home-link-vectors.json")
	for _, raw := range anySlice(vectors["cases"]) {
		spec := mapValue(raw)
		name := fmt.Sprint(spec["name"])
		t.Run(name, func(t *testing.T) {
			var produced any
			if spec["kind"] == "reply_context" {
				produced = map[string]any(ReplyContext(Context(mapValue(spec["context"]))))
			} else {
				replier := &capturingReplier{}
				event := Event{Name: HomeRequestEvent, Data: Data(mapValue(spec["request"])), Context: Context{"source": "skill"}}
				timeout := DefaultHomeHandlerTimeout
				if value, ok := spec["timeout_ms"]; ok {
					timeout = milliseconds(value)
				}
				started := time.Now()
				payload, err := AnswerHomeRequest(context.Background(), replier, event, vectorHandler(mapValue(spec["handler"])), timeout)
				if err != nil {
					t.Fatal(err)
				}
				if elapsed := time.Since(started); elapsed > timeout+time.Second {
					t.Errorf("the answer took %s against a %s bound", elapsed, timeout)
				}
				if len(replier.sent) != 1 || replier.sent[0].msgType != HomeResponseEvent || !reflect.DeepEqual(replier.sent[0].data, payload) {
					t.Fatalf("want exactly one %s carrying the payload, sent %+v", HomeResponseEvent, replier.sent)
				}
				produced = map[string]any(payload)
			}
			recordConformance(t, "home-link-vectors.json", name, produced)
			assertProduced(t, produced, spec["expect"])
		})
	}
}

func TestHomeLinkContractListsMatchTheSDK(t *testing.T) {
	vectors := loadHomeVectors(t, "home-link-vectors.json")
	if fmt.Sprint(HomeResponseTypes()) != fmt.Sprint(anySlice(vectors["response_types"])) {
		t.Errorf("HomeResponseTypes() = %v, vectors %v", HomeResponseTypes(), vectors["response_types"])
	}
	if fmt.Sprint(HomeErrorCodes()) != fmt.Sprint(anySlice(vectors["error_codes"])) {
		t.Errorf("HomeErrorCodes() = %v, vectors %v", HomeErrorCodes(), vectors["error_codes"])
	}
	if vectors["request_type"] != HomeRequestEvent || vectors["response_type"] != HomeResponseEvent {
		t.Error("the event names differ from the contract")
	}
	if milliseconds(vectors["reply_timeout_ms"]) != HomeRequestTimeout {
		t.Errorf("HomeRequestTimeout = %s, contract %v ms", HomeRequestTimeout, vectors["reply_timeout_ms"])
	}
	if DefaultHomeHandlerTimeout != 9000*time.Millisecond {
		t.Errorf("DefaultHomeHandlerTimeout = %s, the reference gives a handler 9000 ms", DefaultHomeHandlerTimeout)
	}
}
