package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The kinds of connection the API knows, sent as spec.connection_type. The
// kind decides what a connection may send and receive; the API may learn more
// of them, so the field is a plain string.
const (
	ConnectionTypeVoiceSatellite = "voice_satellite"
	ConnectionTypeWebChat        = "web_chat"
	ConnectionTypeDeveloper      = "developer"
	ConnectionTypeEmbedded       = "embedded"
	// ConnectionTypeHomeAssistant is a hub's Home Assistant link. A hub holds
	// at most one: a second is refused with ErrAlreadyLinked.
	ConnectionTypeHomeAssistant = "home_assistant"
)

const (
	// DefaultAdmissionTimeout bounds WaitForAdmission. A hub admits a new
	// connection about ninety seconds after it is created.
	DefaultAdmissionTimeout = 180 * time.Second
	// DefaultOperationPollInterval is how often WaitForAdmission reads the
	// operation.
	DefaultOperationPollInterval = 2 * time.Second
)

// ClientID is the id of the connection that was created; "" when the answer
// carried none.
func (r BootstrapIdentityResult) ClientID() string {
	id, _ := r.Client["id"].(string)
	return id
}

// ConnectionType is the kind of connection the API says it created, from the
// answer's spec.connection_type; "" when it named none.
func (r BootstrapIdentityResult) ConnectionType() string {
	spec, _ := r.Client["spec"].(map[string]any)
	kind, _ := spec["connection_type"].(string)
	return kind
}

// operationFromAny reads the operation a create answered with, or nil when it
// carried none that can be followed.
func operationFromAny(raw any) *OperationResource {
	payload, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	var operation OperationResource
	if json.Unmarshal(encoded, &operation) != nil || operation.ID == "" {
		return nil
	}
	return &operation
}

// refusesConnectionType reports a 422 whose problem is about the connection
// type: the API does not know the kind yet.
//
// Only where the API says what is wrong counts: the problem's detail and code,
// and the loc and msg of each validation error, under "errors" or under
// "detail" when that is a list. Never the rest of the body: a validation error
// echoes what was sent as its input, and the request always carries
// spec.connection_type, so a 422 about any other field would read as this one.
func refusesConnectionType(err error) (*APIError, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		return nil, false
	}
	bodyCode, bodyDetail := problemFields(apiErr.Problem)
	said := []string{firstNonEmpty(apiErr.ProblemDetail, bodyDetail), firstNonEmpty(apiErr.Code, bodyCode)}
	for _, key := range []string{"errors", "detail"} {
		for _, item := range anySlice(apiErr.Problem[key]) {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch location := entry["loc"].(type) {
			case []any:
				parts := make([]string, 0, len(location))
				for _, part := range location {
					parts = append(parts, fmt.Sprint(part))
				}
				said = append(said, strings.Join(parts, "."))
			case string:
				said = append(said, location)
			}
			if message, ok := entry["msg"].(string); ok {
				said = append(said, message)
			}
		}
	}
	for _, text := range said {
		if strings.Contains(text, "connection_type") || strings.Contains(text, "connectionType") {
			return apiErr, true
		}
	}
	return apiErr, false
}

// requireConnectionType deletes a connection the API did not make of the kind
// asked for, and refuses it. Left in place it would be an ordinary satellite
// nobody asked for.
func (c *ControlPlane) requireConnectionType(ctx context.Context, client map[string]any, connectionType string) error {
	spec, _ := client["spec"].(map[string]any)
	echoed, _ := spec["connection_type"].(string)
	if echoed == connectionType {
		return nil
	}
	refusal := &UnsupportedConnectionTypeError{ConnectionType: connectionType, Answered: echoed}
	if clientID, _ := client["id"].(string); clientID != "" {
		etag, _ := client["etag"].(string)
		refusal.ClientID = clientID
		refusal.DeleteErr = c.DeleteClient(ctx, clientID, etag)
		refusal.Deleted = refusal.DeleteErr == nil
	}
	return refusal
}

// GetClient reads one connection (a client of a hub), with the etag a change
// to it needs.
func (c *ControlPlane) GetClient(ctx context.Context, clientID string) (map[string]any, error) {
	return c.request(ctx, http.MethodGet, "/v1/clients/"+url.PathEscape(clientID), nil, nil, true)
}

// DeleteClient deletes a connection.
//
// The API wants the connection's current etag as If-Match. With etag "" this
// reads it first; if another writer changed the connection in between (HTTP
// 412) it reads it once more and retries once. A connection that is already
// gone (HTTP 404 on either request) counts as deleted.
func (c *ControlPlane) DeleteClient(ctx context.Context, clientID string, etag string) error {
	if strings.TrimSpace(clientID) == "" {
		return fmt.Errorf("%w: deleting a connection needs its id", ErrAPI)
	}
	path := "/v1/clients/" + url.PathEscape(clientID)
	for attempt := 0; ; attempt++ {
		if strings.TrimSpace(etag) == "" {
			client, err := c.GetClient(ctx, clientID)
			if err != nil {
				if isAPIStatus(err, http.StatusNotFound) {
					return nil
				}
				return err
			}
			if etag, _ = client["etag"].(string); strings.TrimSpace(etag) == "" {
				return fmt.Errorf("%w: the connection resource is missing etag", ErrAPI)
			}
		}
		_, err := c.request(ctx, http.MethodDelete, path, nil, map[string]string{"If-Match": etag}, true)
		switch {
		case err == nil, isAPIStatus(err, http.StatusNotFound):
			return nil
		case attempt == 0 && isAPIStatus(err, http.StatusPreconditionFailed):
			etag = ""
		default:
			return err
		}
	}
}

// isAPIStatus reports an *APIError with this HTTP status.
func isAPIStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

// AdmissionOptions bounds WaitForAdmission. Zero values take the defaults.
type AdmissionOptions struct {
	// Timeout is how long to wait; DefaultAdmissionTimeout when zero.
	Timeout time.Duration
	// PollInterval is how often to read the operation;
	// DefaultOperationPollInterval when zero.
	PollInterval time.Duration
}

// WaitForAdmission waits until the hub has admitted a new connection, about
// ninety seconds after CreateClientIdentity returned it. Pass the result's
// Operation.
//
// It follows the operation, reading GET /v1/operations/{id}:
//
//   - ready: admitted, and it returns nil;
//   - failed or timed_out: *AdmissionFailedError, with the operation's
//     ErrorCode;
//   - requested, committed, applied: it keeps reading;
//   - no operation at all (nil), or HTTP 404 (the API no longer tracks it):
//     admitted at once;
//   - HTTP 429: the next read waits what the API asks (APIError.RetryAfter:
//     the body's retry_after_seconds, else Retry-After, else RateLimit-Reset),
//     or the poll interval when that is longer; asking for longer than the
//     time left is the timeout at once;
//   - HTTP 5xx: ridden out;
//   - HTTP 401 or 403: the *APIError itself, since the token rather than the
//     connection is the trouble (errors.Is ErrAuth for a revoked token or a
//     missing scope);
//   - any other refusal of the wait: *AdmissionFailedError, whose Err is the
//     *APIError with the status, code and detail the API answered;
//   - a request that did not reach the API: returned as it is (errors.Is
//     ErrAPIUnreachable), since losing the API says nothing about the hub.
//
// When opts.Timeout passes first it returns *AdmissionTimeoutError, which
// matches both ErrConnection and ErrTimeout: the connection may still be
// admitted later. Cancelling ctx ends the wait with ErrTimeout and the
// context's error. A hub that refuses the new credentials inside this window
// is not admitting them yet, not refusing them.
//
// An operation whose links.self points at another origin than the API's is
// refused and never fetched: the token goes to the API and nowhere else.
func (c *ControlPlane) WaitForAdmission(ctx context.Context, operation *OperationResource, opts AdmissionOptions) error {
	if operation == nil {
		return nil
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultAdmissionTimeout
	}
	interval := opts.PollInterval
	if interval <= 0 {
		interval = DefaultOperationPollInterval
	}
	link := ""
	if self := operation.Links["self"]; self != nil {
		link = strings.TrimSpace(*self)
	}
	if !c.sameOrigin(link) {
		return fmt.Errorf("%w: the admission operation points outside the Thalovant API", ErrAPI)
	}
	operationID := operation.ID
	if operationID == "" {
		operationID = operationIDFromLink(link)
	}
	if operationID == "" {
		return fmt.Errorf("%w: an operation needs an id to wait on", ErrAPI)
	}
	deadline := time.Now().Add(timeout)
	// Every read is bounded by the wait's own deadline too, so one stalled
	// request cannot hold the wait past it.
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		wait := interval
		current, err := c.GetOperation(waitCtx, operationID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w", ErrTimeout, ctxErr)
		}
		if waitCtx.Err() != nil {
			return &AdmissionTimeoutError{Wait: timeout, OperationID: operationID}
		}
		var apiErr *APIError
		switch {
		case err == nil && current.Status == OperationReady:
			return nil
		case err == nil && (current.Status == OperationFailed || current.Status == OperationTimedOut):
			return &AdmissionFailedError{
				OperationID:  operationID,
				Status:       current.Status,
				ErrorCode:    optionalText(current.ErrorCode),
				ErrorMessage: optionalText(current.ErrorMessage),
			}
		case err == nil:
			// requested, committed, applied: still on its way.
		case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
			return nil
		case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests:
			// A Free plan allows 60 requests a minute, and a wait must not end
			// over one of them: wait what the API asks, never past the
			// deadline. Asking for longer than is left is the same timeout,
			// only later, so it is that timeout now.
			if apiErr.RetryAfter > wait {
				wait = apiErr.RetryAfter
			}
			if wait > time.Until(deadline) {
				return &AdmissionTimeoutError{Wait: timeout, OperationID: operationID}
			}
		case errors.As(err, &apiErr) && apiErr.StatusCode >= 500:
			// The API's trouble, not a verdict on the connection.
		case errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden):
			// The token, not the connection: signing in again fixes it, so
			// it is the API's own error (errors.Is ErrAuth for a revoked token
			// or a missing scope), never a failed admission.
			return err
		case errors.As(err, &apiErr):
			return &AdmissionFailedError{OperationID: operationID, Err: err}
		case err != nil:
			// The API out of reach says nothing about the hub, so it is not a
			// failed admission; it is returned as it is.
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &AdmissionTimeoutError{Wait: timeout, OperationID: operationID}
		}
		if wait < remaining {
			remaining = wait
		}
		if err := sleepContext(ctx, remaining); err != nil {
			return fmt.Errorf("%w: %w", ErrTimeout, err)
		}
	}
}

// sameOrigin reports whether an operation link may be followed with the
// token: a path on the API itself, or an absolute URL with the API's origin --
// its scheme, host and port, the scheme's default port spelled out, so
// https://h and https://h:443 are one origin and http://h and https://h two.
func (c *ControlPlane) sameOrigin(link string) bool {
	lower := strings.ToLower(link)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return !strings.HasPrefix(link, "//")
	}
	target, err := url.Parse(link)
	if err != nil || target.User != nil {
		return false
	}
	api, err := url.Parse(c.APIURL)
	if err != nil {
		return false
	}
	return urlOrigin(target) == urlOrigin(api)
}

// urlOrigin is a URL's scheme, host and port, lower-cased, with the scheme's
// default port filled in when the URL names none.
func urlOrigin(target *url.URL) string {
	scheme := strings.ToLower(target.Scheme)
	port := target.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + strings.ToLower(target.Hostname()) + ":" + port
}

// operationIDFromLink reads the id at the end of a /v1/operations/{id} link.
func operationIDFromLink(link string) string {
	const marker = "/v1/operations/"
	index := strings.LastIndex(link, marker)
	if index < 0 {
		return ""
	}
	id := link[index+len(marker):]
	if cut := strings.IndexAny(id, "?#"); cut >= 0 {
		id = id[:cut]
	}
	id = strings.Trim(id, "/")
	if unescaped, err := url.PathUnescape(id); err == nil {
		id = unescaped
	}
	return id
}

// optionalText is the value of an optional string field, or "".
func optionalText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
