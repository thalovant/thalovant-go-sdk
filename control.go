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

const DefaultControlUserAgent = "ThalovantGoSDK/0.2.2"

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

func NewControlPlane(apiURL string, accessToken string) *ControlPlane {
	return &ControlPlane{
		APIURL:      strings.TrimRight(apiURL, "/") + "/",
		AccessToken: accessToken,
		UserAgent:   DefaultControlUserAgent,
		HTTPClient:  http.DefaultClient,
	}
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

func (c *ControlPlane) GetHub(ctx context.Context, hubID string) (map[string]any, error) {
	return c.request(ctx, http.MethodGet, "/v1/hubs/"+url.PathEscape(hubID), nil, nil, true)
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
	identity := Identity{
		AccessKey:          apiKey,
		Password:           password,
		CryptoKey:          cryptoKey,
		SiteID:             siteID,
		DefaultMaster:      defaultMaster,
		DefaultPort:        443,
		DataPlaneEndpoints: endpoints,
		Protocols:          protocols,
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
		protocol = ProtocolHTTPS
	}
	if protocol != ProtocolHTTPS {
		return nil, fmt.Errorf("%w: %s endpoint metadata is available, but this SDK runtime currently connects through the HTTPS HiveMind HTTP protocol transport", ErrProtocol, strings.ToUpper(string(protocol)))
	}
	endpoint := result.Identity.EndpointFor(ProtocolHTTPS)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: this hub does not expose an HTTPS endpoint for the SDK runtime", ErrProtocol)
	}
	return &SelectedHubEndpoint{Protocol: ProtocolHTTPS, Endpoint: endpoint}, nil
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
