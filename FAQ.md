Last Edit: Claude Opus 5.5 - 2026-11-08 - Motive: Answer the common browser sign-in questions for 0.15.0.

# FAQ

## How do I sign a person in from a CLI or a desktop tool?

`ControlPlane.InteractiveLogin(ctx, thalovant.InteractiveLoginOptions{})`. On a machine with a browser it
signs in through that browser (`BrowserLogin`: PKCE on `http://127.0.0.1:<port>/callback`); over SSH, in
CI, or on Linux without `DISPLAY`/`WAYLAND_DISPLAY` it uses a device login (`LoginWithBrowser`). Both keep
the token on the `ControlPlane` (`AccessToken`, `TokenID`) and return the token answer.

## Why did InteractiveLogin use a device login on my desktop?

Either `CanOpenBrowser(nil, "")` is false (a non-empty `CI`, `SSH_CONNECTION` or `SSH_TTY`, even `CI=false`,
or no display on Linux and the BSDs), or `GET /v1/auth/native/config` did not answer a usable
configuration: an API older than browser sign-in answers 404, and a cleartext API URL (not https, not
loopback http) counts too. Pass `Method: thalovant.LoginMethodBrowser` to force the browser and see the
error.

## The browser did not open. What now?

The URL is always printed as well (`Opening your browser to sign in to Thalovant. If it does not open,
visit:`); open it in a browser on the same machine. A browser on another machine cannot reach
`127.0.0.1` here, which is the point: a link sent to somebody else delivers nothing to the sender.

## A stray tab hit the callback. Did it end the sign-in?

No. A request without this sign-in's `state` is a 400 and the wait goes on; anything but `GET /callback`
is a 404. Only the first callback with the right state is accepted.

## How do I show the URL myself, or open it with my own launcher?

`BrowserLoginOptions.Prompt` receives the URL instead of the printed message, and `Opener` replaces the
platform browser (it is called on its own goroutine, so a console browser can hold it). Set
`OpenBrowser` to a false pointer to only show the URL.

## Which errors can a browser sign-in return?

`errors.Is(err, thalovant.ErrDeviceAccessDenied)` when the person denies, `ErrDeviceCodeExpired` when
nobody answers within `Timeout` (default 5 minutes), `*thalovant.BrowserLoginFailedError` when the browser
came back without a code, an `*APIError` with status 400 and `ProblemDetail` `invalid_grant` when the API
refuses the code, and `ctx.Err()` when `ctx` is done. A code is single-use: start a new sign-in instead
of retrying.

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
