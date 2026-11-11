Last Edit: Claude Sonnet 5.5 - 2026-11-11 - Motive: Intercom (0.16.0): Announce, speakers and settings on the control plane.

# Quick Facts

| Item | Value |
| :--- | :--- |
| Module | `github.com/thalovant/thalovant-go-sdk` (package `thalovant`) |
| Version | 0.16.0 (`VERSION` and `const Version` in `version.go`, kept equal by `TestVersionMatchesVersionFile`; the Auto Release workflow bumps both for a patch release) |
| Go | 1.26.9 or newer (`go.mod`) |
| Data-plane entry points | `NewClient`, `NewClientFromConfig`, `Client.Ask`, `Client.Intents`, `HubSession` |
| Control-plane entry points | `NewControlPlane`, `NewDefaultControlPlane`, methods on `*ControlPlane` (`control.go`, `hub_skills.go`, `chat_apps.go`, `browser_login.go`) |
| Sign-in entry points | `InteractiveLogin` (chooser), `BrowserLogin` (browser, PKCE on a loopback redirect), `LoginWithBrowser` (device login, unchanged), `BeginDeviceLogin`/`PollDeviceLogin`, `RevokeAPIToken` |
| Errors | `*APIError` (`StatusCode`, `Code`, `ProblemDetail`, `Problem`, `Fields`, `RetryAfter`); `ErrAPI`, `ErrAuth`, `ErrPlan`, `ErrAPIUnreachable` (`errors.go`) |
| Validation | `test -z "$(gofmt -l .)"`, `go vet ./...`, `go test -race -count=1 ./...` |
| Parity contract | `contracts/sdk-parity.json`, checked by the Python SDK's `scripts/check-sdk-parity.py --consumer thalovant-go-sdk`; reference digests accepted: `57c7e71f6456…` (current) and `6dfb3bb31ac7…` (browser-login final candidate, Python 39e31d8) |
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

## Intercom (`intercom.go`; 0.16.0; scopes `intercom:read`, `intercom:send`, `hubs:write`)

Announce-only text spoken aloud on a hub's speakers. Not a certified safety system; the platform never stores the text. Named `Announce` / `*Intercom*`: the mesh already has an `intercom` frame.

| Call | Endpoint | Returns |
| :--- | :--- | :--- |
| `GetIntercomSettings(ctx, hubID)` | `GET /v1/hubs/{id}/intercom/settings` | `IntercomSettings` |
| `SetIntercomSettings(ctx, hubID, enabled)` | `PUT /v1/hubs/{id}/intercom/settings` | `IntercomSettings` |
| `ListIntercomSpeakers(ctx, hubID)` | `GET /v1/hubs/{id}/intercom/speakers` | `[]IntercomSpeaker` |
| `UpdateIntercomSpeaker(ctx, hubID, clientID, IntercomSpeakerUpdate)` | `PATCH /v1/hubs/{id}/intercom/speakers/{client_id}` | `IntercomSpeaker` |
| `Announce(ctx, hubID, AnnounceOptions)` | `POST /v1/hubs/{id}/intercom/announce` | `Announcement` |
| `GetAnnouncement(ctx, hubID, id)` | `GET /v1/hubs/{id}/intercom/announcements/{id}` | `Announcement` |
| `ListAnnouncements(ctx, hubID, AnnouncementListOptions)` | `GET /v1/hubs/{id}/intercom/announcements` | `AnnouncementList` |

- Errors (`errors.go`): `ErrIntercomDisabled`, `ErrHubUnreachable`, `ErrRateLimited`, matched with `errors.Is` on the `*APIError`.

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
