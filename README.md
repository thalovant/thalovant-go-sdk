# Thalovant Go SDK

Go SDK for direct Thalovant HiveMind HTTPS clients and agents.

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
Thalovant SDK contract. The live transport targets the preshared-key HiveMind
HTTP path used by Thalovant public hubs.

## Development

```bash
go test ./...
```
