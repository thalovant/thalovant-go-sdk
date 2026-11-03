package thalovant

// Chat apps: a hub owner connects a chat network (Telegram, Discord; Matrix,
// Mattermost and Twitch in beta) to their hub, and the hub answers there. The
// Thalovant API runs the bridge; these are the owner routes under /v1/bridges.
//
// Reads need a token with hubs:read, writes hubs:write. A refusal is an
// *APIError whose Code names it (bridge_token_invalid, bridge_settings_invalid,
// plan_limit, ...) and whose Fields names the settings or secrets a refusal is
// about, never their values.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SecretValue holds a credential the SDK sends and never shows: a bot token, an
// access token, a client secret. Printing it with fmt, encoding it with
// encoding/json or as text, and logging it with log/slog all give the fixed
// marker "[REDACTED]"; the exceptions are %T, which prints the type name, and
// %p, which fmt answers itself without the value. Reveal is the one way to
// read the value, and the SDK calls it only to build a request body.
//
//	token := thalovant.NewSecretValue(os.Getenv("TELEGRAM_BOT_TOKEN"))
//	fmt.Println(token) // [REDACTED]
//
// The value sits behind a pointer, so even a SecretValue held in an unexported
// field of another struct, where fmt cannot call its methods, prints as an
// address rather than as the secret. Two SecretValues made from the same text
// are therefore not ==.
type SecretValue struct {
	value *string
}

// NewSecretValue wraps a credential.
func NewSecretValue(value string) SecretValue {
	return SecretValue{value: &value}
}

// Reveal returns the credential itself. Use it only where the value has to
// leave the program, as the SDK does when it builds a request body.
func (s SecretValue) Reveal() string {
	if s.value == nil {
		return ""
	}
	return *s.value
}

// IsZero reports whether the SecretValue holds nothing: the zero value, or one
// made from "".
func (s SecretValue) IsZero() bool { return s.Reveal() == "" }

// String returns the redaction marker.
func (s SecretValue) String() string { return secretPlaceholder }

// GoString returns the redaction marker, so %#v never prints the value.
func (s SecretValue) GoString() string { return secretPlaceholder }

// Format writes the redaction marker for every verb fmt hands it (quoted for
// %q), so no verb or flag prints the value. fmt answers %T (the type name) and
// %p itself, without calling Format; neither prints the value.
func (s SecretValue) Format(state fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(state, strconv.Quote(secretPlaceholder))
		return
	}
	_, _ = io.WriteString(state, secretPlaceholder)
}

// MarshalJSON encodes the redaction marker, never the value.
func (s SecretValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(secretPlaceholder)
}

// MarshalText encodes the redaction marker, never the value.
func (s SecretValue) MarshalText() ([]byte, error) {
	return []byte(secretPlaceholder), nil
}

// LogValue makes log/slog record the redaction marker, never the value.
func (s SecretValue) LogValue() slog.Value {
	return slog.StringValue(secretPlaceholder)
}

// ChatAppStatus is how a connector is offered. A value the API adds later is
// kept as sent.
type ChatAppStatus string

const (
	ChatAppStatusOffered ChatAppStatus = "offered"
	ChatAppStatusBeta    ChatAppStatus = "beta"
	ChatAppStatusHidden  ChatAppStatus = "hidden"
)

// ChatAppHelper is a step the API can run for a connector itself. A connector
// without "discover" lists no chats (the owner types the ids), one without
// "test_message" refuses a test message (409 bridge_helper_unavailable), and
// one without "avatar" has no picture (404).
type ChatAppHelper string

const (
	ChatAppHelperValidate    ChatAppHelper = "validate"
	ChatAppHelperDiscover    ChatAppHelper = "discover"
	ChatAppHelperTestMessage ChatAppHelper = "test_message"
	ChatAppHelperAvatar      ChatAppHelper = "avatar"
)

// BridgeState is where a bridge is in its life: "setup" until chats are
// chosen, then "active".
type BridgeState string

const (
	BridgeStateSetup  BridgeState = "setup"
	BridgeStateActive BridgeState = "active"
)

// BridgeSessionScope is whose conversation a message continues: each person's
// own ("person") or the whole chat's ("chat").
type BridgeSessionScope string

const (
	BridgeSessionScopePerson BridgeSessionScope = "person"
	BridgeSessionScopeChat   BridgeSessionScope = "chat"
)

// BridgePhase is the bridge's phase as the platform reports it. Branch on
// BridgeStatus.Reason instead; the phase is for display and diagnosis.
type BridgePhase string

const (
	BridgePhaseSetup    BridgePhase = "Setup"
	BridgePhasePending  BridgePhase = "Pending"
	BridgePhaseReady    BridgePhase = "Ready"
	BridgePhaseDegraded BridgePhase = "Degraded"
	BridgePhasePaused   BridgePhase = "Paused"
	BridgePhaseError    BridgePhase = "Error"
)

// BridgeStatusReason is the stable, customer-safe word for a bridge's state.
// A reason the API adds later is kept as sent; treat an unknown one like
// BridgeStatusReasonStarting.
type BridgeStatusReason string

const (
	// BridgeStatusReasonSetup: no chats chosen yet.
	BridgeStatusReasonSetup BridgeStatusReason = "Setup"
	// BridgeStatusReasonStarting: rolling out, or not reported yet.
	BridgeStatusReasonStarting BridgeStatusReason = "Starting"
	// BridgeStatusReasonConnected: running and answering.
	BridgeStatusReasonConnected BridgeStatusReason = "Connected"
	// BridgeStatusReasonPaused: paused by the owner, or held by the platform
	// (see Bridge.Hold).
	BridgeStatusReasonPaused BridgeStatusReason = "Paused"
	// BridgeStatusReasonUnavailable: started but not running, or a
	// platform-side error.
	BridgeStatusReasonUnavailable BridgeStatusReason = "Unavailable"
	// BridgeStatusReasonTokenMissing: the stored credential has not reached
	// the bridge yet.
	BridgeStatusReasonTokenMissing BridgeStatusReason = "TokenMissing"
	// BridgeStatusReasonTokenRejected: the network refused the bridge's
	// credentials; replace them (ReplaceBridgeToken, ReplaceBridgeSecrets).
	BridgeStatusReasonTokenRejected BridgeStatusReason = "TokenRejected"
)

// BridgeStatusSource is where a status came from.
type BridgeStatusSource string

const (
	// BridgeStatusSourceLifecycle: the bridge's own lifecycle (setup).
	BridgeStatusSourceLifecycle BridgeStatusSource = "lifecycle"
	// BridgeStatusSourceOperator: the platform's runtime report.
	BridgeStatusSourceOperator BridgeStatusSource = "operator"
	// BridgeStatusSourceWatcher: the platform's latest recorded conditions.
	BridgeStatusSourceWatcher BridgeStatusSource = "watcher"
	// BridgeStatusSourceUnavailable: nothing has reported yet.
	BridgeStatusSourceUnavailable BridgeStatusSource = "unavailable"
)

// BridgeHoldReason is why the platform paused a bridge.
type BridgeHoldReason string

const (
	// BridgeHoldReasonPlanLimit: the owner's plan allows fewer running
	// bridges. It lifts on its own when the plan has room again, or resume
	// it (UpdateBridge with Paused false) once it does.
	BridgeHoldReasonPlanLimit BridgeHoldReason = "plan_limit"
	// BridgeHoldReasonAccountSuspended: the account was deactivated; the
	// bridge resumes when it is reactivated.
	BridgeHoldReasonAccountSuspended BridgeHoldReason = "account_suspended"
)

// LocalizedText is copy in English, and in French when there is a French
// version (FR is "" otherwise).
type LocalizedText struct {
	EN string `json:"en"`
	FR string `json:"fr"`
}

// ChatAppSecretField is one secret a connector needs, by key.
type ChatAppSecretField struct {
	Key         string         `json:"key"`
	Label       LocalizedText  `json:"label"`
	Description *LocalizedText `json:"description"`
	// Required is true unless the API says otherwise.
	Required bool `json:"required"`
}

// UnmarshalJSON reads a secret field, Required true when the API leaves it out.
func (f *ChatAppSecretField) UnmarshalJSON(data []byte) error {
	type plain ChatAppSecretField
	decoded := plain{Required: true}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*f = ChatAppSecretField(decoded)
	return nil
}

// ChatAppTargets says which chat, channel or room ids a connector takes. An id
// an owner types is matched against Pattern after StripPrefix is dropped and,
// when Lowercase, after folding (Twitch: "#Name" is stored as "name").
type ChatAppTargets struct {
	Label       LocalizedText `json:"label"`
	Pattern     string        `json:"pattern"`
	Max         int           `json:"max"`
	Lowercase   bool          `json:"lowercase"`
	StripPrefix string        `json:"strip_prefix"`
}

// ChatAppOptions are the choices a connector offers its bridges.
type ChatAppOptions struct {
	// RequireMentionFixed is true when the network only lets the bot read
	// what mentions it (Discord): require_mention cannot be turned off.
	RequireMentionFixed bool `json:"require_mention_fixed"`
	// SessionScopes lists the session scopes the connector takes; person and
	// chat when the API leaves it out.
	SessionScopes []string `json:"session_scopes"`
}

// UnmarshalJSON reads the options, SessionScopes person and chat when the API
// sends none.
func (o *ChatAppOptions) UnmarshalJSON(data []byte) error {
	type plain ChatAppOptions
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.SessionScopes == nil {
		decoded.SessionScopes = []string{string(BridgeSessionScopePerson), string(BridgeSessionScopeChat)}
	}
	*o = ChatAppOptions(decoded)
	return nil
}

// ChatAppRelease is the connector release new bridges run.
type ChatAppRelease struct {
	Version    string `json:"version"`
	ReleasedAt string `json:"released_at"`
}

// ChatAppConnector is a chat network a hub can be connected to, as
// ListChatAppConnectors describes it: what to show, which secrets and settings
// it takes, and which helpers the API runs for it.
type ChatAppConnector struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Mark        string        `json:"mark"`
	Status      ChatAppStatus `json:"status"`
	Description LocalizedText `json:"description"`
	SetupSteps  LocalizedText `json:"setup_steps"`
	// ConfigSchema is the JSON Schema of the connector's settings; each
	// property's title is English and its x-title-fr French.
	ConfigSchema map[string]any       `json:"config_schema"`
	Secrets      []ChatAppSecretField `json:"secrets"`
	Targets      ChatAppTargets       `json:"targets"`
	Options      ChatAppOptions       `json:"options"`
	Helpers      []ChatAppHelper      `json:"helpers"`
	// Offered is whether a bridge can be created for it now.
	Offered bool            `json:"offered"`
	Version *ChatAppRelease `json:"version"`
}

// BridgeNetworkOffer is one network and whether a bridge can be made for it.
type BridgeNetworkOffer struct {
	Network string        `json:"network"`
	Offered bool          `json:"offered"`
	Name    string        `json:"name"`
	Mark    string        `json:"mark"`
	Status  ChatAppStatus `json:"status"`
}

// BridgeAllowance is how many bridges the owner's plan allows. Read it before
// CreateBridge: a Free plan allows none (PaidPlan false, and creating answers
// 402), and a full allowance answers 403 plan_limit.
type BridgeAllowance struct {
	// OwnerID is the account the bridges count against: the hub's owner when
	// a hub was named, else the caller.
	OwnerID  string `json:"owner_id"`
	Plan     string `json:"plan"`
	PaidPlan bool   `json:"paid_plan"`
	Limit    int    `json:"limit"`
	// Used counts every bridge the owner has, in setup or active.
	Used      int `json:"used"`
	Remaining int `json:"remaining"`
	// CanCreate is whether CreateBridge would pass the plan checks for this
	// caller now. It says nothing about the token or the network.
	CanCreate bool `json:"can_create"`
}

// BridgeNetworks is what GetBridgeNetworks answers.
type BridgeNetworks struct {
	Networks  []BridgeNetworkOffer `json:"networks"`
	Allowance BridgeAllowance      `json:"allowance"`
}

// BridgeConnectorRef names a bridge's connector and the release it last ran
// (Version "" before its first start).
type BridgeConnectorRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// BridgeBot is the bot a bridge speaks as. Optional text is "" when the API
// sends none.
type BridgeBot struct {
	UserID        string `json:"user_id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name"`
	ApplicationID string `json:"application_id"`
	// AvatarURL is the API's own address for the bot's picture; read it with
	// GetBridgeAvatar.
	AvatarURL string `json:"avatar_url"`
}

// BridgeInviteLinks add the bot to a group or server (AddToGroup) or open a
// direct chat with it (DirectChat); "" for a link the network has none of.
type BridgeInviteLinks struct {
	AddToGroup string `json:"add_to_group"`
	DirectChat string `json:"direct_chat"`
}

// BridgeAllowedChat is a chat the bridge answers in. Only ID is required; the
// rest is what was shown when it was picked, for display. Empty fields are
// left out of a request.
type BridgeAllowedChat struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Kind  string `json:"kind,omitempty"`
	// GuildID and GuildName group Discord channels by server.
	GuildID   string `json:"guild_id,omitempty"`
	GuildName string `json:"guild_name,omitempty"`
}

// BridgeHold says why the platform paused a bridge, and since when. A bridge
// the owner paused carries none.
type BridgeHold struct {
	Reason BridgeHoldReason `json:"reason"`
	Since  string           `json:"since"`
}

// BridgeStatus is how a bridge is doing. Branch on Reason.
type BridgeStatus struct {
	Phase  BridgePhase        `json:"phase"`
	Ready  bool               `json:"ready"`
	Reason BridgeStatusReason `json:"reason"`
	// Message and OperatorReason are platform detail, sent to platform
	// admins only; "" for everybody else.
	Message        string `json:"message"`
	OperatorReason string `json:"operator_reason"`
	// Generation and ObservedGeneration say which change the platform has
	// taken in; nil when not reported.
	Generation         *int64 `json:"generation"`
	ObservedGeneration *int64 `json:"observed_generation"`
	// Applied is true once the running bridge carries everything asked of
	// it, false while a change is on its way, nil in setup or when unknown.
	Applied *bool              `json:"applied"`
	Source  BridgeStatusSource `json:"source"`
}

// Bridge is a hub's connection to one chat network. Timestamps are RFC 3339
// strings, as the API sends them.
type Bridge struct {
	ID      string `json:"id"`
	HubID   string `json:"hub_id"`
	OwnerID string `json:"owner_id"`
	// Network is the connector id (telegram, discord, matrix, ...).
	Network   string              `json:"network"`
	Connector *BridgeConnectorRef `json:"connector"`
	// Settings are the connector's non-secret settings; never secrets.
	Settings map[string]any `json:"settings"`
	// SecretsSet names the secrets stored for it; never their values.
	SecretsSet   []string    `json:"secrets_set"`
	State        BridgeState `json:"state"`
	ResourceName string      `json:"resource_name"`
	Bot          BridgeBot   `json:"bot"`
	// InviteLinks is nil for a connector the owner invites the bot to by
	// hand (its setup steps say how).
	InviteLinks    *BridgeInviteLinks  `json:"invite_links"`
	Allowed        []BridgeAllowedChat `json:"allowed"`
	RequireMention bool                `json:"require_mention"`
	SessionScope   BridgeSessionScope  `json:"session_scope"`
	Paused         bool                `json:"paused"`
	// Hold is set when the platform paused the bridge; nil for the owner's
	// own pause and for a running bridge.
	Hold   *BridgeHold  `json:"hold"`
	Status BridgeStatus `json:"status"`
	// ETag is this version of the bridge; pass it as BridgeUpdate.ETag to
	// refuse an update over somebody else's change (HTTP 412).
	ETag           string `json:"etag"`
	TokenUpdatedAt string `json:"token_updated_at"`
	ActivatedAt    string `json:"activated_at"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	// Operation is the change this response started, when it started one.
	Operation *OperationResource `json:"operation"`
}

// BridgeChat is a chat the bridge's bot can see, from ListBridgeChats.
type BridgeChat struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Kind is the network's word: private, group, supergroup or channel on
	// Telegram; text or announcement on Discord.
	Kind         string `json:"kind"`
	GuildID      string `json:"guild_id"`
	GuildName    string `json:"guild_name"`
	GuildIconURL string `json:"guild_icon_url"`
	// Allowed is whether the bridge already answers there.
	Allowed bool `json:"allowed"`
	// Encrypted is an end-to-end encrypted room the bridge cannot read.
	Encrypted bool `json:"encrypted"`
	// Selectable is whether it can be chosen; true unless the API says not.
	Selectable bool `json:"selectable"`
}

// UnmarshalJSON reads a chat, Selectable true when the API leaves it out.
func (c *BridgeChat) UnmarshalJSON(data []byte) error {
	type plain BridgeChat
	decoded := plain{Selectable: true}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*c = BridgeChat(decoded)
	return nil
}

// AllowedChat is the chat as an entry for BridgeUpdate.Allowed.
func (c BridgeChat) AllowedChat() BridgeAllowedChat {
	return BridgeAllowedChat{ID: c.ID, Title: c.Title, Kind: c.Kind, GuildID: c.GuildID, GuildName: c.GuildName}
}

// BridgeChatList is what ListBridgeChats answers. When ManualEntry is true the
// connector cannot list chats: Data is empty and the owner types the ids,
// which must match the connector's ChatAppTargets.
type BridgeChatList struct {
	Network     string       `json:"network"`
	Data        []BridgeChat `json:"data"`
	Note        string       `json:"note"`
	ManualEntry bool         `json:"manual_entry"`
}

// BridgeTestResult is what SendBridgeTestMessage answers. Language is the
// language the message was posted in: the hub's, else English.
type BridgeTestResult struct {
	Sent     bool   `json:"sent"`
	ChatID   string `json:"chat_id"`
	Language string `json:"language"`
}

// UnmarshalJSON reads a test result, Language "en" when the API sends none.
func (r *BridgeTestResult) UnmarshalJSON(data []byte) error {
	type plain BridgeTestResult
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.Language == "" {
		decoded.Language = "en"
	}
	*r = BridgeTestResult(decoded)
	return nil
}

// BridgeAvatar is the bot's picture: the image bytes and their media type.
type BridgeAvatar struct {
	Content     []byte
	ContentType string
}

// BridgeListOptions narrows ListBridges. Every field is optional: without
// HubID it lists every bridge the caller may see. Limit is 1-500; 0 leaves the
// API's default (200).
type BridgeListOptions struct {
	HubID   string
	OwnerID string
	Limit   int
}

// BridgeCreate is what CreateBridge sends. Set exactly one of Token (a
// connector with a single token: Telegram, Discord, Mattermost) and Secrets
// (every secret the connector names, by key: Matrix's access_token, Twitch's
// refresh_token and client_secret). Settings are the connector's non-secret
// settings, sent when non-nil. Printing a BridgeCreate never shows a secret.
type BridgeCreate struct {
	HubID     string
	Connector string
	Token     SecretValue
	Secrets   map[string]SecretValue
	Settings  map[string]any
}

// BridgeUpdate is what UpdateBridge changes. Only the fields that are set are
// sent: a non-nil Allowed or Settings, a non-nil pointer, a non-empty
// SessionScope.
//
//	// Pause, and resume later.
//	cp.UpdateBridge(ctx, id, thalovant.BridgeUpdate{Paused: new(true)})
//	cp.UpdateBridge(ctx, id, thalovant.BridgeUpdate{Paused: new(false)})
type BridgeUpdate struct {
	// Allowed replaces the chats the bridge answers in, at most 50, never
	// empty. Setting it the first time starts the bridge.
	Allowed        []BridgeAllowedChat
	RequireMention *bool
	SessionScope   BridgeSessionScope
	// Paused pauses (true) or resumes (false) the bridge. Resuming answers
	// 403 plan_limit while the plan's allowance is full.
	Paused *bool
	// Settings are merged into the stored settings and checked again as a
	// whole.
	Settings map[string]any
	// ETag, when set, is sent as If-Match: the update is refused with HTTP
	// 412 if the bridge changed since it was read. "" sends none.
	ETag string
}

func bridgePath(bridgeID string) (string, error) {
	if strings.TrimSpace(bridgeID) == "" {
		return "", fmt.Errorf("%w: a bridge id is required", ErrAPI)
	}
	return "/v1/bridges/" + url.PathEscape(bridgeID), nil
}

// controlInto sends one control-plane request and decodes a 2xx answer into
// out (nothing to decode when out is nil). A non-2xx answer is the usual
// *APIError; a body that does not decode is one too, as in request.
func (c *ControlPlane) controlInto(ctx context.Context, method, path string, payload map[string]any, headers map[string]string, out any) error {
	status, raw, header, err := c.send(ctx, method, path, payload, headers, true)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return apiErrorFromResponse(status, raw, header)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &APIError{StatusCode: status, Detail: "invalid JSON response"}
	}
	return nil
}

func (c *ControlPlane) bridgeInto(ctx context.Context, method, path string, payload map[string]any, headers map[string]string) (Bridge, error) {
	var bridge Bridge
	if err := c.controlInto(ctx, method, path, payload, headers, &bridge); err != nil {
		return Bridge{}, err
	}
	return bridge, nil
}

// revealSecrets is the request body's copy of a secrets map: the only place
// the values are read.
func revealSecrets(secrets map[string]SecretValue) map[string]string {
	revealed := make(map[string]string, len(secrets))
	for key, value := range secrets {
		revealed[key] = value.Reveal()
	}
	return revealed
}

// ListChatAppConnectors lists the chat networks a hub can be connected to, with
// the secrets and settings each takes. Hidden connectors are left out.
func (c *ControlPlane) ListChatAppConnectors(ctx context.Context) ([]ChatAppConnector, error) {
	var envelope struct {
		Data []ChatAppConnector `json:"data"`
	}
	if err := c.controlInto(ctx, http.MethodGet, "/v1/bridges/connectors", nil, nil, &envelope); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// GetBridgeNetworks says which networks can be bridged now and how many
// bridges the plan allows. The allowance is the hub owner's when hubID is
// given ("" for the caller's own).
func (c *ControlPlane) GetBridgeNetworks(ctx context.Context, hubID string) (BridgeNetworks, error) {
	path := "/v1/bridges/networks"
	if hubID != "" {
		path += "?" + url.Values{"hub_id": []string{hubID}}.Encode()
	}
	var networks BridgeNetworks
	if err := c.controlInto(ctx, http.MethodGet, path, nil, nil, &networks); err != nil {
		return BridgeNetworks{}, err
	}
	return networks, nil
}

// ListBridges lists bridges: a hub's when opts.HubID is set, else every one
// the caller may see, narrowed to one owner by opts.OwnerID.
func (c *ControlPlane) ListBridges(ctx context.Context, opts BridgeListOptions) ([]Bridge, error) {
	if opts.Limit < 0 || opts.Limit > 500 {
		return nil, fmt.Errorf("%w: limit must be from 1 to 500", ErrAPI)
	}
	query := url.Values{}
	setStringQuery(query, "hub_id", opts.HubID)
	setStringQuery(query, "owner_id", opts.OwnerID)
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	path := "/v1/bridges"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var envelope struct {
		Data []Bridge `json:"data"`
	}
	if err := c.controlInto(ctx, http.MethodGet, path, nil, nil, &envelope); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// CreateBridge connects a hub to a chat network. The API checks the
// credentials with the network, stores them, and answers the bridge in setup;
// nothing runs until UpdateBridge sets Allowed. Exactly one of create.Token
// and create.Secrets must be set, or nothing is sent.
//
// Requires a paid plan with room in its allowance (GetBridgeNetworks) and
// hubs:write.
func (c *ControlPlane) CreateBridge(ctx context.Context, create BridgeCreate) (Bridge, error) {
	if strings.TrimSpace(create.HubID) == "" {
		return Bridge{}, fmt.Errorf("%w: a hub id is required", ErrAPI)
	}
	if strings.TrimSpace(create.Connector) == "" {
		return Bridge{}, fmt.Errorf("%w: a connector is required", ErrAPI)
	}
	hasToken, hasSecrets := !create.Token.IsZero(), len(create.Secrets) > 0
	if hasToken == hasSecrets {
		return Bridge{}, fmt.Errorf("%w: send a token or secrets, exactly one of them", ErrAPI)
	}
	payload := map[string]any{"hub_id": create.HubID, "connector": create.Connector}
	if hasToken {
		payload["token"] = create.Token.Reveal()
	} else {
		payload["secrets"] = revealSecrets(create.Secrets)
	}
	if create.Settings != nil {
		payload["settings"] = create.Settings
	}
	return c.bridgeInto(ctx, http.MethodPost, "/v1/bridges", payload, nil)
}

// GetBridge reads one bridge with its status.
func (c *ControlPlane) GetBridge(ctx context.Context, bridgeID string) (Bridge, error) {
	path, err := bridgePath(bridgeID)
	if err != nil {
		return Bridge{}, err
	}
	return c.bridgeInto(ctx, http.MethodGet, path, nil, nil)
}

// UpdateBridge changes the fields update sets, and only those. A stale
// update.ETag answers HTTP 412 and changes nothing: read the bridge again and
// retry with its ETag.
func (c *ControlPlane) UpdateBridge(ctx context.Context, bridgeID string, update BridgeUpdate) (Bridge, error) {
	path, err := bridgePath(bridgeID)
	if err != nil {
		return Bridge{}, err
	}
	payload := map[string]any{}
	if update.Allowed != nil {
		payload["allowed"] = update.Allowed
	}
	if update.RequireMention != nil {
		payload["require_mention"] = *update.RequireMention
	}
	if update.SessionScope != "" {
		payload["session_scope"] = update.SessionScope
	}
	if update.Paused != nil {
		payload["paused"] = *update.Paused
	}
	if update.Settings != nil {
		payload["settings"] = update.Settings
	}
	var headers map[string]string
	if update.ETag != "" {
		if strings.TrimSpace(update.ETag) == "" {
			return Bridge{}, fmt.Errorf("%w: etag must not be blank", ErrAPI)
		}
		headers = map[string]string{"If-Match": update.ETag}
	}
	return c.bridgeInto(ctx, http.MethodPatch, path, payload, headers)
}

// ReplaceBridgeToken replaces the token of a connector whose single secret is
// "token" (Telegram, Discord, Mattermost); use ReplaceBridgeSecrets for the
// others. Where the API checks the bot (Telegram, Discord), the token must
// belong to the same bot (409 bridge_token_other_bot otherwise: make a new
// bridge).
func (c *ControlPlane) ReplaceBridgeToken(ctx context.Context, bridgeID string, token SecretValue) (Bridge, error) {
	path, err := bridgePath(bridgeID)
	if err != nil {
		return Bridge{}, err
	}
	if token.IsZero() {
		return Bridge{}, fmt.Errorf("%w: a token is required", ErrAPI)
	}
	return c.bridgeInto(ctx, http.MethodPut, path+"/token", map[string]any{"token": token.Reveal()}, nil)
}

// ReplaceBridgeSecrets replaces the named secrets; the ones not named stay as
// stored.
func (c *ControlPlane) ReplaceBridgeSecrets(ctx context.Context, bridgeID string, secrets map[string]SecretValue) (Bridge, error) {
	path, err := bridgePath(bridgeID)
	if err != nil {
		return Bridge{}, err
	}
	if len(secrets) == 0 {
		return Bridge{}, fmt.Errorf("%w: at least one secret is required", ErrAPI)
	}
	return c.bridgeInto(ctx, http.MethodPut, path+"/secrets", map[string]any{"secrets": revealSecrets(secrets)}, nil)
}

// ListBridgeChats lists the chats the bridge's bot can see, to choose
// BridgeUpdate.Allowed from. Telegram answers 409 bridge_running while the
// bridge runs: pause it first.
func (c *ControlPlane) ListBridgeChats(ctx context.Context, bridgeID string) (BridgeChatList, error) {
	path, err := bridgePath(bridgeID)
	if err != nil {
		return BridgeChatList{}, err
	}
	var chats BridgeChatList
	if err := c.controlInto(ctx, http.MethodGet, path+"/chats", nil, nil, &chats); err != nil {
		return BridgeChatList{}, err
	}
	return chats, nil
}

// SendBridgeTestMessage posts a short test message, in the hub's language, to
// an allowed chat.
func (c *ControlPlane) SendBridgeTestMessage(ctx context.Context, bridgeID, chatID string) (BridgeTestResult, error) {
	path, err := bridgePath(bridgeID)
	if err != nil {
		return BridgeTestResult{}, err
	}
	var result BridgeTestResult
	if err := c.controlInto(ctx, http.MethodPost, path+"/test", map[string]any{"chat_id": chatID}, nil, &result); err != nil {
		return BridgeTestResult{}, err
	}
	return result, nil
}

// maxBridgeAvatarBytes caps the picture GetBridgeAvatar reads. The API serves
// at most 1 MiB; the rest is headroom, so a larger body is refused rather
// than read into memory without bound.
const maxBridgeAvatarBytes = 4 << 20

// GetBridgeAvatar reads the bot's picture. A bot without one answers the
// usual *APIError with HTTP 404, and a body over 4 MiB, or one cut short, an
// *APIError too.
func (c *ControlPlane) GetBridgeAvatar(ctx context.Context, bridgeID string) (BridgeAvatar, error) {
	path, err := bridgePath(bridgeID)
	if err != nil {
		return BridgeAvatar{}, err
	}
	status, raw, header, err := c.sendWithin(ctx, http.MethodGet, path+"/avatar", nil, map[string]string{"Accept": "image/*"}, true, maxBridgeAvatarBytes)
	if err != nil {
		return BridgeAvatar{}, err
	}
	if status < 200 || status > 299 {
		return BridgeAvatar{}, apiErrorFromResponse(status, raw, header)
	}
	contentType := header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return BridgeAvatar{Content: raw, ContentType: contentType}, nil
}

// DeleteBridge stops the bridge and deletes it with its stored credentials.
func (c *ControlPlane) DeleteBridge(ctx context.Context, bridgeID string) error {
	path, err := bridgePath(bridgeID)
	if err != nil {
		return err
	}
	return c.controlInto(ctx, http.MethodDelete, path, nil, nil, nil)
}
