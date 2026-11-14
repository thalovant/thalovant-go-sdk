package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// Home bridges (connection_type "connector") and the refusals that came with
// them. The capability has no shared vectors: these cases mirror the Python
// reference's tests/test_connector_connections.py. Each also holds for an API
// that does not have the behaviour yet.

type ccReply struct {
	status  int
	body    map[string]any
	headers map[string]string
}

// ccAPI answers every request with one reply and records the last body.
func ccAPI(t *testing.T, reply ccReply) (*ControlPlane, *[]map[string]any) {
	t.Helper()
	var sent []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		sent = append(sent, body)
		w.Header().Set("Content-Type", "application/json")
		for key, value := range reply.headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(reply.status)
		_ = json.NewEncoder(w).Encode(reply.body)
	}))
	t.Cleanup(server.Close)
	return NewControlPlane(server.URL, "tok"), &sent
}

var ccHub = map[string]any{"id": "hub-1", "name": "Home", "domain": "home.example.com", "wss_enabled": true, "http_enabled": true}

func ccCreated(connectionType string, extra map[string]any) map[string]any {
	body := map[string]any{
		"id": "client-1", "name": "Bridge", "hub_id": "hub-1", "etag": "e1",
		"spec": map[string]any{"version": "1", "connection_type": connectionType},
		"initial_identify": map[string]any{
			"password": "platform-password", "access_key": "platform-key", "site_id": "bridge",
			"default_port": 443, "default_master": "wss://home.example.com",
		},
	}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

func ccProblem(status int, code string, extra map[string]any) ccReply {
	body := map[string]any{"detail": "refused", "code": code}
	for key, value := range extra {
		body[key] = value
	}
	return ccReply{status: status, body: body}
}

func TestConnectorKindConstantsAreTheWireValues(t *testing.T) {
	if ConnectionTypeConnector != "connector" || ConnectionTypeHomeAssistant != "home_assistant" {
		t.Fatalf("constants: %q %q", ConnectionTypeConnector, ConnectionTypeHomeAssistant)
	}
}

func TestConnectorABridgeIsCreatedWithItsConnectorListAndThePlatformsCredentials(t *testing.T) {
	serves := map[string]any{
		"devices": []any{map[string]any{"id": "d1", "name": "Kitchen", "connection_type": "voice_satellite", "active": true}},
		"count":   float64(1), "warning": nil,
	}
	api, sent := ccAPI(t, ccReply{status: 201, body: ccCreated("connector", map[string]any{"serves": serves})})
	result, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{
		Name: "Bridge", ConnectionType: ConnectionTypeConnector, Spec: map[string]any{"connectors": []any{"music_assistant"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := (*sent)[0]["spec"].(map[string]any)
	if spec["connection_type"] != "connector" || !reflect.DeepEqual(spec["connectors"], []any{"music_assistant"}) {
		t.Fatalf("spec sent: %v", spec)
	}
	// The answer's credentials are the connection's; the ones minted locally are not.
	if result.Identity.AccessKey != "platform-key" || result.Identity.Password != "platform-password" {
		t.Fatalf("identity: %+v", result.Identity)
	}
	if result.ConnectionType() != "connector" || result.Replayed {
		t.Fatalf("kind %q replayed %v", result.ConnectionType(), result.Replayed)
	}
	if !reflect.DeepEqual(result.Serves(), serves) || result.ServesWarning() != "" {
		t.Fatalf("serves: %v warning %q", result.Serves(), result.ServesWarning())
	}
}

func TestConnectorALinkThatServesNobodySaysSo(t *testing.T) {
	serves := map[string]any{"devices": []any{}, "count": float64(0), "warning": "No device of your account is on this hub."}
	api, _ := ccAPI(t, ccReply{status: 201, body: ccCreated("home_assistant", map[string]any{"serves": serves})})
	result, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "HA", ConnectionType: "home_assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ServesWarning() != "No device of your account is on this hub." {
		t.Fatalf("warning: %q", result.ServesWarning())
	}
}

func TestConnectorAnAPIWithoutServesStillWorks(t *testing.T) {
	api, _ := ccAPI(t, ccReply{status: 201, body: ccCreated("connector", nil)})
	result, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "Bridge", ConnectionType: "connector"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Serves() != nil || result.ServesWarning() != "" {
		t.Fatalf("serves: %v %q", result.Serves(), result.ServesWarning())
	}
}

func TestConnectorAReplayedCreateNeverHandsBackAnIdentityTheSDKMadeUp(t *testing.T) {
	cases := map[string]func() ccReply{
		"header": func() ccReply {
			body := ccCreated("connector", nil)
			delete(body, "initial_identify")
			return ccReply{status: 201, body: body, headers: map[string]string{"Idempotent-Replayed": "true"}}
		},
		"marker": func() ccReply {
			body := ccCreated("connector", map[string]any{"replay": map[string]any{
				"code": "idempotent_replay", "credentials_included": false, "detail": "x"}})
			delete(body, "initial_identify")
			return ccReply{status: 201, body: body}
		},
		// Whatever else the answer carries, a replay never yields an identity.
		"header with credentials in the body": func() ccReply {
			body := ccCreated("connector", nil)
			body["spec"].(map[string]any)["apiKey"] = "echoed"
			return ccReply{status: 201, body: body, headers: map[string]string{"Idempotent-Replayed": "TRUE"}}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			api, _ := ccAPI(t, build())
			result, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{
				Name: "Bridge", ConnectionType: "connector", IdempotencyKey: "k1"})
			var replay *CredentialsNotReplayedError
			if !errors.As(err, &replay) || !errors.Is(err, ErrCredentialsNotReplayed) {
				t.Fatalf("want CredentialsNotReplayedError, got %v", err)
			}
			if replay.ClientID() != "client-1" {
				t.Fatalf("client id %q", replay.ClientID())
			}
			if _, has := replay.Client["initial_identify"]; has {
				t.Fatal("the replayed resource carries initial_identify")
			}
			if spec, _ := replay.Client["spec"].(map[string]any); spec["apiKey"] != nil {
				t.Fatal("the replayed resource carries a spec secret")
			}
			if result.Identity.AccessKey != "" {
				t.Fatalf("an identity came back: %+v", result.Identity)
			}
		})
	}
}

func TestConnectorTheIntercomReplayHeaderIsNotTheConnectionReplayHeader(t *testing.T) {
	api, _ := ccAPI(t, ccReply{status: 201, body: ccCreated("connector", nil), headers: map[string]string{"Idempotency-Replayed": "true"}})
	if _, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "Bridge", ConnectionType: "connector"}); err != nil {
		t.Fatal(err)
	}
}

func TestConnectorAnAnswerWithoutCredentialsThatIsNotAReplayKeepsTheOldBehaviour(t *testing.T) {
	body := ccCreated("connector", nil)
	delete(body, "initial_identify")
	api, _ := ccAPI(t, ccReply{status: 201, body: body})
	result, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "Bridge", ConnectionType: "connector"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity.AccessKey == "" {
		t.Fatal("the fallback identity is empty")
	}
}

func TestConnectorALinkOnAnotherAccountsHubNamesTheReason(t *testing.T) {
	api, sent := ccAPI(t, ccProblem(422, "connection_type_unavailable", map[string]any{"reason": "hub_operated_by_another_account"}))
	_, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "HA", ConnectionType: "home_assistant"})
	var unavailable *ConnectionTypeUnavailableError
	var unsupported *UnsupportedConnectionTypeError
	var apiErr *APIError
	if !errors.As(err, &unavailable) || !errors.As(err, &unsupported) || !errors.As(err, &apiErr) {
		t.Fatalf("want an unavailable error that is also unsupported and an APIError, got %T %v", err, err)
	}
	if !errors.Is(err, ErrConnectionTypeUnavailable) || !errors.Is(err, ErrUnsupportedConnectionType) || !errors.Is(err, ErrAPI) {
		t.Fatalf("sentinels: %v", err)
	}
	if unavailable.Reason() != "hub_operated_by_another_account" || apiErr.Code != ConnectionCodeTypeUnavailable || apiErr.StatusCode != 422 {
		t.Fatalf("reason %q code %q status %d", unavailable.Reason(), apiErr.Code, apiErr.StatusCode)
	}
	if len(*sent) != 1 {
		t.Fatalf("nothing to delete: the API made nothing, but %d requests were sent", len(*sent))
	}
}

func TestConnectorAnUnknownOrAmbiguousKindIsUnsupported(t *testing.T) {
	for _, code := range []string{"connection_type_unknown", "connection_type_ambiguous"} {
		api, _ := ccAPI(t, ccProblem(422, code, map[string]any{"known": []any{"voice_satellite", "connector"}}))
		_, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "X", ConnectionType: "plex"})
		var unsupported *UnsupportedConnectionTypeError
		var unavailable *ConnectionTypeUnavailableError
		if !errors.As(err, &unsupported) || errors.As(err, &unavailable) || errors.Is(err, ErrConnectionTypeUnavailable) {
			t.Fatalf("%s: %T %v", code, err, err)
		}
		if unsupported.APIError.Code != code || !reflect.DeepEqual(unsupported.APIError.Known(), []string{"voice_satellite", "connector"}) {
			t.Fatalf("%s: code %q known %v", code, unsupported.APIError.Code, unsupported.APIError.Known())
		}
	}
}

func TestConnectorARefusedConnectorListSaysWhyAndWhatIsKnown(t *testing.T) {
	for _, reason := range []string{"duplicate", "own_kind", "unknown_or_malformed"} {
		api, _ := ccAPI(t, ccProblem(422, "connectors_invalid", map[string]any{"reason": reason, "known": []any{"music_assistant"}}))
		_, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{
			Name: "Bridge", ConnectionType: "connector", Spec: map[string]any{"connectors": []any{"music_assistant", "music_assistant"}}})
		var apiErr *APIError
		if !errors.Is(err, ErrConnectorsInvalid) || !errors.As(err, &apiErr) {
			t.Fatalf("%s: %T %v", reason, err, err)
		}
		if apiErr.Reason() != reason || !reflect.DeepEqual(apiErr.Known(), []string{"music_assistant"}) {
			t.Fatalf("%s: reason %q known %v", reason, apiErr.Reason(), apiErr.Known())
		}
		if errors.Is(err, ErrUnsupportedConnectionType) {
			t.Fatalf("%s: a connector list refusal is not an unsupported kind", reason)
		}
	}
}

func TestConnectorAnOwnerOnlyRefusalNamesTheLink(t *testing.T) {
	for _, code := range []string{
		"device_owner_only_beside_home_link", "setup_session_owner_only_beside_home_link", "device_identity_owner_only_beside_home_link"} {
		api, _ := ccAPI(t, ccProblem(403, code, map[string]any{"link_client_id": "client-9"}))
		_, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "Speaker", OwnerID: "someone"})
		var apiErr *APIError
		if !errors.Is(err, ErrOwnerOnlyBesideLink) || !errors.As(err, &apiErr) {
			t.Fatalf("%s: %T %v", code, err, err)
		}
		if apiErr.LinkClientID() != "client-9" || apiErr.StatusCode != 403 || apiErr.Code != code {
			t.Fatalf("%s: link %q status %d code %q", code, apiErr.LinkClientID(), apiErr.StatusCode, apiErr.Code)
		}
	}
}

func TestConnectorTheOtherNewCodesAreTyped(t *testing.T) {
	cases := []struct {
		status int
		code   string
		extra  map[string]any
		kind   error
	}{
		{409, "hub_scale_out_breaks_links", map[string]any{"links": 2}, ErrHubScaleOutBreaksLinks},
		{409, "idempotency_key_spent", nil, ErrIdempotencyKeySpent},
		{422, "skill_config_private_hub_only", map[string]any{"settings": []any{"url", "token"}}, ErrPrivateHubOnly},
	}
	for _, c := range cases {
		api, _ := ccAPI(t, ccProblem(c.status, c.code, c.extra))
		_, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "X"})
		var apiErr *APIError
		if !errors.Is(err, c.kind) || !errors.As(err, &apiErr) || apiErr.Code != c.code {
			t.Fatalf("%s: %T %v", c.code, err, err)
		}
		for _, other := range []error{ErrConnectorsInvalid, ErrOwnerOnlyBesideLink, ErrConnectionTypeUnavailable} {
			if errors.Is(err, other) {
				t.Fatalf("%s also matches %v", c.code, other)
			}
		}
		if links, ok := apiErr.Links(); c.code == "hub_scale_out_breaks_links" && (!ok || links != 2) {
			t.Fatalf("links %d %v", links, ok)
		}
		if c.code == "skill_config_private_hub_only" && !reflect.DeepEqual(apiErr.Settings(), []string{"url", "token"}) {
			t.Fatalf("settings %v", apiErr.Settings())
		}
	}
}

func TestConnectorTheCodeIsMatchedWithItsStatus(t *testing.T) {
	// The same code under another status is an ordinary error.
	api, _ := ccAPI(t, ccProblem(500, "connectors_invalid", nil))
	_, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "X"})
	if errors.Is(err, ErrConnectorsInvalid) || !errors.Is(err, ErrAPI) {
		t.Fatalf("%v", err)
	}
}

func TestConnectorAStructuredRefusalInsideDetailIsReadToo(t *testing.T) {
	api, _ := ccAPI(t, ccReply{status: 422, body: map[string]any{"detail": map[string]any{
		"code": "connectors_invalid", "reason": "duplicate", "known": []any{"music_assistant"}}}})
	_, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "Bridge", ConnectionType: "connector"})
	var apiErr *APIError
	if !errors.Is(err, ErrConnectorsInvalid) || !errors.As(err, &apiErr) || apiErr.Reason() != "duplicate" {
		t.Fatalf("%T %v", err, err)
	}
}

func TestConnectorAnUnlistedCodeStaysAPlainAPIError(t *testing.T) {
	api, _ := ccAPI(t, ccProblem(409, "something_new", nil))
	_, err := api.CreateClientIdentity(context.Background(), ccHub, BootstrapIdentityOptions{Name: "X"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || err != error(apiErr) {
		t.Fatalf("want a bare *APIError, got %T", err)
	}
	for _, kind := range []error{ErrConnectorsInvalid, ErrOwnerOnlyBesideLink, ErrHubScaleOutBreaksLinks, ErrIdempotencyKeySpent,
		ErrPrivateHubOnly, ErrConnectionTypeUnavailable, ErrUnsupportedConnectionType} {
		if errors.Is(err, kind) {
			t.Fatalf("matches %v", kind)
		}
	}
}
