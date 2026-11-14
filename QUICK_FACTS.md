Last Edit: Claude Sonnet 5.5 - 2026-11-11 - Motive: Intercom final API: urgent scope, speaker Kind/CanPlay/PolicySynced, typed refusals, one text rule, Replayed.

# Quick Facts

| Item | Value |
| :--- | :--- |
| Module | `github.com/thalovant/thalovant-go-sdk` (package `thalovant`) |
| Version | 0.17.0 (`VERSION` and `const Version` in `version.go`, kept equal by `TestVersionMatchesVersionFile`; the Auto Release workflow bumps both for a patch release) |
| Go | 1.26.9 or newer (`go.mod`) |
| Data-plane entry points | `NewClient`, `NewClientFromConfig`, `Client.Ask`, `Client.Intents`, `HubSession` |
| Control-plane entry points | `NewControlPlane`, `NewDefaultControlPlane`, methods on `*ControlPlane` (`control.go`, `hub_skills.go`, `chat_apps.go`, `browser_login.go`) |
| Sign-in entry points | `InteractiveLogin` (chooser), `BrowserLogin` (browser, PKCE on a loopback redirect), `LoginWithBrowser` (device login, unchanged), `BeginDeviceLogin`/`PollDeviceLogin`, `RevokeAPIToken` |
| Errors | `*APIError` (`StatusCode`, `Code`, `ProblemDetail`, `Problem`, `Fields`, `RetryAfter`); `ErrAPI`, `ErrAuth`, `ErrPlan`, `ErrAPIUnreachable` (`errors.go`) |
| Validation | `test -z "$(gofmt -l .)"`, `go vet ./...`, `go test -race -count=1 ./...` |
| Parity contract | `contracts/sdk-parity.json`, checked by the Python SDK's `scripts/check-sdk-parity.py --consumer thalovant-go-sdk`; reference digests accepted: `8687c8429e28…` (connector-connections candidate), `57c7e71f6456…` and `6dfb3bb31ac7…` (browser-login final candidate, Python 39e31d8) |
| Conformance results | `contracts/conformance-results.json`, written by `THALOVANT_CONFORMANCE_OUT=contracts/conformance-results.json go test ./...` |

## Browser sign-in (`browser_login.go`, `loopback.go`)

| Item | Value |
| :--- | :--- |
| Methods on `*ControlPlane` | `BrowserLogin(ctx, BrowserLoginOptions) (map[string]any, error)`, `InteractiveLogin(ctx, InteractiveLoginOptions) (map[string]any, error)` |
| `LoginMethod` | `LoginMethodAuto` (`"auto"`, default), `LoginMethodBrowser` (`"browser"`), `LoginMethodDevice` (`"device"`) |
| Options | `Scopes`, `DeviceName *string` (nil is the default name, `new("")` sends none), `OpenBrowser *bool` (nil is true), `Timeout`, `Prompt func(url string)` (browser only), `Opener func(url string)`, `Method` (chooser only) |
| Routes | `GET /v1/auth/native/config` (no auth; 404 = no browser sign-in), `POST {token_endpoint}` with exactly `code`, `code_verifier`, `client_id`, `redirect_uri` |
| Redirect | `http://127.0.0.1:<port>/callback` (`LoopbackRedirectURI`, ports 1024-65535), listener on `127.0.0.1:0` (`tcp4`), `SO_EXCLUSIVEADDRUSE` on Windows |
| Constants | `SDKClientID = "thalovant-sdk"`, `LoopbackCallbackPath = "/callback"`, `DefaultBrowserLoginTimeout = 300s`, `MaxDeviceNameLength = 120` (code points) |
| Helpers | `CanOpenBrowser(env, platform)`, `ParseNativeSignInConfig`, `NativeSignInConfig`, `BrowserAuthorizationURL`, `BrowserAuthorization`, `NewState` (24 bytes), `NewVerifier` (64 bytes), `ChallengeFor`, `DefaultDeviceName` (`thalovant-go-sdk on <hostname>`) |
| Errors | denied: `ErrDeviceAccessDenied` / `*DeviceLoginDeniedError`; timeout: `ErrDeviceCodeExpired` / `*DeviceLoginExpiredError` (both with nil `APIError`); no code: `*BrowserLoginFailedError` (`Code`); exchange refused: `*APIError` 400 `invalid_grant` |
| Parity capability | `browser-login`; vectors `testdata/browser-login-vectors.json` (119 cases, sha256 `b0c73f808c4f…`, Python 39e31d8) |
| Tests | `browser_login_vectors_test.go` (every vector case, plus the 24 callbacks through a real listener), `browser_login_test.go` (prints, opener goroutine, listener edges, goroutine accounting, secrets) |

## Intercom (`intercom.go`; 0.16.0; scopes `intercom:read`, `intercom:send`, `intercom:urgent`, `hubs:write`)

Announce-only text spoken aloud on a hub's speakers. Not a certified safety system; the API does not store the text (keyed hash and length only). Named `Announce` / `*Intercom*`: the mesh already has an `intercom` frame.

| Call | Endpoint | Returns |
| :--- | :--- | :--- |
| `GetIntercomSettings(ctx, hubID)` | `GET /v1/hubs/{id}/intercom/settings` | `IntercomSettings` |
| `SetIntercomSettings(ctx, hubID, enabled)` | `PUT /v1/hubs/{id}/intercom/settings` | `IntercomSettings` |
| `ListIntercomSpeakers(ctx, hubID)` | `GET /v1/hubs/{id}/intercom/speakers` | `IntercomSpeakerList` (`Items` with `Kind`, `CanPlay`; `PolicySynced`, `UnavailableReason`) |
| `UpdateIntercomSpeaker(ctx, hubID, clientID, IntercomSpeakerUpdate)` | `PATCH /v1/hubs/{id}/intercom/speakers/{client_id}` | `IntercomSpeaker` |
| `Announce(ctx, hubID, AnnounceOptions)` | `POST /v1/hubs/{id}/intercom/announce` (urgent needs `intercom:urgent` too) | `Announcement` (`Replayed`; deliveries have `Detail`) |
| `GetAnnouncement(ctx, hubID, id)` | `GET /v1/hubs/{id}/intercom/announcements/{id}` | `Announcement` |
| `CancelAnnouncement(ctx, hubID, id)` | `POST /v1/hubs/{id}/intercom/announcements/{id}/cancel` (`intercom:send`; 409 `not_cancellable`, `AnnouncementStatus()`) | `Announcement` (`Status` `cancelled`, optional `HubConfirmed *bool`) |
| `ListAnnouncements(ctx, hubID, AnnouncementListOptions)` | `GET /v1/hubs/{id}/intercom/announcements` | `AnnouncementList` |

- Typed refusals (`errors.go`; constants `IntercomCode*`): `ErrUrgentScopeRequired` (kind of `ErrAuth`), `ErrUrgentBusy` (kind of `ErrRateLimited`; `UrgentScope()`), `ErrSkillNotInstalled`, `ErrNotASpeaker` (`SpeakerKind()`, `NotSpeakers()`), `ErrReservedName`, `ErrHubLocked`, `ErrImpersonationNotAllowed`, `ErrTooManyTargets` (`Matched()`), `ErrIntercomUnavailable` (`Reason()`) and `ErrIntercomPlatformDisabled` (kinds of `ErrIntercomDisabled`), `ErrIntercomNotConfigured`, `ErrManagedClient` (`RefusedClientRefs()`). Text rule: trimmed, 1-280 runes, no `<` `>`, control, bidi/format characters, invalid UTF-8, private use or separators (no NFC: a documented deviation); DND on needs a valid IANA timezone and `Start != End`; at most 50 names of 1-64 characters per target list; `Lang` a language tag; the key at most 128 printable ASCII characters.
- Errors (`errors.go`): `ErrIntercomDisabled`, `ErrHubUnreachable` (502; the text may or may not have been spoken; `APIError.AnnouncementID()` names the `failed` record: read it before sending again, and use a new key), `ErrNotCancellable` (409), `ErrRateLimited`, matched with `errors.Is` on the `*APIError`; the plan error `ErrPlan` is HTTP 402 or 403 `plan_limit`.
- `Announce` with `Wait` polls `GetAnnouncement` (`intercom:read`, which `intercom:send` implies); a failed poll returns the accepted `Announcement` with the error.
- `IntercomDNDUpdate.Enabled` is a required `*bool` (the API replaces the whole DND setting).
- Idempotency: one fresh key per announcement (`thalovant.NewRequestID()` or a UUID); the same key replays the stored result (`Idempotency-Replayed: true`) and never re-sends (a replayed `failed` stays `failed`). Ids (hub, client, announcement) that are `.`, `..`, empty or hold `/ ? #`, a backslash or a control character are refused before any request (`validIntercomID`, `intercom.go`). `AnnounceOptions` redacts its text under every `fmt` verb and `log/slog`. No request timeout is set by the SDK.

## Chat apps (`chat_apps.go`)

| Method on `*ControlPlane` | Route | Answers |
| :--- | :--- | :--- |
| `ListChatAppConnectors(ctx)` | `GET /v1/bridges/connectors` | `[]ChatAppConnector` |
| `GetBridgeNetworks(ctx, hubID)` | `GET /v1/bridges/networks?hub_id=` | `BridgeNetworks` (`Networks`, `Allowance`) |
| `ListBridges(ctx, BridgeListOptions)` | `GET /v1/bridges?hub_id=&owner_id=&limit=` | `[]Bridge` (limit 1-500) |
| `CreateBridge(ctx, BridgeCreate)` | `POST /v1/bridges` | `Bridge` (201) |
| `GetBridge(ctx, id)` | `GET /v1/bridges/{id}` | `Bridge` |
| `UpdateBridge(ctx, id, BridgeUpdate)` | `PATCH /v1/bridges/{id}` (`If-Match` when `ETag` is set) | `Bridge` |
| `ReplaceBridgeToken(ctx, id, SecretValue)` | `PUT /v1/bridges/{id}/token` | `Bridge` |
| `ReplaceBridgeSecrets(ctx, id, map[string]SecretValue)` | `PUT /v1/bridges/{id}/secrets` | `Bridge` |
| `ListBridgeChats(ctx, id)` | `GET /v1/bridges/{id}/chats` | `BridgeChatList` |
| `SendBridgeTestMessage(ctx, id, chatID)` | `POST /v1/bridges/{id}/test` | `BridgeTestResult` |
| `GetBridgeAvatar(ctx, id)` | `GET /v1/bridges/{id}/avatar` | `BridgeAvatar` (`Content`, `ContentType`) |
| `DeleteBridge(ctx, id)` | `DELETE /v1/bridges/{id}` | `error` only (204) |

- String enums (unknown values kept): `BridgeState`, `BridgeSessionScope`, `BridgePhase`, `BridgeStatusReason`, `BridgeStatusSource`, `BridgeHoldReason`, `ChatAppStatus`, `ChatAppHelper`.
- `SecretValue`: `NewSecretValue`, `Reveal`, `IsZero`; prints `[REDACTED]` (fmt, JSON, text, slog).
- Tests: `chat_apps_test.go` (16 tests).
- `GetBridgeAvatar` reads at most 4 MiB; a body cut short is an `*APIError` ("incomplete response body") on every control-plane call.
