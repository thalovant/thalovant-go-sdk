package thalovant

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
)

const (
	DefaultControlAPIURL    = "https://api.thalovant.com"
	DefaultControlUserAgent = userAgent

	// DefaultDeviceLoginTimeout bounds how long LoginWithBrowser waits for the
	// user to approve the sign-in request in the browser.
	DefaultDeviceLoginTimeout = 15 * time.Minute

	defaultDevicePollInterval = 5 * time.Second
	deviceSlowDownIncrement   = 5 * time.Second
)

type OperationStatus string

const (
	OperationRequested OperationStatus = "requested"
	OperationCommitted OperationStatus = "committed"
	OperationApplied   OperationStatus = "applied"
	OperationReady     OperationStatus = "ready"
	OperationFailed    OperationStatus = "failed"
	OperationTimedOut  OperationStatus = "timed_out"
)

type OperationResource struct {
	ID            string             `json:"id"`
	Kind          string             `json:"kind"`
	AggregateType string             `json:"aggregate_type"`
	AggregateID   *string            `json:"aggregate_id"`
	Status        OperationStatus    `json:"status"`
	Details       map[string]any     `json:"details"`
	GitCommitSHA  *string            `json:"git_commit_sha"`
	ErrorCode     *string            `json:"error_code"`
	ErrorMessage  *string            `json:"error_message"`
	CreatedAt     string             `json:"created_at"`
	UpdatedAt     string             `json:"updated_at"`
	CommittedAt   *string            `json:"committed_at"`
	AppliedAt     *string            `json:"applied_at"`
	ReadyAt       *string            `json:"ready_at"`
	TerminalAt    *string            `json:"terminal_at"`
	Links         map[string]*string `json:"links"`
}

type ControlPlane struct {
	APIURL      string
	AccessToken string
	UserAgent   string
	HTTPClient  *http.Client
}

// String implements fmt.Stringer so the %v, %s, and %+v verbs render a
// ControlPlane with its AccessToken (a bearer API token) redacted. The receiver
// is a value so a dereferenced *ControlPlane printed with %v is redacted too.
// This is a human-facing formatting guard only; it does not affect json.Marshal.
func (c ControlPlane) String() string {
	return fmt.Sprintf(
		"ControlPlane{APIURL:%q AccessToken:%s UserAgent:%q}",
		c.APIURL, redactSecret(c.AccessToken), c.UserAgent,
	)
}

type BootstrapIdentityOptions struct {
	Name               string
	SiteID             string
	Spec               map[string]any
	OwnerID            string
	Active             *bool
	PreferredProtocols []HubProtocol
	IdempotencyKey     string
}

type BootstrapIdentityResult struct {
	Identity Identity
	Hub      map[string]any
	Client   map[string]any
	Endpoint *SelectedHubEndpoint
}

type AnalyticsOverviewOptions struct {
	Range     string
	Bucket    string
	HubID     string
	ClientID  string
	Country   string
	Message   string
	Utterance string
	Intent    string
	TimeStart string
	TimeEnd   string
	Weekday   *int
	Hour      *int
}

type MemoryListOptions struct {
	Scope          string
	Kind           string
	OwnerID        string
	HubID          string
	Query          string
	IncludeDeleted bool
	IncludeExpired bool
	Limit          int
	Offset         int
}

// HubCreateOptions carries the optional inputs of CreateHub. IdempotencyKey
// overrides the key the SDK generates for the Idempotency-Key header; leave it
// empty to let CreateHub mint one.
type HubCreateOptions struct {
	IdempotencyKey string
}

// ReleaseOptions carries the release policy ReleaseHub and ReleaseRuntimeGroup
// apply. Every field is optional and an unset field is omitted from the request
// body, so the API falls back to the workspace release policy for it. Setting
// Images switches the target to "custom" mode unless Mode is also set.
type ReleaseOptions struct {
	Channel string
	Mode    string
	Version string
	Images  map[string]string
	Reason  string
}

// RuntimeGroupConfigOptions carries the optional inputs of
// UpdateRuntimeGroupConfig. Personas replaces the stored personas when
// non-nil and is omitted from the request body when nil.
type RuntimeGroupConfigOptions struct {
	Personas map[string]any
}

// RuntimeGroupSkillInstallOptions carries the optional inputs of
// InstallRuntimeGroupSkill. The zero value installs an active skill from the
// marketplace catalog: SourceType defaults to "catalog" when empty and Active
// defaults to true when nil. A "git" install needs SourceRef set to the
// repository URL.
type RuntimeGroupSkillInstallOptions struct {
	MarketplaceSkillID string
	SourceType         string
	SourceRef          string
	VersionPin         string
	Active             *bool
}

// MarketplaceSkillListOptions carries the optional inputs of
// ListMarketplaceSkills. OwnerID and IncludeInactive are honored for admin
// tokens only; the API silently scopes a non-admin caller to their own tenant
// and to active entries instead of failing. ForceRefresh re-syncs the global
// catalog from its source before answering, which is slower.
type MarketplaceSkillListOptions struct {
	OwnerID         string
	IncludeInactive bool
	ForceRefresh    bool
}

// RuntimeGroupMarketplaceOptions carries the optional inputs of
// ListRuntimeGroupMarketplace. RefreshInventory forces a live read from the
// runtime operator instead of answering from the cached inventory snapshot.
type RuntimeGroupMarketplaceOptions struct {
	RefreshInventory bool
}

// RuntimeGroupInventoryOptions carries the optional inputs of
// ListRuntimeGroupInventory. Refresh forces a live read from the runtime
// operator; the API also refreshes on its own when it holds no cached
// snapshot.
type RuntimeGroupInventoryOptions struct {
	Refresh bool
}

func NewControlPlane(apiURL string, accessToken string) *ControlPlane {
	return &ControlPlane{
		APIURL:      normalizeControlAPIURL(apiURL),
		AccessToken: accessToken,
		UserAgent:   DefaultControlUserAgent,
		HTTPClient:  http.DefaultClient,
	}
}

func NewDefaultControlPlane(accessToken string) *ControlPlane {
	return NewControlPlane(DefaultControlAPIURL, accessToken)
}

// LoginOptions carries optional login inputs. Scope overrides the default
// token scopes. OTPCode and RecoveryCode satisfy an MFA challenge; the API
// rejects MFA-enabled accounts with HTTP 401 {"code": "mfa_required"} when
// neither is provided.
type LoginOptions struct {
	Scope        string
	OTPCode      string
	RecoveryCode string
}

func (c *ControlPlane) Login(ctx context.Context, email string, password string, scope string) (map[string]any, error) {
	return c.LoginWithOptions(ctx, email, password, LoginOptions{Scope: scope})
}

func (c *ControlPlane) LoginWithOptions(ctx context.Context, email string, password string, opts LoginOptions) (map[string]any, error) {
	payload := map[string]any{"email": email, "password": password}
	if strings.TrimSpace(opts.Scope) != "" {
		payload["scope"] = opts.Scope
	}
	if strings.TrimSpace(opts.OTPCode) != "" {
		payload["otp_code"] = opts.OTPCode
	}
	if strings.TrimSpace(opts.RecoveryCode) != "" {
		payload["recovery_code"] = opts.RecoveryCode
	}
	token, err := c.request(ctx, http.MethodPost, "/v1/auth/token", payload, nil, false)
	if err != nil {
		return nil, err
	}
	accessToken, _ := token["access_token"].(string)
	if accessToken == "" {
		return nil, fmt.Errorf("%w: token response did not include access_token", ErrAPI)
	}
	c.AccessToken = accessToken
	return token, nil
}

// DeviceLoginOptions carries optional device-flow sign-in inputs for
// LoginWithBrowser. Scopes and ClientName are forwarded to the device
// authorization request when set; the server may expand the echoed scopes
// during normalization. OpenBrowser defaults to true when nil. Prompt, when
// set, receives the device authorization payload instead of the default
// message printed to stdout. Timeout bounds the whole approval wait and
// defaults to DefaultDeviceLoginTimeout when zero.
type DeviceLoginOptions struct {
	Scopes      []string
	ClientName  string
	OpenBrowser *bool
	Prompt      func(grant map[string]any)
	Timeout     time.Duration
}

// LoginWithBrowser signs in through the browser device flow and stores the
// returned API token. This is the sign-in path for accounts without a
// password (for example Google sign-in). It requests a device authorization,
// tells the user to visit verification_uri and enter the short user_code
// (set DeviceLoginOptions.Prompt to present it yourself), opens the browser
// at verification_uri_complete on a best-effort basis unless
// DeviceLoginOptions.OpenBrowser is false, and polls until the request is
// approved, denied, expired, the timeout elapses, or ctx is cancelled.
//
// On approval the returned access_token is a durable scoped API token and is
// stored on ControlPlane.AccessToken exactly like Login. Denial, expiry, and
// timeout are reported as ErrDeviceAccessDenied, ErrDeviceCodeExpired, and
// ErrTimeout respectively.
func (c *ControlPlane) LoginWithBrowser(ctx context.Context, opts DeviceLoginOptions) (map[string]any, error) {
	payload := map[string]any{}
	if opts.Scopes != nil {
		payload["scopes"] = opts.Scopes
	}
	if strings.TrimSpace(opts.ClientName) != "" {
		payload["client_name"] = opts.ClientName
	}
	grant, err := c.request(ctx, http.MethodPost, "/v1/auth/device/authorize", payload, nil, false)
	if err != nil {
		return nil, err
	}
	deviceCode := optional(grant["device_code"])
	userCode := optional(grant["user_code"])
	verificationURI := optional(grant["verification_uri"])
	if deviceCode == "" || userCode == "" || verificationURI == "" {
		return nil, fmt.Errorf("%w: device authorization response was incomplete", ErrAPI)
	}
	if err := validateBrowserURL(verificationURI); err != nil {
		return nil, err
	}
	if completeURI := optional(grant["verification_uri_complete"]); completeURI != "" {
		if err := validateBrowserURL(completeURI); err != nil {
			return nil, err
		}
	}
	interval := defaultDevicePollInterval
	if raw, ok := grant["interval"].(float64); ok && raw >= 0 {
		interval = time.Duration(raw * float64(time.Second))
	}

	if opts.Prompt != nil {
		opts.Prompt(grant)
	} else {
		fmt.Printf("To sign in, visit %s and enter the code %s\n", verificationURI, userCode)
	}
	if opts.OpenBrowser == nil || *opts.OpenBrowser {
		if completeURI := optional(grant["verification_uri_complete"]); completeURI != "" {
			// Browser availability is best-effort; a headless host is fine.
			_ = openBrowser(completeURI)
		}
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultDeviceLoginTimeout
	}
	token, err := c.pollDeviceToken(ctx, deviceCode, interval, timeout, sleepContext, time.Now)
	if err != nil {
		return nil, err
	}
	accessToken, _ := token["access_token"].(string)
	if accessToken == "" {
		return nil, fmt.Errorf("%w: token response did not include access_token", ErrAPI)
	}
	c.AccessToken = accessToken
	return token, nil
}

// pollDeviceToken polls the device token endpoint until approval or a
// terminal state. sleep and now are injectable so tests can drive the loop
// without real waiting.
func (c *ControlPlane) pollDeviceToken(
	ctx context.Context,
	deviceCode string,
	interval time.Duration,
	timeout time.Duration,
	sleep func(context.Context, time.Duration) error,
	now func() time.Time,
) (map[string]any, error) {
	deadline := now().Add(timeout)
	wait := interval
	for {
		status, raw, err := c.send(ctx, http.MethodPost, "/v1/auth/device/token", map[string]any{"device_code": deviceCode}, nil, false)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}
		if status >= 200 && status <= 299 {
			result, decodeErr := decodeControlJSON(raw)
			if decodeErr != nil {
				return nil, &APIError{StatusCode: status, Detail: "invalid JSON response"}
			}
			return result, nil
		}
		errorCode := ""
		if status == http.StatusBadRequest {
			if body, decodeErr := decodeControlJSON(raw); decodeErr == nil {
				errorCode, _ = body["error"].(string)
			}
		}
		switch errorCode {
		case "authorization_pending":
			// Keep polling.
		case "slow_down":
			wait += deviceSlowDownIncrement
		case "access_denied":
			return nil, fmt.Errorf("%w: the device sign-in request was denied in the browser", ErrDeviceAccessDenied)
		case "expired_token":
			return nil, fmt.Errorf("%w: the device sign-in code expired before it was approved; call LoginWithBrowser again to request a new code", ErrDeviceCodeExpired)
		default:
			return nil, &APIError{StatusCode: status, Detail: serverErrorDetail(raw)}
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return nil, fmt.Errorf("%w: timed out waiting for the device sign-in to be approved", ErrTimeout)
		}
		if wait < remaining {
			remaining = wait
		}
		if err := sleep(ctx, remaining); err != nil {
			return nil, err
		}
	}
}

// openBrowser launches the platform browser opener. It is a package variable
// so tests can capture the opened URL without spawning a process.
var openBrowser = func(target string) error {
	cmd, err := browserCommand(target, runtime.GOOS)
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// validateBrowserURL rejects option-like targets, local files, executable URI
// schemes and embedded credentials before anything is displayed or launched.
func validateBrowserURL(target string) error {
	endpoint, err := url.Parse(target)
	invalid := err != nil || strings.IndexFunc(target, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0
	if !invalid {
		invalid = (!strings.EqualFold(endpoint.Scheme, "http") && !strings.EqualFold(endpoint.Scheme, "https")) || endpoint.Hostname() == "" || endpoint.User != nil
	}
	if invalid {
		return fmt.Errorf("%w: device verification URL must be an HTTP(S) URL with a host and no embedded credentials", ErrAPI)
	}
	return nil
}

// browserCommand passes the validated URL as one argument, never through a shell.
func browserCommand(target, platform string) (*exec.Cmd, error) {
	if err := validateBrowserURL(target); err != nil {
		return nil, err
	}
	switch platform {
	case "darwin":
		return exec.Command("open", target), nil
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target), nil
	default:
		return exec.Command("xdg-open", target), nil
	}
}

// sleepContext waits for the duration or until ctx is cancelled.
func sleepContext(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *ControlPlane) ListHubs(ctx context.Context, limit int, cursor string, ownerID string) (map[string]any, error) {
	if limit <= 0 {
		limit = 100
	}
	query := url.Values{"limit": []string{fmt.Sprint(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if ownerID != "" {
		query.Set("owner_id", ownerID)
	}
	return c.request(ctx, http.MethodGet, "/v1/hubs?"+query.Encode(), nil, nil, true)
}

func (c *ControlPlane) ListPublicHubs(ctx context.Context, limit int, cursor string) (map[string]any, error) {
	if limit <= 0 {
		limit = 24
	}
	query := url.Values{"limit": []string{fmt.Sprint(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	return c.request(ctx, http.MethodGet, "/v1/public/hubs?"+query.Encode(), nil, nil, false)
}

func (c *ControlPlane) GetOperation(ctx context.Context, operationID string) (OperationResource, error) {
	payload, err := c.request(ctx, http.MethodGet, "/v1/operations/"+url.PathEscape(operationID), nil, nil, true)
	if err != nil {
		return OperationResource{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return OperationResource{}, fmt.Errorf("%w: encode operation response: %v", ErrAPI, err)
	}
	var operation OperationResource
	if err := json.Unmarshal(encoded, &operation); err != nil {
		return OperationResource{}, fmt.Errorf("%w: decode operation response: %v", ErrAPI, err)
	}
	return operation, nil
}

func (c *ControlPlane) ListMemoryItems(ctx context.Context, opts MemoryListOptions) (map[string]any, error) {
	query := url.Values{}
	setStringQuery(query, "scope", opts.Scope)
	setStringQuery(query, "kind", opts.Kind)
	setStringQuery(query, "owner_id", opts.OwnerID)
	setStringQuery(query, "hub_id", opts.HubID)
	setStringQuery(query, "q", opts.Query)
	if opts.IncludeDeleted {
		query.Set("include_deleted", "true")
	}
	if opts.IncludeExpired {
		query.Set("include_expired", "true")
	}
	if opts.Limit > 0 {
		query.Set("limit", fmt.Sprint(opts.Limit))
	}
	if opts.Offset > 0 {
		query.Set("offset", fmt.Sprint(opts.Offset))
	}
	path := "/v1/memory"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return c.request(ctx, http.MethodGet, path, nil, nil, true)
}

func (c *ControlPlane) GetMemorySummary(ctx context.Context, ownerID string) (map[string]any, error) {
	path := "/v1/memory/summary"
	query := url.Values{}
	setStringQuery(query, "owner_id", ownerID)
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return c.request(ctx, http.MethodGet, path, nil, nil, true)
}

func (c *ControlPlane) CreateMemoryItem(ctx context.Context, payload map[string]any) (map[string]any, error) {
	return c.request(ctx, http.MethodPost, "/v1/memory", payload, nil, true)
}

func (c *ControlPlane) GetMemoryItem(ctx context.Context, memoryID string) (map[string]any, error) {
	return c.request(ctx, http.MethodGet, "/v1/memory/"+url.PathEscape(memoryID), nil, nil, true)
}

func (c *ControlPlane) UpdateMemoryItem(ctx context.Context, memoryID string, payload map[string]any) (map[string]any, error) {
	return c.request(ctx, http.MethodPatch, "/v1/memory/"+url.PathEscape(memoryID), payload, nil, true)
}

func (c *ControlPlane) DeleteMemoryItem(ctx context.Context, memoryID string) error {
	_, err := c.request(ctx, http.MethodDelete, "/v1/memory/"+url.PathEscape(memoryID), nil, nil, true)
	return err
}

func (c *ControlPlane) GetAnalyticsOverview(ctx context.Context, opts AnalyticsOverviewOptions) (map[string]any, error) {
	endpoint := "/v1/analytics/overview"
	query := url.Values{}
	setStringQuery(query, "range", opts.Range)
	setStringQuery(query, "bucket", opts.Bucket)
	setStringQuery(query, "hub_id", opts.HubID)
	setStringQuery(query, "client_id", opts.ClientID)
	setStringQuery(query, "country", opts.Country)
	setStringQuery(query, "message", opts.Message)
	setStringQuery(query, "utterance", opts.Utterance)
	setStringQuery(query, "intent", opts.Intent)
	setStringQuery(query, "time_start", opts.TimeStart)
	setStringQuery(query, "time_end", opts.TimeEnd)
	if opts.Weekday != nil {
		query.Set("weekday", fmt.Sprint(*opts.Weekday))
	}
	if opts.Hour != nil {
		query.Set("hour", fmt.Sprint(*opts.Hour))
	}
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	return c.request(ctx, http.MethodGet, endpoint, nil, nil, true)
}

func (c *ControlPlane) GetHub(ctx context.Context, hubID string) (map[string]any, error) {
	return c.request(ctx, http.MethodGet, "/v1/hubs/"+url.PathEscape(hubID), nil, nil, true)
}

func (c *ControlPlane) GetPublicHub(ctx context.Context, hubRef string) (map[string]any, error) {
	return c.request(ctx, http.MethodGet, "/v1/public/hubs/"+url.PathEscape(hubRef), nil, nil, false)
}

// CreateHub creates a hub.
//
// payload mirrors the API's hub create body: "name" and "spec" are required,
// and "slug", "namespace", "runtime_group_id", "domain", "active",
// "visibility", "capacity_profile", and "owner_id" are optional. camelCase
// keys are accepted and sent as snake_case.
//
// An Idempotency-Key header is always sent. To retry safely after a timeout,
// reuse the same explicit HubCreateOptions.IdempotencyKey for every attempt.
// An empty option generates a new key for this call only; repeating such a
// call can create another hub.
//
// Requires a paid plan and a token with the hubs:write scope. A free-plan
// token fails with HTTP 402.
func (c *ControlPlane) CreateHub(ctx context.Context, payload map[string]any, opts HubCreateOptions) (map[string]any, error) {
	idempotencyKey := opts.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = NewRequestID()
	}
	headers := map[string]string{"Idempotency-Key": idempotencyKey}
	return c.request(ctx, http.MethodPost, "/v1/hubs", hubRequestPayload(payload), headers, true)
}

// UpdateHub partially updates a hub.
//
// The API enforces optimistic locking on this route, so etag is required: pass
// the "etag" of the hub resource you read and the SDK sends it as If-Match. A
// stale value fails with HTTP 412 and changes nothing; re-read the
// hub with GetHub and retry with the new etag. Empty or whitespace-only etags
// fail locally with ErrAPI before sending a request.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) UpdateHub(ctx context.Context, hubID string, payload map[string]any, etag string) (map[string]any, error) {
	if strings.TrimSpace(etag) == "" {
		return nil, fmt.Errorf("%w: etag is required for hub updates and deletions", ErrAPI)
	}
	headers := map[string]string{"If-Match": etag}
	return c.request(ctx, http.MethodPatch, "/v1/hubs/"+url.PathEscape(hubID), hubRequestPayload(payload), headers, true)
}

// DeleteHub deletes a hub and its dependent clients and ACLs.
//
// Like UpdateHub this route requires the hub's current etag, sent as If-Match;
// a stale value fails with HTTP 412. Empty or whitespace-only etags fail
// locally with ErrAPI before sending a request.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) DeleteHub(ctx context.Context, hubID string, etag string) error {
	if strings.TrimSpace(etag) == "" {
		return fmt.Errorf("%w: etag is required for hub updates and deletions", ErrAPI)
	}
	headers := map[string]string{"If-Match": etag}
	_, err := c.request(ctx, http.MethodDelete, "/v1/hubs/"+url.PathEscape(hubID), nil, headers, true)
	return err
}

// ReleaseHub applies a hub release policy and returns the updated hub.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) ReleaseHub(ctx context.Context, hubID string, opts ReleaseOptions) (map[string]any, error) {
	path := "/v1/hubs/" + url.PathEscape(hubID) + "/release"
	return c.request(ctx, http.MethodPost, path, releaseRequestPayload(opts), nil, true)
}

// SetHubRating rates a public hub from 1 to 5 and returns the updated hub.
//
// Only public hubs can be rated, and owners cannot rate their own hubs.
// Requires a token with the hubs:write scope; unlike the provisioning routes
// this one is not paid-gated.
func (c *ControlPlane) SetHubRating(ctx context.Context, hubID string, rating int) (map[string]any, error) {
	path := "/v1/hubs/" + url.PathEscape(hubID) + "/rating"
	return c.request(ctx, http.MethodPut, path, map[string]any{"rating": rating}, nil, true)
}

// ClearHubRating removes the caller's rating from a public hub and returns the
// updated hub.
//
// Requires a token with the hubs:write scope; it is not paid-gated.
func (c *ControlPlane) ClearHubRating(ctx context.Context, hubID string) (map[string]any, error) {
	path := "/v1/hubs/" + url.PathEscape(hubID) + "/rating"
	return c.request(ctx, http.MethodDelete, path, nil, nil, true)
}

// GetHubRuntimeCapabilities reads the live skill and intent inventory a hub
// runtime exposes.
//
// Requires a token with the hubs:inspect scope. The API answers HTTP 409 when
// the hub has no connected client that can report inventory and no runtime
// group snapshot to fall back on. ListRuntimeGroupInventory is the read that
// reports a pending source instead of failing.
func (c *ControlPlane) GetHubRuntimeCapabilities(ctx context.Context, hubID string) (map[string]any, error) {
	path := "/v1/hubs/" + url.PathEscape(hubID) + "/runtime-capabilities"
	return c.request(ctx, http.MethodGet, path, nil, nil, true)
}

// ListRuntimeGroups lists the runtime groups visible to the authenticated
// user. An empty ownerID is omitted from the query.
//
// Requires a token with the hubs:read scope.
func (c *ControlPlane) ListRuntimeGroups(ctx context.Context, ownerID string) (map[string]any, error) {
	path := "/v1/runtime-groups"
	query := url.Values{}
	setStringQuery(query, "owner_id", ownerID)
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return c.request(ctx, http.MethodGet, path, nil, nil, true)
}

// GetRuntimeGroup fetches one runtime group.
//
// Requires a token with the hubs:read scope.
func (c *ControlPlane) GetRuntimeGroup(ctx context.Context, runtimeGroupID string) (map[string]any, error) {
	return c.request(ctx, http.MethodGet, "/v1/runtime-groups/"+url.PathEscape(runtimeGroupID), nil, nil, true)
}

// CreateRuntimeGroup creates a runtime group.
//
// payload takes the API's create body: "name" is required, and "description",
// "environment", "owner_id", and "clone_from_default" are optional. camelCase
// keys are accepted and sent as snake_case.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) CreateRuntimeGroup(ctx context.Context, payload map[string]any) (map[string]any, error) {
	return c.request(ctx, http.MethodPost, "/v1/runtime-groups", runtimeGroupRequestPayload(payload), nil, true)
}

// UpdateRuntimeGroup updates a runtime group's "name", "description", or
// "spec". "spec" patches "replicas" and container "resources". Unlike the hub
// routes this one reads no If-Match header.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) UpdateRuntimeGroup(ctx context.Context, runtimeGroupID string, payload map[string]any) (map[string]any, error) {
	path := "/v1/runtime-groups/" + url.PathEscape(runtimeGroupID)
	return c.request(ctx, http.MethodPatch, path, runtimeGroupRequestPayload(payload), nil, true)
}

// GetRuntimeGroupConfig reads a runtime group's runtime configuration and
// personas.
//
// Requires a token with the hubs:read scope.
func (c *ControlPlane) GetRuntimeGroupConfig(ctx context.Context, runtimeGroupID string) (map[string]any, error) {
	path := "/v1/runtime-groups/" + url.PathEscape(runtimeGroupID) + "/config"
	return c.request(ctx, http.MethodGet, path, nil, nil, true)
}

// UpdateRuntimeGroupConfig deep-merges with a revision precondition and at most
// three attempts. Only 412 conflicts trigger a fresh read and merge. Older APIs
// fail before writing. Requires hubs:read and paid hubs:write.
func (c *ControlPlane) UpdateRuntimeGroupConfig(ctx context.Context, runtimeGroupID string, config map[string]any, opts RuntimeGroupConfigOptions) (map[string]any, error) {
	path := "/v1/runtime-groups/" + url.PathEscape(runtimeGroupID) + "/config"
	if config == nil {
		config = map[string]any{}
	}
	input := map[string]any{"config": config}
	if opts.Personas != nil {
		input["personas"] = opts.Personas
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var stable map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&stable); err != nil {
		return nil, err
	}
	delta := stable["config"].(map[string]any)
	for attempt := 0; ; attempt++ {
		status, raw, err := c.send(ctx, http.MethodGet, path, nil, nil, true)
		if err != nil {
			return nil, err
		}
		if status < 200 || status > 299 {
			return nil, &APIError{StatusCode: status, Detail: serverErrorDetail(raw)}
		}
		// Preserve untouched JSON integers/decimals exactly when writing the snapshot back.
		var snapshot map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&snapshot); err != nil || !json.Valid(raw) {
			return nil, &APIError{StatusCode: status, Detail: "invalid JSON response"}
		}
		revision, validRevision := snapshot["revision"].(string)
		base, validConfig := snapshot["config"].(map[string]any)
		if !validRevision || !configRevisionPattern.MatchString(revision) || !validConfig {
			return nil, fmt.Errorf("%w: safe configuration merge requires a valid config and revision from the API", ErrAPI)
		}
		payload := map[string]any{"config": mergeRuntimeConfig(base, delta), "expected_revision": revision}
		if personas, ok := stable["personas"]; ok {
			payload["personas"] = personas
		}
		result, err := c.request(ctx, http.MethodPut, path, payload, nil, true)
		var apiError *APIError
		if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusPreconditionFailed || attempt >= 2 {
			return result, err
		}
	}
}

// ReplaceRuntimeGroupConfig explicitly replaces configuration via unconditional PATCH.
// Personas are replaced only when non-nil. Requires paid hubs:write.
func (c *ControlPlane) ReplaceRuntimeGroupConfig(ctx context.Context, runtimeGroupID string, config map[string]any, opts RuntimeGroupConfigOptions) (map[string]any, error) {
	if config == nil {
		config = map[string]any{}
	}
	payload := map[string]any{"config": config}
	if opts.Personas != nil {
		payload["personas"] = opts.Personas
	}
	return c.request(ctx, http.MethodPatch, "/v1/runtime-groups/"+url.PathEscape(runtimeGroupID)+"/config", payload, nil, true)
}

var configRevisionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func mergeRuntimeConfig(base, delta map[string]any) map[string]any {
	result := cloneMap(base)
	for key, value := range delta {
		old, oldOK := base[key].(map[string]any)
		next, nextOK := value.(map[string]any)
		if oldOK && nextOK {
			result[key] = mergeRuntimeConfig(old, next)
		} else {
			result[key] = value
		}
	}
	return result
}

// ReleaseRuntimeGroup applies a runtime image policy and returns the updated
// runtime group. Options behave like ReleaseHub.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) ReleaseRuntimeGroup(ctx context.Context, runtimeGroupID string, opts ReleaseOptions) (map[string]any, error) {
	path := "/v1/runtime-groups/" + url.PathEscape(runtimeGroupID) + "/release"
	return c.request(ctx, http.MethodPost, path, releaseRequestPayload(opts), nil, true)
}

// DeleteRuntimeGroup deletes a runtime group.
//
// The API answers HTTP 409 for the workspace default group and for a group
// that still has hubs attached.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) DeleteRuntimeGroup(ctx context.Context, runtimeGroupID string) error {
	_, err := c.request(ctx, http.MethodDelete, "/v1/runtime-groups/"+url.PathEscape(runtimeGroupID), nil, nil, true)
	return err
}

// InstallRuntimeGroupSkill installs, or re-installs, a skill in a runtime
// group.
//
// The default source type of "catalog" installs a marketplace skill and
// requires the skill to exist in the catalog; a "git" install needs
// RuntimeGroupSkillInstallOptions.SourceRef. Installing a skill that is
// already present updates the existing entry.
//
// Requires a paid plan and a token with the hubs:write scope. Paid marketplace
// skills also need marketplace access on the tenant plan.
func (c *ControlPlane) InstallRuntimeGroupSkill(ctx context.Context, runtimeGroupID string, skillID string, opts RuntimeGroupSkillInstallOptions) (map[string]any, error) {
	sourceType := opts.SourceType
	if sourceType == "" {
		sourceType = "catalog"
	}
	active := true
	if opts.Active != nil {
		active = *opts.Active
	}
	payload := map[string]any{
		"skill_id":    skillID,
		"source_type": sourceType,
		"active":      active,
	}
	if opts.MarketplaceSkillID != "" {
		payload["marketplace_skill_id"] = opts.MarketplaceSkillID
	}
	if opts.SourceRef != "" {
		payload["source_ref"] = opts.SourceRef
	}
	if opts.VersionPin != "" {
		payload["version_pin"] = opts.VersionPin
	}
	path := "/v1/runtime-groups/" + url.PathEscape(runtimeGroupID) + "/skills"
	return c.request(ctx, http.MethodPost, path, payload, nil, true)
}

// UninstallRuntimeGroupSkill removes a skill from a runtime group.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) UninstallRuntimeGroupSkill(ctx context.Context, runtimeGroupID string, skillID string) error {
	path := "/v1/runtime-groups/" + url.PathEscape(runtimeGroupID) + "/skills/" + url.PathEscape(skillID)
	_, err := c.request(ctx, http.MethodDelete, path, nil, nil, true)
	return err
}

// ListMarketplaceSkills lists the marketplace skill catalog visible to the
// authenticated user.
//
// The returned "data" entries carry the catalog fields an install needs --
// "skill_id", "source_type", "source_ref", "package_name", "version"
// compatibility, "config_schema" and "secret_schema" -- alongside presentation
// and access fields such as "category", "tags", "verified", "access_tier" and
// "billing_sku". Global catalog entries and the caller's own tenant entries
// are both included.
//
// Requires a token with the hubs:read scope. Unlike the provisioning routes
// this catalog is not paid-gated, so free-plan callers can browse the
// marketplace before upgrading; only the install itself needs a paid plan.
func (c *ControlPlane) ListMarketplaceSkills(ctx context.Context, opts MarketplaceSkillListOptions) (map[string]any, error) {
	path := "/v1/marketplace/skills"
	query := url.Values{}
	setStringQuery(query, "owner_id", opts.OwnerID)
	if opts.IncludeInactive {
		query.Set("include_inactive", "true")
	}
	if opts.ForceRefresh {
		query.Set("force_refresh", "true")
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return c.request(ctx, http.MethodGet, path, nil, nil, true)
}

// ListRuntimeGroupMarketplace lists the marketplace catalog resolved against
// one runtime group.
//
// This is the discovery view to use before installing: every catalog entry is
// returned with the group's own state folded in -- whether the skill is
// desired ("active", "version_pin", "source_type"), whether it was observed
// running ("observed_source", "observed_at", intent counts), operator status
// fields, and the access verdict for the tenant plan ("purchase_required",
// "installable", "access_message"). The envelope also carries
// "runtime_group_id", "observed_at", "source", "operator_phase" and
// "operator_message".
//
// Requires a token with the hubs:inspect scope; no paid plan is needed to
// browse. The API answers HTTP 404 for an unknown group and HTTP 403 when the
// caller does not own it.
func (c *ControlPlane) ListRuntimeGroupMarketplace(ctx context.Context, runtimeGroupID string, opts RuntimeGroupMarketplaceOptions) (map[string]any, error) {
	path := "/v1/runtime-groups/" + url.PathEscape(runtimeGroupID) + "/marketplace"
	if opts.RefreshInventory {
		path += "?refresh_inventory=true"
	}
	return c.request(ctx, http.MethodGet, path, nil, nil, true)
}

// ListRuntimeGroupInventory lists the skills a runtime group is actually
// observed running.
//
// Where ListRuntimeGroupMarketplace answers "what could be installed here",
// this answers "what is loaded right now": each entry carries "skill_id",
// "version", "source", "active", "adapt_intents", "padatious_intents",
// "total_intents" and "observed_at". The envelope reports the observation's
// provenance in "source" -- "ovos-runtime-operator", "runtime-group-cache" or
// "ovos-runtime-operator-pending" -- plus "operator_phase" and
// "operator_message".
//
// Unlike GetHubRuntimeCapabilities this route does not answer HTTP 409 when
// nothing is reporting: it returns an empty "data" list with a pending
// "source" instead.
//
// Requires a token with the hubs:inspect scope; no paid plan is needed.
func (c *ControlPlane) ListRuntimeGroupInventory(ctx context.Context, runtimeGroupID string, opts RuntimeGroupInventoryOptions) (map[string]any, error) {
	path := "/v1/runtime-groups/" + url.PathEscape(runtimeGroupID) + "/inventory"
	if opts.Refresh {
		path += "?refresh=true"
	}
	return c.request(ctx, http.MethodGet, path, nil, nil, true)
}

func (c *ControlPlane) CreateClient(ctx context.Context, payload map[string]any, idempotencyKey string) (map[string]any, error) {
	if idempotencyKey == "" {
		idempotencyKey = NewRequestID()
	}
	return c.request(ctx, http.MethodPost, "/v1/clients", payload, map[string]string{"Idempotency-Key": idempotencyKey}, true)
}

func (c *ControlPlane) CreateClientIdentityForHubID(ctx context.Context, hubID string, opts BootstrapIdentityOptions) (BootstrapIdentityResult, error) {
	hub, err := c.GetHub(ctx, hubID)
	if err != nil {
		return BootstrapIdentityResult{}, err
	}
	return c.CreateClientIdentity(ctx, hub, opts)
}

func (c *ControlPlane) CreateClientIdentity(ctx context.Context, hub map[string]any, opts BootstrapIdentityOptions) (BootstrapIdentityResult, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return BootstrapIdentityResult{}, fmt.Errorf("%w: client name is required", ErrAPI)
	}
	hubID := optional(value(hub, "id"))
	if hubID == "" {
		return BootstrapIdentityResult{}, fmt.Errorf("%w: hub resource is missing id", ErrAPI)
	}
	siteID := cleanSiteID(firstNonEmpty(opts.SiteID, opts.Name))
	apiKey, err := newControlSecret()
	if err != nil {
		return BootstrapIdentityResult{}, err
	}
	password, err := newControlSecret()
	if err != nil {
		return BootstrapIdentityResult{}, err
	}
	spec := map[string]any{"version": "1"}
	for key, val := range opts.Spec {
		// opts.Spec is caller-supplied, and a legacy crypto key in it would be
		// sent to /v1/clients and could come back inside an ApiError -- the
		// redaction list covers only the secrets minted here. v3 issues no
		// crypto key, so drop both spellings.
		if key == "cryptoKey" || key == "crypto_key" {
			continue
		}
		spec[key] = val
	}
	spec["apiKey"] = apiKey
	spec["password"] = password
	spec["siteId"] = siteID

	active := true
	if opts.Active != nil {
		active = *opts.Active
	}
	payload := map[string]any{
		"hub_id": hubID,
		"name":   opts.Name,
		"spec":   spec,
		"active": active,
	}
	if opts.OwnerID != "" {
		payload["owner_id"] = opts.OwnerID
	}
	client, err := c.CreateClient(ctx, payload, opts.IdempotencyKey)
	if err != nil {
		return BootstrapIdentityResult{}, err
	}

	protocols := ProtocolSettingsFromMap(hub)
	endpoints := DataPlaneEndpointsFromHub(hub)
	selected := SelectDataPlaneEndpoint(endpoints, protocols, opts.PreferredProtocols)
	defaultMaster, err := controlDefaultMaster(hub, endpoints, selected)
	if err != nil {
		return BootstrapIdentityResult{}, err
	}
	var identity Identity
	if initialIdentify := mapFromAny(client["initial_identify"]); initialIdentify != nil {
		initialIdentify["data_plane_endpoints"] = endpoints.Map(false)
		initialIdentify["protocols"] = protocols.SpecMap()
		identity, err = IdentityFromMap(initialIdentify)
		if err != nil {
			return BootstrapIdentityResult{}, err
		}
	} else {
		identity = Identity{
			AccessKey:          apiKey,
			Password:           password,
			SiteID:             siteID,
			DefaultMaster:      defaultMaster,
			DefaultPort:        443,
			DataPlaneEndpoints: endpoints,
			Protocols:          protocols,
		}
	}
	return BootstrapIdentityResult{Identity: identity, Hub: hub, Client: client, Endpoint: selected}, nil
}

func (r BootstrapIdentityResult) SelectedProtocol() HubProtocol {
	if r.Endpoint == nil {
		return ""
	}
	return r.Endpoint.Protocol
}

func (r BootstrapIdentityResult) Summary(includeSecrets bool) map[string]any {
	identity := r.Identity.Summary()
	hub := r.Hub
	client := r.Client
	if includeSecrets {
		identity["access_key"] = r.Identity.AccessKey
		identity["password"] = r.Identity.Password
		if r.Identity.MQTT != nil {
			identity["mqtt"] = r.Identity.MQTT.Map(true)
		}
	} else {
		// The raw hub/client maps echo the freshly minted data-plane secrets:
		// initial_identify's access_key/password/crypto_key/mqtt.password, the
		// initial_identify_token, and the spec's apiKey/password/cryptoKey. Gate
		// them behind includeSecrets exactly like the identity fields above so
		// the default summary is safe to log.
		hub = redactBootstrapSecrets(r.Hub)
		client = redactBootstrapSecrets(r.Client)
	}
	summary := map[string]any{
		"identity":          identity,
		"hub":               hub,
		"client":            client,
		"selected_protocol": r.SelectedProtocol(),
	}
	if r.Endpoint != nil {
		summary["selected_endpoint"] = r.Endpoint.Endpoint
	}
	return summary
}

// bootstrapSecretKeys names the map keys whose values the bootstrap hub/client
// maps may carry as freshly minted data-plane secrets. Comparison is
// case-insensitive so both snake_case and camelCase spellings (crypto_key vs
// cryptoKey, api_key vs apiKey) match. "username" is included because the MQTT
// broker username is the data-plane access key, which the SDK treats as secret
// everywhere else (MqttBrokerCredentials.Map(false) omits it); redacting only
// access_key while leaving the same value under mqtt.username would be an
// incomplete gate.
var bootstrapSecretKeys = map[string]struct{}{
	"access_key":             {},
	"password":               {},
	"crypto_key":             {},
	"cryptokey":              {},
	"api_key":                {},
	"apikey":                 {},
	"username":               {},
	"initial_identify_token": {},
}

// redactBootstrapSecrets returns a deep copy of value with the values of any
// secret-bearing keys (see bootstrapSecretKeys) replaced by secretPlaceholder,
// at every depth. It never mutates value, so the includeSecrets=true path can
// still hand back the raw maps untouched. A nil input stays nil.
func redactBootstrapSecrets(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	redacted, _ := redactSensitiveTree(value).(map[string]any)
	return redacted
}

// redactSensitiveTree returns value with the values of any secret-named keys
// (see bootstrapSecretKeys) replaced by secretPlaceholder, recursing through
// nested maps and slices of ANY key/element type. It reflects rather than
// switching on map[string]any/[]any alone so a caller-built result carrying a
// typed container (map[string]string, []string, []map[string]string, ...)
// cannot smuggle a nested secret past the gate. The source is never mutated:
// each container is rebuilt as a fresh map[string]any / []any.
func redactSensitiveTree(value any) any {
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Map:
		cloned := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			key := fmt.Sprint(iter.Key().Interface())
			if _, secret := bootstrapSecretKeys[strings.ToLower(key)]; secret {
				cloned[key] = secretPlaceholder
				continue
			}
			cloned[key] = redactSensitiveTree(iter.Value().Interface())
		}
		return cloned
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			// A []byte is opaque bytes, not a container of nested values; leave
			// it intact rather than exploding it into a slice of numbers. A
			// secret-named []byte is already redacted by the map branch above
			// before recursion ever reaches it.
			return value
		}
		cloned := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			cloned[i] = redactSensitiveTree(rv.Index(i).Interface())
		}
		return cloned
	default:
		return value
	}
}

func (c *ControlPlane) RequireRuntimeProtocol(result BootstrapIdentityResult, protocol HubProtocol) (*SelectedHubEndpoint, error) {
	if protocol == "" {
		selected, err := defaultRuntimeProtocol(result.Identity)
		if err != nil {
			return nil, err
		}
		protocol = selected
	}
	if protocol == ProtocolMQTT && result.Identity.MQTT == nil {
		return nil, fmt.Errorf("%w: MQTT is enabled, but the API did not return client-scoped MQTT broker credentials", ErrProtocol)
	}
	endpoint := result.Identity.EndpointFor(protocol)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: this hub does not expose a %s endpoint for the SDK runtime", ErrProtocol, strings.ToUpper(string(protocol)))
	}
	return &SelectedHubEndpoint{Protocol: protocol, Endpoint: endpoint}, nil
}

func (c *ControlPlane) request(ctx context.Context, method string, path string, payload map[string]any, headers map[string]string, auth bool) (map[string]any, error) {
	status, raw, err := c.send(ctx, method, path, payload, headers, auth)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, &APIError{StatusCode: status, Detail: serverErrorDetail(raw)}
	}
	result, decodeErr := decodeControlJSON(raw)
	if decodeErr != nil {
		return nil, &APIError{StatusCode: status, Detail: "invalid JSON response"}
	}
	return result, nil
}

func (c *ControlPlane) send(ctx context.Context, method string, path string, payload map[string]any, headers map[string]string, auth bool) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIURL+strings.TrimLeft(path, "/"), body)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: invalid control request", ErrAPI)
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", c.UserAgent)
	if payload != nil {
		req.Header.Set("content-type", "application/json")
	}
	for key, val := range headers {
		req.Header.Set(key, val)
	}
	if auth {
		if c.AccessToken == "" {
			return 0, nil, fmt.Errorf("%w: missing access token", ErrAPI)
		}
		req.Header.Set("authorization", "Bearer "+c.AccessToken)
	}
	// Bind passwords, device codes and bearer credentials to a secure endpoint.
	// Literal loopback development endpoints remain supported without DNS
	// resolution; a remote name resolving to loopback is not an exception.
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	sendsCookies := client.Jar != nil && len(client.Jar.Cookies(req.URL)) != 0
	if auth || payload != nil || req.URL.User != nil || sendsCookies || req.Header.Get("authorization") != "" || req.Header.Get("cookie") != "" || req.Header.Get("proxy-authorization") != "" {
		if err := requireControlCredentialEndpoint(req.URL); err != nil {
			return 0, nil, err
		}
	}
	scopedClient := *client
	// 307/308 redirects can forward the original JSON password body. Keep the
	// injected client's transport/jar while preventing all redirect hops.
	scopedClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := scopedClient.Do(req)
	if err != nil {
		// url.Error includes the complete request URL, and an injected transport
		// may return arbitrary credential-bearing text. Preserve only known,
		// safe context errors; never retain the transport cause in the chain.
		if cause := ctx.Err(); cause == context.Canceled || cause == context.DeadlineExceeded {
			return 0, nil, fmt.Errorf("%w: %w", ErrAPI, cause)
		}
		return 0, nil, fmt.Errorf("%w: control request failed", ErrAPI)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// requireControlCredentialEndpoint permits HTTP only for literal local development.
func requireControlCredentialEndpoint(endpoint *url.URL) error {
	if endpoint.User != nil {
		return fmt.Errorf("%w: control API URL must not contain credentials", ErrAPI)
	}
	if endpoint.Scheme == "https" && endpoint.Hostname() != "" {
		return nil
	}
	if endpoint.Scheme == "http" {
		switch strings.ToLower(endpoint.Hostname()) {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return fmt.Errorf("%w: credential-bearing control requests require HTTPS; HTTP is allowed only for localhost, 127.0.0.1 or [::1] development", ErrAPI)
}

// maxServerErrorDetail bounds how much of a surfaced server message is echoed
// into an error string.
const maxServerErrorDetail = 256

// serverErrorOmitted stands in for a body that is not a recognizable JSON error
// object, so a response that reflects the request cannot launder secrets into an
// error string.
const serverErrorOmitted = "(server error response omitted)"

// serverErrorDetailFields is the allowlist of top-level JSON fields a control
// plane error body may surface. They are the server's own human-readable
// message, never the echoed request payload, which carries the freshly minted
// apiKey/password/cryptoKey under other keys such as "spec".
var serverErrorDetailFields = []string{"detail", "message", "error", "error_description", "code", "title"}

// serverErrorDetail turns a raw non-2xx response body into a short, single-line
// error detail. It never interpolates arbitrary body text: it decodes the body
// as a JSON object and surfaces only the allowlisted, non-secret message fields
// (whitespace-collapsed and length-bounded). A body that is not a JSON object,
// or that carries none of those fields, is omitted entirely -- a failed
// POST /v1/clients response can echo the request's apiKey/password/cryptoKey,
// and truncation alone would not protect a secret near the start of the body.
func serverErrorDetail(raw []byte) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "(no response body)"
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return serverErrorOmitted
	}
	parts := make([]string, 0, len(serverErrorDetailFields))
	for _, field := range serverErrorDetailFields {
		text, ok := decoded[field].(string)
		if !ok {
			continue
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return serverErrorOmitted
	}
	detail := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	if runes := []rune(detail); len(runes) > maxServerErrorDetail {
		detail = string(runes[:maxServerErrorDetail]) + "…"
	}
	return detail
}

func decodeControlJSON(raw []byte) (map[string]any, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return map[string]any{}, nil
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON response", ErrAPI)
	}
	return data, nil
}

func newControlSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func normalizeControlAPIURL(apiURL string) string {
	normalized := strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if normalized == "" {
		normalized = DefaultControlAPIURL
	}
	normalized = strings.TrimSuffix(normalized, "/v1")
	return strings.TrimRight(normalized, "/") + "/"
}

// snakeCaseRequestPayload copies a request body, renaming the camelCase keys
// the API takes as snake_case. Renaming rather than duplicating keeps an
// unknown camelCase key from being silently dropped by the API's request
// model.
// snakeCaseRequestPayload rewrites camelCase caller keys to the snake_case
// spelling the API expects.
//
// A caller that passes BOTH spellings of one field -- ownerId and owner_id --
// used to get whichever the map happened to yield last, and Go randomizes map
// iteration order, so the same call could send either value from one run to
// the next. The already-correct spelling wins instead: it is what the API
// documents, and it makes the outcome the same every time.
func snakeCaseRequestPayload(payload map[string]any, renames map[string]string) map[string]any {
	if payload == nil {
		return nil
	}
	data := make(map[string]any, len(payload))
	// Canonical keys first, so a rename can never displace one.
	for key, val := range payload {
		if _, renamed := renames[key]; !renamed {
			data[key] = val
		}
	}
	for key, val := range payload {
		target, renamed := renames[key]
		if !renamed {
			continue
		}
		if _, taken := data[target]; taken {
			continue
		}
		data[target] = val
	}
	return data
}

func hubRequestPayload(payload map[string]any) map[string]any {
	return snakeCaseRequestPayload(payload, map[string]string{
		"ownerId":         "owner_id",
		"runtimeGroupId":  "runtime_group_id",
		"capacityProfile": "capacity_profile",
		"isLocked":        "is_locked",
	})
}

func runtimeGroupRequestPayload(payload map[string]any) map[string]any {
	return snakeCaseRequestPayload(payload, map[string]string{
		"ownerId":          "owner_id",
		"cloneFromDefault": "clone_from_default",
	})
}

// releaseRequestPayload builds a release-apply body, omitting the options the
// caller left unset. The result is never nil, so a fully unset ReleaseOptions
// still sends an empty JSON object rather than no body at all.
func releaseRequestPayload(opts ReleaseOptions) map[string]any {
	payload := map[string]any{}
	if opts.Channel != "" {
		payload["channel"] = opts.Channel
	}
	if opts.Mode != "" {
		payload["mode"] = opts.Mode
	}
	if opts.Version != "" {
		payload["version"] = opts.Version
	}
	if opts.Images != nil {
		payload["images"] = opts.Images
	}
	if opts.Reason != "" {
		payload["reason"] = opts.Reason
	}
	return payload
}

func setStringQuery(query url.Values, key string, val string) {
	if strings.TrimSpace(val) != "" {
		query.Set(key, val)
	}
}

func cleanSiteID(value string) string {
	cleaned := strings.ReplaceAll(strings.TrimSpace(value), "_", "-")
	cleaned = strings.Join(strings.Fields(cleaned), "-")
	if cleaned == "" {
		return "thalovant-client"
	}
	return cleaned
}

func controlDefaultMaster(hub map[string]any, endpoints HubDataPlaneEndpoints, selected *SelectedHubEndpoint) (string, error) {
	if endpoints.HTTPS != "" {
		return stripEndpointPath(endpoints.HTTPS), nil
	}
	if domain := optional(value(hub, "domain")); domain != "" {
		return EndpointFromDomain(domain, ProtocolHTTPS), nil
	}
	if selected != nil {
		return stripEndpointPath(selected.Endpoint), nil
	}
	return "", fmt.Errorf("%w: hub resource does not expose a usable data-plane endpoint", ErrAPI)
}

func stripEndpointPath(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(endpoint, "/")
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}
