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

	reply, err := client.Ask(ctx, "Tell me a short clean joke.", thalovant.RequestOptions{})
	if err != nil {
		panic(err)
	}
fmt.Println(reply.Text)
}
```

`NewDefaultControlPlane` uses `https://api.thalovant.com`. Use
`NewControlPlane` only for local development or a self-hosted control plane.

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

## Use An Existing Identity

If you already downloaded an identity from the dashboard or stored one from a
previous provisioning step:

```go
client, err := thalovant.NewClientFromFile("_identity.json")
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

Environment variables are supported too:

```go
client, err := thalovant.NewClientFromEnv()
```

## Protocols

Hubs may expose one or more public data-plane protocols:

- `wss`: secure realtime WebSocket, the default public path.
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

MQTT identities include a broker endpoint, username, password, TLS flag, and
topic prefix. The broker credentials are scoped to that client and should be
treated like a password.

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

- `missing access token`: call `control.Login(...)` before private
  control-plane actions, or pass an access token to `NewControlPlane`.
- `API access requires a paid plan`: upgrade the workspace before using the SDK
  control-plane API to provision private resources.
- `unsupported protocol`: the hub does not expose that protocol, or the
  identity was created before that protocol was enabled.
- MQTT fails immediately: create or download a fresh client identity after MQTT
  is enabled. MQTT needs the per-client `Identity.MQTT` credentials.
- A request times out: set `RequestOptions{Timeout: ...}`.

## API Shape

- `NewDefaultControlPlane(accessToken)`
- `NewControlPlane(apiURL, accessToken)` for local or self-hosted control planes
- `control.Login(ctx, email, password, scope)`
- `control.ListPublicHubs(ctx, limit, cursor)`
- `control.GetPublicHub(ctx, hubRef)`
- `control.ListHubs(ctx, limit, cursor, ownerID)`
- `control.GetHub(ctx, hubID)`
- `control.CreateClientIdentityForHubID(ctx, hubID, options)`
- `IdentityFromFile(path)`
- `NewClientFromFile(path)`
- `NewClientFromEnv()`
- `NewClientWithOptions(identity, ClientOptions{Protocol: ...})`
- `client.Ask(ctx, text, options)`
- `client.SendUtterance(ctx, text, options)`
- `client.SendAction(ctx, payload, options)`
- `client.SendCode(ctx, value, options)`
- `client.Conversation(options)`

## Development

```bash
go test ./...
```
