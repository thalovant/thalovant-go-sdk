package thalovant

import (
	"fmt"
	"net/url"
	"strings"
)

type HubProtocol string

const (
	ProtocolWSS   HubProtocol = "wss"
	ProtocolHTTPS HubProtocol = "https"
	ProtocolMQTT  HubProtocol = "mqtt"
)

type HubProtocolSettings struct {
	WSS  bool `json:"wss"`
	HTTP bool `json:"http"`
	MQTT bool `json:"mqtt"`
}

func DefaultHubProtocolSettings() HubProtocolSettings {
	return HubProtocolSettings{WSS: true}
}

func ProtocolSettingsFromMap(values map[string]any) HubProtocolSettings {
	if values == nil {
		return DefaultHubProtocolSettings()
	}
	source := values
	if spec := mapFromAny(values["spec"]); spec != nil {
		source = spec
	}
	protocols := mapFromAny(source["protocols"])
	network := mapFromAny(source["network"])
	return HubProtocolSettings{
		WSS:  enabledValue(firstValue(protocols, network, "wss", "websocket"), true),
		HTTP: enabledValue(firstValue(protocols, network, "http", "https"), false),
		MQTT: enabledValue(firstValue(protocols, network, "mqtt"), false),
	}
}

func (s HubProtocolSettings) EnabledProtocols() []HubProtocol {
	enabled := make([]HubProtocol, 0, 3)
	if s.WSS {
		enabled = append(enabled, ProtocolWSS)
	}
	if s.HTTP {
		enabled = append(enabled, ProtocolHTTPS)
	}
	if s.MQTT {
		enabled = append(enabled, ProtocolMQTT)
	}
	return enabled
}

func (s HubProtocolSettings) IsEnabled(protocol HubProtocol) bool {
	switch protocol {
	case ProtocolWSS:
		return s.WSS
	case ProtocolHTTPS:
		return s.HTTP
	case ProtocolMQTT:
		return s.MQTT
	default:
		return false
	}
}

func (s HubProtocolSettings) SpecMap() map[string]any {
	return map[string]any{
		"wss":  map[string]any{"enabled": s.WSS},
		"http": map[string]any{"enabled": s.HTTP},
		"mqtt": map[string]any{"enabled": s.MQTT},
	}
}

type HubDataPlaneEndpoints struct {
	HTTPS string `json:"https,omitempty"`
	WSS   string `json:"wss,omitempty"`
	MQTT  string `json:"mqtt,omitempty"`
}

func DataPlaneEndpointsFromMap(values map[string]any) HubDataPlaneEndpoints {
	if values == nil {
		return HubDataPlaneEndpoints{}
	}
	source := values
	for _, key := range []string{"data_plane_endpoints", "dataPlaneEndpoints", "endpoints"} {
		if candidate := mapFromAny(values[key]); candidate != nil {
			source = candidate
			break
		}
	}
	return HubDataPlaneEndpoints{
		HTTPS: normalizeEndpoint(value(source, "https", "http")),
		WSS:   normalizeEndpoint(value(source, "wss", "ws")),
		MQTT:  normalizeEndpoint(value(source, "mqtt", "mqtts")),
	}
}

func DataPlaneEndpointsFromHub(hub map[string]any) HubDataPlaneEndpoints {
	endpoints := DataPlaneEndpointsFromMap(hub)
	settings := ProtocolSettingsFromMap(hub)
	domain := optional(value(hub, "domain"))
	if domain == "" {
		return endpoints
	}
	if endpoints.HTTPS == "" && settings.HTTP {
		endpoints.HTTPS = EndpointFromDomain(domain, ProtocolHTTPS)
	}
	if endpoints.WSS == "" && settings.WSS {
		endpoints.WSS = EndpointFromDomain(domain, ProtocolWSS)
	}
	return endpoints
}

func (e HubDataPlaneEndpoints) EndpointFor(protocol HubProtocol) string {
	switch protocol {
	case ProtocolHTTPS:
		return e.HTTPS
	case ProtocolWSS:
		return e.WSS
	case ProtocolMQTT:
		return e.MQTT
	default:
		return ""
	}
}

func (e HubDataPlaneEndpoints) HTTPBase(fallbackMaster string, fallbackPort int, fallbackPath string) string {
	if e.HTTPS != "" {
		return endpointBase(e.HTTPS, fallbackPort, "")
	}
	host := strings.Replace(fallbackMaster, "wss://", "https://", 1)
	host = strings.Replace(host, "ws://", "http://", 1)
	return endpointBase(host, fallbackPort, fallbackPath)
}

func (e HubDataPlaneEndpoints) Map(redactCredentials bool) map[string]string {
	data := map[string]string{}
	for key, endpoint := range map[string]string{
		"https": e.HTTPS,
		"wss":   e.WSS,
		"mqtt":  e.MQTT,
	} {
		if endpoint == "" {
			continue
		}
		if redactCredentials {
			endpoint = redactEndpointCredentials(endpoint)
		}
		data[key] = endpoint
	}
	return data
}

func EndpointFromDomain(domain string, protocol HubProtocol) string {
	normalized := strings.TrimRight(strings.TrimSpace(domain), "/")
	switch protocol {
	case ProtocolWSS:
		if strings.HasPrefix(normalized, "wss://") || strings.HasPrefix(normalized, "ws://") {
			return normalizeEndpoint(normalized)
		}
		if strings.HasPrefix(normalized, "https://") || strings.HasPrefix(normalized, "http://") {
			return normalizeEndpoint(replaceAnyPrefix(normalized, map[string]string{
				"https://": "wss://",
				"http://":  "wss://",
			}))
		}
		return normalizeEndpoint("wss://" + normalized)
	case ProtocolHTTPS:
		if strings.HasPrefix(normalized, "https://") || strings.HasPrefix(normalized, "http://") {
			return normalizeEndpoint(replaceAnyPrefix(normalized, map[string]string{"http://": "https://"}))
		}
		if strings.HasPrefix(normalized, "wss://") || strings.HasPrefix(normalized, "ws://") {
			return normalizeEndpoint(replaceAnyPrefix(normalized, map[string]string{
				"wss://": "https://",
				"ws://":  "https://",
			}))
		}
		return normalizeEndpoint("https://" + normalized)
	default:
		return ""
	}
}

func endpointBase(master string, defaultPort int, defaultPath string) string {
	if parsed, err := url.Parse(master); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		netloc := parsed.Host
		hostPart := netloc
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		if !strings.Contains(hostPart, ":") {
			netloc = fmt.Sprintf("%s:%d", netloc, defaultPort)
		}
		path := joinURLPath(parsed.Path, defaultPath)
		return fmt.Sprintf("%s://%s%s", parsed.Scheme, netloc, path)
	}
	return fmt.Sprintf("%s:%d%s", strings.TrimRight(master, "/"), defaultPort, defaultPath)
}

func mapFromAny(raw any) map[string]any {
	if values, ok := raw.(map[string]any); ok {
		return values
	}
	if values, ok := raw.(map[string]string); ok {
		converted := make(map[string]any, len(values))
		for key, value := range values {
			converted[key] = value
		}
		return converted
	}
	return nil
}

func firstValue(primary map[string]any, secondary map[string]any, keys ...string) any {
	for _, source := range []map[string]any{primary, secondary} {
		if source == nil {
			continue
		}
		for _, key := range keys {
			if raw, ok := source[key]; ok {
				return raw
			}
		}
	}
	return nil
}

func enabledValue(raw any, fallback bool) bool {
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	case map[string]any:
		return enabledValue(value["enabled"], fallback)
	}
	return fallback
}

func normalizeEndpoint(raw any) string {
	endpoint := optional(raw)
	if endpoint == "" {
		return ""
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	switch parsed.Scheme {
	case "http", "https", "ws", "wss", "mqtt", "mqtts":
		return strings.TrimRight(endpoint, "/")
	default:
		return ""
	}
}

func replaceAnyPrefix(value string, replacements map[string]string) string {
	for prefix, replacement := range replacements {
		if strings.HasPrefix(value, prefix) {
			return replacement + strings.TrimPrefix(value, prefix)
		}
	}
	return value
}

func redactEndpointCredentials(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	parsed.User = nil
	return strings.TrimRight(parsed.String(), "/")
}
