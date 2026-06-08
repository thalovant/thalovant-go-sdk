# Thalovant Go SDK

Go SDK for direct Thalovant hub data-plane clients and agents.

Full documentation: <https://docs.thalovant.com/developers/sdks/go/>

```bash
go get github.com/thalovant/thalovant-go-sdk
```

```go
package main

import (
	"context"
	"fmt"

	thalovant "github.com/thalovant/thalovant-go-sdk"
)

func main() {
	client, err := thalovant.NewClientFromFile("_identity.json")
	if err != nil {
		panic(err)
	}
	reply, err := client.Ask(context.Background(), "Tell me a short clean joke.", thalovant.RequestOptions{})
	if err != nil {
		panic(err)
	}
	fmt.Println(reply.Text)
}
```

## Status

This is an alpha SDK scaffold with identity, event, session, conversation,
AES-GCM preshared-key helpers, protocol endpoint helpers, and an HTTP transport
shape compatible with the Thalovant SDK contract. The live transport targets
the preshared-key HTTPS HTTP-protocol path used by Thalovant public hubs.

## Protocols

Identity or hub payloads may include `data_plane_endpoints` for `https`, `wss`,
and `mqtt`, plus `protocols.wss/http/mqtt.enabled` flags. When MQTT is enabled
for the hub, identity payloads may also include a client-scoped `mqtt` block
with `endpoint`, `username`, `password`, and `topic_prefix`.

```go
identity, err := thalovant.IdentityFromFile("_identity.json")
if err != nil {
	panic(err)
}

fmt.Println(identity.EnabledProtocols())
fmt.Println(identity.EndpointFor(thalovant.ProtocolHTTPS))
fmt.Println(identity.EndpointFor(thalovant.ProtocolWSS))
fmt.Println(identity.EndpointFor(thalovant.ProtocolMQTT))
```

You can also create a hub client through the Thalovant API:

```go
control := thalovant.NewControlPlane("https://dash.thalovant.com/api", "")
_, _ = control.Login(ctx, "you@example.com", "password", "")

result, err := control.CreateClientIdentityForHubID(ctx, "hub-id", thalovant.BootstrapIdentityOptions{
	Name: "kiosk-1",
})
if err != nil {
	panic(err)
}

client := thalovant.NewClient(result.Identity)
```

The SDK generates `apiKey`, `password`, and `cryptoKey` locally and sends them
to the API once. The API can store them in Vault and return only secret
references. When MQTT is enabled, `result.Identity.MQTT` contains the broker
credentials returned by the API. Do not log `result.Summary(true)`.

## Generic Client Context

```go
context := thalovant.BuildClientContext(nil, thalovant.ClientContextOptions{
	UserID:       "user-42",
	UserName:     "Ada",
	AuthProvider: "oidc",
	Roles:        []string{"member"},
	Platform:     "kiosk",
	Source:       "checkout-kiosk",
	Channel:      "chat",
})

reply, err := client.Ask(ctx, "Show the next instruction.", thalovant.RequestOptions{Context: context})
```

## Actions, Codes, And Rich Output

```go
conversation := client.Conversation(thalovant.ConversationOptions{SessionID: "work-session"})

_ = conversation.SendAction(ctx, `/choose{"id":"42"}`, thalovant.ActionOptions{Title: "Choose item"})
_ = conversation.SendCode(ctx, "SN-001-XYZ", thalovant.CodeOptions{Kind: "qr", Label: "serial"})

items := reply.DisplayItems(600)
```

Identity files may include `default_path` for hubs exposed behind a reverse
proxy path, for example `/public`. Newer identities should prefer explicit
`data_plane_endpoints` when the API provides them.

## Development

```bash
go test ./...
```
