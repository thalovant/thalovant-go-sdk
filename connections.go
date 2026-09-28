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
func refusesConnectionType(err error) (*APIError, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		return nil, false
	}
	text := apiErr.Detail
	if apiErr.Problem != nil {
		if encoded, marshalErr := json.Marshal(apiErr.Problem); marshalErr == nil {
			text = string(encoded)
		}
	}
	return apiErr, strings.Contains(text, "connection_type") || strings.Contains(text, "connectionType")
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
//   - HTTP 5xx, or a request that did not reach the API: ridden out.
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
	for {
		current, err := c.GetOperation(ctx, operationID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w", ErrTimeout, ctxErr)
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
		case errors.As(err, &apiErr) && apiErr.StatusCode >= 500:
			// The API's trouble, not a verdict on the connection.
		case errors.As(err, &apiErr):
			return &AdmissionFailedError{OperationID: operationID, ErrorCode: apiErr.Code, Err: err}
		}
		// A request that did not reach the API is ridden out like a 5xx: a
		// dropped request is not a verdict either, and the deadline still
		// holds.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &AdmissionTimeoutError{Wait: timeout, OperationID: operationID}
		}
		if interval < remaining {
			remaining = interval
		}
		if err := sleepContext(ctx, remaining); err != nil {
			return fmt.Errorf("%w: %w", ErrTimeout, err)
		}
	}
}

// sameOrigin reports whether an operation link may be followed with the
// token: a path on the API itself, or an absolute URL with the API's scheme,
// host and port.
func (c *ControlPlane) sameOrigin(link string) bool {
	lower := strings.ToLower(link)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return !strings.HasPrefix(link, "//")
	}
	target, err := url.Parse(link)
	if err != nil {
		return false
	}
	api, err := url.Parse(c.APIURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(target.Scheme, api.Scheme) && strings.EqualFold(target.Host, api.Host) && target.User == nil
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
