# Thalovant Go SDK

Go SDK for connecting services, CLIs, devices, and agents to Thalovant hubs.

The control API is used to discover hubs and provision a client identity. After
that, the SDK talks directly to the hub data plane over HTTPS, WSS, or MQTTS.

Full docs: <https://docs.thalovant.com/developers/sdks/go/>

## What You Need

- A Thalovant account with API access for authenticated control-plane actions.
- A hub id or slug.
- A client identity for that hub. You can create one through the API or use one
  downloaded from the dashboard.

## Install

Use Go 1.25 or newer so the SDK receives supported upstream networking security fixes.

```bash
go get github.com/thalovant/thalovant-go-sdk
```

## Quick Start

```go
package main

import (
	"context"
	"fmt"

	thalovant "github.com/thalovant/thalovant-go-sdk"
)

func main() {
	ctx := context.Background()
	control := thalovant.NewDefaultControlPlane("")

	// Public hub discovery does not require auth.
	publicHubs, err := control.ListPublicHubs(ctx, 12, "")
	if err != nil {
		panic(err)
	}
	for _, raw := range publicHubs["data"].([]any) {
		hub := raw.(map[string]any)
		fmt.Println(hub["id"], hub["slug"], hub["title"])
	}

	// Auth is required when creating a client identity.
	if _, err := control.Login(ctx, "you@example.com", "password", ""); err != nil {
		panic(err)
	}

	result, err := control.CreateClientIdentityForHubID(ctx, "hub-id", thalovant.BootstrapIdentityOptions{
		Name:               "go-demo-client",
		PreferredProtocols: []thalovant.HubProtocol{thalovant.ProtocolWSS, thalovant.ProtocolHTTPS, thalovant.ProtocolMQTT},
	})
	if err != nil {
		panic(err)
	}

	client, err := thalovant.NewClientWithOptions(result.Identity, thalovant.ClientOptions{
		Protocol: thalovant.ProtocolWSS,
	})
	if err != nil {
		panic(err)
	}
	defer client.Close(ctx)

	info, err := client.ConnectWithInfo(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println("connected in", info.ConnectMS, "ms")

	reply, err := client.Ask(ctx, "Tell me a short clean joke.", thalovant.RequestOptions{})
	if err != nil {
		panic(err)
	}
	fmt.Println(reply.Text)
}
```

`NewDefaultControlPlane` uses `https://api.thalovant.com`. Use
`NewControlPlane` only for local development or a self-hosted control plane.

### Login With MFA

Accounts with multi-factor authentication enabled are rejected with HTTP 401
`{"code": "mfa_required"}` by a plain `Login` call. Use `LoginWithOptions` to
pass a TOTP code, or a recovery code when the authenticator is unavailable:

```go
control := thalovant.NewDefaultControlPlane("")

// With a TOTP code from an authenticator app.
_, err := control.LoginWithOptions(ctx, "you@example.com", "password", thalovant.LoginOptions{
	OTPCode: "123456",
})

// Or with a one-time recovery code.
_, err = control.LoginWithOptions(ctx, "you@example.com", "password", thalovant.LoginOptions{
	RecoveryCode: "your-recovery-code",
})
```

`LoginOptions.Scope` matches the `scope` argument of `Login`. Empty fields are
omitted from the request body, so `LoginWithOptions` with a zero-value
`LoginOptions` behaves exactly like `Login` without a scope.

### Sign In With the Browser (Device Flow)

Accounts without a password (for example Google sign-in) use the device flow.
`LoginWithBrowser` prints a verification URL and a short user code, opens the
browser on a best-effort basis, and polls until you approve the request:

```go
control := thalovant.NewDefaultControlPlane("")

token, err := control.LoginWithBrowser(ctx, thalovant.DeviceLoginOptions{
	Scopes:     []string{"hubs:read", "clients:write"}, // optional
	ClientName: "my-cli",                               // optional label in the dashboard
})
if err != nil {
	panic(err)
}
fmt.Println("signed in, token id:", token["token_id"])
```

On approval the returned `access_token` is a durable scoped API token; it is
stored on `control.AccessToken` exactly like `Login`, so subsequent
control-plane calls are authenticated. The server may expand the echoed
`scopes` during normalization.

Options:

- `OpenBrowser`: `*bool`, defaults to true when nil. Set it to a false pointer
  on headless hosts; the plain verification URL and code are always shown.
- `Prompt`: `func(grant map[string]any)` replaces the default stdout message.
  The grant carries `verification_uri`, `user_code`, and
  `verification_uri_complete`.
- `Timeout`: total approval wait, 15 minutes when zero.

Failures are distinct sentinel errors: `errors.Is(err,
thalovant.ErrDeviceAccessDenied)` when the request is denied in the browser,
`thalovant.ErrDeviceCodeExpired` when the code expires unapproved (call
`LoginWithBrowser` again for a new code), and `thalovant.ErrTimeout` when the
wait elapses. Context cancellation is honored between polls.

### CI: Direct API Token Auth

Non-interactive environments should skip login entirely and construct the
control plane with a pre-provisioned API token, such as one issued by
`LoginWithBrowser` on a workstation:

```go
control := thalovant.NewDefaultControlPlane(os.Getenv("THALOVANT_API_TOKEN"))

page, err := control.ListHubs(ctx, 50, "", "")
```

`ControlPlane.AccessToken` is an exported field, so an existing instance can
also be pointed at a token directly: `control.AccessToken = token`.

Keep `result.Identity` secret: it holds the client's data-plane credentials.
`result.Summary(false)` — the default — is safe to log: it redacts the secret
fields of the identity **and** of the raw `hub`/`client` maps (the
`initial_identify` access key/password/crypto key/MQTT password, the
`initial_identify_token`, and the echoed spec `apiKey`/`password`/`cryptoKey`).
The crypto key is no longer issued, but the redaction still names it so an
older stored payload that carries one cannot be logged.
`result.Summary(true)` returns every one of those secrets in the clear and must
never be logged or written to an untrusted sink. The redaction covers
human-facing formatting only; `json.Marshal` of the identity itself (for the
identity file you persist with `chmod 600`) still contains the real secrets by
design.

## List Your Hubs

Authenticated accounts can list owned or visible hubs:

```go
control := thalovant.NewDefaultControlPlane("")
_, _ = control.Login(ctx, "you@example.com", "password", "")

page, err := control.ListHubs(ctx, 50, "", "")
if err != nil {
	panic(err)
}
for _, raw := range page["data"].([]any) {
	hub := raw.(map[string]any)
	fmt.Println(hub["id"], hub["slug"], hub["title"])
}
```

## Provision Hubs

Hubs, runtime groups, and skills can be created and managed from code. These
routes need a **paid plan** and a token with the **`hubs:write`** scope
("Create and update your hubs" on the dashboard's API Tokens page). A free-plan
token fails with `HTTP 402` and `API access requires a paid plan.`, and a token
without the scope fails with `HTTP 403` and `Insufficient scopes`.

```go
control := thalovant.NewDefaultControlPlane(os.Getenv("THALOVANT_API_TOKEN"))

// 1. Discover what is installable before provisioning anything.
catalog, err := control.ListMarketplaceSkills(ctx, thalovant.MarketplaceSkillListOptions{})
if err != nil {
	panic(err)
}
for _, raw := range catalog["data"].([]any) {
	skill := raw.(map[string]any)
	fmt.Println(skill["skill_id"], skill["title"], skill["access_tier"])
}

// 2. Create a runtime group to run the skills.
group, err := control.CreateRuntimeGroup(ctx, map[string]any{
	"name":        "kiosks",
	"description": "Lobby kiosks",
})
if err != nil {
	panic(err)
}
groupID := group["id"].(string)

// 3. Create a hub attached to it.
hub, err := control.CreateHub(ctx, map[string]any{
	"name":             "joke-garden",
	"runtime_group_id": groupID,
	"spec":             map[string]any{"protocols": map[string]any{"wss": map[string]any{"enabled": true}}},
}, thalovant.HubCreateOptions{})
if err != nil {
	panic(err)
}
hubID := hub["id"].(string)

// 4. Install a skill from the marketplace catalog.
if _, err := control.InstallRuntimeGroupSkill(ctx, groupID, "skill-weather", thalovant.RuntimeGroupSkillInstallOptions{}); err != nil {
	panic(err)
}

// 5. Release: roll the runtime and the hub onto a release channel.
_, err = control.ReleaseRuntimeGroup(ctx, groupID, thalovant.ReleaseOptions{Channel: "stable"})
_, err = control.ReleaseHub(ctx, hubID, thalovant.ReleaseOptions{Channel: "stable"})
```

Creating a hub is idempotent. `CreateHub` always sends an `Idempotency-Key`
header, so a call retried after a timeout returns the hub that was already
created instead of making a second one. Set `HubCreateOptions.IdempotencyKey`
to control the key; leave it empty and the SDK generates one.

Updating and deleting a hub use optimistic locking, so `etag` is a required
argument rather than an option. Pass the `etag` from the hub resource you read;
the SDK sends it as `If-Match`, and the API rejects a stale or missing value
with `HTTP 412` without changing anything:

```go
hub, err := control.GetHub(ctx, hubID)
hub, err = control.UpdateHub(ctx, hubID, map[string]any{"active": false}, hub["etag"].(string))
err = control.DeleteHub(ctx, hubID, hub["etag"].(string))
```

Deleting a hub also deletes its clients and ACLs. Runtime groups have no
`If-Match` requirement, but the API refuses to delete the workspace default
group or a group that still has hubs attached (`HTTP 409`).

Payload maps take the API's snake_case keys; the camelCase spellings
(`runtimeGroupId`, `ownerId`, `capacityProfile`, `isLocked`,
`cloneFromDefault`) are accepted too and are renamed before the request is
sent, so neither spelling is silently dropped.

Runtime configuration is merged, not replaced, and `Personas` is replaced only
when set:

```go
_, err = control.UpdateRuntimeGroupConfig(ctx, groupID, map[string]any{"lang": "en-us"}, thalovant.RuntimeGroupConfigOptions{})

config, err := control.GetRuntimeGroupConfig(ctx, groupID)
fmt.Println(config["config"])
```

Rating a public hub with `SetHubRating` and `ClearHubRating` needs the
`hubs:write` scope but, unlike the routes above, no paid plan. Reading what a
hub is actually running needs the `hubs:inspect` scope instead:

```go
capabilities, err := control.GetHubRuntimeCapabilities(ctx, hubID)
fmt.Println(capabilities["counts"].(map[string]any)["total_intents"])
```

## Discover Skills

The marketplace catalog is readable with the **`hubs:read`** scope and, unlike
the provisioning routes above, is **not paid-gated** — a free-plan token can
browse the whole catalog before upgrading, and only the install needs a paid
plan.

```go
catalog, err := control.ListMarketplaceSkills(ctx, thalovant.MarketplaceSkillListOptions{})
if err != nil {
	panic(err)
}
for _, raw := range catalog["data"].([]any) {
	skill := raw.(map[string]any)
	fmt.Println(skill["skill_id"], skill["category"], skill["access_tier"])
}
```

Each entry carries what an install needs (`skill_id`, `source_type`,
`source_ref`, `config_schema`, `secret_schema`) next to presentation fields
(`title`, `summary`, `tags`, `verified`). Admin tokens can additionally set
`OwnerID` to read another tenant's catalog and `IncludeInactive` to see retired
entries; both are silently ignored for non-admin callers, which are scoped to
their own tenant and to active entries. `ForceRefresh` re-syncs the global
catalog from source first, which is slower.

Two group-scoped reads need the **`hubs:inspect`** scope and are likewise not
paid-gated. The first resolves the catalog against one runtime group, so each
entry reports whether it is already desired, whether it was observed running,
and whether the tenant plan allows installing it:

```go
view, err := control.ListRuntimeGroupMarketplace(ctx, groupID, thalovant.RuntimeGroupMarketplaceOptions{})
if err != nil {
	panic(err)
}
for _, raw := range view["data"].([]any) {
	entry := raw.(map[string]any)
	if entry["installable"] == true && entry["active"] != true {
		fmt.Println("available:", entry["skill_id"])
	}
}
```

The second answers what the group is actually running right now, rather than
what could be installed:

```go
inventory, err := control.ListRuntimeGroupInventory(ctx, groupID, thalovant.RuntimeGroupInventoryOptions{Refresh: true})
if err != nil {
	panic(err)
}
fmt.Println(inventory["source"], len(inventory["data"].([]any)))
```

Both answer from a cached inventory snapshot by default; set
`RefreshInventory` or `Refresh` to force a live read from the runtime operator.
When nothing is reporting yet, `ListRuntimeGroupInventory` returns an empty
`data` list with a pending `source` rather than failing —
`GetHubRuntimeCapabilities` is the one that answers `HTTP 409` in that case.

## Workspace Analytics

Authenticated accounts can read the same overview used by the dashboard:

```go
overview, err := control.GetAnalyticsOverview(ctx, thalovant.AnalyticsOverviewOptions{
	Range: "7d",
	HubID: "hub-id",
})
if err != nil {
	panic(err)
}
fmt.Println(overview["totals"])
```

## Durable Memory

Private Daily Desk and workspace assistants can manage explicit opt-in memory:

```go
memory, err := control.CreateMemoryItem(ctx, map[string]any{
	"scope":   "workspace",
	"kind":    "preference",
	"content": "Prefer America/Toronto for scheduling.",
	"tags":    []string{"timezone"},
})
if err != nil {
	panic(err)
}
fmt.Println(memory["id"])

items, err := control.ListMemoryItems(ctx, thalovant.MemoryListOptions{
	Scope: "workspace",
	Query: "timezone",
})
if err != nil {
	panic(err)
}
fmt.Println(items["data"])
```

## Use An Existing Identity

For local development, store one or more identities in the protected SDK config:

```bash
mkdir -p ~/.config/thalovant
chmod 700 ~/.config/thalovant
$EDITOR ~/.config/thalovant/config.yaml
chmod 600 ~/.config/thalovant/config.yaml
```

```yaml
profile: prod
profiles:
  prod:
    identity:
      access_key: ...
      password: ...
      site_id: demo-agent
      default_master: https://jokes.thalovant.io
      data_plane_endpoints:
        wss: wss://jokes.thalovant.io/public
        https: https://jokes.thalovant.io/public
        mqtt: mqtts://mqtt.thalovant.com:8883
      mqtt:
        endpoint: mqtts://mqtt.thalovant.com:8883
        username: ...
        password: ...
        topic_prefix: hubs/hub-id/clients/client-id
        tls: true
```

```go
client, err := thalovant.NewClientFromConfig("", "prod")
if err != nil {
	panic(err)
}
defer client.Close(ctx)

reply, err := client.Ask(ctx, "What can this hub do?", thalovant.RequestOptions{})
if err != nil {
	panic(err)
}
fmt.Println(reply.Text)
```

SDKs reject config files that are readable or writable by other users on Linux
and macOS. Keep this file out of git.

Raw identity files are supported too:

```go
client, err := thalovant.NewClientFromFile("_identity.json")
```

Environment variables are supported too:

```go
client, err := thalovant.NewClientFromEnv()
```

## Protocols

Hubs may expose one or more public data-plane protocols:

- `wss`: secure realtime WebSocket, the default public path and SDK preference.
- `https`: request/response HTTP protocol exposed as HTTPS.
- `mqtt`: broker-mediated MQTT over TLS. Requires per-client broker credentials.

### Transport Security

`wss`, `https`, and `mqtt` connections perform the HiveMind **v3 Noise handshake**
(`Noise_XXpsk2_25519_ChaChaPoly_SHA256`, or `KKpsk0` once the hub's static key
is pinned). It is the only key exchange a HiveMind-core 5.x hub accepts: there
is no pre-shared `crypto_key` any more, no cleartext path, and a connection that
cannot complete the handshake never becomes ready. A WebSocket hub refusal
may close with code `1008`.

Nothing extra has to be provisioned. The Noise pre-shared key is derived from
the identity `password` with argon2id, salted with the hub's node id, so an
identity that can authenticate can already handshake.

Two files persist beside the SDK config file (`~/.config/thalovant` unless
`XDG_CONFIG_HOME` or `%APPDATA%` says otherwise), both `0600`:

- `noise_key` — this client's static X25519 key. It has to persist: a hub pins
  it on first contact, so regenerating it makes the client look like a
  different peer and the hub refuses it.
- `noise_pins.json` — the hub static keys this client has pinned.

Set `NoiseStateDir` on `WSSTransport`, `HTTPTransport`, or `MQTTTransport`
to use another persistent directory. Reuse the same directory and identity
when switching transports; do not regenerate a paired client's static key.

The first connection to a hub trusts the key it presents and records it. A
later connection presenting a different key is **refused**, because the SDK
cannot tell a reinstalled hub from another machine answering at the same
address. If the hub really was replaced, clear the pin deliberately:

```go
if err := thalovant.ForgetNoisePin("", nodeID); err != nil {
	panic(err)
}
```

The derivation costs 64 MiB and a few hundred milliseconds. WSS caches the
result per hub; HTTP and MQTT derive from the current password on each fresh
connection. All transports retain the hub pin after authentication failures.

HTTP preserves the hub's replica affinity cookie and posts encrypted frames as
Base64 form data with `binary=1`; encrypted replies arrive through
`/get_binary_messages`. Both non-success HTTP status codes and JSON `error`
responses invalidate the connection. MQTT carries the same Noise frames as raw
binary payloads after its initial cleartext HELLO and Noise exchange. TLS remains
required on HTTP and MQTT because the access key and broker credentials also
need protection. `MQTTTransport.TLSConfig` can supply private CA roots.

A broker disconnect invalidates MQTT readiness. Call `Connect` again to
resubscribe and negotiate a fresh Noise session; Paho's automatic connection
resumption is disabled because it would retain stale encryption counters.
Concurrent sends serialize complete encrypted messages and their chunks.
Passing `encrypt=false` to `SendHiveMessage` cannot bypass Noise.

For an explicit transport and persistent identity state:

```go
transport := thalovant.NewHTTPTransport(identity)
transport.NoiseStateDir = "/var/lib/my-agent/thalovant"
if err := transport.Connect(ctx); err != nil {
    return err
}
defer transport.Disconnect(ctx)
// Connect returns only after the Noise exchange and encrypted HELLO succeed.
err := transport.EmitBus(ctx, "ovos.intent.list", thalovant.Data{},
    thalovant.Context{"request_id": thalovant.NewSessionID()})
```

`HTTPTransport.RemoteStaticKey()` and `MQTTTransport.RemoteStaticKey()` expose
the authenticated hub key, as WSS already does. An interrupted or tampered
session must reconnect before sending again.

Inspect what an identity supports:

```go
identity := result.Identity

fmt.Println(identity.EnabledProtocols())
fmt.Println(identity.EndpointFor(thalovant.ProtocolWSS))
fmt.Println(identity.EndpointFor(thalovant.ProtocolHTTPS))
fmt.Println(identity.EndpointFor(thalovant.ProtocolMQTT))
if identity.MQTT != nil {
	fmt.Println(identity.MQTT.Endpoint)
}
```

Connect with a specific protocol:

```go
for _, protocol := range []thalovant.HubProtocol{
	thalovant.ProtocolWSS,
	thalovant.ProtocolHTTPS,
	thalovant.ProtocolMQTT,
} {
	if !identity.SupportsProtocol(protocol) {
		continue
	}
	if protocol == thalovant.ProtocolMQTT && identity.MQTT == nil {
		continue
	}

	client, err := thalovant.NewClientWithOptions(identity, thalovant.ClientOptions{Protocol: protocol})
	if err != nil {
		panic(err)
	}
	reply, err := client.Ask(ctx, fmt.Sprintf("Reply over %s.", protocol), thalovant.RequestOptions{})
	_ = client.Close(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(protocol, reply.Text)
}
```

Use `client.ConnectWithInfo(ctx)` when you need connection telemetry for
benchmarks or health dashboards. The returned snapshot includes phase,
socket/open time, handshake time, total connect time, and last error.

Use `client.Query(ctx, ...)` for the direct HiveMind query frame path when the
hub supports it. It avoids broad bus fanout and is the preferred request/reply
API for low-latency app integrations.

```go
reply, err := client.Query(ctx, "What time is it in Toronto?", thalovant.QueryOptions{})
```

MQTT identities include a broker endpoint, username, password, TLS flag, and
topic prefix. The broker credentials are scoped to that client and should be
treated like a password. Public identities should use `mqtts://`; the SDK also
honors an explicit `tls: true` flag from the identity.

## Conversations

Use a conversation when related turns should share one session.

```go
conversation := client.Conversation(thalovant.ConversationOptions{Lang: "en-us"})

first, err := conversation.Ask(ctx, "Remember that my favorite color is blue.", thalovant.RequestOptions{})
if err != nil {
	panic(err)
}
second, err := conversation.Ask(ctx, "What color did I mention?", thalovant.RequestOptions{})
if err != nil {
	panic(err)
}

fmt.Println(first.Text)
fmt.Println(second.Text)
```

## Client Context

Context lets skills know which app, device, user, or channel made the request.

```go
requestContext := thalovant.BuildClientContext(nil, thalovant.ClientContextOptions{
	UserID:       "user-42",
	UserName:     "Ada",
	AuthProvider: "oidc",
	Roles:        []string{"member"},
	Platform:     "kiosk",
	Source:       "checkout-kiosk",
	Channel:      "chat",
})

reply, err := client.Ask(ctx, "Show the next instruction.", thalovant.RequestOptions{
	Context: requestContext,
})
```

## Actions And Exact Inputs

Use actions for button payloads and codes for exact typed or scanned values.

```go
conversation := client.Conversation(thalovant.ConversationOptions{SessionID: "work-session"})

_ = conversation.SendAction(ctx, `/choose{"id":"42"}`, thalovant.ActionOptions{Title: "Choose item"})
_ = conversation.SendCode(ctx, "SN-001-XYZ", thalovant.CodeOptions{Kind: "qr", Label: "serial"})
```

## Rich Responses

Replies can include text, choices, tables, images, or attachments.

```go
items := reply.DisplayItems(600)
for _, item := range items {
	if item.Kind == "text" {
		fmt.Println(item.Text)
	}
}
```

## What A Hub Can Be Asked

A connected client can ask its hub what can be said, over its own session and
with no control-plane token. The hub runtime keeps an intent manifest: every
intent each skill registered, per language, and for template intents the
sentences the skill's locale files wrote, `{slot}` placeholders included.

```go
inventory, err := client.Intents(ctx, []string{"en-us", "fr-fr"})
if err != nil {
	var denied *thalovant.PolicyDeniedError
	if errors.As(err, &denied) {
		// This connection may not publish denied.DeniedType; denied.Allowed
		// lists what it may.
	}
	panic(err)
}
for _, skill := range inventory.Skills {
	fmt.Println(skill.SkillID, skill.Languages())
	for _, intent := range skill.Intents {
		fmt.Println("  ", intent.ID(), intent.Engine, intent.Examples("en-us", 2))
	}
}
```

Each `HubIntent` carries `Phrases` keyed by language, `PhrasesFor(lang)` —
tags compare case-insensitively with `_` and `-` folded, so `fr_FR` finds
`fr-fr` — and `Examples(lang, limit)`, which prefers whole sentences to ones
with a slot, shorter first. `Engine` is `padatious` for a template intent and
`adapt` for a keyword one.

`inventory.Source` is `intent-manifest` when the sentences came from the
manifest. A hub whose connection may not publish `ovos.intent.list` is asked
for the engines' own manifests instead: the result then carries names only,
`Source` is `engine-manifests`, `Denied` names the refused query and
`HasPhrases()` is false. `IntentOptions` tunes the call — `Timeout` bounds each
query the hub is sent (5 seconds when zero), `Fallback` set to a false pointer
returns the `*PolicyDeniedError` instead of falling back, and `Describe` set to
a false pointer skips the per-intent describes and returns names and engines
only:

```go
no := false
inventory, err := client.Intents(ctx, nil, thalovant.IntentOptions{
	Timeout:  3 * time.Second,
	Fallback: &no,
})
```

A nil or empty language list asks for `en-us`; tags are trimmed, and a
language repeated under another spelling (`en-us`, `en-US`, `en_us`) is asked
once, under the first spelling given. Like `Ask`, these calls read the
transport's event channel, so run them one at a time on a client. The two
underlying queries are exposed too:

```go
// ovos.intent.list: one row per registration in one language.
rows, err := client.ListIntents(ctx, "en-us")

// ovos.intent.describe: the registrations behind one intent, sentences included.
definitions, err := client.DescribeIntent(ctx, rows[0].SkillID, rows[0].IntentName, "en-us")
fmt.Println(definitions[0].Samples)
```

The connection must be allowed to publish `ovos.intent.list`;
`ovos.intent.describe` is needed only when the sentences are asked for, which
is the default, so `Describe` set to a false pointer needs the listing alone.
A hub that answers a listing `{"ok": false}` has failed the query rather than
refused the type: `Intents` and `ListIntents` return an error wrapping
`ErrRuntime` carrying the hub's own text, and the engines' manifests are not
asked instead — a listing that failed is not a hub with no intents. The same
answer to a describe is a real one, meaning the hub does not know that
registration, so that intent simply carries no sentences.

When the runtime does not attach definitions to the listing, each intent is
described individually; those requests go out `thalovant.DescribeBatch` (32)
at a time so a hub with many intents cannot burst more replies than the
transport's channel holds. An intent the hub does not describe in time simply
carries no sentences.

`json.Marshal(inventory)` produces the same snake_case shape as the Python
SDK's `as_dict()`, so the output can be handed to a satellite, an installer
or an agent as is.

## Common Issues

- `missing access token`: call `control.Login(...)` or
  `control.LoginWithBrowser(...)` before private control-plane actions, or
  pass an access token to `NewControlPlane`.
- `HTTP 401` with `"code": "mfa_required"`: the account has MFA enabled; use
  `control.LoginWithOptions(...)` with an `OTPCode` or `RecoveryCode`.
- The account has no password (Google sign-in): use
  `control.LoginWithBrowser(...)`, or mint a durable token once and pass it to
  `NewDefaultControlPlane` in CI.
- `API access requires a paid plan`: upgrade the workspace before using the SDK
  control-plane API to provision private resources. Hub ratings and the
  marketplace catalog are readable without one.
- `HTTP 412` with `"ETag mismatch"`: the `etag` passed to `UpdateHub` or
  `DeleteHub` is stale or empty. Re-read the hub with `GetHub` and retry with
  the `etag` it returns; nothing was changed.
- `unsupported protocol`: the hub does not expose that protocol, or the
  identity was created before that protocol was enabled.
- MQTT fails immediately: create or download a fresh client identity after MQTT
  is enabled. MQTT needs the per-client `Identity.MQTT` credentials.
- `the hub refused "ovos.intent.list"`: the connection's allow-list does not
  include the intent manifest queries. Connections the control plane
  provisions for SDK clients allow `ovos.intent.list`, `ovos.intent.describe`
  (needed only when definitions are asked for)
  and the two engine manifest reads by default; for an older client identity,
  allow them in the dashboard's connection settings or create a fresh
  identity. The error is a `*thalovant.PolicyDeniedError` (`errors.As`) that
  carries the refused type and the allowed list; by default `client.Intents`
  falls back to the engines' manifests and returns names only.
- `ovos.intent.list failed: ...`: the hub accepted the query and could not
  answer it — the text after the colon is the hub's own. This is an error
  wrapping `ErrRuntime`, not a `*PolicyDeniedError`, and no fallback is
  attempted: the hub's intents are unknown, not absent. Retry, or check the
  runtime's logs.
- A request times out: set `RequestOptions{Timeout: ...}`.
- `HTTP 429` with `"code": "token_rate_limited"`: the API token exceeded its
  plan's per-minute request rate (60 requests per minute on the free plan).
  The response carries a `Retry-After` header and a matching
  `retry_after_seconds`; wait that long and resend.
- `HTTP 429` with `"code": "token_quota_exceeded"`: the API token exhausted
  its plan's daily or monthly call quota. The body names which in `quota`
  (`daily` or `monthly`) alongside `limit` and `used`, and `Retry-After`
  points at the next UTC day or month boundary.

Both 429s apply to token-authenticated control-plane calls and are returned as
errors wrapping `ErrAPI`, with the status and response body in the message.
The SDK does not retry automatically: `Retry-After` is authoritative, so honor
it before resending. Per-plan limits are listed in the dashboard and at
<https://docs.thalovant.com/developers/sdks/go/>.

## API Shape

- `NewDefaultControlPlane(accessToken)`
- `NewControlPlane(apiURL, accessToken)` for local or self-hosted control planes
- `control.Login(ctx, email, password, scope)`
- `control.LoginWithOptions(ctx, email, password, LoginOptions{Scope: ..., OTPCode: ..., RecoveryCode: ...})`
- `control.LoginWithBrowser(ctx, DeviceLoginOptions{Scopes: ..., ClientName: ..., OpenBrowser: ..., Prompt: ..., Timeout: ...})`
- `control.ListPublicHubs(ctx, limit, cursor)`
- `control.GetPublicHub(ctx, hubRef)`
- `control.ListHubs(ctx, limit, cursor, ownerID)`
- `control.GetHub(ctx, hubID)`
- `control.CreateHub(ctx, payload, HubCreateOptions{IdempotencyKey: ...})`
- `control.UpdateHub(ctx, hubID, payload, etag)`
- `control.DeleteHub(ctx, hubID, etag)`
- `control.ReleaseHub(ctx, hubID, ReleaseOptions{Channel: ..., Mode: ..., Version: ..., Images: ..., Reason: ...})`
- `control.SetHubRating(ctx, hubID, rating)`
- `control.ClearHubRating(ctx, hubID)`
- `control.GetHubRuntimeCapabilities(ctx, hubID)`
- `control.ListRuntimeGroups(ctx, ownerID)`
- `control.GetRuntimeGroup(ctx, runtimeGroupID)`
- `control.CreateRuntimeGroup(ctx, payload)`
- `control.UpdateRuntimeGroup(ctx, runtimeGroupID, payload)`
- `control.GetRuntimeGroupConfig(ctx, runtimeGroupID)`
- `control.UpdateRuntimeGroupConfig(ctx, runtimeGroupID, config, RuntimeGroupConfigOptions{Personas: ...})`
- `control.ReleaseRuntimeGroup(ctx, runtimeGroupID, ReleaseOptions{...})`
- `control.DeleteRuntimeGroup(ctx, runtimeGroupID)`
- `control.InstallRuntimeGroupSkill(ctx, runtimeGroupID, skillID, RuntimeGroupSkillInstallOptions{MarketplaceSkillID: ..., SourceType: ..., SourceRef: ..., VersionPin: ..., Active: ...})`
- `control.UninstallRuntimeGroupSkill(ctx, runtimeGroupID, skillID)`
- `control.ListMarketplaceSkills(ctx, MarketplaceSkillListOptions{OwnerID: ..., IncludeInactive: ..., ForceRefresh: ...})`
- `control.ListRuntimeGroupMarketplace(ctx, runtimeGroupID, RuntimeGroupMarketplaceOptions{RefreshInventory: ...})`
- `control.ListRuntimeGroupInventory(ctx, runtimeGroupID, RuntimeGroupInventoryOptions{Refresh: ...})`
- `control.GetOperation(ctx, operationID)`
- `control.GetAnalyticsOverview(ctx, options)`
- `control.ListMemoryItems(ctx, options)`
- `control.GetMemorySummary(ctx, ownerID)`
- `control.CreateMemoryItem(ctx, payload)`
- `control.GetMemoryItem(ctx, memoryID)`
- `control.UpdateMemoryItem(ctx, memoryID, payload)`
- `control.DeleteMemoryItem(ctx, memoryID)`
- `control.CreateClientIdentityForHubID(ctx, hubID, options)`
- `IdentityFromConfig(path, profile)`
- `IdentityFromFile(path)`
- `NewClientFromConfig(path, profile)`
- `NewClientFromFile(path)`
- `NewClientFromEnv()`
- `NewClientWithOptions(identity, ClientOptions{Protocol: ...})`
- `client.ConnectWithInfo(ctx)`
- `client.ConnectionInfo()`
- `client.Query(ctx, text, options)`
- `client.Ask(ctx, text, options)`
- `client.SendUtterance(ctx, text, options)`
- `client.SendAction(ctx, payload, options)`
- `client.SendCode(ctx, value, options)`
- `client.Conversation(options)`
- `client.Intents(ctx, languages, IntentOptions{Timeout: ..., Describe: ..., Fallback: ...})`
- `client.ListIntents(ctx, lang, IntentOptions{Timeout: ..., IncludeDefinitions: ...})`
- `client.DescribeIntent(ctx, skillID, intentName, lang, IntentOptions{Timeout: ...})`

## Development

```bash
go test ./...
```
