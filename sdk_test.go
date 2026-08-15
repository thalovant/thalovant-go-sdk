package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

type hangingTransport struct {
	events       chan Event
	disconnected int
}

func (t *hangingTransport) Connect(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (t *hangingTransport) Disconnect(context.Context) error {
	t.disconnected++
	return nil
}

func (t *hangingTransport) Healthcheck() TransportHealth {
	return TransportHealth{}
}

func (t *hangingTransport) ConnectionInfo() TransportConnectionInfo {
	return TransportConnectionInfo{}
}

func (t *hangingTransport) EmitBus(context.Context, string, Data, Context) error {
	return nil
}

func (t *hangingTransport) Events() <-chan Event {
	return t.events
}

type queryTransport struct {
	events    chan Event
	hive      chan HiveMessage
	sent      []HiveMessage
	connected bool
}

func newQueryTransport() *queryTransport {
	return &queryTransport{
		events: make(chan Event, 4),
		hive:   make(chan HiveMessage, 4),
	}
}

func (t *queryTransport) Connect(context.Context) error {
	t.connected = true
	return nil
}

func (t *queryTransport) Disconnect(context.Context) error {
	t.connected = false
	return nil
}

func (t *queryTransport) Healthcheck() TransportHealth {
	return TransportHealth{Connected: t.connected, HandshakeComplete: t.connected, TransportAlive: t.connected}
}

func (t *queryTransport) ConnectionInfo() TransportConnectionInfo {
	return TransportConnectionInfo{Phase: ConnectionReady}
}

func (t *queryTransport) EmitBus(context.Context, string, Data, Context) error {
	return nil
}

func (t *queryTransport) Events() <-chan Event {
	return t.events
}

func (t *queryTransport) SendHiveMessage(_ context.Context, message HiveMessage, _ bool) error {
	t.sent = append(t.sent, message)
	queryID, _ := message.Metadata["query_id"].(string)
	context := mapValue(mapValue(message.Payload["payload"])["context"])
	t.hive <- HiveMessage{
		MsgType:  "query",
		Metadata: map[string]any{"query_id": queryID},
		Payload: map[string]any{
			"msg_type": "bus",
			"payload": map[string]any{
				"type":    EventSpeak,
				"data":    map[string]any{"utterance": "direct answer"},
				"context": context,
			},
		},
	}
	t.hive <- HiveMessage{
		MsgType:  "query",
		Metadata: map[string]any{"query_id": queryID},
		Payload:  map[string]any{"type": "hive.query.complete", "data": map[string]any{}, "context": context},
	}
	return nil
}

func (t *queryTransport) HiveMessages() <-chan HiveMessage {
	return t.hive
}

func TestIdentityFromMapNormalizesAliases(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "https://hub.example.com/",
		"port":     "443",
		"path":     "/hivemind/public",
		"metadata": map[string]any{"thalovant_owner_id": "owner-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.AccessKey != "access" || identity.DefaultMaster != "https://hub.example.com" || identity.DefaultPort != 443 || identity.DefaultPath != "/hivemind/public" {
		t.Fatalf("unexpected identity: %+v", identity)
	}
	if identity.EndpointBase() != "https://hub.example.com:443/hivemind/public" {
		t.Fatalf("unexpected endpoint %s", identity.EndpointBase())
	}
	if identity.Metadata["thalovant_owner_id"] != "owner-1" {
		t.Fatalf("unexpected metadata: %+v", identity.Metadata)
	}
}

func TestIdentityUsesProtocolAwareDataPlaneEndpoints(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "wss://hub.example.com",
		"port":     443,
		"path":     "/hivemind/public",
		"data_plane_endpoints": map[string]any{
			"https": "https://api.example.com/hivemind/public",
			"wss":   "wss://socket.example.com/hivemind/public",
			"mqtt":  "mqtts://mqtt.example.com:8883",
		},
		"protocols": map[string]any{
			"wss":  map[string]any{"enabled": true},
			"http": map[string]any{"enabled": true},
			"mqtt": map[string]any{"enabled": true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if identity.EndpointBase() != "https://api.example.com:443/hivemind/public" {
		t.Fatalf("unexpected endpoint %s", identity.EndpointBase())
	}
	if identity.EndpointFor(ProtocolWSS) != "wss://socket.example.com/hivemind/public" {
		t.Fatalf("unexpected wss endpoint %s", identity.EndpointFor(ProtocolWSS))
	}
	if identity.EndpointFor(ProtocolMQTT) != "mqtts://mqtt.example.com:8883" {
		t.Fatalf("unexpected mqtt endpoint %s", identity.EndpointFor(ProtocolMQTT))
	}
	if !identity.SupportsProtocol(ProtocolHTTPS) {
		t.Fatal("expected https protocol support")
	}
	if got := identity.EnabledProtocols(); len(got) != 3 || got[0] != ProtocolWSS || got[1] != ProtocolHTTPS || got[2] != ProtocolMQTT {
		t.Fatalf("unexpected protocols: %+v", got)
	}
}

func TestIdentityLoadsMQTTCredentialsAndRedactsByDefault(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "wss://hub.example.com",
		"mqtt": map[string]any{
			"endpoint":     "mqtts://mqtt.example.com:8883",
			"username":     "access",
			"password":     "broker-password",
			"topic_prefix": "hivemind/hub/access",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.MQTT == nil {
		t.Fatal("expected mqtt credentials")
	}
	if identity.MQTT.Endpoint != "mqtts://mqtt.example.com:8883" || identity.MQTT.Username != "access" {
		t.Fatalf("unexpected mqtt credentials: %+v", identity.MQTT)
	}
	redacted := identity.Summary()["mqtt"].(map[string]any)
	if _, ok := redacted["password"]; ok {
		t.Fatal("mqtt password should be redacted by default")
	}
	if redacted["endpoint"] != "mqtts://mqtt.example.com:8883" || redacted["tls"] != true {
		t.Fatalf("unexpected redacted mqtt summary: %+v", redacted)
	}
	full := identity.MQTT.Map(true)
	if full["password"] != "broker-password" || full["topic_prefix"] != "hivemind/hub/access" {
		t.Fatalf("unexpected full mqtt map: %+v", full)
	}
}

func TestIdentityFromConfigLoadsYAMLProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte(`
version: 1
profile: prod
profiles:
  prod:
    identity:
      access_key: access
      password: secret
      site_id: site
      default_master: https://hub.example.com
      default_port: 443
      mqtt:
        endpoint: mqtts://mqtt.example.com:8883
        username: access
        password: broker-password
        topic_prefix: hivemind/hub/access
`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	identity, err := IdentityFromConfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if identity.AccessKey != "access" || identity.MQTT == nil || identity.MQTT.Password != "broker-password" {
		t.Fatalf("unexpected identity from config: %+v", identity)
	}
}

func TestIdentityFromFileLoadsPrivateJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_identity.json")
	raw, err := json.Marshal(map[string]any{
		"access_key":     "access",
		"password":       "secret",
		"site_id":        "site",
		"default_master": "https://hub.example.com",
		"default_port":   443,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	identity, err := IdentityFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if identity.AccessKey != "access" || identity.DefaultMaster != "https://hub.example.com" {
		t.Fatalf("unexpected identity from file: %+v", identity)
	}
}

func TestIdentityFromFileRejectsPermissiveFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ACLs are not represented by POSIX mode bits")
	}
	path := filepath.Join(t.TempDir(), "_identity.json")
	if err := os.WriteFile(path, []byte(`{"access_key":"access","password":"secret","site_id":"site","default_master":"https://hub.example.com"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := IdentityFromFile(path); err == nil || !strings.Contains(err.Error(), "too permissive") {
		t.Fatalf("expected permissive file error, got %v", err)
	}
}

func TestIdentityFromConfigRejectsPermissiveFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ACLs are not represented by POSIX mode bits")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("identity: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := IdentityFromConfig(path, ""); err == nil || !strings.Contains(err.Error(), "too permissive") {
		t.Fatalf("expected permissive file error, got %v", err)
	}
}

func TestDataPlaneEndpointsFromHubResource(t *testing.T) {
	endpoints := DataPlaneEndpointsFromHub(map[string]any{
		"domain": "jokes.thalovant.io",
		"spec": map[string]any{
			"protocols": map[string]any{
				"wss":  map[string]any{"enabled": true},
				"http": map[string]any{"enabled": true},
				"mqtt": map[string]any{"enabled": false},
			},
		},
	})

	if endpoints.WSS != "wss://jokes.thalovant.io" {
		t.Fatalf("unexpected wss endpoint %s", endpoints.WSS)
	}
	if endpoints.HTTPS != "https://jokes.thalovant.io" {
		t.Fatalf("unexpected https endpoint %s", endpoints.HTTPS)
	}
	if endpoints.MQTT != "" {
		t.Fatalf("unexpected mqtt endpoint %s", endpoints.MQTT)
	}
}

func TestSelectDataPlaneEndpoint(t *testing.T) {
	selected := SelectDataPlaneEndpoint(
		HubDataPlaneEndpoints{HTTPS: "https://hub.example.com/public", WSS: "wss://hub.example.com/public"},
		HubProtocolSettings{WSS: true, HTTP: true},
		[]HubProtocol{ProtocolMQTT, ProtocolWSS, ProtocolHTTPS},
	)
	if selected == nil || selected.Protocol != ProtocolWSS || selected.Endpoint != "wss://hub.example.com/public" {
		t.Fatalf("unexpected selected endpoint: %+v", selected)
	}
}

func TestNewClientWithOptionsRequiresMQTTCredentials(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "https://hub.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewClientWithOptions(identity, ClientOptions{Protocol: ProtocolMQTT}); err == nil || !strings.Contains(err.Error(), "MQTT") {
		t.Fatalf("expected MQTT credential error, got %v", err)
	}
}

func TestNewClientWithOptionsSelectsWSSAndMQTT(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":        "access",
		"password":   "secret",
		"crypto_key": "0123456789abcdef",
		"site":       "site",
		"host":       "https://hub.example.com",
		"data_plane_endpoints": map[string]any{
			"https": "https://hub.example.com",
			"wss":   "wss://hub.example.com",
			"mqtt":  "mqtts://mqtt.example.com:8883",
		},
		"mqtt": map[string]any{
			"endpoint":     "mqtts://mqtt.example.com:8883",
			"username":     "access",
			"password":     "broker-password",
			"topic_prefix": "hivemind/hub/access",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	autoClient, err := NewClientWithOptions(identity, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := autoClient.Transport.(*WSSTransport); !ok {
		t.Fatalf("expected default WSS transport, got %T", autoClient.Transport)
	}
	if client, err := NewClientWithOptions(identity, ClientOptions{Protocol: ProtocolWSS}); err != nil || client.Transport == nil {
		t.Fatalf("expected WSS client, got client=%v err=%v", client, err)
	}
	if client, err := NewClientWithOptions(identity, ClientOptions{Protocol: ProtocolMQTT}); err != nil || client.Transport == nil {
		t.Fatalf("expected MQTT client, got client=%v err=%v", client, err)
	}
	topics, err := MQTTTopicsForIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if topics.Inbound != "hivemind/hub/access/in" || topics.Outbound != "hivemind/hub/access/out" || topics.Status != "hivemind/hub/access/status" {
		t.Fatalf("unexpected topics: %+v", topics)
	}
}

func TestNewClientWithOptionsFallsBackToHTTPSWhenWSSIsMissing(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "https://hub.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClientWithOptions(identity, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.Transport.(*HTTPTransport); !ok {
		t.Fatalf("expected fallback HTTP transport, got %T", client.Transport)
	}
}

func TestClientConnectWithInfoReturnsConnectionSnapshot(t *testing.T) {
	var sawHello bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/connect":
			_, _ = w.Write([]byte(`{}`))
		case "/get_messages":
			_, _ = w.Write([]byte(`{"messages":[{"msg_type":"handshake","payload":{"preshared_key":true},"metadata":{},"route":[]}]}`))
		case "/send_message":
			sawHello = true
			_, _ = w.Write([]byte(`{}`))
		case "/disconnect":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	identity, err := IdentityFromMap(map[string]any{
		"key":        "access",
		"password":   "secret",
		"crypto_key": "0123456789abcdef",
		"site":       "site",
		"host":       server.URL,
		"data_plane_endpoints": map[string]any{
			"https": server.URL,
		},
		"protocols": map[string]any{
			"http": map[string]any{"enabled": true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClientWithOptions(identity, ClientOptions{Protocol: ProtocolHTTPS})
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.ConnectWithInfo(context.Background())
	defer client.Close(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !sawHello {
		t.Fatal("expected SDK to answer handshake with hello")
	}
	if info.Phase != ConnectionReady || info.ConnectMS < 0 || info.HandshakeMS < 0 {
		t.Fatalf("unexpected connection info: %+v", info)
	}
	if health := client.Healthcheck(); health.Connection.Phase != ConnectionReady {
		t.Fatalf("unexpected health connection: %+v", health.Connection)
	}
}

func TestClientConnectEnforcesDefaultTimeout(t *testing.T) {
	transport := &hangingTransport{events: make(chan Event)}
	client := &Client{Identity: Identity{SiteID: "site"}, Transport: transport, ConnectTimeout: 10 * time.Millisecond}

	err := client.Connect(context.Background())

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if transport.disconnected != 1 {
		t.Fatalf("expected disconnect after timeout, got %d", transport.disconnected)
	}
}

func TestClientQueryUsesDirectHiveMindQueryFrame(t *testing.T) {
	transport := newQueryTransport()
	client := &Client{Identity: Identity{SiteID: "site"}, Transport: transport}

	reply, err := client.Query(context.Background(), "what is up?", QueryOptions{SessionID: "query-session"})
	if err != nil {
		t.Fatal(err)
	}

	if reply.Text != "direct answer" || reply.SessionID != "query-session" || reply.RequestID == "" {
		t.Fatalf("unexpected reply: %+v", reply)
	}
	if len(transport.sent) != 1 {
		t.Fatalf("expected one query frame, got %d", len(transport.sent))
	}
	frame := transport.sent[0]
	if frame.MsgType != "query" || frame.Metadata["query_id"] != reply.RequestID {
		t.Fatalf("unexpected query frame metadata: %+v", frame)
	}
	inner := frame.Payload
	if inner["msg_type"] != "bus" {
		t.Fatalf("unexpected inner frame: %+v", inner)
	}
	payload := mapValue(inner["payload"])
	if payload["type"] != EventRecognizerLoopUtterance {
		t.Fatalf("unexpected inner payload: %+v", payload)
	}
	context := mapValue(payload["context"])
	if SessionIDFromContext(context) != "query-session" || RequestIDFromContext(context) != reply.RequestID {
		t.Fatalf("missing correlation context: %+v", context)
	}
}

func TestMQTTTopicsUseTopicPrefixVerbatimAndStripSlashes(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "https://hub.example.com",
		"mqtt": map[string]any{
			"endpoint":     "mqtts://mqtt.example.com:8883",
			"username":     "access",
			"password":     "broker-password",
			"topic_prefix": "/hivemind/hub-1/access/",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	topics, err := MQTTTopicsForIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if topics.Inbound != "hivemind/hub-1/access/in" || topics.Outbound != "hivemind/hub-1/access/out" || topics.Status != "hivemind/hub-1/access/status" {
		t.Fatalf("unexpected topics: %+v", topics)
	}
}

func TestMQTTTopicsRequireTopicPrefix(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "https://hub.example.com",
		"mqtt": map[string]any{
			"endpoint": "mqtts://mqtt.example.com:8883",
			"username": "access",
			"password": "broker-password",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MQTTTopicsForIdentity(identity); err == nil || !strings.Contains(err.Error(), "topic_prefix") {
		t.Fatalf("expected topic_prefix error, got %v", err)
	}
}

// mqttIdentityWithTopicPrefix builds an Identity carrying the given TopicPrefix
// verbatim (bypassing the map loader's own TrimSpace) so MQTTTopicsForIdentity's
// validation is exercised on exactly the supplied bytes.
func mqttIdentityWithTopicPrefix(prefix string) Identity {
	return Identity{
		AccessKey:     "access",
		Password:      "secret",
		SiteID:        "site",
		DefaultMaster: "https://hub.example.com",
		DefaultPort:   443,
		MQTT: &MqttBrokerCredentials{
			Endpoint:    "mqtts://mqtt.example.com:8883",
			Username:    "access",
			Password:    "broker-password",
			TopicPrefix: prefix,
		},
	}
}

func TestMQTTTopicsRejectWhitespaceOnlyTopicPrefix(t *testing.T) {
	identity := mqttIdentityWithTopicPrefix("  \t \n ")
	if _, err := MQTTTopicsForIdentity(identity); err == nil || !strings.Contains(err.Error(), "topic_prefix") {
		t.Fatalf("expected topic_prefix error for whitespace-only prefix, got %v", err)
	}
}

func TestMQTTTopicsRejectInvalidTopicPrefixCharacters(t *testing.T) {
	cases := map[string]string{
		"multi-level wildcard":  "hivemind/hub-1/access/#",
		"single-level wildcard": "hivemind/+/access",
		"control char":          "hivemind/hub-1/acc\x01ess",
		"nul byte":              "hivemind/hub-1/acc\x00ess",
	}
	for name, prefix := range cases {
		t.Run(name, func(t *testing.T) {
			identity := mqttIdentityWithTopicPrefix(prefix)
			if _, err := MQTTTopicsForIdentity(identity); err == nil || !strings.Contains(err.Error(), "not valid in an MQTT topic") {
				t.Fatalf("expected invalid-character error for %q, got %v", prefix, err)
			}
		})
	}
}

func TestPahoBrokerURLHonorsTLSFlag(t *testing.T) {
	secure, err := pahoBrokerURL("mqtt://mqtt.example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	if secure != "ssl://mqtt.example.com" {
		t.Fatalf("unexpected secure broker URL %s", secure)
	}
	plain, err := pahoBrokerURL("mqtt://mqtt.example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "tcp://mqtt.example.com" {
		t.Fatalf("unexpected plain broker URL %s", plain)
	}
}

func TestControlPlaneBootstrapKeepsGeneratedSecretsLocal(t *testing.T) {
	var sawAuthorization bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/token":
			_, _ = w.Write([]byte(`{"access_token":"token","expires_in":3600}`))
		case "/v1/hubs/hub-1":
			if r.Header.Get("authorization") == "Bearer token" {
				sawAuthorization = true
			}
			_, _ = w.Write([]byte(`{"id":"hub-1","name":"joke-garden","domain":"jokes.thalovant.io","spec":{"protocols":{"wss":{"enabled":true},"http":{"enabled":true},"mqtt":{"enabled":false}}}}`))
		case "/v1/clients":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			spec := mapValue(payload["spec"])
			if spec["apiKey"] == "" || spec["password"] == "" || spec["cryptoKey"] == "" {
				t.Fatalf("missing generated credentials in payload: %+v", spec)
			}
			_, _ = w.Write([]byte(`{"id":"client-1","name":"kiosk","hub_id":"hub-1","spec":{"version":"1","apiKeyRef":{"name":"secret","key":"apiKey"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "")
	if _, err := control.Login(context.Background(), "ada@example.com", "secret", ""); err != nil {
		t.Fatal(err)
	}
	result, err := control.CreateClientIdentityForHubID(context.Background(), "hub-1", BootstrapIdentityOptions{Name: "kiosk"})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAuthorization {
		t.Fatal("expected authenticated hub request")
	}
	if result.Identity.AccessKey == "" || result.Identity.Password == "" || result.Identity.CryptoKey == "" {
		t.Fatalf("expected local identity secrets: %+v", result.Identity)
	}
	if result.Identity.EndpointFor(ProtocolHTTPS) != "https://jokes.thalovant.io:443" {
		t.Fatalf("unexpected endpoint %s", result.Identity.EndpointFor(ProtocolHTTPS))
	}
	if result.SelectedProtocol() != ProtocolWSS {
		t.Fatalf("unexpected selected protocol %s", result.SelectedProtocol())
	}
	runtime, err := control.RequireRuntimeProtocol(result, "")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Protocol != ProtocolWSS || runtime.Endpoint != "wss://jokes.thalovant.io" {
		t.Fatalf("unexpected default runtime endpoint: %+v", runtime)
	}
	if _, ok := result.Summary(false)["identity"].(map[string]any)["access_key"]; ok {
		t.Fatal("summary should redact identity secrets by default")
	}
	if _, ok := result.Summary(true)["identity"].(map[string]any)["access_key"]; !ok {
		t.Fatal("summary should include secrets when requested")
	}
}

func TestControlPlaneUserAgentMatchesModuleRelease(t *testing.T) {
	// Assert against the derived value, never a version literal: a literal here
	// would just be one more copy to hand-maintain, and it would keep passing
	// while every copy drifted together.
	expected := "ThalovantGoSDK/" + Version
	if DefaultControlUserAgent != expected {
		t.Fatalf("control-plane user agent %q does not derive from Version %q (want %q)", DefaultControlUserAgent, Version, expected)
	}
	if DefaultUserAgent != expected {
		t.Fatalf("data-plane user agent %q does not derive from Version %q (want %q)", DefaultUserAgent, Version, expected)
	}
	var sawUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUserAgent = r.Header.Get("user-agent")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"meta":{"count":0,"next":null},"links":{"next":null}}`))
	}))
	defer server.Close()

	if _, err := NewControlPlane(server.URL, "").ListPublicHubs(context.Background(), 1, ""); err != nil {
		t.Fatal(err)
	}
	if sawUserAgent != expected {
		t.Fatalf("unexpected user-agent header %q, want %q", sawUserAgent, expected)
	}
}

func TestVersionMatchesVersionFile(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	declared := strings.TrimSpace(string(raw))
	if declared != Version {
		t.Fatalf("VERSION file declares %q but thalovant.Version is %q; the release bump must move both", declared, Version)
	}
}

func TestNoSourceFileHardCodesAUserAgentVersion(t *testing.T) {
	// The pattern is built from userAgentProduct rather than spelled out, so
	// this file does not contain the literal it forbids and therefore needs no
	// exemption from its own scan -- every .go file in the package, tests
	// included, is checked.
	hardCoded := regexp.MustCompile(regexp.QuoteMeta(userAgentProduct) + `/\d`)

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("found no .go sources to scan for pinned user-agent literals")
	}

	var offenders []string
	for _, source := range sources {
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if hardCoded.Match(content) {
			offenders = append(offenders, source)
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("user agents must derive from thalovant.Version, but a pinned %s/<version> literal was found in: %s", userAgentProduct, strings.Join(offenders, ", "))
	}
}

func TestControlPlaneLoginSendsMFAFieldsOnlyWhenGiven(t *testing.T) {
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/token" {
			http.NotFound(w, r)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","token_type":"bearer","expires_in":3600}`))
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "")
	if _, err := control.Login(context.Background(), "ada@example.com", "secret", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := control.LoginWithOptions(context.Background(), "ada@example.com", "secret", LoginOptions{OTPCode: "123456"}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.LoginWithOptions(context.Background(), "ada@example.com", "secret", LoginOptions{Scope: "admin", RecoveryCode: "rescue-code"}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.LoginWithOptions(context.Background(), "ada@example.com", "secret", LoginOptions{OTPCode: "  ", RecoveryCode: ""}); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 4 {
		t.Fatalf("expected 4 login requests, saw %d", len(payloads))
	}
	for _, index := range []int{0, 3} {
		payload := payloads[index]
		if _, ok := payload["otp_code"]; ok {
			t.Fatalf("payload %d should omit otp_code: %+v", index, payload)
		}
		if _, ok := payload["recovery_code"]; ok {
			t.Fatalf("payload %d should omit recovery_code: %+v", index, payload)
		}
	}
	if payloads[1]["otp_code"] != "123456" {
		t.Fatalf("expected otp_code in payload: %+v", payloads[1])
	}
	if _, ok := payloads[1]["recovery_code"]; ok {
		t.Fatalf("payload should omit recovery_code: %+v", payloads[1])
	}
	if payloads[2]["recovery_code"] != "rescue-code" || payloads[2]["scope"] != "admin" {
		t.Fatalf("expected recovery_code and scope in payload: %+v", payloads[2])
	}
	if _, ok := payloads[2]["otp_code"]; ok {
		t.Fatalf("payload should omit otp_code: %+v", payloads[2])
	}
	if control.AccessToken != "token" {
		t.Fatalf("expected stored access token, got %q", control.AccessToken)
	}
}

const testDeviceGrant = `{"device_code":"device-code-1","user_code":"WDJB-MJHT","verification_uri":"https://dash.thalovant.com/activate","verification_uri_complete":"https://dash.thalovant.com/activate?user_code=WDJB-MJHT","expires_in":900,"interval":0}`

const testDeviceToken = `{"access_token":"device-token","token_type":"bearer","scopes":["hubs:read","clients:write"],"expires_at":"2027-08-13T00:00:00Z","token_id":"token-1"}`

type scriptedReply struct {
	status int
	body   string
}

type deviceFlowCalls struct {
	authorize []map[string]any
	token     []map[string]any
}

// newDeviceFlowServer scripts the two device-flow endpoints. Token replies
// are consumed in order and the last one repeats for further polls. Both
// endpoints reject authenticated requests, matching the public API contract.
func newDeviceFlowServer(t *testing.T, grantJSON string, replies []scriptedReply, calls *deviceFlowCalls) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s %s", r.Method, r.URL.Path)
		}
		if authorization := r.Header.Get("authorization"); authorization != "" {
			t.Errorf("device endpoints must not receive authorization, got %q", authorization)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode %s body: %v", r.URL.Path, err)
		}
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			calls.authorize = append(calls.authorize, body)
			_, _ = w.Write([]byte(grantJSON))
		case "/v1/auth/device/token":
			calls.token = append(calls.token, body)
			reply := replies[0]
			if len(replies) > 1 {
				replies = replies[1:]
			}
			w.WriteHeader(reply.status)
			_, _ = w.Write([]byte(reply.body))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestControlPlaneLoginWithBrowserPollsUntilToken(t *testing.T) {
	calls := &deviceFlowCalls{}
	server := newDeviceFlowServer(t, testDeviceGrant, []scriptedReply{
		{http.StatusBadRequest, `{"error":"authorization_pending"}`},
		{http.StatusBadRequest, `{"error":"authorization_pending"}`},
		{http.StatusOK, testDeviceToken},
	}, calls)
	defer server.Close()

	var opened []string
	restoreBrowser := openBrowser
	openBrowser = func(target string) error {
		opened = append(opened, target)
		return nil
	}
	defer func() { openBrowser = restoreBrowser }()

	var prompts []map[string]any
	control := NewControlPlane(server.URL, "")
	token, err := control.LoginWithBrowser(context.Background(), DeviceLoginOptions{
		Scopes:     []string{"hubs:read"},
		ClientName: "gotest",
		Prompt:     func(grant map[string]any) { prompts = append(prompts, grant) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if token["access_token"] != "device-token" || token["token_id"] != "token-1" {
		t.Fatalf("unexpected token payload: %+v", token)
	}
	if control.AccessToken != "device-token" {
		t.Fatalf("expected the device token to be stored, got %q", control.AccessToken)
	}
	if len(prompts) != 1 || prompts[0]["verification_uri"] != "https://dash.thalovant.com/activate" || prompts[0]["user_code"] != "WDJB-MJHT" {
		t.Fatalf("unexpected prompt payloads: %+v", prompts)
	}
	if len(opened) != 1 || opened[0] != "https://dash.thalovant.com/activate?user_code=WDJB-MJHT" {
		t.Fatalf("unexpected browser opens: %v", opened)
	}
	if len(calls.authorize) != 1 {
		t.Fatalf("expected one authorize request, saw %d", len(calls.authorize))
	}
	scopes, _ := calls.authorize[0]["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != "hubs:read" || calls.authorize[0]["client_name"] != "gotest" {
		t.Fatalf("unexpected authorize payload: %+v", calls.authorize[0])
	}
	if len(calls.token) != 3 {
		t.Fatalf("expected three token polls, saw %d", len(calls.token))
	}
	for _, payload := range calls.token {
		if payload["device_code"] != "device-code-1" || len(payload) != 1 {
			t.Fatalf("unexpected token poll payload: %+v", payload)
		}
	}
}

func TestControlPlaneLoginWithBrowserSkipsBrowserWhenDisabled(t *testing.T) {
	calls := &deviceFlowCalls{}
	server := newDeviceFlowServer(t, testDeviceGrant, []scriptedReply{
		{http.StatusOK, testDeviceToken},
	}, calls)
	defer server.Close()

	restoreBrowser := openBrowser
	openBrowser = func(target string) error {
		t.Errorf("the browser must not open, got %q", target)
		return nil
	}
	defer func() { openBrowser = restoreBrowser }()

	openBrowserFlag := false
	control := NewControlPlane(server.URL, "")
	if _, err := control.LoginWithBrowser(context.Background(), DeviceLoginOptions{
		OpenBrowser: &openBrowserFlag,
		Prompt:      func(map[string]any) {},
	}); err != nil {
		t.Fatal(err)
	}
	if len(calls.authorize) != 1 || len(calls.authorize[0]) != 0 {
		t.Fatalf("expected an empty authorize payload, got %+v", calls.authorize)
	}
}

func TestControlPlaneDevicePollSlowDownGrowsInterval(t *testing.T) {
	calls := &deviceFlowCalls{}
	server := newDeviceFlowServer(t, testDeviceGrant, []scriptedReply{
		{http.StatusBadRequest, `{"error":"authorization_pending"}`},
		{http.StatusBadRequest, `{"error":"slow_down"}`},
		{http.StatusBadRequest, `{"error":"authorization_pending"}`},
		{http.StatusOK, testDeviceToken},
	}, calls)
	defer server.Close()

	var sleeps []time.Duration
	control := NewControlPlane(server.URL, "")
	token, err := control.pollDeviceToken(
		context.Background(),
		"device-code-1",
		5*time.Second,
		900*time.Second,
		func(_ context.Context, wait time.Duration) error {
			sleeps = append(sleeps, wait)
			return nil
		},
		func() time.Time { return time.Unix(0, 0) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if token["access_token"] != "device-token" {
		t.Fatalf("unexpected token payload: %+v", token)
	}
	expected := []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second}
	if len(sleeps) != len(expected) {
		t.Fatalf("unexpected sleeps: %v", sleeps)
	}
	for index, wait := range expected {
		if sleeps[index] != wait {
			t.Fatalf("unexpected sleeps: %v", sleeps)
		}
	}
}

func TestControlPlaneLoginWithBrowserReportsAccessDenied(t *testing.T) {
	calls := &deviceFlowCalls{}
	server := newDeviceFlowServer(t, testDeviceGrant, []scriptedReply{
		{http.StatusBadRequest, `{"error":"access_denied"}`},
	}, calls)
	defer server.Close()

	openBrowserFlag := false
	control := NewControlPlane(server.URL, "")
	_, err := control.LoginWithBrowser(context.Background(), DeviceLoginOptions{
		OpenBrowser: &openBrowserFlag,
		Prompt:      func(map[string]any) {},
	})
	if !errors.Is(err, ErrDeviceAccessDenied) {
		t.Fatalf("expected ErrDeviceAccessDenied, got %v", err)
	}
	if control.AccessToken != "" {
		t.Fatalf("no token should be stored after denial, got %q", control.AccessToken)
	}
}

func TestControlPlaneLoginWithBrowserReportsExpiredCode(t *testing.T) {
	calls := &deviceFlowCalls{}
	server := newDeviceFlowServer(t, testDeviceGrant, []scriptedReply{
		{http.StatusBadRequest, `{"error":"expired_token"}`},
	}, calls)
	defer server.Close()

	openBrowserFlag := false
	control := NewControlPlane(server.URL, "")
	_, err := control.LoginWithBrowser(context.Background(), DeviceLoginOptions{
		OpenBrowser: &openBrowserFlag,
		Prompt:      func(map[string]any) {},
	})
	if !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("expected ErrDeviceCodeExpired, got %v", err)
	}
	if control.AccessToken != "" {
		t.Fatalf("no token should be stored after expiry, got %q", control.AccessToken)
	}
}

func TestControlPlaneDevicePollTimesOut(t *testing.T) {
	calls := &deviceFlowCalls{}
	server := newDeviceFlowServer(t, testDeviceGrant, []scriptedReply{
		{http.StatusBadRequest, `{"error":"authorization_pending"}`},
	}, calls)
	defer server.Close()

	now := time.Unix(0, 0)
	control := NewControlPlane(server.URL, "")
	_, err := control.pollDeviceToken(
		context.Background(),
		"device-code-1",
		5*time.Second,
		10*time.Second,
		func(_ context.Context, wait time.Duration) error {
			now = now.Add(wait)
			return nil
		},
		func() time.Time { return now },
	)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	if len(calls.token) != 3 {
		t.Fatalf("expected three token polls before timing out, saw %d", len(calls.token))
	}
	if !now.Equal(time.Unix(10, 0)) {
		t.Fatalf("expected the fake clock to stop at the deadline, got %v", now)
	}
}

func TestControlPlaneLoginWithBrowserHonorsContextCancel(t *testing.T) {
	calls := &deviceFlowCalls{}
	slowGrant := strings.Replace(testDeviceGrant, `"interval":0`, `"interval":600`, 1)
	server := newDeviceFlowServer(t, slowGrant, []scriptedReply{
		{http.StatusBadRequest, `{"error":"authorization_pending"}`},
	}, calls)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()

	openBrowserFlag := false
	control := NewControlPlane(server.URL, "")
	_, err := control.LoginWithBrowser(ctx, DeviceLoginOptions{
		OpenBrowser: &openBrowserFlag,
		Prompt:      func(map[string]any) {},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if control.AccessToken != "" {
		t.Fatalf("no token should be stored after cancellation, got %q", control.AccessToken)
	}
}

func TestControlPlaneDefaultAPIURLAndV1Normalization(t *testing.T) {
	if got := NewDefaultControlPlane("").APIURL; got != "https://api.thalovant.com/" {
		t.Fatalf("unexpected default API URL %q", got)
	}
	if got := NewControlPlane("", "").APIURL; got != "https://api.thalovant.com/" {
		t.Fatalf("unexpected empty API URL %q", got)
	}
	if got := NewControlPlane("https://api.thalovant.com/v1", "").APIURL; got != "https://api.thalovant.com/" {
		t.Fatalf("unexpected normalized API URL %q", got)
	}
	if got := NewControlPlane("https://dash.example.com/api/v1", "").APIURL; got != "https://dash.example.com/api/" {
		t.Fatalf("unexpected dashboard-compatible API URL %q", got)
	}
}

func TestControlPlaneListsPublicHubsWithoutAuth(t *testing.T) {
	var sawPublicList bool
	var sawPublicDetail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.Header.Get("authorization") != "" {
			t.Fatalf("public routes should not send authorization header")
		}
		switch r.URL.Path {
		case "/v1/public/hubs":
			sawPublicList = true
			if r.URL.Query().Get("limit") != "12" {
				t.Fatalf("unexpected limit query %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"hub-public","name":"joke-garden","slug":"joke-garden","title":"Joke Garden"}],"meta":{"count":1,"next":null},"links":{"next":null}}`))
		case "/v1/public/hubs/joke-garden":
			sawPublicDetail = true
			_, _ = w.Write([]byte(`{"id":"hub-public","name":"joke-garden","slug":"joke-garden","title":"Joke Garden"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "token")
	page, err := control.ListPublicHubs(context.Background(), 12, "")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := control.GetPublicHub(context.Background(), "joke-garden")
	if err != nil {
		t.Fatal(err)
	}
	items := page["data"].([]any)
	if mapValue(items[0])["slug"] != "joke-garden" || hub["title"] != "Joke Garden" {
		t.Fatalf("unexpected public hub payloads page=%+v hub=%+v", page, hub)
	}
	if !sawPublicList || !sawPublicDetail {
		t.Fatalf("expected both public routes list=%v detail=%v", sawPublicList, sawPublicDetail)
	}
}

func TestControlPlaneGetsTypedOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operations/operation-1" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("authorization") != "Bearer token" {
			t.Fatalf("unexpected authorization header %q", r.Header.Get("authorization"))
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"operation-1","kind":"gitops.commit","aggregate_type":"gitops","aggregate_id":null,"status":"committed","details":{"git_commit_created":true},"git_commit_sha":"abc123","error_code":null,"error_message":null,"created_at":"2026-07-11T00:00:00Z","updated_at":"2026-07-11T00:00:01Z","committed_at":"2026-07-11T00:00:01Z","applied_at":null,"ready_at":null,"terminal_at":null,"links":{"self":"/v1/operations/operation-1"}}`))
	}))
	defer server.Close()

	operation, err := NewControlPlane(server.URL, "token").GetOperation(context.Background(), "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != OperationCommitted || operation.GitCommitSHA == nil || *operation.GitCommitSHA != "abc123" {
		t.Fatalf("unexpected operation: %+v", operation)
	}
	if operation.Details["git_commit_created"] != true {
		t.Fatalf("unexpected operation details: %+v", operation.Details)
	}
}

func TestControlPlaneManagesMemoryItems(t *testing.T) {
	var sawDelete bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.Header.Get("authorization") != "Bearer token" {
			t.Fatalf("unexpected authorization header %q", r.Header.Get("authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memory":
			query := r.URL.Query()
			expected := map[string]string{
				"scope":           "workspace",
				"kind":            "preference",
				"owner_id":        "owner-1",
				"hub_id":          "hub-1",
				"q":               "timezone",
				"include_deleted": "true",
				"include_expired": "true",
				"limit":           "25",
				"offset":          "50",
			}
			for key, val := range expected {
				if got := query.Get(key); got != val {
					t.Fatalf("unexpected %s query %q in %q", key, got, r.URL.RawQuery)
				}
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"memory-1","content":"UTC"}],"meta":{"count":1,"next":null},"links":{"next":null}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memory/summary":
			if r.URL.Query().Get("owner_id") != "owner-1" {
				t.Fatalf("unexpected summary query %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"total":1,"by_scope":{"workspace":1},"by_kind":{"preference":1},"expired":0,"deleted":0}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memory":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["scope"] != "workspace" || payload["content"] != "Use UTC." {
				t.Fatalf("unexpected create payload %+v", payload)
			}
			_, _ = w.Write([]byte(`{"id":"memory-1","scope":"workspace","kind":"preference","content":"Use UTC."}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memory/memory-1":
			_, _ = w.Write([]byte(`{"id":"memory-1","scope":"workspace","kind":"preference","content":"Use UTC."}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/memory/memory-1":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["content"] != "Use America/Toronto." {
				t.Fatalf("unexpected update payload %+v", payload)
			}
			_, _ = w.Write([]byte(`{"id":"memory-1","scope":"workspace","kind":"preference","content":"Use America/Toronto."}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/memory/memory-1":
			sawDelete = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "token")
	page, err := control.ListMemoryItems(context.Background(), MemoryListOptions{
		Scope:          "workspace",
		Kind:           "preference",
		OwnerID:        "owner-1",
		HubID:          "hub-1",
		Query:          "timezone",
		IncludeDeleted: true,
		IncludeExpired: true,
		Limit:          25,
		Offset:         50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page["data"].([]any)) != 1 {
		t.Fatalf("unexpected memory page %+v", page)
	}
	summary, err := control.GetMemorySummary(context.Background(), "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if summary["total"] != float64(1) {
		t.Fatalf("unexpected memory summary %+v", summary)
	}
	created, err := control.CreateMemoryItem(context.Background(), map[string]any{
		"scope":   "workspace",
		"kind":    "preference",
		"content": "Use UTC.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created["id"] != "memory-1" {
		t.Fatalf("unexpected created memory %+v", created)
	}
	got, err := control.GetMemoryItem(context.Background(), "memory-1")
	if err != nil {
		t.Fatal(err)
	}
	if got["content"] != "Use UTC." {
		t.Fatalf("unexpected memory item %+v", got)
	}
	updated, err := control.UpdateMemoryItem(context.Background(), "memory-1", map[string]any{
		"content": "Use America/Toronto.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated["content"] != "Use America/Toronto." {
		t.Fatalf("unexpected updated memory %+v", updated)
	}
	if err := control.DeleteMemoryItem(context.Background(), "memory-1"); err != nil {
		t.Fatal(err)
	}
	if !sawDelete {
		t.Fatal("expected delete request")
	}
}

func TestControlPlaneGetsAnalyticsOverview(t *testing.T) {
	hour := 0
	weekday := 6
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path != "/v1/analytics/overview" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer token" {
			t.Fatalf("unexpected authorization header %q", r.Header.Get("authorization"))
		}
		query := r.URL.Query()
		expected := map[string]string{
			"range":      "30d",
			"bucket":     "1d",
			"hub_id":     "hub-1",
			"client_id":  "client-1",
			"country":    "CA",
			"message":    "speak",
			"utterance":  "hello",
			"intent":     "DailyDeskIntent",
			"time_start": "2026-05-03T20:00:00Z",
			"time_end":   "2026-05-03T21:00:00Z",
			"weekday":    "6",
			"hour":       "0",
		}
		for key, val := range expected {
			if got := query.Get(key); got != val {
				t.Fatalf("unexpected %s query %q in %q", key, got, r.URL.RawQuery)
			}
		}
		_, _ = w.Write([]byte(`{"meta":{"scope":"admin"},"totals":{"utterances":7}}`))
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "token")
	overview, err := control.GetAnalyticsOverview(context.Background(), AnalyticsOverviewOptions{
		Range:     "30d",
		Bucket:    "1d",
		HubID:     "hub-1",
		ClientID:  "client-1",
		Country:   "CA",
		Message:   "speak",
		Utterance: "hello",
		Intent:    "DailyDeskIntent",
		TimeStart: "2026-05-03T20:00:00Z",
		TimeEnd:   "2026-05-03T21:00:00Z",
		Weekday:   &weekday,
		Hour:      &hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mapValue(overview["meta"])["scope"] != "admin" || mapValue(overview["totals"])["utterances"] != float64(7) {
		t.Fatalf("unexpected analytics overview: %+v", overview)
	}
}

// recordedControlRequest captures what the provisioning helpers actually put
// on the wire: verb, path, query, the headers the routes key off, and the
// decoded body. body stays nil when no body was sent at all, which is what
// separates "sent {}" from "sent nothing".
type recordedControlRequest struct {
	method         string
	path           string
	rawQuery       string
	ifMatch        string
	idempotencyKey string
	body           map[string]any
}

func recordControlRequest(r *http.Request) recordedControlRequest {
	entry := recordedControlRequest{
		method:         r.Method,
		path:           r.URL.Path,
		rawQuery:       r.URL.RawQuery,
		ifMatch:        r.Header.Get("If-Match"),
		idempotencyKey: r.Header.Get("Idempotency-Key"),
	}
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
		entry.body = payload
	}
	return entry
}

func TestControlPlaneProvisionsHubs(t *testing.T) {
	var requests []recordedControlRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer token" {
			t.Errorf("unexpected authorization header %q", r.Header.Get("authorization"))
		}
		requests = append(requests, recordControlRequest(r))
		w.Header().Set("content-type", "application/json")
		if r.Method == http.MethodDelete && r.URL.Path == "/v1/hubs/hub-1" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"id":"hub-1","etag":"etag-2","counts":{"total_intents":3}}`))
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "token")
	ctx := context.Background()

	created, err := control.CreateHub(ctx, map[string]any{
		"name":            "joke-garden",
		"runtimeGroupId":  "rg-1",
		"ownerId":         "owner-1",
		"capacityProfile": "autoscaling",
		"isLocked":        false,
		"spec":            map[string]any{"protocols": map[string]any{"wss": map[string]any{"enabled": true}}},
	}, HubCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if created["id"] != "hub-1" {
		t.Fatalf("unexpected created hub %+v", created)
	}
	if _, err := control.CreateHub(ctx, map[string]any{"name": "second"}, HubCreateOptions{IdempotencyKey: "hub-create-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.UpdateHub(ctx, "hub-1", map[string]any{"active": false}, "etag-1"); err != nil {
		t.Fatal(err)
	}
	if err := control.DeleteHub(ctx, "hub-1", "etag-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := control.ReleaseHub(ctx, "hub-1", ReleaseOptions{
		Channel: "stable",
		Mode:    "custom",
		Version: "1.4.0",
		Images:  map[string]string{"ovos-core": "ghcr.io/thalovant/ovos-core:1.4.0"},
		Reason:  "pin the kiosk fleet",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.ReleaseHub(ctx, "hub-1", ReleaseOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.SetHubRating(ctx, "hub-1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := control.ClearHubRating(ctx, "hub-1"); err != nil {
		t.Fatal(err)
	}
	capabilities, err := control.GetHubRuntimeCapabilities(ctx, "hub-1")
	if err != nil {
		t.Fatal(err)
	}
	if mapValue(capabilities["counts"])["total_intents"] != float64(3) {
		t.Fatalf("unexpected runtime capabilities %+v", capabilities)
	}

	if len(requests) != 9 {
		t.Fatalf("unexpected request count %d: %+v", len(requests), requests)
	}

	create := requests[0]
	if create.method != http.MethodPost || create.path != "/v1/hubs" {
		t.Fatalf("unexpected create request %+v", create)
	}
	if create.idempotencyKey == "" {
		t.Fatal("create hub must send a generated Idempotency-Key")
	}
	for key, want := range map[string]any{
		"name":             "joke-garden",
		"runtime_group_id": "rg-1",
		"owner_id":         "owner-1",
		"capacity_profile": "autoscaling",
		"is_locked":        false,
	} {
		if create.body[key] != want {
			t.Fatalf("unexpected create body %s=%v, want %v: %+v", key, create.body[key], want, create.body)
		}
	}
	for _, camel := range []string{"runtimeGroupId", "ownerId", "capacityProfile", "isLocked"} {
		if _, present := create.body[camel]; present {
			t.Fatalf("camelCase key %s must be renamed, not duplicated: %+v", camel, create.body)
		}
	}
	if requests[1].idempotencyKey != "hub-create-1" {
		t.Fatalf("explicit idempotency key was not sent: %+v", requests[1])
	}

	update := requests[2]
	if update.method != http.MethodPatch || update.path != "/v1/hubs/hub-1" || update.ifMatch != "etag-1" {
		t.Fatalf("unexpected update request %+v", update)
	}
	if update.body["active"] != false {
		t.Fatalf("unexpected update body %+v", update.body)
	}

	del := requests[3]
	if del.method != http.MethodDelete || del.path != "/v1/hubs/hub-1" || del.ifMatch != "etag-2" {
		t.Fatalf("unexpected delete request %+v", del)
	}

	release := requests[4]
	if release.method != http.MethodPost || release.path != "/v1/hubs/hub-1/release" {
		t.Fatalf("unexpected release request %+v", release)
	}
	if release.body["channel"] != "stable" || release.body["mode"] != "custom" ||
		release.body["version"] != "1.4.0" || release.body["reason"] != "pin the kiosk fleet" {
		t.Fatalf("unexpected release body %+v", release.body)
	}
	if mapValue(release.body["images"])["ovos-core"] != "ghcr.io/thalovant/ovos-core:1.4.0" {
		t.Fatalf("unexpected release images %+v", release.body)
	}
	if emptyRelease := requests[5]; emptyRelease.body == nil || len(emptyRelease.body) != 0 {
		t.Fatalf("an unset ReleaseOptions must send an empty JSON object: %+v", emptyRelease)
	}

	rate := requests[6]
	if rate.method != http.MethodPut || rate.path != "/v1/hubs/hub-1/rating" || rate.body["rating"] != float64(5) {
		t.Fatalf("unexpected rating request %+v", rate)
	}
	cleared := requests[7]
	if cleared.method != http.MethodDelete || cleared.path != "/v1/hubs/hub-1/rating" || cleared.body != nil {
		t.Fatalf("unexpected rating clear request %+v", cleared)
	}
	inspect := requests[8]
	if inspect.method != http.MethodGet || inspect.path != "/v1/hubs/hub-1/runtime-capabilities" {
		t.Fatalf("unexpected runtime capabilities request %+v", inspect)
	}
}

func TestControlPlaneManagesRuntimeGroups(t *testing.T) {
	var requests []recordedControlRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, recordControlRequest(r))
		w.Header().Set("content-type", "application/json")
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"id":"rg-1","name":"kiosks","config":{"lang":"en-us"}}`))
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "token")
	ctx := context.Background()

	if _, err := control.ListRuntimeGroups(ctx, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := control.ListRuntimeGroups(ctx, ""); err != nil {
		t.Fatal(err)
	}
	group, err := control.GetRuntimeGroup(ctx, "rg-1")
	if err != nil {
		t.Fatal(err)
	}
	if group["id"] != "rg-1" {
		t.Fatalf("unexpected runtime group %+v", group)
	}
	if _, err := control.CreateRuntimeGroup(ctx, map[string]any{
		"name":             "kiosks",
		"description":      "Lobby kiosks",
		"ownerId":          "owner-1",
		"cloneFromDefault": true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.UpdateRuntimeGroup(ctx, "rg-1", map[string]any{
		"description": "Lobby and cafe kiosks",
		"spec":        map[string]any{"replicas": 2},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.GetRuntimeGroupConfig(ctx, "rg-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := control.UpdateRuntimeGroupConfig(ctx, "rg-1", map[string]any{"lang": "en-us"}, RuntimeGroupConfigOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.UpdateRuntimeGroupConfig(ctx, "rg-1", map[string]any{"lang": "fr-ca"}, RuntimeGroupConfigOptions{
		Personas: map[string]any{"default": "concierge"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.ReleaseRuntimeGroup(ctx, "rg-1", ReleaseOptions{Channel: "stable"}); err != nil {
		t.Fatal(err)
	}
	if err := control.DeleteRuntimeGroup(ctx, "rg-1"); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 10 {
		t.Fatalf("unexpected request count %d: %+v", len(requests), requests)
	}
	// No runtime-group route reads If-Match or an idempotency header, so the
	// SDK must not invent one for them.
	for _, entry := range requests {
		if entry.ifMatch != "" || entry.idempotencyKey != "" {
			t.Fatalf("runtime-group routes take no If-Match or Idempotency-Key: %+v", entry)
		}
	}

	if requests[0].method != http.MethodGet || requests[0].path != "/v1/runtime-groups" || requests[0].rawQuery != "owner_id=owner-1" {
		t.Fatalf("unexpected list request %+v", requests[0])
	}
	if requests[1].rawQuery != "" {
		t.Fatalf("an empty owner id must be omitted from the query: %+v", requests[1])
	}
	if requests[2].method != http.MethodGet || requests[2].path != "/v1/runtime-groups/rg-1" {
		t.Fatalf("unexpected get request %+v", requests[2])
	}

	create := requests[3]
	if create.method != http.MethodPost || create.path != "/v1/runtime-groups" {
		t.Fatalf("unexpected create request %+v", create)
	}
	if create.body["owner_id"] != "owner-1" || create.body["clone_from_default"] != true {
		t.Fatalf("unexpected create body %+v", create.body)
	}
	if _, present := create.body["cloneFromDefault"]; present {
		t.Fatalf("camelCase key must be renamed, not duplicated: %+v", create.body)
	}

	update := requests[4]
	if update.method != http.MethodPatch || update.path != "/v1/runtime-groups/rg-1" {
		t.Fatalf("unexpected update request %+v", update)
	}
	if mapValue(update.body["spec"])["replicas"] != float64(2) {
		t.Fatalf("unexpected update body %+v", update.body)
	}

	if requests[5].method != http.MethodGet || requests[5].path != "/v1/runtime-groups/rg-1/config" {
		t.Fatalf("unexpected config read %+v", requests[5])
	}

	config := requests[6]
	if config.method != http.MethodPatch || config.path != "/v1/runtime-groups/rg-1/config" {
		t.Fatalf("unexpected config update %+v", config)
	}
	if mapValue(config.body["config"])["lang"] != "en-us" {
		t.Fatalf("unexpected config body %+v", config.body)
	}
	if _, present := config.body["personas"]; present {
		t.Fatalf("personas must be omitted when unset: %+v", config.body)
	}
	if personas := mapValue(requests[7].body["personas"]); personas["default"] != "concierge" {
		t.Fatalf("unexpected personas body %+v", requests[7].body)
	}

	release := requests[8]
	if release.method != http.MethodPost || release.path != "/v1/runtime-groups/rg-1/release" || release.body["channel"] != "stable" {
		t.Fatalf("unexpected release request %+v", release)
	}
	if _, present := release.body["mode"]; present {
		t.Fatalf("unset release options must be omitted: %+v", release.body)
	}
	if requests[9].method != http.MethodDelete || requests[9].path != "/v1/runtime-groups/rg-1" {
		t.Fatalf("unexpected delete request %+v", requests[9])
	}
}

func TestControlPlaneDiscoversAndInstallsSkills(t *testing.T) {
	var requests []recordedControlRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, recordControlRequest(r))
		w.Header().Set("content-type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/runtime-groups/rg-1/inventory":
			_, _ = w.Write([]byte(`{"runtime_group_id":"rg-1","data":[],"source":"ovos-runtime-operator-pending"}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"skill_id":"skill-weather","access_tier":"free","installable":true}]}`))
		}
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "token")
	ctx := context.Background()

	catalog, err := control.ListMarketplaceSkills(ctx, MarketplaceSkillListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog["data"].([]any)) != 1 {
		t.Fatalf("unexpected catalog %+v", catalog)
	}
	if _, err := control.ListMarketplaceSkills(ctx, MarketplaceSkillListOptions{
		OwnerID:         "owner-1",
		IncludeInactive: true,
		ForceRefresh:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.ListRuntimeGroupMarketplace(ctx, "rg-1", RuntimeGroupMarketplaceOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.ListRuntimeGroupMarketplace(ctx, "rg-1", RuntimeGroupMarketplaceOptions{RefreshInventory: true}); err != nil {
		t.Fatal(err)
	}
	// A runtime group with nothing reporting answers with an empty data list
	// and a pending source instead of the 409 the hub route returns.
	inventory, err := control.ListRuntimeGroupInventory(ctx, "rg-1", RuntimeGroupInventoryOptions{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if inventory["source"] != "ovos-runtime-operator-pending" || len(inventory["data"].([]any)) != 0 {
		t.Fatalf("unexpected inventory %+v", inventory)
	}
	if _, err := control.ListRuntimeGroupInventory(ctx, "rg-1", RuntimeGroupInventoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.InstallRuntimeGroupSkill(ctx, "rg-1", "skill-weather", RuntimeGroupSkillInstallOptions{}); err != nil {
		t.Fatal(err)
	}
	inactive := false
	if _, err := control.InstallRuntimeGroupSkill(ctx, "rg-1", "skill-lab", RuntimeGroupSkillInstallOptions{
		MarketplaceSkillID: "mk-1",
		SourceType:         "git",
		SourceRef:          "https://github.com/thalovant/skill-lab",
		VersionPin:         "0.2.0",
		Active:             &inactive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.UninstallRuntimeGroupSkill(ctx, "rg-1", "skill-weather"); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 9 {
		t.Fatalf("unexpected request count %d: %+v", len(requests), requests)
	}
	if requests[0].method != http.MethodGet || requests[0].path != "/v1/marketplace/skills" || requests[0].rawQuery != "" {
		t.Fatalf("false and empty catalog options must be omitted: %+v", requests[0])
	}
	if requests[1].rawQuery != "force_refresh=true&include_inactive=true&owner_id=owner-1" {
		t.Fatalf("unexpected catalog query %+v", requests[1])
	}
	if requests[2].path != "/v1/runtime-groups/rg-1/marketplace" || requests[2].rawQuery != "" {
		t.Fatalf("refresh_inventory must be omitted when false: %+v", requests[2])
	}
	if requests[3].rawQuery != "refresh_inventory=true" {
		t.Fatalf("unexpected group marketplace query %+v", requests[3])
	}
	if requests[4].path != "/v1/runtime-groups/rg-1/inventory" || requests[4].rawQuery != "refresh=true" {
		t.Fatalf("unexpected inventory query %+v", requests[4])
	}
	if requests[5].rawQuery != "" {
		t.Fatalf("refresh must be omitted when false: %+v", requests[5])
	}

	install := requests[6]
	if install.method != http.MethodPost || install.path != "/v1/runtime-groups/rg-1/skills" {
		t.Fatalf("unexpected install request %+v", install)
	}
	if install.body["skill_id"] != "skill-weather" || install.body["source_type"] != "catalog" || install.body["active"] != true {
		t.Fatalf("a zero-value install must default to an active catalog install: %+v", install.body)
	}
	for _, key := range []string{"marketplace_skill_id", "source_ref", "version_pin"} {
		if _, present := install.body[key]; present {
			t.Fatalf("unset install option %s must be omitted: %+v", key, install.body)
		}
	}

	gitInstall := requests[7]
	for key, want := range map[string]any{
		"skill_id":             "skill-lab",
		"marketplace_skill_id": "mk-1",
		"source_type":          "git",
		"source_ref":           "https://github.com/thalovant/skill-lab",
		"version_pin":          "0.2.0",
		"active":               false,
	} {
		if gitInstall.body[key] != want {
			t.Fatalf("unexpected install body %s=%v, want %v: %+v", key, gitInstall.body[key], want, gitInstall.body)
		}
	}

	uninstall := requests[8]
	if uninstall.method != http.MethodDelete || uninstall.path != "/v1/runtime-groups/rg-1/skills/skill-weather" {
		t.Fatalf("unexpected uninstall request %+v", uninstall)
	}
}

func TestControlPlaneProvisioningErrorsCarryAPIStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/hubs":
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"detail":"API access requires a paid plan."}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/hubs/hub-1":
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"detail":"ETag mismatch"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/hubs/hub-1":
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"detail":"ETag mismatch"}`))
		case r.URL.Path == "/v1/runtime-groups/rg-1/marketplace":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"detail":"Not authorized"}`))
		case r.URL.Path == "/v1/hubs/hub-1/runtime-capabilities":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"Live skills and intents are not available for this hub yet."}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "token")
	ctx := context.Background()

	_, paidErr := control.CreateHub(ctx, map[string]any{"name": "joke-garden"}, HubCreateOptions{})
	assertControlAPIError(t, paidErr, "402", "paid plan")

	_, staleErr := control.UpdateHub(ctx, "hub-1", map[string]any{"active": false}, "stale-etag")
	assertControlAPIError(t, staleErr, "412", "ETag mismatch")

	assertControlAPIError(t, control.DeleteHub(ctx, "hub-1", "stale-etag"), "412", "ETag mismatch")

	_, forbiddenErr := control.ListRuntimeGroupMarketplace(ctx, "rg-1", RuntimeGroupMarketplaceOptions{})
	assertControlAPIError(t, forbiddenErr, "403", "Not authorized")

	_, conflictErr := control.GetHubRuntimeCapabilities(ctx, "hub-1")
	assertControlAPIError(t, conflictErr, "409", "not available")
}

func assertControlAPIError(t *testing.T, err error, status string, detail string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an HTTP %s error", status)
	}
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("error %v does not wrap ErrAPI", err)
	}
	if !strings.Contains(err.Error(), status) || !strings.Contains(err.Error(), detail) {
		t.Fatalf("error %v does not report HTTP %s and %q", err, status, detail)
	}
}

func TestControlPlaneBootstrapPreservesAPIReturnedMQTTCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v1/hubs/hub-mqtt":
			_, _ = w.Write([]byte(`{"id":"hub-mqtt","name":"mqtt-hub","domain":"mqtt.thalovant.io","data_plane_endpoints":{"https":"https://mqtt.thalovant.io","wss":"wss://mqtt.thalovant.io","mqtt":"mqtts://broker.thalovant.io:8883"},"spec":{"protocols":{"wss":{"enabled":true},"http":{"enabled":true},"mqtt":{"enabled":true,"brokerUrl":"mqtts://broker.thalovant.io:8883"}}}}`))
		case "/v1/clients":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			spec := mapValue(payload["spec"])
			response := map[string]any{
				"id":     "client-mqtt",
				"name":   payload["name"],
				"hub_id": payload["hub_id"],
				"spec":   map[string]any{"version": "1", "apiKeyRef": map[string]any{"name": "secret", "key": "apiKey"}},
				"initial_identify": map[string]any{
					"access_key":     spec["apiKey"],
					"password":       spec["password"],
					"crypto_key":     spec["cryptoKey"],
					"site_id":        spec["siteId"],
					"default_master": "wss://mqtt.thalovant.io",
					"mqtt": map[string]any{
						"endpoint":     "mqtts://broker.thalovant.io:8883",
						"username":     spec["apiKey"],
						"password":     "broker-password",
						"topic_prefix": "hivemind/hub-mqtt/" + optional(spec["apiKey"]),
					},
				},
			}
			if err := json.NewEncoder(w).Encode(response); err != nil {
				t.Fatal(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "token")
	result, err := control.CreateClientIdentityForHubID(context.Background(), "hub-mqtt", BootstrapIdentityOptions{Name: "kiosk"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity.MQTT == nil {
		t.Fatal("expected mqtt credentials")
	}
	if result.Identity.MQTT.Endpoint != "mqtts://broker.thalovant.io:8883" || result.Identity.MQTT.Password != "broker-password" {
		t.Fatalf("unexpected mqtt credentials: %+v", result.Identity.MQTT)
	}
	if result.Identity.EndpointFor(ProtocolMQTT) != "mqtts://broker.thalovant.io:8883" {
		t.Fatalf("unexpected mqtt endpoint %s", result.Identity.EndpointFor(ProtocolMQTT))
	}
	runtime, err := control.RequireRuntimeProtocol(result, ProtocolMQTT)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Protocol != ProtocolMQTT || runtime.Endpoint != "mqtts://broker.thalovant.io:8883" {
		t.Fatalf("unexpected mqtt runtime endpoint: %+v", runtime)
	}
	identity := result.Summary(false)["identity"].(map[string]any)
	if mqtt := identity["mqtt"].(map[string]any); mqtt["password"] != nil {
		t.Fatalf("mqtt password should be redacted by default: %+v", mqtt)
	}
	identity = result.Summary(true)["identity"].(map[string]any)
	if mqtt := identity["mqtt"].(map[string]any); mqtt["password"] != "broker-password" {
		t.Fatalf("mqtt password should be included with secrets: %+v", mqtt)
	}
}

func TestRuntimeCryptoKeyTruncates(t *testing.T) {
	got := string(RuntimeCryptoKey("0123456789abcdef-extra"))
	if got != "0123456789abcdef" {
		t.Fatalf("unexpected runtime key %q", got)
	}
}

func TestBuildClientContext(t *testing.T) {
	context := BuildClientContext(nil, ClientContextOptions{
		UserID:       "u-1",
		UserName:     "Ada",
		AuthToken:    "token",
		AuthProvider: "oidc",
		Roles:        []string{"operator"},
		Platform:     "mobile",
		Source:       "device-1",
		Channel:      "chat",
		DeviceID:     "phone-1",
		Metadata:     map[string]any{"shift": "night"},
	})
	if mapValue(context["user"])["name"] != "Ada" || mapValue(context["auth"])["provider"] != "oidc" {
		t.Fatalf("unexpected context: %+v", context)
	}
	if mapValue(context["device"])["platform"] != "mobile" || mapValue(context["metadata"])["shift"] != "night" {
		t.Fatalf("unexpected context metadata: %+v", context)
	}
}

func TestDisplayItemsFromEventData(t *testing.T) {
	rich, _ := json.Marshal(map[string]any{
		"table":         `[{"name":"part","status":"ok"}]`,
		"attachment":    map[string]any{"type": "image", "payload": map[string]any{"src": "https://example.com/image.png"}},
		"quick_replies": []map[string]any{{"title": "Continue", "payload": "/continue"}},
	})
	items := DisplayItemsFromEventData(Data{
		"utterance":       "<speak>Hello</speak>",
		"rich_media_data": string(rich),
	}, EventSpeak, 0)
	if len(items) != 4 {
		t.Fatalf("expected 4 display items, got %+v", items)
	}
	if items[0].Kind != "text" || items[0].Text != "Hello" {
		t.Fatalf("unexpected text item: %+v", items[0])
	}
	if items[2].Kind != "image" || items[2].URL != "https://example.com/image.png" {
		t.Fatalf("unexpected image item: %+v", items[2])
	}
	choices, ok := items[3].Data.([]map[string]any)
	if !ok || choices[0]["payload"] != "/continue" {
		t.Fatalf("unexpected choices: %+v", items[3].Data)
	}
}

func TestEncryptAsJSONRoundTrips(t *testing.T) {
	encrypted, err := EncryptAsJSON("0123456789abcdef-extra", "hello")
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := DecryptFromJSON("0123456789abcdef-extra", encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != "hello" {
		t.Fatalf("unexpected plaintext %q", decrypted)
	}
}

func TestEncryptAsBinaryRoundTrips(t *testing.T) {
	plaintext := []byte("hello")
	encrypted, err := EncryptAsBinary("0123456789abcdef-extra", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := DecryptBinary("0123456789abcdef-extra", encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if string(decrypted) != string(plaintext) {
		t.Fatalf("unexpected plaintext %q", string(decrypted))
	}
}

func TestHiveBinaryFrameRoundTrips(t *testing.T) {
	encoded, err := EncodeHiveBinaryFrame(HiveMessage{
		MsgType: "bus",
		Payload: map[string]any{
			"type":    "test.event",
			"data":    map[string]any{"ok": true},
			"context": map[string]any{"metadata": map[string]any{"thalovant_owner_id": "owner-1"}},
		},
		Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeHiveBinaryFrame(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if encoded[0] != 0x82 || decoded.MsgType != "bus" {
		t.Fatalf("unexpected encoded frame or message: %x %+v", encoded[:2], decoded)
	}
	if mapValue(decoded.Payload["context"])["metadata"] == nil {
		t.Fatalf("unexpected decoded payload: %+v", decoded.Payload)
	}
}

func TestHiveBinaryFrameRoundTripsLargePayload(t *testing.T) {
	data := strings.Repeat("x", 1<<20)
	encoded, err := EncodeHiveBinaryFrame(HiveMessage{
		MsgType:  "bus",
		Payload:  map[string]any{"data": data},
		Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeHiveBinaryFrame(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Payload["data"] != data {
		t.Fatal("large HiveMind binary payload did not round-trip")
	}
}

func TestEventContextMatching(t *testing.T) {
	context := ContextWithCorrelation(nil, "session-1", "site", "en-us", "request-1")
	event := Event{Name: EventSpeak, Data: Data{"utterance": "hi"}, Context: context}
	if event.Text() != "hi" || event.SessionID() != "session-1" || event.RequestID() != "request-1" {
		t.Fatalf("unexpected event: %+v", event)
	}
	if !EventMatchesContext(event, context) {
		t.Fatal("expected event to match context")
	}
	if EventMatchesContext(event, ContextWithCorrelation(nil, "other", "", "", "")) {
		t.Fatal("expected event not to match different session")
	}
}
