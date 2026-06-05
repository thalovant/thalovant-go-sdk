# Thalovant Go SDK

Go SDK for direct Thalovant hub HTTPS clients and agents.

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
AES-GCM preshared-key helpers, and an HTTP transport shape compatible with the
Thalovant SDK contract. The live transport targets the preshared-key HTTP path
used by Thalovant public hubs.

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
proxy path, for example `/public`.

## Development

```bash
go test ./...
```
