package thalovant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Chat apps: the owner routes under /v1/bridges.

const (
	fakeBotToken     = "123456:tg-SECRET-fake-bot-token-do-not-log"
	fakeAccessToken  = "syt_matrix-SECRET-fake-access-token"
	fakeRefreshToken = "twitch-SECRET-fake-refresh-token"
	fakeClientSecret = "twitch-SECRET-fake-client-secret"
)

var fakeSecrets = []string{fakeBotToken, fakeAccessToken, fakeRefreshToken, fakeClientSecret}

// bridgeJSON is a bridge as the API answers it, every optional member set.
const bridgeJSON = `{
  "id": "6f1d1a52-6c3b-4d0b-9a57-8f0c9a3f2e11",
  "hub_id": "0b5d7f0e-2a3c-4d6e-8f10-1a2b3c4d5e6f",
  "owner_id": "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d",
  "network": "telegram",
  "connector": {"id": "telegram", "name": "Telegram", "version": "1.4.0"},
  "settings": {"homeserver": "https://matrix.example.org"},
  "secrets_set": ["token"],
  "state": "active",
  "resource_name": "bridge-6f1d1a52",
  "bot": {"user_id": "123456", "username": "myhub_bot", "display_name": "My Hub", "application_id": null, "avatar_url": "https://api.thalovant.com/v1/bridges/6f1d1a52/avatar"},
  "invite_links": {"add_to_group": "https://t.me/myhub_bot?startgroup=thalovant", "direct_chat": "https://t.me/myhub_bot"},
  "allowed": [{"id": "-100123", "title": "Family", "kind": "supergroup", "guild_id": null, "guild_name": null}],
  "require_mention": true,
  "session_scope": "chat",
  "paused": true,
  "hold": {"reason": "plan_limit", "since": "2026-11-02T10:00:00Z"},
  "status": {"phase": "Paused", "ready": false, "reason": "Paused", "message": null, "operator_reason": null, "generation": 4, "observed_generation": 3, "applied": false, "source": "operator"},
  "etag": "e-7",
  "token_updated_at": "2026-11-01T09:00:00Z",
  "activated_at": "2026-11-01T09:05:00Z",
  "created_at": "2026-11-01T09:00:00Z",
  "updated_at": "2026-11-02T10:00:00Z",
  "operation": {"id": "op-1", "kind": "bridge.sync", "aggregate_type": "bridge", "aggregate_id": "6f1d1a52", "status": "requested", "details": {}, "created_at": "2026-11-02T10:00:00Z", "updated_at": "2026-11-02T10:00:00Z", "links": {}}
}`

// minimalBridgeJSON is a bridge with every optional member left out, and
// enum values this SDK has never heard of.
const minimalBridgeJSON = `{
  "id": "b-2", "hub_id": "h-1", "owner_id": "o-1", "network": "matrix",
  "state": "archived", "resource_name": "bridge-b-2",
  "bot": {"user_id": "bridge:b-2", "avatar_url": "/v1/bridges/b-2/avatar"},
  "allowed": [], "require_mention": false, "session_scope": "thread", "paused": false,
  "status": {"phase": "Hibernating", "reason": "QuotaSpent", "source": "satellite"},
  "etag": "e-1", "token_updated_at": "t", "created_at": "c", "updated_at": "u"
}`

const connectorsJSON = `{"data": [
  {"id": "telegram", "name": "Telegram", "mark": "telegram", "status": "offered",
   "description": {"en": "Answer in Telegram.", "fr": "Répondre dans Telegram."},
   "setup_steps": {"en": "Talk to BotFather.", "fr": null},
   "config_schema": {"type": "object"},
   "secrets": [{"key": "token", "label": {"en": "Bot token", "fr": "Jeton"}, "description": null}],
   "targets": {"label": {"en": "Chat id"}, "pattern": "^-?[0-9]{1,20}$", "max": 50},
   "options": {"require_mention_fixed": false},
   "helpers": ["validate", "discover", "test_message", "avatar"],
   "offered": true,
   "version": {"version": "1.4.0", "released_at": "2026-10-30T12:00:00Z"}},
  {"id": "twitch", "name": "Twitch", "mark": "twitch", "status": "preview",
   "description": {"en": "Answer in Twitch chat."},
   "setup_steps": {"en": "Register an application."},
   "config_schema": {"type": "object", "required": ["bot_login", "client_id"], "properties": {"bot_login": {"type": "string", "title": "Bot login", "x-title-fr": "Identifiant du bot"}}},
   "secrets": [
     {"key": "refresh_token", "label": {"en": "Refresh token"}, "required": true},
     {"key": "client_secret", "label": {"en": "Client secret"}, "description": {"en": "From the developer console."}, "required": false}],
   "targets": {"label": {"en": "Channel"}, "pattern": "^[a-z0-9_]{4,25}$", "max": 50, "lowercase": true, "strip_prefix": "#"},
   "options": {"require_mention_fixed": false, "session_scopes": ["chat"]},
   "helpers": ["teleport"],
   "offered": false,
   "version": null}
]}`

const networksJSON = `{
  "networks": [
    {"network": "telegram", "offered": true, "name": "Telegram", "mark": "telegram", "status": "offered"},
    {"network": "discord", "offered": false}
  ],
  "allowance": {"owner_id": "o-1", "plan": "Home", "paid_plan": true, "limit": 2, "used": 1, "remaining": 1, "can_create": true}
}`

// recordedRequest is one request a fake API saw.
type recordedRequest struct {
	Method        string
	Path          string
	Query         string
	IfMatch       string
	HasIfMatch    bool
	Accept        string
	ContentType   string
	Authorization string
	Body          map[string]any
	RawBody       string
}

// fakeBridgeAPI answers every /v1/bridges route the way the API does and
// records what it was sent.
type fakeBridgeAPI struct {
	mu       sync.Mutex
	requests []recordedRequest
}

// handler records each request and answers it by route.
func (f *fakeBridgeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen := recordedRequest{
			Method:        r.Method,
			Path:          r.URL.EscapedPath(),
			Query:         r.URL.RawQuery,
			IfMatch:       r.Header.Get("If-Match"),
			Accept:        r.Header.Get("Accept"),
			ContentType:   r.Header.Get("Content-Type"),
			Authorization: r.Header.Get("Authorization"),
			RawBody:       string(raw),
		}
		_, seen.HasIfMatch = r.Header["If-Match"]
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &seen.Body); err != nil {
				t.Errorf("%s %s: body is not a JSON object: %v", r.Method, r.URL.Path, err)
			}
		}
		f.mu.Lock()
		f.requests = append(f.requests, seen)
		f.mu.Unlock()

		path := r.URL.Path
		switch {
		case path == "/v1/bridges/connectors":
			writeJSON(w, 200, connectorsJSON)
		case path == "/v1/bridges/networks":
			writeJSON(w, 200, networksJSON)
		case path == "/v1/bridges" && r.Method == http.MethodGet:
			writeJSON(w, 200, `{"data": [`+bridgeJSON+`, `+minimalBridgeJSON+`]}`)
		case path == "/v1/bridges" && r.Method == http.MethodPost:
			writeJSON(w, 201, bridgeJSON)
		case strings.HasSuffix(path, "/chats"):
			writeJSON(w, 200, `{"network": "telegram", "data": [
			  {"id": "-100123", "title": "Family", "kind": "supergroup", "allowed": true},
			  {"id": "-100456", "title": "Secret room", "kind": "group", "guild_id": "42", "guild_name": "Home", "guild_icon_url": "https://cdn.example/icon.png", "encrypted": true, "selectable": false}],
			  "note": "Add the bot to a group first."}`)
		case strings.HasSuffix(path, "/test"):
			writeJSON(w, 200, `{"sent": true, "chat_id": "-100123", "language": "fr-fr"}`)
		case strings.HasSuffix(path, "/avatar"):
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nfake-image"))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(w, 200, bridgeJSON)
		}
	}
}

// take returns the requests seen since the last take, and forgets them.
func (f *fakeBridgeAPI) take() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := f.requests
	f.requests = nil
	return seen
}

// writeJSON answers a JSON body with the status given.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// newFakeBridgeAPI is a control plane talking to a fresh fake API.
func newFakeBridgeAPI(t *testing.T) (*ControlPlane, *fakeBridgeAPI) {
	t.Helper()
	fake := &fakeBridgeAPI{}
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	return NewControlPlane(server.URL, "api-token"), fake
}

// only returns the one request a call sent.
func only(t *testing.T, fake *fakeBridgeAPI) recordedRequest {
	t.Helper()
	seen := fake.take()
	if len(seen) != 1 {
		t.Fatalf("want exactly one request, got %d: %+v", len(seen), seen)
	}
	if seen[0].Authorization != "Bearer api-token" {
		t.Fatalf("missing bearer token: %q", seen[0].Authorization)
	}
	return seen[0]
}

// assertRoute fails unless the request went to that method, path and query.
func assertRoute(t *testing.T, seen recordedRequest, method, path, query string) {
	t.Helper()
	if seen.Method != method || seen.Path != path || seen.Query != query {
		t.Fatalf("sent %s %s?%s, want %s %s?%s", seen.Method, seen.Path, seen.Query, method, path, query)
	}
}

// assertBody fails unless the request body is exactly want.
func assertBody(t *testing.T, seen recordedRequest, want map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(seen.Body, want) {
		got, _ := json.Marshal(seen.Body)
		expected, _ := json.Marshal(want)
		t.Fatalf("%s %s body = %s, want %s", seen.Method, seen.Path, got, expected)
	}
}

// Every read route asks the path and query the API serves, and decodes the answer.
func TestChatAppReadRoutesAskWhatTheAPIServes(t *testing.T) {
	cp, fake := newFakeBridgeAPI(t)
	ctx := context.Background()

	connectors, err := cp.ListChatAppConnectors(ctx)
	if err != nil || len(connectors) != 2 {
		t.Fatalf("ListChatAppConnectors = %v, %v", connectors, err)
	}
	assertRoute(t, only(t, fake), "GET", "/v1/bridges/connectors", "")

	wantAllowance := BridgeAllowance{OwnerID: "o-1", Plan: "Home", PaidPlan: true, Limit: 2, Used: 1, Remaining: 1, CanCreate: true}
	for hubID, query := range map[string]string{"h/1": "hub_id=h%2F1", "": ""} {
		networks, err := cp.GetBridgeNetworks(ctx, hubID)
		if err != nil {
			t.Fatal(err)
		}
		assertRoute(t, only(t, fake), "GET", "/v1/bridges/networks", query)
		if networks.Allowance != wantAllowance || len(networks.Networks) != 2 || networks.Networks[0].Network != "telegram" {
			t.Fatalf("GetBridgeNetworks(%q) = %+v", hubID, networks)
		}
	}

	bridges, err := cp.ListBridges(ctx, BridgeListOptions{HubID: "h-1", OwnerID: "o-1", Limit: 500})
	if err != nil || len(bridges) != 2 || bridges[0].Network != "telegram" || bridges[1].Network != "matrix" {
		t.Fatalf("ListBridges = %+v, %v", bridges, err)
	}
	assertRoute(t, only(t, fake), "GET", "/v1/bridges", "hub_id=h-1&limit=500&owner_id=o-1")
	if _, err := cp.ListBridges(ctx, BridgeListOptions{}); err != nil {
		t.Fatal(err)
	}
	assertRoute(t, only(t, fake), "GET", "/v1/bridges", "")

	if _, err := cp.GetBridge(ctx, "b/1"); err != nil {
		t.Fatal(err)
	}
	assertRoute(t, only(t, fake), "GET", "/v1/bridges/b%2F1", "")

	chats, err := cp.ListBridgeChats(ctx, "b/1")
	if err != nil {
		t.Fatal(err)
	}
	assertRoute(t, only(t, fake), "GET", "/v1/bridges/b%2F1/chats", "")
	if chats.Network != "telegram" || len(chats.Data) != 2 || chats.Note != "Add the bot to a group first." || chats.ManualEntry {
		t.Fatalf("chats = %+v", chats)
	}
	family, secret := chats.Data[0], chats.Data[1]
	if !family.Allowed || !family.Selectable || family.Encrypted || family.GuildID != "" {
		t.Fatalf("a chat with the optional members left out: %+v", family)
	}
	if secret.Selectable || !secret.Encrypted || secret.GuildName != "Home" || secret.GuildIconURL != "https://cdn.example/icon.png" {
		t.Fatalf("a chat with every member: %+v", secret)
	}
	if got := secret.AllowedChat(); got != (BridgeAllowedChat{ID: "-100456", Title: "Secret room", Kind: "group", GuildID: "42", GuildName: "Home"}) {
		t.Fatalf("AllowedChat = %+v", got)
	}

	avatar, err := cp.GetBridgeAvatar(ctx, "b/1")
	if err != nil {
		t.Fatal(err)
	}
	seen := only(t, fake)
	assertRoute(t, seen, "GET", "/v1/bridges/b%2F1/avatar", "")
	if seen.Accept != "image/*" {
		t.Fatalf("avatar Accept = %q", seen.Accept)
	}
	if !bytes.Equal(avatar.Content, []byte("\x89PNG\r\n\x1a\nfake-image")) || avatar.ContentType != "image/png" {
		t.Fatalf("avatar = %q %q", avatar.Content, avatar.ContentType)
	}
}

// Every write route sends its method, path and body, and an update only the
// fields it sets, with If-Match only for an etag.
func TestChatAppWriteRoutesSendOnlyWhatWasSet(t *testing.T) {
	cp, fake := newFakeBridgeAPI(t)
	ctx := context.Background()

	created, err := cp.CreateBridge(ctx, BridgeCreate{HubID: "h-1", Connector: "telegram", Token: NewSecretValue(fakeBotToken)})
	if err != nil || created.ID == "" {
		t.Fatalf("CreateBridge = %+v, %v", created, err)
	}
	seen := only(t, fake)
	assertRoute(t, seen, "POST", "/v1/bridges", "")
	assertBody(t, seen, map[string]any{"hub_id": "h-1", "connector": "telegram", "token": fakeBotToken})
	if seen.ContentType != "application/json" {
		t.Fatalf("content type = %q", seen.ContentType)
	}

	_, err = cp.CreateBridge(ctx, BridgeCreate{
		HubID:     "h-1",
		Connector: "twitch",
		Secrets:   map[string]SecretValue{"refresh_token": NewSecretValue(fakeRefreshToken), "client_secret": NewSecretValue(fakeClientSecret)},
		Settings:  map[string]any{"bot_login": "myhubbot", "client_id": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertBody(t, only(t, fake), map[string]any{
		"hub_id":    "h-1",
		"connector": "twitch",
		"secrets":   map[string]any{"refresh_token": fakeRefreshToken, "client_secret": fakeClientSecret},
		"settings":  map[string]any{"bot_login": "myhubbot", "client_id": "abc123"},
	})

	// An update sends the fields it sets and nothing else, and If-Match only
	// with an etag.
	_, err = cp.UpdateBridge(ctx, "b/1", BridgeUpdate{
		Allowed:        []BridgeAllowedChat{{ID: "-100123", Title: "Family", Kind: "supergroup"}, {ID: "555"}},
		RequireMention: new(false),
		SessionScope:   BridgeSessionScopeChat,
		Settings:       map[string]any{"homeserver": "https://matrix.example.org"},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen = only(t, fake)
	assertRoute(t, seen, "PATCH", "/v1/bridges/b%2F1", "")
	if seen.HasIfMatch {
		t.Fatalf("If-Match sent without an etag: %q", seen.IfMatch)
	}
	assertBody(t, seen, map[string]any{
		"allowed": []any{
			map[string]any{"id": "-100123", "title": "Family", "kind": "supergroup"},
			map[string]any{"id": "555"},
		},
		"require_mention": false,
		"session_scope":   "chat",
		"settings":        map[string]any{"homeserver": "https://matrix.example.org"},
	})

	for _, paused := range []bool{true, false} {
		if _, err := cp.UpdateBridge(ctx, "b-1", BridgeUpdate{Paused: new(paused), ETag: "e-7"}); err != nil {
			t.Fatal(err)
		}
		seen = only(t, fake)
		if !seen.HasIfMatch || seen.IfMatch != "e-7" {
			t.Fatalf("If-Match = %q (sent %v)", seen.IfMatch, seen.HasIfMatch)
		}
		assertBody(t, seen, map[string]any{"paused": paused})
	}

	if _, err := cp.ReplaceBridgeToken(ctx, "b/1", NewSecretValue(fakeBotToken)); err != nil {
		t.Fatal(err)
	}
	seen = only(t, fake)
	assertRoute(t, seen, "PUT", "/v1/bridges/b%2F1/token", "")
	assertBody(t, seen, map[string]any{"token": fakeBotToken})

	if _, err := cp.ReplaceBridgeSecrets(ctx, "b/1", map[string]SecretValue{"access_token": NewSecretValue(fakeAccessToken)}); err != nil {
		t.Fatal(err)
	}
	seen = only(t, fake)
	assertRoute(t, seen, "PUT", "/v1/bridges/b%2F1/secrets", "")
	assertBody(t, seen, map[string]any{"secrets": map[string]any{"access_token": fakeAccessToken}})

	result, err := cp.SendBridgeTestMessage(ctx, "b/1", "-100123")
	if err != nil || !result.Sent || result.ChatID != "-100123" || result.Language != "fr-fr" {
		t.Fatalf("SendBridgeTestMessage = %+v, %v", result, err)
	}
	seen = only(t, fake)
	assertRoute(t, seen, "POST", "/v1/bridges/b%2F1/test", "")
	assertBody(t, seen, map[string]any{"chat_id": "-100123"})

	if err := cp.DeleteBridge(ctx, "b/1"); err != nil {
		t.Fatal(err)
	}
	seen = only(t, fake)
	assertRoute(t, seen, "DELETE", "/v1/bridges/b%2F1", "")
	if seen.RawBody != "" {
		t.Fatalf("DELETE sent a body: %q", seen.RawBody)
	}
}

// A bridge with every optional member set decodes all of them.
func TestBridgeReadsEveryMemberTheAPISends(t *testing.T) {
	var bridge Bridge
	if err := json.Unmarshal([]byte(bridgeJSON), &bridge); err != nil {
		t.Fatal(err)
	}
	if bridge.Connector == nil || *bridge.Connector != (BridgeConnectorRef{ID: "telegram", Name: "Telegram", Version: "1.4.0"}) {
		t.Fatalf("connector = %+v", bridge.Connector)
	}
	if bridge.Settings["homeserver"] != "https://matrix.example.org" || !reflect.DeepEqual(bridge.SecretsSet, []string{"token"}) {
		t.Fatalf("settings/secrets_set = %v %v", bridge.Settings, bridge.SecretsSet)
	}
	if bridge.State != BridgeStateActive || bridge.SessionScope != BridgeSessionScopeChat || !bridge.Paused || !bridge.RequireMention {
		t.Fatalf("state/scope/paused/mention = %+v", bridge)
	}
	if bridge.Bot != (BridgeBot{UserID: "123456", Username: "myhub_bot", DisplayName: "My Hub", AvatarURL: "https://api.thalovant.com/v1/bridges/6f1d1a52/avatar"}) {
		t.Fatalf("bot = %+v", bridge.Bot)
	}
	if bridge.InviteLinks == nil || bridge.InviteLinks.AddToGroup != "https://t.me/myhub_bot?startgroup=thalovant" || bridge.InviteLinks.DirectChat != "https://t.me/myhub_bot" {
		t.Fatalf("invite links = %+v", bridge.InviteLinks)
	}
	if len(bridge.Allowed) != 1 || bridge.Allowed[0] != (BridgeAllowedChat{ID: "-100123", Title: "Family", Kind: "supergroup"}) {
		t.Fatalf("allowed = %+v", bridge.Allowed)
	}
	if bridge.Hold == nil || bridge.Hold.Reason != BridgeHoldReasonPlanLimit || bridge.Hold.Since != "2026-11-02T10:00:00Z" {
		t.Fatalf("hold = %+v", bridge.Hold)
	}
	status := bridge.Status
	if status.Phase != BridgePhasePaused || status.Reason != BridgeStatusReasonPaused || status.Source != BridgeStatusSourceOperator || status.Ready {
		t.Fatalf("status = %+v", status)
	}
	if status.Generation == nil || *status.Generation != 4 || status.ObservedGeneration == nil || *status.ObservedGeneration != 3 || status.Applied == nil || *status.Applied {
		t.Fatalf("status generations/applied = %+v", status)
	}
	if bridge.ETag != "e-7" || bridge.ActivatedAt != "2026-11-01T09:05:00Z" || bridge.TokenUpdatedAt == "" || bridge.CreatedAt == "" || bridge.UpdatedAt == "" {
		t.Fatalf("etag/timestamps = %+v", bridge)
	}
	if bridge.Operation == nil || bridge.Operation.ID != "op-1" || bridge.Operation.Status != OperationRequested {
		t.Fatalf("operation = %+v", bridge.Operation)
	}
}

// Enum values added later survive, and missing optionals read as nil or empty.
func TestBridgeKeepsUnknownValuesAndToleratesMissingOptionals(t *testing.T) {
	var bridge Bridge
	if err := json.Unmarshal([]byte(minimalBridgeJSON), &bridge); err != nil {
		t.Fatalf("a bridge with values this SDK does not know was refused: %v", err)
	}
	if bridge.State != "archived" || bridge.SessionScope != "thread" {
		t.Fatalf("unknown state/scope lost: %q %q", bridge.State, bridge.SessionScope)
	}
	if bridge.Status.Phase != "Hibernating" || bridge.Status.Reason != "QuotaSpent" || bridge.Status.Source != "satellite" {
		t.Fatalf("unknown status values lost: %+v", bridge.Status)
	}
	if bridge.Connector != nil || bridge.InviteLinks != nil || bridge.Hold != nil || bridge.Operation != nil {
		t.Fatalf("absent objects are not nil: %+v", bridge)
	}
	if bridge.Status.Generation != nil || bridge.Status.ObservedGeneration != nil || bridge.Status.Applied != nil || bridge.Status.Ready {
		t.Fatalf("absent status members: %+v", bridge.Status)
	}
	if bridge.Settings != nil || bridge.SecretsSet != nil || bridge.ActivatedAt != "" || bridge.Bot.Username != "" {
		t.Fatalf("absent members are not empty: %+v", bridge)
	}

	var held Bridge
	if err := json.Unmarshal([]byte(strings.Replace(minimalBridgeJSON, `"paused": false`, `"paused": true, "hold": {"reason": "billing_review", "since": "s"}`, 1)), &held); err != nil {
		t.Fatal(err)
	}
	if held.Hold == nil || held.Hold.Reason != "billing_review" {
		t.Fatalf("unknown hold reason lost: %+v", held.Hold)
	}
}

// Connectors read the API's defaults and keep statuses and helpers added later.
func TestChatAppConnectorsReadDefaultsAndUnknownValues(t *testing.T) {
	var envelope struct {
		Data []ChatAppConnector `json:"data"`
	}
	if err := json.Unmarshal([]byte(connectorsJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	telegram, twitch := envelope.Data[0], envelope.Data[1]

	if telegram.Status != ChatAppStatusOffered || !telegram.Offered || telegram.Mark != "telegram" {
		t.Fatalf("telegram = %+v", telegram)
	}
	if telegram.Description != (LocalizedText{EN: "Answer in Telegram.", FR: "Répondre dans Telegram."}) || telegram.SetupSteps.FR != "" {
		t.Fatalf("localized text = %+v %+v", telegram.Description, telegram.SetupSteps)
	}
	if len(telegram.Secrets) != 1 || !telegram.Secrets[0].Required || telegram.Secrets[0].Description != nil || telegram.Secrets[0].Label.FR != "Jeton" {
		t.Fatalf("a secret field leaving required out is required: %+v", telegram.Secrets)
	}
	if !reflect.DeepEqual(telegram.Options.SessionScopes, []string{"person", "chat"}) || telegram.Options.RequireMentionFixed {
		t.Fatalf("options default = %+v", telegram.Options)
	}
	if !reflect.DeepEqual(telegram.Helpers, []ChatAppHelper{ChatAppHelperValidate, ChatAppHelperDiscover, ChatAppHelperTestMessage, ChatAppHelperAvatar}) {
		t.Fatalf("helpers = %v", telegram.Helpers)
	}
	if telegram.Targets.Max != 50 || telegram.Targets.Lowercase || telegram.Targets.StripPrefix != "" {
		t.Fatalf("targets = %+v", telegram.Targets)
	}
	if telegram.Version == nil || *telegram.Version != (ChatAppRelease{Version: "1.4.0", ReleasedAt: "2026-10-30T12:00:00Z"}) {
		t.Fatalf("version = %+v", telegram.Version)
	}

	if twitch.Status != "preview" || twitch.Offered || twitch.Version != nil {
		t.Fatalf("an unknown status is kept, a null version is nil: %+v", twitch)
	}
	if !reflect.DeepEqual(twitch.Helpers, []ChatAppHelper{"teleport"}) {
		t.Fatalf("an unknown helper is kept: %v", twitch.Helpers)
	}
	if twitch.Secrets[0].Key != "refresh_token" || !twitch.Secrets[0].Required || twitch.Secrets[1].Required || twitch.Secrets[1].Description.EN != "From the developer console." {
		t.Fatalf("secrets = %+v", twitch.Secrets)
	}
	if !twitch.Targets.Lowercase || twitch.Targets.StripPrefix != "#" || !reflect.DeepEqual(twitch.Options.SessionScopes, []string{"chat"}) {
		t.Fatalf("targets/options = %+v %+v", twitch.Targets, twitch.Options)
	}
	properties, _ := twitch.ConfigSchema["properties"].(map[string]any)
	login, _ := properties["bot_login"].(map[string]any)
	if login["x-title-fr"] != "Identifiant du bot" {
		t.Fatalf("config schema = %v", twitch.ConfigSchema)
	}
}

// Networks, test results and chat lists read their defaults.
func TestBridgeNetworksAndSmallResultsReadTheirDefaults(t *testing.T) {
	var networks BridgeNetworks
	if err := json.Unmarshal([]byte(networksJSON), &networks); err != nil {
		t.Fatal(err)
	}
	if len(networks.Networks) != 2 || networks.Networks[0] != (BridgeNetworkOffer{Network: "telegram", Offered: true, Name: "Telegram", Mark: "telegram", Status: ChatAppStatusOffered}) {
		t.Fatalf("networks = %+v", networks.Networks)
	}
	if networks.Networks[1] != (BridgeNetworkOffer{Network: "discord"}) {
		t.Fatalf("an offer with only network and offered = %+v", networks.Networks[1])
	}
	if networks.Allowance != (BridgeAllowance{OwnerID: "o-1", Plan: "Home", PaidPlan: true, Limit: 2, Used: 1, Remaining: 1, CanCreate: true}) {
		t.Fatalf("allowance = %+v", networks.Allowance)
	}

	var result BridgeTestResult
	if err := json.Unmarshal([]byte(`{"sent": true, "chat_id": "1"}`), &result); err != nil || result.Language != "en" {
		t.Fatalf("a test result without language = %+v, %v", result, err)
	}
	var chats BridgeChatList
	if err := json.Unmarshal([]byte(`{"network": "matrix", "data": [], "manual_entry": true}`), &chats); err != nil || !chats.ManualEntry || chats.Note != "" || len(chats.Data) != 0 {
		t.Fatalf("a manual-entry chat list = %+v, %v", chats, err)
	}
}

// Input the API would refuse is refused locally, with nothing sent.
func TestChatAppCallsRefuseBadInputWithoutSendingIt(t *testing.T) {
	cp, fake := newFakeBridgeAPI(t)
	ctx := context.Background()
	token := NewSecretValue(fakeBotToken)
	secrets := map[string]SecretValue{"access_token": NewSecretValue(fakeAccessToken)}

	refused := map[string]error{}
	_, refused["both token and secrets"] = cp.CreateBridge(ctx, BridgeCreate{HubID: "h", Connector: "matrix", Token: token, Secrets: secrets})
	_, refused["neither token nor secrets"] = cp.CreateBridge(ctx, BridgeCreate{HubID: "h", Connector: "telegram"})
	_, refused["an empty token"] = cp.CreateBridge(ctx, BridgeCreate{HubID: "h", Connector: "telegram", Token: NewSecretValue("")})
	_, refused["no hub"] = cp.CreateBridge(ctx, BridgeCreate{Connector: "telegram", Token: token})
	_, refused["no connector"] = cp.CreateBridge(ctx, BridgeCreate{HubID: "h", Token: token})
	_, refused["limit 501"] = cp.ListBridges(ctx, BridgeListOptions{Limit: 501})
	_, refused["limit -1"] = cp.ListBridges(ctx, BridgeListOptions{Limit: -1})
	_, refused["a blank bridge id"] = cp.GetBridge(ctx, " ")
	_, refused["a blank etag"] = cp.UpdateBridge(ctx, "b", BridgeUpdate{Paused: new(true), ETag: "  "})
	_, refused["no token"] = cp.ReplaceBridgeToken(ctx, "b", SecretValue{})
	_, refused["no secrets"] = cp.ReplaceBridgeSecrets(ctx, "b", nil)
	refused["delete without an id"] = cp.DeleteBridge(ctx, "")
	_, refused["avatar without an id"] = cp.GetBridgeAvatar(ctx, "")

	for name, err := range refused {
		if !errors.Is(err, ErrAPI) {
			t.Errorf("%s: want ErrAPI, got %v", name, err)
			continue
		}
		for _, secret := range fakeSecrets {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error names a secret: %v", name, err)
			}
		}
	}
	if seen := fake.take(); len(seen) != 0 {
		t.Fatalf("refused input reached the API: %+v", seen)
	}
}

// refusingAPI answers every request with one Problem+JSON refusal.
func refusingAPI(t *testing.T, status int, body string, header map[string]string) *ControlPlane {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		for key, value := range header {
			w.Header().Set(key, value)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return NewControlPlane(server.URL, "api-token")
}

// Refusals carry their code and fields and keep the plan, auth and retry rules.
func TestChatAppRefusalsCarryCodeFieldsAndTheUsualKinds(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		status     int
		body       string
		header     map[string]string
		code       string
		fields     []string
		is         error
		retryAfter time.Duration
	}{
		{
			name:   "settings refused, fields at the top",
			status: 422,
			body:   `{"type":"about:blank","title":"HTTPException","status":422,"detail":"These settings are not valid for Matrix.","code":"bridge_settings_invalid","fields":["homeserver","user_id"]}`,
			code:   "bridge_settings_invalid",
			fields: []string{"homeserver", "user_id"},
		},
		{
			name:   "secrets refused, fields inside an object detail, non-strings dropped",
			status: 422,
			body:   `{"detail":{"detail":"Missing secrets.","code":"bridge_secrets_invalid","fields":["client_secret",7,null,{"x":1},"refresh_token"]}}`,
			code:   "bridge_secrets_invalid",
			fields: []string{"client_secret", "refresh_token"},
		},
		{
			name:   "an address outside the rules",
			status: 422,
			body:   `{"detail":"The address must be public.","code":"bridge_url_not_public","fields":["homeserver"]}`,
			code:   "bridge_url_not_public",
			fields: []string{"homeserver"},
		},
		{
			name:   "a refusal with no fields",
			status: 409,
			body:   `{"detail":"This bot already serves a bridge.","code":"bridge_bot_in_use"}`,
			code:   "bridge_bot_in_use",
		},
		{
			name:   "fields that are not a list",
			status: 422,
			body:   `{"detail":"Bad.","code":"bridge_settings_invalid","fields":"homeserver"}`,
			code:   "bridge_settings_invalid",
		},
		{
			name:   "the plan's allowance is full",
			status: 403,
			body:   `{"detail":"Your plan allows 2 chat bridges.","code":"plan_limit","resource":"chat_bridge","limit":2,"used":2,"plan":"Home"}`,
			code:   "plan_limit",
			is:     ErrPlan,
		},
		{
			name:   "a Free plan",
			status: 402,
			body:   `{"detail":"Chat bridges need a paid plan."}`,
			is:     ErrPlan,
		},
		{
			name:   "a token without the scope",
			status: 403,
			body:   `{"detail":"Insufficient scopes"}`,
			is:     ErrAuth,
		},
		{
			name:   "a revoked token",
			status: 401,
			body:   `{"detail":"Not authenticated"}`,
			is:     ErrAuth,
		},
		{
			name:       "the per-bridge limit",
			status:     429,
			body:       `{"detail":"Too many test messages.","code":"bridge_rate_limited"}`,
			header:     map[string]string{"Retry-After": "17"},
			code:       "bridge_rate_limited",
			retryAfter: 17 * time.Second,
		},
		{
			name:   "a stale etag",
			status: 412,
			body:   `{"detail":"ETag mismatch"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := refusingAPI(t, tc.status, tc.body, tc.header)
			_, err := cp.UpdateBridge(ctx, "b-1", BridgeUpdate{Paused: new(false), ETag: "e-1"})
			apiErr := refusedAPIError(t, err)
			if apiErr.StatusCode != tc.status || apiErr.Code != tc.code {
				t.Fatalf("status/code = %d %q, want %d %q", apiErr.StatusCode, apiErr.Code, tc.status, tc.code)
			}
			if !reflect.DeepEqual(apiErr.Fields, tc.fields) {
				t.Fatalf("Fields = %#v, want %#v", apiErr.Fields, tc.fields)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("want errors.Is %v: %v", tc.is, err)
			}
			for _, other := range []error{ErrPlan, ErrAuth} {
				if other != tc.is && errors.Is(err, other) {
					t.Fatalf("matched %v as well: %v", other, err)
				}
			}
			if apiErr.RetryAfter != tc.retryAfter {
				t.Fatalf("RetryAfter = %v, want %v", apiErr.RetryAfter, tc.retryAfter)
			}
		})
	}

	// Every typed call answers the same *APIError, the avatar's 404 included.
	cp := refusingAPI(t, 404, `{"detail":"This bot has no picture"}`, nil)
	calls := map[string]func() error{
		"ListChatAppConnectors": func() error { _, err := cp.ListChatAppConnectors(ctx); return err },
		"GetBridgeNetworks":     func() error { _, err := cp.GetBridgeNetworks(ctx, "h"); return err },
		"ListBridges":           func() error { _, err := cp.ListBridges(ctx, BridgeListOptions{}); return err },
		"CreateBridge": func() error {
			_, err := cp.CreateBridge(ctx, BridgeCreate{HubID: "h", Connector: "telegram", Token: NewSecretValue(fakeBotToken)})
			return err
		},
		"GetBridge":             func() error { _, err := cp.GetBridge(ctx, "b"); return err },
		"ReplaceBridgeToken":    func() error { _, err := cp.ReplaceBridgeToken(ctx, "b", NewSecretValue(fakeBotToken)); return err },
		"ListBridgeChats":       func() error { _, err := cp.ListBridgeChats(ctx, "b"); return err },
		"SendBridgeTestMessage": func() error { _, err := cp.SendBridgeTestMessage(ctx, "b", "1"); return err },
		"GetBridgeAvatar":       func() error { _, err := cp.GetBridgeAvatar(ctx, "b"); return err },
		"DeleteBridge":          func() error { return cp.DeleteBridge(ctx, "b") },
		"ReplaceBridgeSecrets": func() error {
			_, err := cp.ReplaceBridgeSecrets(ctx, "b", map[string]SecretValue{"token": NewSecretValue(fakeBotToken)})
			return err
		},
	}
	for name, call := range calls {
		apiErr := refusedAPIError(t, call())
		if apiErr.StatusCode != 404 || apiErr.ProblemDetail != "This bot has no picture" {
			t.Errorf("%s: %#v", name, apiErr)
		}
	}
}

// A 2xx body that is not JSON is an *APIError, not a zero bridge.
func TestChatAppAnswerThatIsNotJSONIsAnAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer server.Close()
	_, err := NewControlPlane(server.URL, "api-token").GetBridge(context.Background(), "b")
	if apiErr := refusedAPIError(t, err); apiErr.StatusCode != 200 || apiErr.Detail != "invalid JSON response" {
		t.Fatalf("%#v", apiErr)
	}
}

// Fields are read like Code: top level first, then an object detail; strings only.
func TestProblemFieldNamesFollowTheCodeRules(t *testing.T) {
	cases := []struct {
		name    string
		problem map[string]any
		want    []string
	}{
		{"nothing", nil, nil},
		{"no fields", map[string]any{"code": "x"}, nil},
		{"top level", map[string]any{"fields": []any{"a", "b"}}, []string{"a", "b"}},
		{"top level wins over the envelope", map[string]any{"fields": []any{"outer"}, "detail": map[string]any{"fields": []any{"inner"}}}, []string{"outer"}},
		{"the envelope when the top has none", map[string]any{"detail": map[string]any{"fields": []any{"inner"}}}, []string{"inner"}},
		{"the envelope when the top has no strings", map[string]any{"fields": []any{1.0}, "detail": map[string]any{"fields": []any{"inner"}}}, []string{"inner"}},
		{"strings only", map[string]any{"fields": []any{"a", 2.0, true, nil, []any{"b"}, "c"}}, []string{"a", "c"}},
		{"a detail that is a sentence", map[string]any{"detail": "no fields here"}, nil},
	}
	for _, tc := range cases {
		if got := problemFieldNames(tc.problem); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.name, got, tc.want)
		}
	}
	// An APIError built by hand still reads as it always did.
	if apiErr := (&APIError{StatusCode: 422, Detail: "x"}); apiErr.Fields != nil {
		t.Fatal("Fields on a literal")
	}
}

// A SecretValue prints the marker through fmt, JSON, text and slog, nested or not.
func TestSecretValuePrintsTheMarkerEverywhere(t *testing.T) {
	secret := NewSecretValue(fakeBotToken)
	if secret.Reveal() != fakeBotToken || secret.IsZero() {
		t.Fatal("Reveal lost the value")
	}
	if !(SecretValue{}).IsZero() || (SecretValue{}).Reveal() != "" || !NewSecretValue("").IsZero() {
		t.Fatal("an empty SecretValue")
	}

	type caller struct {
		Token  SecretValue
		hidden SecretValue
	}
	holder := caller{Token: secret, hidden: secret}
	create := BridgeCreate{HubID: "h", Connector: "twitch", Token: secret, Secrets: map[string]SecretValue{"client_secret": NewSecretValue(fakeClientSecret)}}
	values := []any{secret, &secret, holder, &holder, create, &create, []SecretValue{secret}, map[string]SecretValue{"token": secret}}

	var printed []string
	for _, value := range values {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T", "%p", "%10.3s", "%-8v"} {
			printed = append(printed, fmt.Sprintf(format, value))
		}
		printed = append(printed, fmt.Sprint(value), fmt.Sprintln(value))
	}
	// fmt answers %T and %p itself, without Format; neither shows the value.
	if fmt.Sprintf("%T", secret) != "thalovant.SecretValue" || !strings.HasPrefix(fmt.Sprintf("%p", secret), "%!p(") {
		t.Fatalf("%%T and %%p: %T %p", secret, secret)
	}
	if secret.String() != "[REDACTED]" || secret.GoString() != "[REDACTED]" || fmt.Sprintf("%v", secret) != "[REDACTED]" || fmt.Sprintf("%q", secret) != `"[REDACTED]"` {
		t.Fatalf("the marker: %s %s %v %q", secret.String(), secret.GoString(), secret, secret)
	}

	for _, value := range []any{secret, holder, create, map[string]SecretValue{"token": secret}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		printed = append(printed, string(raw))
	}
	if raw, _ := json.Marshal(secret); string(raw) != `"[REDACTED]"` {
		t.Fatalf("json.Marshal = %s", raw)
	}
	text, _ := secret.MarshalText()
	printed = append(printed, string(text))
	if string(text) != "[REDACTED]" {
		t.Fatalf("MarshalText = %s", text)
	}

	var logs bytes.Buffer
	for _, handler := range []slog.Handler{slog.NewTextHandler(&logs, nil), slog.NewJSONHandler(&logs, nil)} {
		logger := slog.New(handler)
		logger.Info("creating a bridge", "token", secret, slog.Any("create", create), slog.Group("g", "secret", secret))
		logger.Info("holder", "holder", holder)
	}
	printed = append(printed, logs.String())
	if !strings.Contains(logs.String(), "[REDACTED]") {
		t.Fatalf("slog lost the marker: %s", logs.String())
	}

	for _, output := range printed {
		for _, leak := range fakeSecrets {
			if strings.Contains(output, leak) {
				t.Fatalf("printed a secret: %s", output)
			}
		}
	}
}

// Secrets reach the request body and never the error a refusal returns.
func TestChatAppSecretsReachOnlyTheRequestBody(t *testing.T) {
	ctx := context.Background()
	// The API refuses, and the refusal is printed every way a caller might.
	var (
		mu     sync.Mutex
		bodies []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"detail":"Telegram refused this token.","code":"bridge_token_invalid"}`))
	}))
	defer server.Close()
	cp := NewControlPlane(server.URL, "api-token")

	var errs []error
	_, err := cp.CreateBridge(ctx, BridgeCreate{HubID: "h", Connector: "telegram", Token: NewSecretValue(fakeBotToken)})
	errs = append(errs, err)
	_, err = cp.CreateBridge(ctx, BridgeCreate{HubID: "h", Connector: "twitch", Secrets: map[string]SecretValue{"refresh_token": NewSecretValue(fakeRefreshToken), "client_secret": NewSecretValue(fakeClientSecret)}})
	errs = append(errs, err)
	_, err = cp.ReplaceBridgeToken(ctx, "b", NewSecretValue(fakeBotToken))
	errs = append(errs, err)
	_, err = cp.ReplaceBridgeSecrets(ctx, "b", map[string]SecretValue{"access_token": NewSecretValue(fakeAccessToken)})
	errs = append(errs, err)

	for _, err := range errs {
		if refusedAPIError(t, err).Code != "bridge_token_invalid" {
			t.Fatalf("code lost: %#v", err)
		}
		forms := displayForms(err)
		forms["%#v"] = fmt.Sprintf("%#v", err)
		for form, output := range forms {
			for _, leak := range fakeSecrets {
				if strings.Contains(output, leak) {
					t.Fatalf("%s names a secret: %s", form, output)
				}
			}
		}
	}
	// What did reach the API was the value itself, not the marker.
	mu.Lock()
	joined := strings.Join(bodies, "\n")
	mu.Unlock()
	for _, secret := range fakeSecrets {
		if !strings.Contains(joined, secret) {
			t.Fatalf("the request bodies lack a secret they should carry: %s", strings.ReplaceAll(joined, secret, "?"))
		}
	}
	if strings.Contains(joined, "[REDACTED]") {
		t.Fatal("a request body carries the marker instead of the value")
	}
}

// leakyTransport fails the way a careless transport might: with the request
// body in its error.
type leakyTransport struct{}

// RoundTrip fails with the request body in its message.
func (leakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	return nil, fmt.Errorf("dial failed while sending %s to %s", raw, r.URL)
}

// A transport failure that quotes the body still names no secret.
func TestChatAppTransportFailureNamesNoSecret(t *testing.T) {
	cp := NewControlPlane("https://api.example.com", "api-token")
	cp.HTTPClient = &http.Client{Transport: leakyTransport{}}
	_, err := cp.CreateBridge(context.Background(), BridgeCreate{HubID: "h", Connector: "telegram", Token: NewSecretValue(fakeBotToken)})
	if !errors.Is(err, ErrAPIUnreachable) {
		t.Fatalf("want ErrAPIUnreachable, got %v", err)
	}
	for form, output := range displayForms(err) {
		if strings.Contains(output, fakeBotToken) {
			t.Fatalf("%s names the token: %s", form, output)
		}
	}
}

// rawAnswer answers every request with the bytes given, as written, and then
// closes the connection: a body shorter than its Content-Length is cut short.
func rawAnswer(t *testing.T, response string) *ControlPlane {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buffered.WriteString(response)
		_ = buffered.Flush()
	}))
	t.Cleanup(server.Close)
	return NewControlPlane(server.URL, "api-token")
}

// A body shorter than its Content-Length is an error, not partial bytes.
func TestChatAppBodyCutShortIsNotAnAnswer(t *testing.T) {
	ctx := context.Background()
	cut := "HTTP/1.1 200 OK\r\nContent-Type: image/png\r\nContent-Length: 100\r\n\r\n\x89PNG-partial"
	if _, err := rawAnswer(t, cut).GetBridgeAvatar(ctx, "b"); err == nil {
		t.Fatal("a picture cut short was returned as complete")
	} else if apiErr := refusedAPIError(t, err); apiErr.StatusCode != 200 || apiErr.Detail != "incomplete response body" {
		t.Fatalf("%#v", apiErr)
	}
	cutJSON := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 500\r\n\r\n" + bridgeJSON[:200]
	if _, err := rawAnswer(t, cutJSON).GetBridge(ctx, "b"); err == nil {
		t.Fatal("a bridge cut short was returned")
	} else if apiErr := refusedAPIError(t, err); apiErr.Detail != "incomplete response body" {
		t.Fatalf("%#v", apiErr)
	}
}

// The avatar read stops at its cap and refuses a larger body.
func TestChatAppAvatarReadIsBounded(t *testing.T) {
	ctx := context.Background()
	for _, size := range []int{maxBridgeAvatarBytes, maxBridgeAvatarBytes + 1} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(bytes.Repeat([]byte{0xff}, size))
		}))
		avatar, err := NewControlPlane(server.URL, "api-token").GetBridgeAvatar(ctx, "b")
		server.Close()
		if size <= maxBridgeAvatarBytes {
			if err != nil || len(avatar.Content) != size || avatar.ContentType != "image/jpeg" {
				t.Fatalf("a picture at the cap: %d bytes, %v", len(avatar.Content), err)
			}
			continue
		}
		if apiErr := refusedAPIError(t, err); apiErr.Detail != "response body too large" || avatar.Content != nil {
			t.Fatalf("a picture over the cap: %#v, %d bytes", apiErr, len(avatar.Content))
		}
	}
}

// The avatar read refuses an oversized error body as well as an oversized
// picture: the cap applies before the status is read.
func TestChatAppAvatarErrorBodyIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, maxBridgeAvatarBytes+1))
	}))
	defer server.Close()
	avatar, err := NewControlPlane(server.URL, "api-token").GetBridgeAvatar(context.Background(), "b")
	if apiErr := refusedAPIError(t, err); apiErr.StatusCode != http.StatusInternalServerError || apiErr.Detail != "response body too large" || avatar.Content != nil {
		t.Fatalf("an oversized error body: %#v, %d bytes", apiErr, len(avatar.Content))
	}
}
