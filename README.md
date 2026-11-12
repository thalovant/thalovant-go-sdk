Last Edit: Claude Sonnet 5.5 - 2026-11-11 - Motive: Intercom (0.16.0), final API: intercom:urgent, speaker Kind/CanPlay/PolicySynced, typed refusals, one text rule, DND timezone, Replayed.

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

## Intercom

Say a short text aloud on a hub's speakers. Off by default at every level, announce-only,
and **not a certified safety system**: it does not replace smoke or CO alarms, medical
alert devices or emergency services. The API does not store the text (it keeps a keyed hash
and the length), and intercom does not record or store speech: while an alert sounds, the hub
only checks whether a reply is an acknowledgement. (The names are `Announce` and `*Intercom*`:
the HiveMind mesh already has an `intercom` frame.) Scopes: `intercom:read`, `intercom:send`
(announce and cancel; plus `intercom:read` with `Wait`), `intercom:urgent` **in addition to**
`intercom:send` for an urgent announcement (`Priority` urgent, and so every `Repeat`), and
`hubs:write` for settings and speakers. `intercom:urgent` is implied by nothing and implies
nothing; a token without it is refused before anything is spent (`ErrUrgentScopeRequired`), so
request it only when you offer an urgent control. Intercom ships dark: until the platform
operator turns it on, `GetIntercomSettings` reads `UnavailableReason` `"platform"` for every hub
and every write and announcement is `ErrIntercomPlatformDisabled` (503; nothing was stored or
sent; reads and `CancelAnnouncement` keep working).

```go
_, _ = api.SetIntercomSettings(ctx, hubID, true) // hubs:write
speakers, _ := api.ListIntercomSpeakers(ctx, hubID) // intercom:read; a speaker is addressed by its ClientID
speaker := speakers.Items[0] // Kind, CanPlay; speakers.PolicySynced, speakers.UnavailableReason
yes, kitchen := true, "kitchen"
_, _ = api.UpdateIntercomSpeaker(ctx, hubID, speaker.ClientID, thalovant.IntercomSpeakerUpdate{Receive: &yes, Room: &kitchen})

// A fresh unique key for each announcement you mean to send (see "Idempotency keys" below):
// NewRequestID is 128 random bits; a UUID (github.com/google/uuid) works as well.
said, err := api.Announce(ctx, hubID, thalovant.AnnounceOptions{
	Text: "Dinner is ready", Targets: thalovant.SpeakerTargets(speaker.ClientID), // or RoomTargets("kitchen")
	IdempotencyKey: thalovant.NewRequestID(),
})
urgent, err := api.Announce(ctx, hubID, thalovant.AnnounceOptions{
	Text: "Please answer", Targets: thalovant.AllSpeakers(), Priority: thalovant.IntercomPriorityUrgent,
	Repeat: &thalovant.IntercomRepeat{Times: 3, IntervalSeconds: 20}, IdempotencyKey: thalovant.NewRequestID(),
	Wait: true, // polls while "sending"; needs intercom:read as well, and intercom:urgent for urgent
})

// Changed your mind, or sent it to the wrong room? Stop it while it is still repeating.
sent, _ := api.Announce(ctx, hubID, thalovant.AnnounceOptions{
	Text: "Wake up", Targets: thalovant.AllSpeakers(), Priority: thalovant.IntercomPriorityUrgent,
	Repeat: &thalovant.IntercomRepeat{Times: 10, IntervalSeconds: 60}, IdempotencyKey: thalovant.NewRequestID(),
})
stopped, err := api.CancelAnnouncement(ctx, hubID, sent.ID) // intercom:send
// stopped.Status == "cancelled"; stopped.HubConfirmed is a *bool: false when the hub could not confirm
```

### Idempotency keys: one fresh key per announcement

Set `IdempotencyKey` to a fresh unique key (a UUID) for every announcement you mean to send.
The API scopes a key to `(hub, key)` and remembers it for 24 hours:

- **The same key replays the stored announcement** (the response header is
  `Idempotency-Replayed: true`, which the SDK reads into `Announcement.Replayed`) and **never
  sends again**, whatever its status. A replayed `failed` announcement stays `failed`; it is not
  retried. The same key with a different request is a 409. A key is at most 128 printable ASCII
  characters. Do not clear what a person typed, or say "sent", on `Replayed` without saying that
  nothing new was sent.
- **A constant key silences you.** A literal key such as `"dinner"` sends once; every later
  announcement with that key, for 24 hours, returns the first one's answer and nothing is
  spoken. Never copy a literal key into real code, and never derive it from the text or the date.
- **Reuse a key only for the same attempt**, after a network drop or a timeout: the call then
  returns what the first attempt did (and its id) instead of speaking twice.
- **To send the same text again on purpose, use a new key.**
- **After a 502 `hub_unreachable`** (`errors.Is(err, ErrHubUnreachable)`) the hub may or may not
  have spoken. The API recorded the attempt as a `failed` announcement and puts its id in the
  problem body: `apiErr.AnnouncementID()`. Read it with `GetAnnouncement` and look at the
  deliveries before you send again. Resending with the **same** key will not retry (it replays
  the `failed` record); a deliberate second send needs a **new** key.

```go
key := thalovant.NewRequestID()
_, err := api.Announce(ctx, hubID, thalovant.AnnounceOptions{Text: "Dinner is ready", Targets: thalovant.RoomTargets("kitchen"), IdempotencyKey: key})
var apiErr *thalovant.APIError
if errors.Is(err, thalovant.ErrHubUnreachable) && errors.As(err, &apiErr) && apiErr.AnnouncementID() != "" {
	recorded, _ := api.GetAnnouncement(ctx, hubID, apiErr.AnnouncementID()) // what did the hub do?
	fmt.Println(recorded.Status, recorded.Deliveries)
}
```

### Stopping an announcement

An urgent announcement that repeats can be stopped. These are the real controls:

- **`CancelAnnouncement(ctx, hubID, announcementID)`** (`intercom:send`, owner or admin of the
  hub) asks the hub to stop repeating and returns the announcement as `cancelled`. It works only
  while the status is `sending`; for one that already completed, failed or was cancelled the API
  answers 409 `not_cancellable` (`errors.Is(err, ErrNotCancellable)`). If the hub cannot confirm
  the cancel, the record is still `cancelled` and `HubConfirmed` points to `false`: speech already
  queued on the hub may still play. `apiErr.AnnouncementStatus()` is the current status: refresh
  the result instead of showing an error. Stopping is never refused for platform, hub or plan
  reasons: it works while the platform switch is off (`HubConfirmed` is then `false`, no hub was
  called).
- **The hub's Intercom switch** (`SetIntercomSettings(ctx, hubID, false)`, `hubs:write`) takes
  effect when the hub's runtime updates, which can take as long as a rollout; the hub's repeat
  loop also stops by itself when it sees the switch off.
- **The platform kill switch** (operator side) and a speaker whose `Receive` is turned off also
  stop the repeats.
- **Somebody answering at a speaker.** An urgent announcement stops being repeated once it is
  acknowledged by voice ("I'm okay", "got it", "stop").

Nothing here is instant for sound that is already playing, and none of it makes intercom a
safety system.

### Ids, timeouts and printing options

The hub id, a speaker's client id and an announcement id are each one path segment. The SDK
refuses (an error that matches `ErrAPI`) and sends nothing when an id is empty or blank, is `.`
or `..` (dot segments), or holds `/`, `?`, `#`, a backslash or a control character. The API waits
up to ten seconds for the hub, and this SDK sets no request timeout of its own: a context or an
`HTTPClient.Timeout` shorter than 20 seconds can report a spoken announcement as failed, so give
`Announce` at least 20 seconds. `AnnounceOptions` redacts its text under every `fmt` verb and in
`log/slog` (`<N chars>`), so printing or logging the options never prints what is spoken aloud.

`Wait` needs **`intercom:read` as well as `intercom:send`** (the polls are `GetAnnouncement`).
`WaitTimeout` and `PollInterval` must not be negative (zero is the default); they are checked
before anything is sent, and `WaitTimeout` bounds each poll, not only the sleep between polls.
A poll can fail after the announcement was accepted (a token with only `intercom:send` is
refused at the first poll; a 429; the network): `Announce` then returns the accepted
announcement (its `ID` is set) together with the error. Always set a fresh `IdempotencyKey` with
`Wait`: calling `Announce` again with that same key replays the stored announcement (with its
`ID`) and does not announce a second time.

A speaker's `IntercomDNDUpdate.Enabled` is a `*bool` and required: the API replaces the whole
do-not-disturb setting and stores a missing `enabled` as `false`, so an update that only
carries `QuietHours` must still say whether DND is on. DND that is switched on needs
`QuietHours` with a `Timezone` that is a valid IANA name (at most 64 characters, e.g.
`Europe/Paris`) and a `Start` that differs from the `End`; a `Timezone` that is given is checked
even when DND is off. `IntercomSpeakerUpdate` has exactly `Receive`, `Room`/`ClearRoom`, `Groups`
and `DND`: send only what changed. All of that is checked before anything is sent.

### Speakers: `Kind`, `CanPlay` and `PolicySynced`

`ListIntercomSpeakers` returns an `IntercomSpeakerList`: every connection of the hub (the
platform's own Intercom client is not listed) in `Items`, plus `PolicySynced` and
`UnavailableReason`. Each `IntercomSpeaker` has:

- `Kind`: the connection kind (`voice_satellite`, `web_chat`, `developer`, `embedded`,
  `home_assistant`; anything else reads `voice_satellite`).
- `CanPlay`: the **effective** "can this connection play speech". True by default for a
  `voice_satellite` and `web_chat`; false for `home_assistant`, `developer` and `embedded`
  unless its owner declared `spec.intercom.can_play: true` on the **connection**
  (`PATCH /v1/clients/{id}`, never on the speaker). A connection that cannot play is listed (grey
  it out) but `Receive: true` is refused with `ErrNotASpeaker` (422 `not_a_speaker`,
  `apiErr.SpeakerKind()`), it is excluded from `all`, rooms and groups, and naming it in
  `Targets.Speakers` is the same error with `apiErr.NotSpeakers()`.
- `PolicySynced` (`*bool`): set on the answer to `UpdateIntercomSpeaker`, nil on list items; the
  list has its own. `false` means the hub may hold an out-of-date consent map (a push failed and
  is retried in the background), so a speaker whose `Receive` was just turned off can still be
  spoken to by a routine until it clears: show "Changes may take a moment to reach the home".

### The text and the targets

`Announce` checks the text with the API's own rule before anything is sent, so a refusal never
costs a request and never echoes the text: trimmed, 1 to 280 characters, no `<` or `>` (a
speaker's text-to-speech may read them as markup), no control characters (a newline or a tab
included), no bidirectional overrides or isolates and no other invisible format characters (the
zero-width joiner and non-joiner are allowed), no invalid UTF-8 (lone surrogates), no private-use
characters and no line or paragraph separators. **One difference from the Python and Node SDKs:**
this one does not NFC-normalise the text first (the Go standard library has no normaliser and
`golang.org/x/text` is not a dependency), so the 280 characters are counted as sent and a text in
decomposed form can be refused here that the API would accept once composed. `Lang`, when set, is
a language tag (`en`, `en-US`). Target lists hold at most 50 names of 1 to 64 characters; `all`,
a room or a group that resolves to more than 50 speakers is `ErrTooManyTargets` (422,
`apiErr.Matched()`): name a room or a group.

### What an answer means

`DeliveryDelivered` means the hub handed the speech to that speaker's connection. There is **no
playback receipt**: do not say "heard" or "played". Only `DeliveryAcked` (an urgent announcement
somebody answered) is a person's answer, and `DeliveryFailed` is not proof that nothing was
spoken. A delivery's `Detail` is a fixed code (`KnownDeliveryDetail`: `hub_no_response`,
`rate_limited`, `skill_not_installed`, ...) or `""`; treat a code you do not know as opaque.
`AckedBy` is the display name of a client of the hub, or `"unknown"`. A hub that does not answer
within 5 seconds ends `failed` with every delivery `hub_no_response`; a repeating urgent
announcement is failed on read once it is older than `times x interval + 90 s`.

At most two urgent announcements repeat on one hub at a time (the platform has a pool too): the
next is `ErrUrgentBusy` (429, `apiErr.UrgentScope()` `hub` or `platform`, `RetryAfter`) with
nothing stored and no rate token spent: cancel one that is running, or wait. An urgent
announcement on a hub whose runtime has no active Intercom skill is `ErrSkillNotInstalled` (409):
nothing was sent.

### Errors

Refusals are `*APIError`s, and the `IntercomCode*` constants name each problem code. Problems are
`application/problem+json`: read `Code`, then `Problem["message"]` (or `ProblemDetail`).
`errors.Is` reaches:

| Sentinel | When |
| :--- | :--- |
| `ErrIntercomDisabled` | `intercom_disabled`: the hub's switch is off (the next two are kinds of it) |
| `ErrIntercomUnavailable` | 422 `intercom_unavailable`: a kid hub or a public hub; `apiErr.Reason()` |
| `ErrIntercomPlatformDisabled` | 503 `intercom_disabled_platform`: switched off for the whole platform |
| `ErrIntercomNotConfigured` | 503 `intercom_not_configured`: the operator has not finished setting it up |
| `ErrUrgentScopeRequired` | 403 `urgent_scope_required`: needs `intercom:urgent` too (a kind of `ErrAuth`) |
| `ErrUrgentBusy` | 429 `intercom_urgent_busy`: the urgent pool is full (a kind of `ErrRateLimited`) |
| `ErrSkillNotInstalled` | 409 `skill_not_installed`: an urgent announcement on a hub with no Intercom skill |
| `ErrNotASpeaker` | 422 `not_a_speaker`: `Receive` (`SpeakerKind()`) or a `Speakers` target (`NotSpeakers()`) on a connection that cannot play speech |
| `ErrTooManyTargets` | 422 `too_many_targets`: more than 50 speakers resolved; `Matched()` |
| `ErrReservedName` | 422 `reserved_name`: a connection named like the platform's Intercom client |
| `ErrManagedClient` | 403 `managed_client`: the platform's Intercom client cannot be managed; `RefusedClientRefs()` |
| `ErrHubLocked` | 403 `hub_locked`: nothing was changed |
| `ErrImpersonationNotAllowed` | 403 `impersonation_not_allowed`: an impersonated session cannot announce |
| `ErrHubUnreachable` | 502 `hub_unreachable`: the text may or may not have been spoken; `AnnouncementID()` names the recorded `failed` announcement: read it before sending again, and use a new key, because the same key replays the failed record |
| `ErrNotCancellable` | 409 `not_cancellable`; `AnnouncementStatus()` |
| `ErrRateLimited` | 429 (a hub: 20 normal a minute, 10 urgent an hour); `RetryAfter` |
| `ErrPlan`, `ErrAuth` | HTTP 402 or 403 `plan_limit`; a missing token or scope |

`owner_locked` and `account_locked` (403) arrive as a plain `*APIError` with `Code` set. The
intercom codes decide by code, whatever the status.

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
