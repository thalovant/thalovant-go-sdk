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
	"strings"
)

const (
	DefaultControlAPIURL    = "https://api.thalovant.com"
	DefaultControlUserAgent = "ThalovantGoSDK/0.3.0"
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
	Admin     bool
	Range     string
	Bucket    string
	OwnerID   string
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

func (c *ControlPlane) Login(ctx context.Context, email string, password string, scope string) (map[string]any, error) {
	payload := map[string]any{"email": email, "password": password}
	if strings.TrimSpace(scope) != "" {
		payload["scope"] = scope
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
	if opts.Admin {
		endpoint = "/v1/admin/analytics/overview"
	}
	query := url.Values{}
	setStringQuery(query, "range", opts.Range)
	setStringQuery(query, "bucket", opts.Bucket)
	if opts.Admin {
		setStringQuery(query, "owner_id", opts.OwnerID)
	}
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
	if includeSecrets {
		identity["access_key"] = r.Identity.AccessKey
		identity["password"] = r.Identity.Password
		identity["crypto_key"] = r.Identity.CryptoKey
		if r.Identity.MQTT != nil {
			identity["mqtt"] = r.Identity.MQTT.Map(true)
		}
	}
	summary := map[string]any{
		"identity":          identity,
		"hub":               r.Hub,
		"client":            r.Client,
		"selected_protocol": r.SelectedProtocol(),
	}
	if r.Endpoint != nil {
		summary["selected_endpoint"] = r.Endpoint.Endpoint
	}
	return summary
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
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIURL+strings.TrimLeft(path, "/"), body)
	if err != nil {
		return nil, err
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
			return nil, fmt.Errorf("%w: missing access token", ErrAPI)
		}
		req.Header.Set("authorization", "Bearer "+c.AccessToken)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPI, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrAPI, resp.StatusCode, string(raw))
	}
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
