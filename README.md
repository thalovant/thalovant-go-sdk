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

Keep `result.Identity` secret. It contains the client credentials used by the
hub. Do not log `result.Summary(true)`.

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
  control-plane API to provision private resources.
- `unsupported protocol`: the hub does not expose that protocol, or the
  identity was created before that protocol was enabled.
- MQTT fails immediately: create or download a fresh client identity after MQTT
  is enabled. MQTT needs the per-client `Identity.MQTT` credentials.
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

## Development

```bash
go test ./...
```
