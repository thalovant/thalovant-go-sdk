# Thalovant Go SDK

[![Go reference](https://pkg.go.dev/badge/github.com/thalovant/thalovant-go-sdk.svg)](https://pkg.go.dev/github.com/thalovant/thalovant-go-sdk) [![CI](https://github.com/thalovant/thalovant-go-sdk/actions/workflows/ci.yml/badge.svg)](https://github.com/thalovant/thalovant-go-sdk/actions/workflows/ci.yml) [![Licence](https://img.shields.io/github/license/thalovant/thalovant-go-sdk)](LICENSE) [![Docs](https://img.shields.io/badge/docs-docs.thalovant.com-5c6bc0)](https://docs.thalovant.com/developers/sdks/go/)

Go SDK for connecting services, CLIs, devices, and agents to Thalovant hubs.

The control API is used to discover hubs and provision a client identity. After
that, the SDK talks directly to the hub data plane over HTTPS, WSS, or MQTTS.

Full documentation: <https://docs.thalovant.com/developers/sdks/go/>

## Requirements

- Go 1.26 or newer, so the SDK receives supported upstream networking security fixes.
- A Thalovant account with API access for authenticated control-plane actions, a hub id or slug, and a client identity for that hub (create one through the API or use one downloaded from the dashboard).

## Install

```bash
go get github.com/thalovant/thalovant-go-sdk
```

## Quick start

Store an identity in the protected SDK config (`~/.config/thalovant/config.yaml`, mode `600`; the
SDK rejects config files other users can read or write), then:

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

The config format, signing in, and provisioning a first identity are in the documentation.

## Documentation

| Topic | Where |
| :--- | :--- |
| Install, first request, sign in (MFA, passwordless, API token) | [Go SDK](https://docs.thalovant.com/developers/sdks/go/) |
| Saved identities and where the SDK keeps its keys | [Go SDK](https://docs.thalovant.com/developers/sdks/go/) |
| Choosing a protocol, context, listening for events | [Go SDK](https://docs.thalovant.com/developers/sdks/go/) |
| Listing what a hub can be asked, provisioning hubs | [Go SDK](https://docs.thalovant.com/developers/sdks/go/) |
| Skills through a hub, request helpers, managed sessions, reply claims | [Go SDK](https://docs.thalovant.com/developers/sdks/go/) |
| Common issues | [Go SDK](https://docs.thalovant.com/developers/sdks/go/) |
| All developer documentation | <https://docs.thalovant.com> |

Symbol-level reference is on [pkg.go.dev](https://pkg.go.dev/github.com/thalovant/thalovant-go-sdk).
Release notes are in [CHANGELOG.md](CHANGELOG.md).

## Not yet in the documentation

These topics have no counterpart on the documentation page, so a short version stays here.

- **API errors.** A refused control-plane request returns a `*thalovant.APIError`
  (`errors.Is(err, thalovant.ErrAPI)`). Read `StatusCode`, `Code`, `ProblemDetail`
  (the API's whole sentence) and `Problem` (the whole JSON error body as a `map[string]any`)
  from the error itself; `Error()` is a shortened display line.
- **Hub refusals.** A refusal ends an ask at once instead of at its deadline. Use `errors.As`
  with `*thalovant.PolicyDeniedError` (`Code`, `Quota`, `Allowed`, `DeniedType`) and
  `*thalovant.UnansweredError` (`Said`); `errors.Is(err, thalovant.ErrTimeout)` covers a late hub.
- **Transport security.** `wss`, `https`, and `mqtt` perform the HiveMind v3 Noise handshake. The
  client key (`noise_key`) and pinned hub keys (`noise_pins.json`) persist beside the SDK config
  file; do not regenerate a paired client's key. A hub that pinned another key refuses this one
  with `*thalovant.ClientKeyRejectedError`.

## Development

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race -count=1 ./...
```

## Security

Report vulnerabilities as described in [SECURITY.md](https://github.com/thalovant/.github/blob/main/SECURITY.md).

## Licence

[MIT](LICENSE).
