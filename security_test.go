package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// F8: %v/%s/%+v of the secret-bearing types must redact, while json.Marshal
// (the wire protocol and the identity-file persistence path) must still carry
// the real secret values.

func TestIdentityStringRedactsSecretsButJSONRetainsThem(t *testing.T) {
	identity := Identity{
		AccessKey:     "ak-SECRET-1111",
		Password:      "pw-SECRET-2222",
		CryptoKey:     "ck-SECRET-3333",
		SiteID:        "site-1",
		DefaultMaster: "https://hub.example.com",
		DefaultPort:   443,
		PublicKey:     "pub-not-secret",
		MQTT: &MqttBrokerCredentials{
			Endpoint: "mqtts://broker.example.com:8883",
			Username: "mqtt-user-SECRET-4444",
			Password: "mqtt-pw-SECRET-5555",
			TLS:      true,
		},
	}
	secrets := []string{
		identity.AccessKey, identity.Password, identity.CryptoKey,
		identity.MQTT.Username, identity.MQTT.Password,
	}

	for _, format := range []string{"%v", "%s", "%+v"} {
		rendered := fmt.Sprintf(format, identity)
		for _, secret := range secrets {
			if strings.Contains(rendered, secret) {
				t.Fatalf("Identity %s leaks secret %q: %s", format, secret, rendered)
			}
		}
		if !strings.Contains(rendered, secretPlaceholder) {
			t.Fatalf("Identity %s redacted nothing: %s", format, rendered)
		}
		if !strings.Contains(rendered, "site-1") || !strings.Contains(rendered, "hub.example.com") {
			t.Fatalf("Identity %s dropped non-secret context: %s", format, rendered)
		}
	}
	// A *Identity prints through the value method too (fmt dereferences).
	if strings.Contains(fmt.Sprintf("%+v", &identity), identity.AccessKey) {
		t.Fatalf("*Identity %%+v leaks the access key")
	}

	// json.Marshal (wire + identity file) MUST retain the secrets.
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if !strings.Contains(string(raw), secret) {
			t.Fatalf("json.Marshal(Identity) dropped secret %q: %s", secret, raw)
		}
	}
	// The identity-file read path (json -> map -> IdentityFromMap) round-trips.
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatal(err)
	}
	back, err := IdentityFromMap(asMap)
	if err != nil {
		t.Fatal(err)
	}
	if back.AccessKey != identity.AccessKey || back.Password != identity.Password || back.CryptoKey != identity.CryptoKey {
		t.Fatalf("identity secrets did not survive json round-trip: %+v", back)
	}
	if back.MQTT == nil || back.MQTT.Password != identity.MQTT.Password || back.MQTT.Username != identity.MQTT.Username {
		t.Fatalf("mqtt secrets did not survive json round-trip: %+v", back.MQTT)
	}
}

func TestMqttBrokerCredentialsStringRedactsSecrets(t *testing.T) {
	creds := MqttBrokerCredentials{
		Endpoint: "mqtts://broker.example.com:8883",
		Username: "user-SECRET-aaaa",
		Password: "pass-SECRET-bbbb",
		TLS:      true,
	}
	for _, arg := range []any{creds, &creds} {
		for _, format := range []string{"%v", "%s", "%+v"} {
			rendered := fmt.Sprintf(format, arg)
			if strings.Contains(rendered, creds.Username) || strings.Contains(rendered, creds.Password) {
				t.Fatalf("%T %s leaks credentials: %s", arg, format, rendered)
			}
			if !strings.Contains(rendered, "broker.example.com") {
				t.Fatalf("%T %s dropped the endpoint: %s", arg, format, rendered)
			}
		}
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), creds.Password) || !strings.Contains(string(raw), creds.Username) {
		t.Fatalf("json.Marshal dropped mqtt secrets: %s", raw)
	}
}

func TestControlPlaneStringRedactsAccessToken(t *testing.T) {
	cp := NewControlPlane("https://api.example.com", "token-SECRET-zzzz")
	for _, arg := range []any{*cp, cp} {
		for _, format := range []string{"%v", "%s", "%+v"} {
			rendered := fmt.Sprintf(format, arg)
			if strings.Contains(rendered, "token-SECRET-zzzz") {
				t.Fatalf("%T %s leaks the access token: %s", arg, format, rendered)
			}
			if !strings.Contains(rendered, secretPlaceholder) {
				t.Fatalf("%T %s redacted nothing: %s", arg, format, rendered)
			}
		}
	}
	if cp.AccessToken != "token-SECRET-zzzz" {
		t.Fatal("String() must not mutate the access token")
	}
}

// F1: the default bootstrap Summary must redact the secrets echoed in the raw
// hub/client maps; the include-secrets path must still reveal them; and neither
// must mutate the caller's original maps.

func TestBootstrapSummaryRedactsHubAndClientSecretsByDefault(t *testing.T) {
	client := map[string]any{
		"id":                     "client-1",
		"initial_identify_token": "iit-SECRET-token",
		"initial_identify": map[string]any{
			"access_key": "ak-SECRET",
			"password":   "pw-SECRET",
			"crypto_key": "ck-SECRET",
			"site_id":    "site-1",
			"mqtt": map[string]any{
				"endpoint": "mqtts://broker.example.com:8883",
				"username": "ak-SECRET",
				"password": "mqttpw-SECRET",
			},
		},
		"spec": map[string]any{
			"version":   "1",
			"apiKey":    "specak-SECRET",
			"password":  "specpw-SECRET",
			"cryptoKey": "specck-SECRET",
		},
	}
	hub := map[string]any{
		"id":        "hub-1",
		"name":      "kiosk-hub",
		"bootstrap": map[string]any{"access_key": "hub-ak-SECRET"},
	}
	result := BootstrapIdentityResult{
		Identity: Identity{
			AccessKey: "id-ak-SECRET", Password: "id-pw-SECRET", CryptoKey: "id-ck-SECRET",
			SiteID: "site-1", DefaultMaster: "https://hub.example.com", DefaultPort: 443,
		},
		Hub:    hub,
		Client: client,
	}

	secrets := []string{
		"iit-SECRET-token", "ak-SECRET", "pw-SECRET", "ck-SECRET", "mqttpw-SECRET",
		"specak-SECRET", "specpw-SECRET", "specck-SECRET", "hub-ak-SECRET",
		"id-ak-SECRET", "id-pw-SECRET", "id-ck-SECRET",
	}

	safe, err := json.Marshal(result.Summary(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(safe), secret) {
			t.Fatalf("default summary leaks secret %q: %s", secret, safe)
		}
	}
	if !strings.Contains(string(safe), secretPlaceholder) {
		t.Fatalf("default summary redacted nothing: %s", safe)
	}
	if !strings.Contains(string(safe), "hub-1") || !strings.Contains(string(safe), "kiosk-hub") || !strings.Contains(string(safe), "client-1") {
		t.Fatalf("default summary dropped non-secret fields: %s", safe)
	}

	revealed, err := json.Marshal(result.Summary(true))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"iit-SECRET-token", "specak-SECRET", "mqttpw-SECRET", "id-ak-SECRET"} {
		if !strings.Contains(string(revealed), secret) {
			t.Fatalf("include-secrets summary dropped secret %q: %s", secret, revealed)
		}
	}

	// Redaction must operate on a copy, never the caller's original maps.
	if client["initial_identify_token"] != "iit-SECRET-token" {
		t.Fatal("Summary mutated the original client map")
	}
	if ii := client["initial_identify"].(map[string]any); ii["access_key"] != "ak-SECRET" {
		t.Fatal("Summary mutated a nested client secret")
	}
}

// F7: a failed data-plane connection must not carry the ?authorization= access
// key into LastError, which ConnectionInfo()/Healthcheck() serialize to JSON.

func TestScrubTransportErrorStripsAuthorizationQuery(t *testing.T) {
	secret := "dXNlcjphY2Nlc3Nrey12345"
	urlErr := &url.Error{
		Op:  "Post",
		URL: "https://hub.example.com:443/connect?authorization=" + secret,
		Err: errors.New("connection refused"),
	}
	msg := scrubTransportError(urlErr).Error()
	if strings.Contains(msg, "authorization") || strings.Contains(msg, secret) {
		t.Fatalf("scrubbed error still leaks the authorization query: %q", msg)
	}
	if !strings.Contains(msg, "hub.example.com") || !strings.Contains(msg, "connection refused") {
		t.Fatalf("scrubbed error dropped useful context: %q", msg)
	}
	plain := errors.New("some other failure")
	if scrubTransportError(plain) != plain {
		t.Fatal("non-url errors must pass through unchanged")
	}
}

func TestStripURLQueryRemovesQueryAndFragment(t *testing.T) {
	if got := stripURLQuery("https://h/connect?authorization=abc#frag"); got != "https://h/connect" {
		t.Fatalf("unexpected stripped url %q", got)
	}
	if got := stripURLQuery(""); got != "" {
		t.Fatalf("empty input should stay empty, got %q", got)
	}
}

func TestHTTPTransportConnectDoesNotLeakAccessKeyInLastError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serverURL := server.URL
	server.Close() // force a connection-refused dial on the next request

	identity := Identity{
		AccessKey:     "ak-LEAKME-abcdef0123456789",
		Password:      "pw",
		SiteID:        "site",
		DefaultMaster: serverURL,
		DefaultPort:   443,
	}
	transport := NewHTTPTransport(identity)
	authToken := transport.Authorization()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := transport.Connect(ctx); err == nil {
		t.Fatal("expected connect to fail against a closed server")
	}

	health := transport.Healthcheck()
	info := transport.ConnectionInfo()
	rawInfo, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for label, text := range map[string]string{
		"Healthcheck.LastError":    health.LastError,
		"ConnectionInfo.LastError": info.LastError,
		"json(ConnectionInfo)":     string(rawInfo),
	} {
		if text == "" {
			t.Fatalf("%s was empty; expected a recorded connection error", label)
		}
		if strings.Contains(text, "authorization") {
			t.Fatalf("%s leaks the authorization query: %q", label, text)
		}
		if strings.Contains(text, authToken) {
			t.Fatalf("%s leaks the data-plane access token: %q", label, text)
		}
		if strings.Contains(text, identity.AccessKey) {
			t.Fatalf("%s leaks the raw access key: %q", label, text)
		}
	}
	if !strings.Contains(info.LastError, "127.0.0.1") {
		t.Fatalf("scrubbed LastError dropped the host context: %q", info.LastError)
	}
}

// F9: non-2xx control-plane errors keep the status plus a bounded, single-line
// server detail instead of interpolating the raw (possibly secret-echoing) body.

func TestServerErrorDetailBoundsAndCollapsesBody(t *testing.T) {
	if got := serverErrorDetail(nil); got != "(no response body)" {
		t.Fatalf("empty body: got %q", got)
	}
	if got := serverErrorDetail([]byte("  \n\t ")); got != "(no response body)" {
		t.Fatalf("whitespace-only body: got %q", got)
	}
	multiline := serverErrorDetail([]byte("first line\nsecond line\r\nthird"))
	if strings.ContainsAny(multiline, "\n\r") {
		t.Fatalf("detail must be single-line: %q", multiline)
	}
	if multiline != "first line second line third" {
		t.Fatalf("unexpected collapsed detail: %q", multiline)
	}
	long := serverErrorDetail([]byte(strings.Repeat("A", maxServerErrorDetail+50)))
	if runes := []rune(long); len(runes) != maxServerErrorDetail+1 { // +1 for the ellipsis
		t.Fatalf("expected %d runes, got %d: %q", maxServerErrorDetail+1, len(runes), long)
	}
	if !strings.HasSuffix(long, "…") {
		t.Fatalf("truncated detail should end with an ellipsis: %q", long)
	}
}

func TestControlPlaneErrorBodyIsBoundedAndSingleLine(t *testing.T) {
	body := "error-head\n" + strings.Repeat("x", maxServerErrorDetail) + "TAIL-MARKER"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	control := NewControlPlane(server.URL, "")
	_, err := control.Login(context.Background(), "ada@example.com", "secret", "")
	if err == nil {
		t.Fatal("expected an error from a 500 response")
	}
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("error should wrap ErrAPI: %v", err)
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\n\r") {
		t.Fatalf("error must be single-line: %q", msg)
	}
	if !strings.Contains(msg, "500") {
		t.Fatalf("error should report the HTTP status: %q", msg)
	}
	if strings.Contains(msg, "TAIL-MARKER") {
		t.Fatalf("error must be truncated, not echo the whole body: %q", msg)
	}
	if !strings.HasSuffix(msg, "…") {
		t.Fatalf("truncated error should end with an ellipsis: %q", msg)
	}
}

// Polarity foot-gun guard: the two identically named Map methods have inverted
// booleans. These assertions pin the documented behavior so a future edit that
// "aligns" them trips a test.

func TestDataPlaneEndpointsMapRedactsWhenTrue(t *testing.T) {
	endpoints := HubDataPlaneEndpoints{HTTPS: "https://user:pass@hub.example.com"}
	if got := endpoints.Map(true)["https"]; strings.Contains(got, "user:pass") {
		t.Fatalf("Map(true) should redact embedded credentials, got %q", got)
	}
	if got := endpoints.Map(false)["https"]; !strings.Contains(got, "user:pass") {
		t.Fatalf("Map(false) should keep the endpoint verbatim, got %q", got)
	}
}

func TestMqttCredentialsMapRevealsWhenTrue(t *testing.T) {
	creds := MqttBrokerCredentials{Endpoint: "mqtts://b:8883", Username: "u", Password: "p"}
	if _, ok := creds.Map(true)["password"]; !ok {
		t.Fatal("Map(true) should include the password")
	}
	if _, ok := creds.Map(false)["password"]; ok {
		t.Fatal("Map(false) should omit the password")
	}
}
