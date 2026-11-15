package thalovant

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// secretBearing builds one of every secret-bearing value, all holding the
// marker "LEAK", and the result of a connector-link create whose raw client
// answer carries initial_identify and the spec's apiKey and password.
func secretBearing() map[string]any {
	identity := Identity{
		AccessKey: "LEAK-access", Password: "LEAK-password", SiteID: "site",
		DefaultMaster: "wss://hub.example.com", DefaultPort: 443,
		MQTT: &MqttBrokerCredentials{Endpoint: "mqtts://b:8883", Username: "LEAK-user", Password: "LEAK-mqtt", TLS: true},
	}
	client := map[string]any{
		"id":        "client-1",
		"callbacks": []any{"https://cb:LEAK-cb-url@cb.example.com/path"},
		"spec":      map[string]any{"apiKey": "LEAK-spec-key", "password": "LEAK-spec-password", "connectors": []any{"jellyfin"}},
		"initial_identify": map[string]any{
			"access_key": "LEAK-access", "password": "LEAK-password",
			"mqtt": map[string]any{"username": "LEAK-user", "password": "LEAK-mqtt"},
		},
		"initial_identify_token": "LEAK-token",
	}
	result := BootstrapIdentityResult{
		Identity: identity,
		Hub:      map[string]any{"id": "hub-1", "domain": "home.example.com", "data_plane_endpoints": map[string]any{"wss": "wss://u:LEAK-hub-url@home.example.com"}},
		Client:   client,
		Endpoint: &SelectedHubEndpoint{Protocol: ProtocolWSS, Endpoint: "wss://u:LEAK-url@hub.example.com"},
	}
	return map[string]any{
		"Identity":                 identity,
		"*Identity":                &identity,
		"MqttBrokerCredentials":    *identity.MQTT,
		"BootstrapIdentityResult":  result,
		"*BootstrapIdentityResult": &result,
		"ControlPlane":             ControlPlane{APIURL: "https://api.example.com", AccessToken: "LEAK-bearer"},
		"APIToken":                 APIToken{AccessToken: "LEAK-bearer", TokenType: "Bearer"},
		"DeviceAuthorization":      DeviceAuthorization{DeviceCode: "LEAK-device", UserCode: "ABCD-EFGH"},
		"NativeSignIn":             NativeSignIn{AuthorizationURL: "https://x/authorize", State: "LEAK-state", Verifier: "LEAK-verifier"},
		"ClientContextOptions":     ClientContextOptions{UserID: "u", AuthToken: "LEAK-auth", AuthClaims: map[string]any{"sub": "LEAK-claim"}},
	}
}

func TestSecretBearingTypesRedactUnderEveryVerb(t *testing.T) {
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%10v", "%-20s"}
	for name, value := range secretBearing() {
		for _, verb := range verbs {
			rendered := fmt.Sprintf(verb, value)
			if strings.Contains(rendered, "LEAK") || strings.Contains(strings.ToLower(rendered), "leak") {
				t.Errorf("%s %s prints a secret: %s", name, verb, rendered)
			}
		}
		for _, line := range []string{fmt.Sprint(value), fmt.Sprintln(value), fmt.Sprintf("%v", []any{value}), fmt.Sprintf("%v", map[string]any{"k": value})} {
			if strings.Contains(line, "LEAK") {
				t.Errorf("%s nested or Sprint prints a secret: %s", name, line)
			}
		}
	}
}

func TestBootstrapIdentityResultKeepsNonSecretContext(t *testing.T) {
	rendered := fmt.Sprintf("%+v", secretBearing()["BootstrapIdentityResult"])
	for _, want := range []string{"client-1", "hub-1", "home.example.com", "jellyfin", secretPlaceholder, "hub.example.com"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered result lost %q: %s", want, rendered)
		}
	}
}

func TestSecretBearingTypesRedactInSlog(t *testing.T) {
	for name, value := range secretBearing() {
		for _, handler := range []string{"json", "text"} {
			var out strings.Builder
			var h slog.Handler
			if handler == "json" {
				h = slog.NewJSONHandler(&out, nil)
			} else {
				h = slog.NewTextHandler(&out, nil)
			}
			slog.New(h).Info("m", "value", value)
			if strings.Contains(out.String(), "LEAK") {
				t.Errorf("%s slog %s handler prints a secret: %s", name, handler, out.String())
			}
		}
	}
}

func TestBootstrapIdentityResultJSONRedactsButIdentityJSONKeepsSecrets(t *testing.T) {
	values := secretBearing()
	encoded, err := json.Marshal(values["BootstrapIdentityResult"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "LEAK") {
		t.Fatalf("result JSON prints a secret: %s", encoded)
	}
	identity := values["Identity"].(Identity)
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	// The identity file is the one place the real values are written.
	if !strings.Contains(string(raw), "LEAK-access") || !strings.Contains(string(raw), "LEAK-password") {
		t.Fatalf("Identity JSON must keep the secrets for the identity file: %s", raw)
	}
}

func TestResultSecretsStillReachableOnPurpose(t *testing.T) {
	result := secretBearing()["BootstrapIdentityResult"].(BootstrapIdentityResult)
	if result.Identity.AccessKey != "LEAK-access" || result.Client["initial_identify_token"] != "LEAK-token" {
		t.Fatal("redaction must not change the data the caller holds")
	}
	if got := result.Summary(true)["identity"].(map[string]any)["access_key"]; got != "LEAK-access" {
		t.Fatalf("Summary(true) must reveal on request: %v", got)
	}
}
