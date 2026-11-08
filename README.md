Last Edit: Claude Opus 5.5 - 2026-11-08 - Motive: Document browser sign-in (InteractiveLogin, BrowserLogin) for 0.15.0, final reference wording.

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/thalovant/.github/main/profile/assets/thalovant-logo-dark.png">
    <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/thalovant/.github/main/profile/assets/thalovant-logo-light.png">
    <img src="https://raw.githubusercontent.com/thalovant/.github/main/profile/assets/thalovant-logo-light.png" alt="Thalovant" width="320">
  </picture>
</p>

# Thalovant Go SDK

[![Go reference](https://pkg.go.dev/badge/github.com/thalovant/thalovant-go-sdk.svg)](https://pkg.go.dev/github.com/thalovant/thalovant-go-sdk) [![CI](https://github.com/thalovant/thalovant-go-sdk/actions/workflows/ci.yml/badge.svg)](https://github.com/thalovant/thalovant-go-sdk/actions/workflows/ci.yml) [![Licence](https://img.shields.io/github/license/thalovant/thalovant-go-sdk)](LICENSE) [![Docs](https://img.shields.io/badge/docs-docs.thalovant.com-5c6bc0)](https://docs.thalovant.com/developers/sdks/go/)

Go SDK for connecting services, CLIs, devices, and agents to Thalovant hubs.

The control API is used to discover hubs and provision a client identity. After
that, the SDK talks directly to the hub data plane over HTTPS, WSS, or MQTTS.

Full documentation: <https://docs.thalovant.com/developers/sdks/go/>

## Requirements

- Go 1.26.9 or newer (Go 1.27.2 or newer on the 1.27 line), so the SDK builds with a standard library that has the upstream HTTP/2 security fixes (GO-2026-6611).
- A Thalovant account with API access for authenticated control-plane actions, a hub id or slug, and a client identity for that hub (create one through the API or use one downloaded from the dashboard).

## Install

```bash
go get github.com/thalovant/thalovant-go-sdk
```

## Quick start

Store an identity in the protected SDK config (`~/.config/thalovant/config.yaml`, mode `600`; the
SDK rejects config files other users can read or write), then:

```go
package main

import (
	"context"
	"fmt"

	"github.com/thalovant/thalovant-go-sdk"
)

func main() {
	ctx := context.Background()

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
}
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
  (the API's whole sentence), `Fields` (the settings or secrets a refusal names) and `Problem`
  (the whole JSON error body as a `map[string]any`)
  from the error itself; `Error()` is a shortened display line.
- **Hub refusals.** A refusal ends an ask at once instead of at its deadline. Use `errors.As`
  with `*thalovant.PolicyDeniedError` (`Code`, `Quota`, `Allowed`, `DeniedType`) and
  `*thalovant.UnansweredError` (`Said`); `errors.Is(err, thalovant.ErrTimeout)` covers a late hub.
- **Transport security.** `wss`, `https`, and `mqtt` perform the HiveMind v3 Noise handshake. The
  client key (`noise_key`) and pinned hub keys (`noise_pins.json`) live in `NoiseStateDir` when you
  set it; otherwise in the directory of the identity file the SDK read, or, for an identity that
  did not come from a file, beside the SDK config file. Back up that directory, not only the
  config directory, and do not regenerate a paired client's key. A hub that pinned another key
  refuses this one with `*thalovant.ClientKeyRejectedError`.

## Signing in through the browser

On a machine with a browser, sign a person in through that browser. The SDK listens on
`127.0.0.1` on a port the system picks, opens the dashboard's consent page, and the browser that
approves hands the code back to this computer (OAuth authorization code with PKCE on a loopback
redirect, RFC 8252). Unlike a device code, which can be read off one screen and typed in
anywhere, a link or code sent to someone on another computer delivers nothing to the sender. (A
forwarded callback port, such as `ssh -L`, or another user of the same computer can still complete
one.)

```go
ctx := context.Background()
control := thalovant.NewDefaultControlPlane("")
token, err := control.InteractiveLogin(ctx, thalovant.InteractiveLoginOptions{})
if err != nil {
	panic(err)
}
fmt.Println("signed in; token", control.TokenID, "scopes", token["scopes"])
```

- `InteractiveLogin` uses the browser when the machine can show one (`CanOpenBrowser`: never with
  `CI`, `SSH_CONNECTION` or `SSH_TTY` set; macOS and Windows yes; elsewhere only with `DISPLAY` or
  `WAYLAND_DISPLAY`) and the API offers it (`GET /v1/auth/native/config`), and a device login
  (`LoginWithBrowser`) otherwise. `Method: thalovant.LoginMethodBrowser` or
  `thalovant.LoginMethodDevice` forces one. Once the browser was opened, its outcome stands: it
  never turns into a device login.
- `BrowserLogin(ctx, thalovant.BrowserLoginOptions{...})` is the browser sign-in on its own:
  `Scopes` (with none, no `scope` is sent and the API grants its default, `hubs:read
  clients:write` today), `DeviceName` (nil gives `thalovant-go-sdk on <hostname>`; `new("")`
  sends no name), `OpenBrowser`, `Timeout` (default 5 minutes), `Prompt` and `Opener` (replaces
  the platform browser; it runs on its own goroutine).
- Without `Prompt`, the URL is printed to stdout whether or not the browser opens, since opening a
  browser can fail silently. With `Prompt`, nothing is printed: `Prompt` receives the URL instead,
  so show it there. `InteractiveLogin` takes no `Prompt` and always prints.
- The token is kept exactly as a device login keeps it (`AccessToken`, `TokenID`), and
  `RevokeAPIToken(ctx, "")` revokes it.
- Errors: `errors.Is(err, thalovant.ErrDeviceAccessDenied)` when the person denies,
  `thalovant.ErrDeviceCodeExpired` when nobody answers in time, a `*thalovant.BrowserLoginFailedError`
  when the browser came back without a code, and the `*thalovant.APIError` (400, `invalid_grant`)
  when the API will not exchange the code. An API without browser sign-in answers the
  configuration 404. A cancelled `ctx` closes the port. The code, the PKCE verifier, the state and
  the token never appear in an error.

## Chat apps

A hub owner can connect a chat network to a hub, and the hub answers there. Telegram and Discord
are offered; Matrix, Mattermost and Twitch are in beta. The calls are on `ControlPlane`. Reads need
a token with `hubs:read`, writes `hubs:write`.

Start with the allowance. A Free plan allows no bridge (`PaidPlan` is false, and creating one
answers 402), and a full allowance answers 403 `plan_limit`. `ListChatAppConnectors` says which
secrets and settings each network takes.

```go
cp := thalovant.NewDefaultControlPlane(os.Getenv("THALOVANT_API_TOKEN"))

networks, err := cp.GetBridgeNetworks(ctx, hubID)
if err != nil {
	return err
}
if !networks.Allowance.CanCreate {
	return fmt.Errorf("plan %s allows %d chat bridges, %d in use",
		networks.Allowance.Plan, networks.Allowance.Limit, networks.Allowance.Used)
}
```

Credentials go in a `thalovant.SecretValue`. Printing one, encoding it as JSON or logging it with
`log/slog` gives `[REDACTED]`; the SDK reads the value only to build the request body.

**Telegram** (and Discord): one bot token. The bridge starts in setup, and runs once it has chats.

```go
bridge, err := cp.CreateBridge(ctx, thalovant.BridgeCreate{
	HubID:     hubID,
	Connector: "telegram",
	Token:     thalovant.NewSecretValue(os.Getenv("TELEGRAM_BOT_TOKEN")),
})
if err != nil {
	return err
}
// Add the bot to a group with bridge.InviteLinks.AddToGroup, then choose where it answers.
chats, err := cp.ListBridgeChats(ctx, bridge.ID)
if err != nil {
	return err
}
var allowed []thalovant.BridgeAllowedChat
for _, chat := range chats.Data {
	if chat.Selectable && len(allowed) < 50 { // a bridge answers in at most 50 chats
		allowed = append(allowed, chat.AllowedChat())
	}
}
bridge, err = cp.UpdateBridge(ctx, bridge.ID, thalovant.BridgeUpdate{Allowed: allowed, ETag: bridge.ETag})
if err != nil {
	return err
}
_, err = cp.SendBridgeTestMessage(ctx, bridge.ID, allowed[0].ID)
```

**Matrix**: settings for the homeserver and the bot's user, and the access token as a secret. A
connector without chat discovery answers `ManualEntry: true`: type the room ids
(`!abc:example.org`), which must match the connector's `Targets.Pattern`.

```go
bridge, err := cp.CreateBridge(ctx, thalovant.BridgeCreate{
	HubID:     hubID,
	Connector: "matrix",
	Secrets: map[string]thalovant.SecretValue{
		"access_token": thalovant.NewSecretValue(os.Getenv("MATRIX_ACCESS_TOKEN")),
	},
	Settings: map[string]any{"homeserver": "https://matrix.example.org", "user_id": "@hub:example.org"},
})
if err != nil {
	return err
}
bridge, err = cp.UpdateBridge(ctx, bridge.ID, thalovant.BridgeUpdate{
	Allowed: []thalovant.BridgeAllowedChat{{ID: "!abc:example.org", Title: "Family"}},
})
```

**Twitch**: the bot's login and the application's client id as settings, the refresh token and
the client secret as secrets. Targets are channel logins; `#Name` is stored as `name`.

```go
bridge, err := cp.CreateBridge(ctx, thalovant.BridgeCreate{
	HubID:     hubID,
	Connector: "twitch",
	Secrets: map[string]thalovant.SecretValue{
		"refresh_token": thalovant.NewSecretValue(os.Getenv("TWITCH_REFRESH_TOKEN")),
		"client_secret": thalovant.NewSecretValue(os.Getenv("TWITCH_CLIENT_SECRET")),
	},
	Settings: map[string]any{"bot_login": "myhubbot", "client_id": os.Getenv("TWITCH_CLIENT_ID")},
})
```

Pause and resume with `UpdateBridge`; there is no separate call. Replace credentials with
`ReplaceBridgeToken` or `ReplaceBridgeSecrets` (secrets not named stay as stored), and remove a
bridge with `DeleteBridge`.

```go
_, err = cp.UpdateBridge(ctx, bridge.ID, thalovant.BridgeUpdate{Paused: new(true)})
_, err = cp.UpdateBridge(ctx, bridge.ID, thalovant.BridgeUpdate{Paused: new(false)})
```

`bridge.Status.Reason` is the stable word to branch on (a value added later is kept as sent;
treat an unknown one like `Starting`):

| Reason | Meaning |
| :--- | :--- |
| `Setup` | No chats chosen yet. |
| `Starting` | Starting, or not reported yet. |
| `Connected` | Running and answering. |
| `Paused` | Paused by the owner, or held by the platform (see `Hold`). |
| `Unavailable` | Started but not running, or a platform-side error. |
| `TokenMissing` | The stored credential has not reached the bridge yet. |
| `TokenRejected` | The network refused the credentials: replace them. |

`bridge.Hold` is nil when the owner paused the bridge. The platform sets it when it paused the
bridge itself: `plan_limit` (the plan allows fewer running bridges; it lifts on its own when the
plan has room again, or resume it then) or `account_suspended` (it resumes when the account is
reactivated).

A refusal is a `*thalovant.APIError`. Branch on `Code`; `Fields` names the settings or secrets it
is about, never their values. `errors.Is(err, thalovant.ErrPlan)` covers 402 and 403
`plan_limit`, and `RetryAfter` is set on a 429.

| Status | Codes |
| :--- | :--- |
| 409 | `bridge_bot_in_use`, `bridge_helper_unavailable`, `bridge_token_missing`, `bridge_token_other_bot`, `bridge_running` (pause before listing Telegram chats), `bridge_telegram_updates_busy` |
| 422 | `bridge_token_malformed`, `bridge_token_invalid`, `bridge_token_not_a_bot`, `bridge_network_refused`, `bridge_allowed_required`, `bridge_chat_id_invalid`, `bridge_chat_not_allowed`, `bridge_settings_invalid`, `bridge_secrets_invalid`, `bridge_url_invalid`, `bridge_url_port_not_allowed`, `bridge_url_not_public`, `bridge_url_unresolvable`, `bridge_too_many_targets`, `bridge_mention_required` |
| 402, 403 | Free plan (no code); `plan_limit` with `resource: "chat_bridge"` |
| 412 | `ETag mismatch`: read the bridge again and retry with its `ETag` |
| 429 | `bridge_rate_limited`, `bridge_network_rate_limited` |
| 502, 503, 504 | `bridge_network_unavailable`, `bridge_secret_store_failed`, `bridge_network_not_offered`, `bridge_secret_store_unavailable`, `bridge_network_timeout` |

## Development

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race -count=1 ./...
```

## Security

Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Licence

[MIT](LICENSE).
