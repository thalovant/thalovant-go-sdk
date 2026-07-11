package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if topics.C2S != "hivemind/hub/c2s/access" || topics.S2C != "hivemind/hub/s2c/access" || topics.Status != "hivemind/hub/status/access" {
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

func TestMQTTTopicsAppendHubIDForScopedACLs(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "https://hub.example.com",
		"mqtt": map[string]any{
			"endpoint":     "mqtts://mqtt.example.com:8883",
			"username":     "access",
			"password":     "broker-password",
			"topic_prefix": "hivemind",
			"hub_id":       "hub-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	topics, err := MQTTTopicsForIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if topics.C2S != "hivemind/hub-1/c2s/access" || topics.S2C != "hivemind/hub-1/s2c/access" || topics.Status != "hivemind/hub-1/status/access" {
		t.Fatalf("unexpected topics: %+v", topics)
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
		_, _ = w.Write([]byte(`{"id":"operation-1","kind":"gitops.commit","aggregate_type":"gitops","aggregate_id":null,"status":"committed","details":{"git_commit_created":true},"git_commit_sha":"abc123","error_code":null,"error_message":null,"created_at":"2026-07-11T00:00:00Z","updated_at":"2026-07-11T00:00:01Z","committed_at":"2026-07-11T00:00:01Z","applied_at":null,"ready_at":null,"terminal_at":null,"links":{"self":"/api/v1/operations/operation-1"}}`))
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
		if r.URL.Path != "/v1/admin/analytics/overview" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer token" {
			t.Fatalf("unexpected authorization header %q", r.Header.Get("authorization"))
		}
		query := r.URL.Query()
		expected := map[string]string{
			"range":      "30d",
			"bucket":     "1d",
			"owner_id":   "owner-1",
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
		Admin:     true,
		Range:     "30d",
		Bucket:    "1d",
		OwnerID:   "owner-1",
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
