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
		DataPlaneEndpoints: HubDataPlaneEndpoints{
			HTTPS: "https://ep-user:ep-pass-SECRET-6666@hub.example.com",
			WSS:   "wss://hub.example.com",
		},
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
		"ep-pass-SECRET-6666", // userinfo embedded in a data-plane endpoint URL
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
		// Typed containers a caller could hand-build must be traversed too.
		"typed_creds": map[string]string{"password": "typedmap-SECRET"},
		"typed_list":  []map[string]string{{"crypto_key": "typedslice-SECRET"}},
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
		"typedmap-SECRET", "typedslice-SECRET",
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
	for _, secret := range []string{"iit-SECRET-token", "specak-SECRET", "mqttpw-SECRET", "id-ak-SECRET", "typedmap-SECRET", "typedslice-SECRET"} {
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
	// The fallback path (url.Parse fails on the invalid control character) must
	// drop a fragment, not only a query.
	malformed := "http://host/\x7fpath#authorization=frag"
	if _, err := url.Parse(malformed); err == nil {
		t.Fatal("test precondition: expected url.Parse to reject the malformed URL")
	}
	if got := stripURLQuery(malformed); strings.ContainsAny(got, "#") || strings.Contains(got, "authorization") {
		t.Fatalf("fallback must strip the fragment, got %q", got)
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

func TestServerErrorDetailSurfacesOnlyAllowlistedJSONFields(t *testing.T) {
	if got := serverErrorDetail(nil); got != "(no response body)" {
		t.Fatalf("empty body: got %q", got)
	}
	if got := serverErrorDetail([]byte("  \n\t ")); got != "(no response body)" {
		t.Fatalf("whitespace-only body: got %q", got)
	}
	// A non-JSON body is omitted, never echoed.
	if got := serverErrorDetail([]byte("<html>token-abc-1234</html>")); got != serverErrorOmitted {
		t.Fatalf("non-json body should be omitted, got %q", got)
	}
	// JSON without an allowlisted field is omitted (the request-echo case).
	if got := serverErrorDetail([]byte(`{"spec":{"apiKey":"leak-SECRET"}}`)); got != serverErrorOmitted {
		t.Fatalf("json without an allowlisted field should be omitted, got %q", got)
	}
	// A failed POST /v1/clients response that both echoes the request and
	// carries a server message must surface only the message, never the secrets.
	echoed := serverErrorDetail([]byte(`{"detail":"invalid client spec","spec":{"apiKey":"ak-SECRET","password":"pw-SECRET","cryptoKey":"ck-SECRET"}}`))
	if echoed != "invalid client spec" {
		t.Fatalf("expected only the detail message, got %q", echoed)
	}
	for _, secret := range []string{"ak-SECRET", "pw-SECRET", "ck-SECRET"} {
		if strings.Contains(echoed, secret) {
			t.Fatalf("detail leaked request secret %q: %s", secret, echoed)
		}
	}
	// The surfaced message is whitespace-collapsed to a single line.
	if got := serverErrorDetail([]byte("{\"message\":\"line one\\nline two\"}")); got != "line one line two" {
		t.Fatalf("message whitespace not collapsed: %q", got)
	}
	// An over-long message is bounded with an ellipsis.
	long := serverErrorDetail([]byte(`{"detail":"` + strings.Repeat("A", maxServerErrorDetail+50) + `"}`))
	if runes := []rune(long); len(runes) != maxServerErrorDetail+1 { // +1 for the ellipsis
		t.Fatalf("expected %d runes, got %d: %q", maxServerErrorDetail+1, len(runes), long)
	}
	if !strings.HasSuffix(long, "…") {
		t.Fatalf("truncated detail should end with an ellipsis: %q", long)
	}
}

func TestControlPlaneErrorBodyIsBoundedAndScrubbed(t *testing.T) {
	// A JSON error that both overflows the bound and echoes the request's
	// generated secrets under "spec".
	body := `{"detail":"` + strings.Repeat("d", maxServerErrorDetail) + `TAIL-MARKER","spec":{"apiKey":"echoed-SECRET","password":"echoed-pw-SECRET"}}`
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
	for _, secret := range []string{"echoed-SECRET", "echoed-pw-SECRET"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error leaked an echoed request secret %q: %s", secret, msg)
		}
	}
	if strings.Contains(msg, "TAIL-MARKER") {
		t.Fatalf("error must be truncated, not echo the whole message: %q", msg)
	}
	if !strings.HasSuffix(msg, "…") {
		t.Fatalf("truncated error should end with an ellipsis: %q", msg)
	}

	// A non-JSON error body is omitted entirely rather than echoed.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream said secret-xyz"))
	}))
	defer plain.Close()
	if _, err := NewControlPlane(plain.URL, "").Login(context.Background(), "ada@example.com", "secret", ""); err == nil || strings.Contains(err.Error(), "secret-xyz") {
		t.Fatalf("non-json error body must be omitted, got %v", err)
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
