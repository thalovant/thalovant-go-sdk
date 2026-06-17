package thalovant

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultConfigFilename = "config.yaml"

type Identity struct {
	AccessKey          string                 `json:"access_key"`
	Password           string                 `json:"password"`
	CryptoKey          string                 `json:"crypto_key,omitempty"`
	SiteID             string                 `json:"site_id"`
	DefaultMaster      string                 `json:"default_master"`
	DefaultPort        int                    `json:"default_port"`
	DefaultPath        string                 `json:"default_path,omitempty"`
	PublicKey          string                 `json:"public_key,omitempty"`
	Metadata           map[string]any         `json:"metadata,omitempty"`
	DataPlaneEndpoints HubDataPlaneEndpoints  `json:"data_plane_endpoints,omitempty"`
	Protocols          HubProtocolSettings    `json:"protocols,omitempty"`
	MQTT               *MqttBrokerCredentials `json:"mqtt,omitempty"`
}

type MqttBrokerCredentials struct {
	Endpoint    string `json:"endpoint"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	TopicPrefix string `json:"topic_prefix,omitempty"`
	HubID       string `json:"hub_id,omitempty"`
	C2STopic    string `json:"c2s_topic,omitempty"`
	S2CTopic    string `json:"s2c_topic,omitempty"`
	StatusTopic string `json:"status_topic,omitempty"`
	HashTopics  bool   `json:"hash_topics,omitempty"`
	QOS         byte   `json:"qos,omitempty"`
	TLS         bool   `json:"tls"`
}

func MqttBrokerCredentialsFromMap(raw any) *MqttBrokerCredentials {
	values := mapFromAny(raw)
	if values == nil {
		return nil
	}
	endpoint := optional(value(values, "endpoint", "broker_url", "brokerUrl"))
	username := optional(value(values, "username", "broker_username", "brokerUsername"))
	password := optional(value(values, "password", "broker_password", "brokerPassword"))
	if endpoint == "" || username == "" || password == "" {
		return nil
	}
	return &MqttBrokerCredentials{
		Endpoint:    endpoint,
		Username:    username,
		Password:    password,
		TopicPrefix: optional(value(values, "topic_prefix", "topicPrefix")),
		HubID:       optional(value(values, "hub_id", "hubId")),
		C2STopic:    optional(value(values, "c2s_topic", "c2sTopic")),
		S2CTopic:    optional(value(values, "s2c_topic", "s2cTopic")),
		StatusTopic: optional(value(values, "status_topic", "statusTopic")),
		HashTopics:  boolValue(value(values, "hash_topics", "hashTopics"), false),
		QOS:         qosValue(value(values, "qos"), 1),
		TLS:         boolValue(value(values, "tls"), strings.HasPrefix(endpoint, "mqtts://")),
	}
}

func (m MqttBrokerCredentials) Map(includeSecrets bool) map[string]any {
	data := map[string]any{
		"endpoint": m.Endpoint,
		"tls":      m.TLS,
	}
	if includeSecrets {
		data["username"] = m.Username
		data["password"] = m.Password
		if m.TopicPrefix != "" {
			data["topic_prefix"] = m.TopicPrefix
		}
		if m.HubID != "" {
			data["hub_id"] = m.HubID
		}
		if m.C2STopic != "" {
			data["c2s_topic"] = m.C2STopic
		}
		if m.S2CTopic != "" {
			data["s2c_topic"] = m.S2CTopic
		}
		if m.StatusTopic != "" {
			data["status_topic"] = m.StatusTopic
		}
		if m.HashTopics {
			data["hash_topics"] = true
		}
		if m.QOS != 1 {
			data["qos"] = m.QOS
		}
	}
	return data
}

func IdentityFromFile(path string) (Identity, error) {
	if err := assertSecureIdentityFile(path); err != nil {
		return Identity{}, err
	}
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

func DefaultConfigPath() (string, error) {
	if configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); configHome != "" {
		return filepath.Join(configHome, "thalovant", DefaultConfigFilename), nil
	}
	if runtime.GOOS == "windows" {
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			return filepath.Join(appData, "Thalovant", DefaultConfigFilename), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: unable to resolve home directory", ErrIdentity)
	}
	return filepath.Join(home, ".config", "thalovant", DefaultConfigFilename), nil
}

func IdentityFromConfig(path string, profile string) (Identity, error) {
	if strings.TrimSpace(path) == "" {
		defaultPath, err := DefaultConfigPath()
		if err != nil {
			return Identity{}, err
		}
		path = defaultPath
	}
	if err := assertSecureConfigFile(path); err != nil {
		return Identity{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: unable to read Thalovant config file %s", ErrIdentity, path)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return Identity{}, fmt.Errorf("%w: Thalovant config file is not valid YAML", ErrIdentity)
	}
	selected, err := identityConfigMap(values, profile)
	if err != nil {
		return Identity{}, err
	}
	return IdentityFromMap(selected)
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
		"mqtt": map[string]any{
			"endpoint":     firstNonEmpty(os.Getenv(prefix+"MQTT_ENDPOINT"), os.Getenv(prefix+"HUB_MQTT_HOST")),
			"username":     os.Getenv(prefix + "MQTT_USERNAME"),
			"password":     os.Getenv(prefix + "MQTT_PASSWORD"),
			"topic_prefix": os.Getenv(prefix + "MQTT_TOPIC_PREFIX"),
			"hub_id":       os.Getenv(prefix + "MQTT_HUB_ID"),
			"c2s_topic":    os.Getenv(prefix + "MQTT_C2S_TOPIC"),
			"s2c_topic":    os.Getenv(prefix + "MQTT_S2C_TOPIC"),
			"status_topic": os.Getenv(prefix + "MQTT_STATUS_TOPIC"),
			"hash_topics":  os.Getenv(prefix + "MQTT_HASH_TOPICS"),
			"qos":          os.Getenv(prefix + "MQTT_QOS"),
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
		Metadata:           cloneMap(mapFromAny(values["metadata"])),
		DataPlaneEndpoints: DataPlaneEndpointsFromMap(values),
		Protocols:          ProtocolSettingsFromMap(values),
		MQTT:               MqttBrokerCredentialsFromMap(values["mqtt"]),
	}
	if identity.AccessKey == "" || identity.Password == "" || identity.SiteID == "" || identity.DefaultMaster == "" {
		return Identity{}, ErrIdentity
	}
	return identity, nil
}

func identityConfigMap(values map[string]any, profile string) (map[string]any, error) {
	if values == nil {
		return nil, fmt.Errorf("%w: Thalovant config file must contain a YAML object", ErrIdentity)
	}
	if profiles := mapFromAny(values["profiles"]); profiles != nil {
		profileName := firstNonEmpty(profile, optional(value(values, "profile", "default_profile", "defaultProfile")), "default")
		selected := mapFromAny(profiles[profileName])
		if selected == nil {
			return nil, fmt.Errorf("%w: missing Thalovant config profile %s", ErrIdentity, profileName)
		}
		return profileIdentityMap(selected), nil
	}
	return profileIdentityMap(values), nil
}

func profileIdentityMap(values map[string]any) map[string]any {
	if identity := mapFromAny(values["identity"]); identity != nil {
		return identity
	}
	return values
}

func assertSecureConfigFile(path string) error {
	return assertSecureSecretFile(path, "Thalovant config file")
}

func assertSecureIdentityFile(path string) error {
	return assertSecureSecretFile(path, "identity file")
}

func assertSecureSecretFile(path string, description string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: unable to read %s %s", ErrIdentity, description, path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s is too permissive: %s. Run `chmod 600 %s`", ErrIdentity, strings.ToUpper(description[:1])+description[1:], path, path)
	}
	return nil
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
	if len(i.Metadata) > 0 {
		summary["metadata"] = cloneMap(i.Metadata)
	}
	if i.MQTT != nil {
		summary["mqtt"] = i.MQTT.Map(false)
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

func boolValue(raw any, fallback bool) bool {
	switch value := raw.(type) {
	case bool:
		return value
	case int:
		return value != 0
	case int64:
		return value != 0
	case float64:
		return value != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}

func qosValue(raw any, fallback byte) byte {
	switch value := raw.(type) {
	case int:
		if value == 0 || value == 1 {
			return byte(value)
		}
	case int64:
		if value == 0 || value == 1 {
			return byte(value)
		}
	case float64:
		if value == 0 || value == 1 {
			return byte(value)
		}
	case string:
		switch strings.TrimSpace(value) {
		case "0":
			return 0
		case "1":
			return 1
		}
	}
	return fallback
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
