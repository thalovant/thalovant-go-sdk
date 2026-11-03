Last Edit: Claude Opus 5.5 - 2026-11-03 - Motive: Add chat apps (the /v1/bridges owner routes) and record the 0.14.0 surface.

# Quick Facts

| Item | Value |
| :--- | :--- |
| Module | `github.com/thalovant/thalovant-go-sdk` (package `thalovant`) |
| Version | 0.14.0 (`VERSION` and `const Version` in `version.go`, kept equal by `TestVersionMatchesVersionFile`) |
| Go | 1.26.9 or newer (`go.mod`) |
| Data-plane entry points | `NewClient`, `NewClientFromConfig`, `Client.Ask`, `Client.Intents`, `HubSession` |
| Control-plane entry points | `NewControlPlane`, `NewDefaultControlPlane`, methods on `*ControlPlane` (`control.go`, `hub_skills.go`, `chat_apps.go`) |
| Errors | `*APIError` (`StatusCode`, `Code`, `ProblemDetail`, `Problem`, `Fields`, `RetryAfter`); `ErrAPI`, `ErrAuth`, `ErrPlan`, `ErrAPIUnreachable` (`errors.go`) |
| Validation | `test -z "$(gofmt -l .)"`, `go vet ./...`, `go test -race -count=1 ./...` |
| Parity contract | `contracts/sdk-parity.json`, checked by the Python SDK's `scripts/check-sdk-parity.py --consumer thalovant-go-sdk` |

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
- Tests: `chat_apps_test.go` (13 tests).
