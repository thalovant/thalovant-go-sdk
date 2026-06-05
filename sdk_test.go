package thalovant

import (
	"testing"
)

func TestIdentityFromMapNormalizesAliases(t *testing.T) {
	identity, err := IdentityFromMap(map[string]any{
		"key":      "access",
		"password": "secret",
		"site":     "site",
		"host":     "https://hub.example.com/",
		"port":     "443",
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.AccessKey != "access" || identity.DefaultMaster != "https://hub.example.com" || identity.DefaultPort != 443 {
		t.Fatalf("unexpected identity: %+v", identity)
	}
}

func TestRuntimeCryptoKeyTruncates(t *testing.T) {
	got := string(RuntimeCryptoKey("0123456789abcdef-extra"))
	if got != "0123456789abcdef" {
		t.Fatalf("unexpected runtime key %q", got)
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
