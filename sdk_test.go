package thalovant

import (
	"encoding/json"
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
