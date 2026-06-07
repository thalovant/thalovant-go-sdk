package thalovant

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Identity struct {
	AccessKey          string                `json:"access_key"`
	Password           string                `json:"password"`
	CryptoKey          string                `json:"crypto_key,omitempty"`
	SiteID             string                `json:"site_id"`
	DefaultMaster      string                `json:"default_master"`
	DefaultPort        int                   `json:"default_port"`
	DefaultPath        string                `json:"default_path,omitempty"`
	PublicKey          string                `json:"public_key,omitempty"`
	DataPlaneEndpoints HubDataPlaneEndpoints `json:"data_plane_endpoints,omitempty"`
	Protocols          HubProtocolSettings   `json:"protocols,omitempty"`
}

func IdentityFromFile(path string) (Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: unable to read identity file %s", ErrIdentity, path)
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return Identity{}, fmt.Errorf("%w: identity file is not valid JSON", ErrIdentity)
	}
	return IdentityFromMap(values)
}

func IdentityFromEnv(prefix string) (Identity, error) {
	if prefix == "" {
		prefix = "THALOVANT_"
	}
	return IdentityFromMap(map[string]any{
		"access_key":     os.Getenv(prefix + "ACCESS_KEY"),
		"password":       os.Getenv(prefix + "PASSWORD"),
		"crypto_key":     os.Getenv(prefix + "CRYPTO_KEY"),
		"site_id":        os.Getenv(prefix + "SITE_ID"),
		"default_master": firstNonEmpty(os.Getenv(prefix+"HUB_HTTP_HOST"), os.Getenv(prefix+"DEFAULT_MASTER")),
		"default_port":   firstNonEmpty(os.Getenv(prefix+"HUB_HTTP_PORT"), os.Getenv(prefix+"DEFAULT_PORT")),
		"default_path":   firstNonEmpty(os.Getenv(prefix+"HUB_HTTP_PATH"), os.Getenv(prefix+"DEFAULT_PATH")),
		"data_plane_endpoints": map[string]any{
			"https": firstNonEmpty(os.Getenv(prefix+"HUB_HTTPS_HOST"), os.Getenv(prefix+"HUB_HTTP_HOST")),
			"wss":   firstNonEmpty(os.Getenv(prefix+"HUB_WSS_HOST"), os.Getenv(prefix+"HUB_WEBSOCKET_HOST")),
			"mqtt":  os.Getenv(prefix + "HUB_MQTT_HOST"),
		},
	})
}

func IdentityFromMap(values map[string]any) (Identity, error) {
	port, err := intValue(value(values, "default_port", "port", "hub_http_port"))
	if err != nil {
		return Identity{}, err
	}
	if port == 0 {
		port = 5679
	}
	identity := Identity{
		AccessKey:          required(value(values, "access_key", "key", "api_key"), "access_key"),
		Password:           required(value(values, "password"), "password"),
		CryptoKey:          optional(value(values, "crypto_key", "cryptoKey")),
		SiteID:             required(value(values, "site_id", "siteId", "site"), "site_id"),
		DefaultMaster:      strings.TrimRight(required(value(values, "default_master", "host", "hub_http_host", "master"), "default_master"), "/"),
		DefaultPort:        port,
		DefaultPath:        normalizePath(optional(value(values, "default_path", "defaultPath", "hub_http_path", "path", "uri_path"))),
		PublicKey:          optional(value(values, "public_key", "publicKey")),
		DataPlaneEndpoints: DataPlaneEndpointsFromMap(values),
		Protocols:          ProtocolSettingsFromMap(values),
	}
	if identity.AccessKey == "" || identity.Password == "" || identity.SiteID == "" || identity.DefaultMaster == "" {
		return Identity{}, ErrIdentity
	}
	return identity, nil
}

func (i Identity) EndpointBase() string {
	return i.DataPlaneEndpoints.HTTPBase(i.DefaultMaster, i.DefaultPort, i.DefaultPath)
}

func (i Identity) EndpointFor(protocol HubProtocol) string {
	if protocol == ProtocolHTTPS {
		return i.EndpointBase()
	}
	return i.DataPlaneEndpoints.EndpointFor(protocol)
}

func (i Identity) EnabledProtocols() []HubProtocol {
	return i.Protocols.EnabledProtocols()
}

func (i Identity) SupportsProtocol(protocol HubProtocol) bool {
	return i.Protocols.IsEnabled(protocol)
}

func (i Identity) Summary() map[string]any {
	summary := map[string]any{
		"site_id":        i.SiteID,
		"default_master": i.DefaultMaster,
		"default_port":   i.DefaultPort,
		"default_path":   i.DefaultPath,
	}
	if endpoints := i.DataPlaneEndpoints.Map(true); len(endpoints) > 0 {
		summary["data_plane_endpoints"] = endpoints
	}
	return summary
}

func value(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if raw, ok := values[key]; ok {
			return raw
		}
	}
	return nil
}

func required(raw any, field string) string {
	val := optional(raw)
	if val == "" {
		return ""
	}
	return val
}

func optional(raw any) string {
	if raw == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

func intValue(raw any) (int, error) {
	if raw == nil || optional(raw) == "" {
		return 0, nil
	}
	switch val := raw.(type) {
	case int:
		return val, nil
	case float64:
		return int(val), nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			return 0, fmt.Errorf("%w: default_port must be an integer", ErrIdentity)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%w: default_port must be an integer", ErrIdentity)
	}
}

func firstNonEmpty(values ...string) string {
	for _, val := range values {
		if strings.TrimSpace(val) != "" {
			return val
		}
	}
	return ""
}

func normalizePath(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	return "/" + path
}

func joinURLPath(parts ...string) string {
	var cleaned []string
	for _, part := range parts {
		part = strings.Trim(part, "/")
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	return "/" + strings.Join(cleaned, "/")
}
