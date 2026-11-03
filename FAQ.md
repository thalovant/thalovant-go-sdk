Last Edit: Claude Opus 5.5 - 2026-11-03 - Motive: Answer the common chat apps questions for 0.14.0.

# FAQ

## How do I connect a hub to Telegram, Discord, Matrix, Mattermost or Twitch?

Read `ControlPlane.GetBridgeNetworks(ctx, hubID)` first: `Allowance.CanCreate` says whether the plan
has room. Then `CreateBridge` with a `BridgeCreate`: `Token` for Telegram, Discord and Mattermost,
`Secrets` (by key) for Matrix (`access_token`) and Twitch (`refresh_token`, `client_secret`), plus
`Settings` (Matrix `homeserver`, `user_id`; Twitch `bot_login`, `client_id`). The bridge starts in
setup and runs once `UpdateBridge` sets `Allowed`. See the README's "Chat apps" section.

## Why does CreateBridge fail before sending anything?

It needs a hub id, a connector, and exactly one of `Token` and `Secrets`. Both, neither, or an
empty token are refused locally with `ErrAPI`, as the API would refuse them.

## How do I pause or resume a bridge?

`UpdateBridge(ctx, id, thalovant.BridgeUpdate{Paused: new(true)})`, and `new(false)` to resume.
There is no separate route. Resuming answers 403 `plan_limit` while the plan's allowance is full.

## What does a 412 from UpdateBridge mean?

The `ETag` you sent is stale: somebody changed the bridge since you read it. Read it again with
`GetBridge` and retry with its `ETag`. Leave `ETag` empty to send no `If-Match`.

## Which settings or secrets did the API refuse?

`errors.As(err, &apiErr)` and read `apiErr.Fields`: the names (never the values) for
`bridge_settings_invalid`, `bridge_secrets_invalid` and the `bridge_url_*` codes.

## Can a token end up in a log?

Not through the SDK. `SecretValue` prints `[REDACTED]` for every fmt verb, `json.Marshal`,
`MarshalText` and `log/slog`, and errors never carry the request body. `Reveal` returns the value;
call it only where the value must leave your program.

## The API added a status reason my code does not know. Does decoding fail?

No. The enums are string types; an unknown value is kept as sent. Treat an unknown
`BridgeStatusReason` like `Starting`.

## Why does ListBridgeChats return no chats for Matrix?

Connectors without chat discovery answer `ManualEntry: true`. Type the ids yourself; they must
match the connector's `Targets.Pattern` (from `ListChatAppConnectors`).
