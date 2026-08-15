package thalovant

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
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
			return decodeControlJSON(raw)
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
			return nil, fmt.Errorf("%w: HTTP %d: %s", ErrAPI, status, serverErrorDetail(raw))
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
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
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
// The request is idempotent: an Idempotency-Key header is always sent, using
// HubCreateOptions.IdempotencyKey when set and a generated key otherwise, so a
// create retried after a timeout returns the first hub instead of making a
// second one.
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
// stale or empty value fails with HTTP 412 and changes nothing; re-read the
// hub with GetHub and retry with the new etag.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) UpdateHub(ctx context.Context, hubID string, payload map[string]any, etag string) (map[string]any, error) {
	headers := map[string]string{"If-Match": etag}
	return c.request(ctx, http.MethodPatch, "/v1/hubs/"+url.PathEscape(hubID), hubRequestPayload(payload), headers, true)
}

// DeleteHub deletes a hub and its dependent clients and ACLs.
//
// Like UpdateHub this route requires the hub's current etag, sent as If-Match;
// a stale or empty value fails with HTTP 412.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) DeleteHub(ctx context.Context, hubID string, etag string) error {
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

// UpdateRuntimeGroupConfig merges runtime configuration into a runtime group.
//
// The API merges config into the stored configuration rather than replacing
// it, and marks the group pending so the runtime operator reconciles the
// change. RuntimeGroupConfigOptions.Personas is replaced only when non-nil.
//
// Requires a paid plan and a token with the hubs:write scope.
func (c *ControlPlane) UpdateRuntimeGroupConfig(ctx context.Context, runtimeGroupID string, config map[string]any, opts RuntimeGroupConfigOptions) (map[string]any, error) {
	if config == nil {
		config = map[string]any{}
	}
	payload := map[string]any{"config": config}
	if opts.Personas != nil {
		payload["personas"] = opts.Personas
	}
	path := "/v1/runtime-groups/" + url.PathEscape(runtimeGroupID) + "/config"
	return c.request(ctx, http.MethodPatch, path, payload, nil, true)
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
	cryptoKey, err := newControlSecret()
	if err != nil {
		return BootstrapIdentityResult{}, err
	}

	spec := map[string]any{"version": "1"}
	for key, val := range opts.Spec {
		spec[key] = val
	}
	spec["apiKey"] = apiKey
	spec["password"] = password
	spec["cryptoKey"] = cryptoKey
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
			CryptoKey:          cryptoKey,
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
		identity["crypto_key"] = r.Identity.CryptoKey
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

func redactSensitiveTree(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, val := range typed {
			if _, secret := bootstrapSecretKeys[strings.ToLower(key)]; secret {
				cloned[key] = secretPlaceholder
				continue
			}
			cloned[key] = redactSensitiveTree(val)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i, val := range typed {
			cloned[i] = redactSensitiveTree(val)
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
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrAPI, status, serverErrorDetail(raw))
	}
	return decodeControlJSON(raw)
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
		return 0, nil, err
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
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrAPI, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// maxServerErrorDetail bounds how much of a non-2xx response body is echoed
// into an error message.
const maxServerErrorDetail = 256

// serverErrorDetail turns a raw response body into a short, single-line detail
// for an error message. It collapses all whitespace (so an embedded newline
// cannot break log parsing) and truncates to maxServerErrorDetail runes. This
// keeps the error bounded instead of interpolating an unbounded body that, for
// routes such as POST /v1/clients, may echo the request's freshly minted
// apiKey/password/cryptoKey back to the caller.
func serverErrorDetail(raw []byte) string {
	detail := strings.Join(strings.Fields(string(raw)), " ")
	if detail == "" {
		return "(no response body)"
	}
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
func snakeCaseRequestPayload(payload map[string]any, renames map[string]string) map[string]any {
	if payload == nil {
		return nil
	}
	data := make(map[string]any, len(payload))
	for key, val := range payload {
		if target, ok := renames[key]; ok {
			key = target
		}
		data[key] = val
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
