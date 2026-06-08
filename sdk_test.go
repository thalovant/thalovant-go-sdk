package thalovant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIdentityFromMapNormalizesAliases(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "https://hub.example.com/",
		"port":     "443",
		"path":     "/hivemind/public",
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

func TestNewClientWithOptionsRejectsUnsupportedRuntimeProtocol(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "https://hub.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewClientWithOptions(identity, ClientOptions{Protocol: ProtocolMQTT}); err == nil || !strings.Contains(err.Error(), "unsupported protocol") {
		t.Fatalf("expected unsupported protocol error, got %v", err)
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
	if result.SelectedProtocol() != ProtocolHTTPS {
		t.Fatalf("unexpected selected protocol %s", result.SelectedProtocol())
	}
	if _, ok := result.Summary(false)["identity"].(map[string]any)["access_key"]; ok {
		t.Fatal("summary should redact identity secrets by default")
	}
	if _, ok := result.Summary(true)["identity"].(map[string]any)["access_key"]; !ok {
		t.Fatal("summary should include secrets when requested")
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
