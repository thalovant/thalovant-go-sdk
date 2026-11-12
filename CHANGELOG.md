Last Edit: Claude Sonnet 5.5 - 2026-11-12 - Motive: Intercom review round 2 (CodeRabbit PR 92): urgent-scope precedence, the accepted announcement kept on a malformed POST answer, docs aligned with the API.

# Changelog

## 0.16.0 — Unreleased

Intercom. Every addition is new API. Ports thalovant-python-sdk 0.12.0 (reference digest `69badc4fb072`; its `docs/intercom-porting.md` is the brief).

- Review fixes (CodeRabbit, PR 92, second round). A 403 that names `urgent_scope_required` is `ErrUrgentScopeRequired` (still an `ErrAuth`) even when it also says "Insufficient scopes"; `Announce` returns the partly decoded announcement, with its `ID`, together with the "unexpected announcement shape" error. The docs say what the API does: `intercom:send` implies `intercom:read` (`Wait` needs no further scope), a repeat call with the same key and `Wait` false returns the stored announcement without a status read, a constant key replays or is a 409, `errors.Is(err, ErrIntercomDisabled)` also matches the platform and unavailable kinds, and `CancelAnnouncement` is open to the owner, a manager, a plain member, an administrator and an impersonated session.
- **Announce-only text spoken aloud on a hub's speakers**, through the routes under `/v1/hubs/{hub_id}/intercom`: `ControlPlane.Announce(ctx, hubID, AnnounceOptions)`, `GetAnnouncement`, `ListAnnouncements(ctx, hubID, AnnouncementListOptions)`, `ListIntercomSpeakers`, `UpdateIntercomSpeaker` (only what is set is sent; `ClearRoom` sends `room: null`), `GetIntercomSettings` and `SetIntercomSettings`. Scopes: `intercom:read`, `intercom:send`, `hubs:write` for settings and speakers. **Not a certified safety system**; the platform never stores the text. Named `Announce` and `*Intercom*` because the mesh already has an `intercom` frame.
- Models in `intercom.go` (`IntercomSettings`, `IntercomSpeaker`, `IntercomPolicy`, `Announcement`, `IntercomDelivery`, `AnnouncementList`, `IntercomTargets` with `AllSpeakers`, `SpeakerTargets`, `RoomTargets` and `GroupTargets`, `IntercomRepeat`); every status is an open string type. The text (1-280 characters counted as runes, trimmed, no control characters), the targets (exactly one of all, speakers (at most 50), rooms, groups) and the repeat (urgent only, 1-10 times, 5-120 seconds) are checked before anything is sent and refused as `ErrAPI`. `Wait` polls a `sending` announcement and returns the latest answer on timeout; the context ends the wait. its polls use `intercom:read`, which `intercom:send` implies; `WaitTimeout` and `PollInterval` must not be negative (checked before the POST) and the deadline bounds each poll. A poll that fails after the announcement was accepted returns the accepted `Announcement` (its `ID` set) with the error; reuse the `IdempotencyKey` to retry. `IntercomDNDUpdate.Enabled` is a required `*bool` (the API replaces the whole DND setting and stores a missing `enabled` as false).
- `errors.Is` reaches three new kinds of `*APIError`: `ErrIntercomDisabled` (`intercom_disabled`), `ErrHubUnreachable` (502 `hub_unreachable`) and `ErrRateLimited` (any 429).
- Intercom, contract amendment 2 (client-side security review; ports Python's `docs/intercom-porting.md`). `ControlPlane.CancelAnnouncement(ctx, hubID, announcementID)` stops an urgent announcement that is still `sending` (`POST .../announcements/{id}/cancel`, `intercom:send`): it returns the announcement, status `AnnouncementCancelled`, and an optional `HubConfirmed *bool` (false when the hub could not confirm; nil on every other answer, and when the API sends a non-boolean); a 409 `not_cancellable` is `ErrNotCancellable`. `APIError.AnnouncementID()` returns the id of the `failed` record the API stored for a 502. Hub, speaker (client) and announcement ids that are empty, `.`, `..`, or hold `/`, `?`, `#`, a backslash or a control character are refused (an error matching `ErrAPI`) before any request (they were percent-escaped; dot segments were not rejected). `AnnounceOptions` redacts its text under every `fmt` verb and in `log/slog` (`String`, `GoString`, `Format`, `LogValue`). This SDK sets no request timeout: the docs say to give `Announce` at least 20 s. README, FAQ and QUICK_FACTS use a fresh `thalovant.NewRequestID()` idempotency key per announcement and say that the same key replays the stored result without sending (a replayed `failed` stays `failed`), that a deliberate second send needs a new key, and to read the announcement after a 502; the wording no longer says "nothing listens" or that the text is "never stored" without scope.
- Intercom, the final API (platform security review; ports Python's `docs/intercom-porting.md`). **Scope `intercom:urgent`** (`IntercomUrgentScope`, with `IntercomReadScope` and `IntercomSendScope`): an urgent announcement (`Priority` urgent, so every `Repeat`) needs it in addition to `intercom:send`; without it `Announce` returns `ErrUrgentScopeRequired` (403 `urgent_scope_required`; `errors.Is(err, ErrAuth)` holds too) before anything is spent. **Speakers** gain `Kind`, `CanPlay` (the effective "can this connection play speech": a connection that cannot is listed but cannot receive, `ErrNotASpeaker`, 422 `not_a_speaker`, with `APIError.SpeakerKind()` or `NotSpeakers()`) and `PolicySynced` (`*bool`, on the PATCH answer). **`ListIntercomSpeakers` now returns an `IntercomSpeakerList`** (`Items`, `PolicySynced`, `UnavailableReason`) instead of a slice; `IntercomUnavailableReason` can be `"platform"` (constants `IntercomUnavailablePlatform`, `...Plan`, `...KidHub`, `...Public`). `IntercomSpeakerUpdate` has only `Receive`, `Room`/`ClearRoom`, `Groups` and `DND`. **Typed refusals and `IntercomCode*` constants** for `urgent_scope_required`, `intercom_urgent_busy` (`ErrUrgentBusy`, a kind of `ErrRateLimited`; `UrgentScope()`), `skill_not_installed`, `not_a_speaker`, `reserved_name`, `hub_locked`, `impersonation_not_allowed`, `too_many_targets` (`Matched()`), `intercom_unavailable` (`ErrIntercomUnavailable`, `Reason()`) and `intercom_disabled_platform` (`ErrIntercomPlatformDisabled`; both kinds of `ErrIntercomDisabled`), `intercom_not_configured`, `managed_client` (`RefusedClientRefs()`); `APIError.AnnouncementStatus()` names the current status of a `not_cancellable` 409. The intercom codes (including `hub_unreachable` and `not_cancellable`, which also needed their status before) decide by code, whatever the status. **Validation before any request, as the API does it:** the text is trimmed, 1 to 280 runes, with no `<` or `>`, no control, bidirectional or other invisible format characters (zero-width joiner and non-joiner allowed), no invalid UTF-8, private-use characters or separators (it replaces the control-character check; an error never echoes the text; **deviation: no NFC normalisation**, `golang.org/x/text` is not a dependency); DND switched on needs `QuietHours` with a valid IANA `Timezone` (at most 64 characters; `time.LoadLocation`, with a shape-only fallback where the machine has no timezone database) and `Start != End`, and any given timezone is checked; every target list holds at most 50 names of 1 to 64 characters (was: speakers only); `Lang` is a language tag; `IdempotencyKey` is at most 128 printable ASCII characters. **`Announce`** sets `Announcement.Replayed` (the response header `Idempotency-Replayed: true`, kept after `Wait`) and each delivery has its `Detail` code (`KnownDeliveryDetail`). Docs say what `delivered` means (the hub handed the speech over; no playback receipt), that the platform switch ships off (`ErrIntercomPlatformDisabled`) and the real stop controls. `VERSION` stays 0.16.0 (the release is unreleased; nothing was published). The parity record acknowledges the reference's new digest `69badc4fb072`.
- The `intercom` parity capability is recorded (required), and the other capabilities' evidence hash for `errors.go` is refreshed: it changed only by this addition. `VERSION` and `version.go` to 0.16.0.


## 0.15.0 — 2026-11-08

Browser sign-in: the OAuth authorization code grant with PKCE (RFC 7636, S256 only) on a loopback redirect (RFC 8252 section 7.3), the Python reference's 0.11.0 round (thalovant/thalovant-python-sdk#113 at 39e31d8, reference digest `6dfb3bb31ac7…`). A device code can be read off one screen and typed in anywhere; with a loopback redirect the browser that approves hands the code to its own computer, so a link or code sent to someone on another computer delivers nothing to the sender (a forwarded callback port or another user of the same computer can still complete one). New API only; device login is unchanged and no existing signature changes.

- `ControlPlane.BrowserLogin(ctx, BrowserLoginOptions)` signs in through the browser. It reads `GET /v1/auth/native/config` (no auth), binds `127.0.0.1` on port 0 before building the URL, prints `Opening your browser to sign in to Thalovant. If it does not open, visit:` and the URL (or hands it to `Prompt`; `To sign in to Thalovant, visit:` when `OpenBrowser` is false), opens the browser, and waits for `http://127.0.0.1:<port>/callback`. The code is exchanged once at the configuration's `token_endpoint` with exactly `code`, `code_verifier`, `client_id` and `redirect_uri`, and the token is kept as a device login keeps it (`AccessToken`, `TokenID`), so `RevokeAPIToken` revokes it the same way. It returns the raw token answer, `ai_disclosure` included, as `LoginWithBrowser` does.
- Options: `Scopes` (empty strings dropped; with none left, `scope` is left out and the API grants its own default), `DeviceName *string` (only nil gets `DefaultDeviceName()`, `thalovant-go-sdk on <hostname>`; `new("")` leaves `device_name` out and keeps the hostname off the consent page; cut to 120 code points), `OpenBrowser` (nil is true), `Timeout` (default `DefaultBrowserLoginTimeout`, 300 s, on the monotonic clock), `Prompt` (receives the URL instead of the printed message, so nothing is printed; it is called on the calling goroutine, as the message is printed, and the timeout counts from its return), and `Opener`, which replaces the platform browser and runs on its own goroutine, so a console browser that holds the call does not hold the sign-in.
- The configuration is unusable (and `auto` signs in with a device login) when `authorization_endpoint` is not https or loopback http, carries credentials, a query or a fragment, names port 0 or one above 65535, or has a host, as written, that is not a bracketed IPv6 address, an IPv4 address (required when the last label is numeric) or ASCII letter/digit/inner-hyphen labels of at most 253 characters (`xn--` for an international name; no `%`, `_` or empty label); when `token_endpoint` is not a path on the API (one leading `/`, no colon anywhere, every `%` followed by two hex digits, no `?`, `#`, `\` or whitespace); when `sdk_client_id` is not `[A-Za-z0-9._-]{1,64}`; or when `code_challenge_methods_supported` (a decoded `[]any` or a `[]string`) lacks `S256`.
- No control-plane request leaves the API's origin: every request, on every route, is refused with an `ErrAPI` error with no status when its URL's scheme, host and port are not `APIURL`'s, as when a `ControlPlane` built by hand has no trailing slash on `APIURL`.
- The listener serves only `GET /callback`; anything else is a 404 (a `HEAD` answer carries the headers and no body, its `Content-Length` the page's, an oversized `HEAD` included), and a missing, repeated or wrong `state` (compared in constant time) is a 400 while the wait goes on, so a stray tab cannot end a real sign-in. A complete head without a request line, or a head over 16 KiB, is a 400 with the page headers; a connection that closes, or sends nothing for 10 seconds, before a whole head has arrived is closed without an answer. After every answer the write side is shut and what the browser still sends is read and dropped (at most 2 s and 1 MiB) before the connection closes, so a reset cannot cost the browser its answer. At most 64 connections are handled at once; past that the listener stops accepting until one closes. The first right-state callback is accepted and the port stops listening; its tab is answered once the sign-in has ended, however long the exchange takes (`Signed in to Thalovant. You can close this tab.` or `Sign-in was not completed.`; closing the listener answers it at once), and every answer carries `Content-Type: text/html; charset=utf-8`, `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, `Content-Security-Policy: default-src 'none'` and `X-Content-Type-Options: nosniff`. Callback values are decoded as Python's `parse_qsl` decodes them. On Windows the socket is exclusive (`SO_EXCLUSIVEADDRUSE`). Nothing is logged.
- Errors reuse the device-login types: denied is `errors.Is(err, ErrDeviceAccessDenied)` (a `*DeviceLoginDeniedError` with a nil `APIError`), a timeout is `ErrDeviceCodeExpired` (a `*DeviceLoginExpiredError`, nil `APIError`), and `invalid_grant` is the `*APIError` with status 400 and detail `invalid_grant`. Another error from the consent page, or no code, is the new `*BrowserLoginFailedError`, which matches `ErrAPI` (Go's `APIError` cannot say "no status"); its `Code` keeps only RFC 6749's seven error codes (the first `error` value; `access_denied` sent with another `error` is such a failure, not a denial), and anything else, the state or a code included, is left out. A configuration or token answer that is not a usable JSON object is an `ErrAPI` error with no status. A cleartext API URL (not https, not loopback http) is refused before anything is asked or opened. A cancelled `ctx` closes the port and returns `ctx.Err()`; when `ctx` is done by the time `Prompt` returns, the browser is not opened.
- The code, the verifier, the state and any token never reach an error. A refused exchange's answer is decoded and redacted (with any `access_token`, `refresh_token` or `id_token` value it carries) before its display line is built and cut, in one pass, the longest value first, never wholly inside an existing `[redacted]` (a token that itself carries `[redacted]` is still matched whole); the error returned is a fresh `*APIError` with no wrapped cause, and a transport failure keeps nothing of the request. An answer over 1 MiB, or carrying more than 64 distinct tokens, is not echoed at all (`Detail` `(server error response omitted)`, no `Problem`); the scrubber is built once per answer and uses no regular expression. The exchange is sent at most once: its body has no `GetBody`, so the HTTP transport never replays it (HTTP/2 would otherwise replay it on a stream the server reset with `PROTOCOL_ERROR`), since a code presented twice revokes the token the first exchange minted.
- `ControlPlane.InteractiveLogin(ctx, InteractiveLoginOptions)` chooses: `LoginMethodAuto` (the default) signs in through the browser when `CanOpenBrowser(nil, "")` holds (never with `CI`, `SSH_CONNECTION` or `SSH_TTY` set; macOS and Windows yes; elsewhere only with `DISPLAY` or `WAYLAND_DISPLAY`; an empty variable is not set) and the configuration is usable, and with a device login (`LoginWithBrowser`, `DeviceName` as `ClientName`, none sent for an empty one) otherwise. `LoginMethodBrowser` and `LoginMethodDevice` force one; a forced browser sign-in against an API without it is that error, never a device login, and once the browser was opened its outcome stands. `Timeout` zero means each method's own default. `Opener` serves both methods, the device login's verification page included. Android maps to `linux` and iOS stays `ios`, so `auto` picks a device login on both.
- The pieces are exported to be checked on their own: `CanOpenBrowser(env, platform)`, `ParseNativeSignInConfig` and `NativeSignInConfig`, `BrowserAuthorizationURL` and `BrowserAuthorization`, `LoopbackRedirectURI`, `NewState`, `DefaultDeviceName`, and the constants `SDKClientID`, `LoopbackCallbackPath`, `DefaultBrowserLoginTimeout` and `MaxDeviceNameLength`. `NewVerifier` and `ChallengeFor` already were.
- Every goroutine the loopback listener runs (the accept loop and one per connection) has returned when `BrowserLogin` returns, on every path: approved, denied, failed, expired, cancelled, a refused exchange, a panicking `Prompt`. Connections a browser opens and never uses are closed without waiting for them. Two goroutines outside the listener may still be running: a caller's own `Opener` call, which is not waited for, and, with no `Opener`, the platform launcher's wait for the process it started (`open`, `xdg-open` or `rundll32`), which ends when that process exits, as for device login.
- `LoginWithBrowser` now runs through an unexported `loginWithDevice` that takes an opener, so `InteractiveLogin` can pass its `Opener` to a device login. With none it opens the platform browser exactly as before.
- Conformance: `testdata/browser-login-vectors.json` is the reference's file at 39e31d8, byte-identical (sha256 `b0c73f808c4f…`). All 119 cases run (`browser_login_vectors_test.go`), `sign_ins` and `chooser` for real over a loopback API and a raw TCP browser, and every callback case through a real listener too; each recorded digest equals the reference's. The parity contract gains `browser-login`, accepts both the current (`57c7e71f6456…`) and the final candidate (`6dfb3bb31ac7…`) reference digests during the rollout, and refreshes the `control.go` evidence of `api-errors`, `chat-apps`, `connection-admission`, `connection-kinds`, `control` and `device-login`; the origin check is the one behaviour change there, and their own tests and vectors pass unchanged.

## 0.14.2 — 2026-11-08

The CodeRabbit review-body items on #65 and #69 (#88). Behaviour changes only; no signature changes.

- `AnswerHomeRequests` and `AnswerHomeRequest` no longer leave a goroutine behind for every request when a handler or a `Replier` ignores its context. Go cannot stop a goroutine, so such a handler or reply keeps its goroutine until it returns. Now, while four handlers are still running past their time, a request is answered `timeout` at once and its handler is not started; while four replies are still running past the hub's bound, no reply is started and `AnswerHomeRequest` returns an `ErrTimeout` that says so (`OnReplyError` hears it). Each `AnswerHomeRequests` subscription keeps its own counts. Calls of `AnswerHomeRequest` on its own share one count of handlers and keep one count of replies for each `Replier` (the same pointer, or an equal value), so a stuck `Replier` holds up only its own answers; a `Replier` value that cannot be compared, such as a struct holding a slice, cannot be told apart from another and is not counted, so pass a pointer. Handlers and Repliers that honour their context are not limited, since their goroutines end at their deadline; the SDK's `Client` and `HubSession` honour it. The `Replier` doc now says `Reply` must return once its context ends. The Python reference runs at most four plain handlers at once and answers `timeout` when all four are taken. (CodeRabbit on #69.)
- `UsualForm` agrees with the Python reference on two kinds of tag. A tag with an empty subtag (`"en-"`, `"en--us"`) has no usual form; it was `"en-us"`. A language CLDR knows through its macrolanguage has one, in its own code: `"arb"` is `"arb-eg"` and `"cmn"` is `"cmn-cn"`, where there was none. Run against the reference over 4,453 tags (every language, alias, macrolanguage and likely-subtag key in the shared data, every tag in the language-matching vectors, and malformed ones), the two now answer the same for all of them. A tag ending in a singleton (`"en-x"`) is still English, as in the reference. `Client.Intents` retries an empty listing in `UsualForm`'s answer, so it now retries those languages and no longer retries the malformed tags. (CodeRabbit on #65.)
- `HubIntentInventory.ListedIn` is an empty slice, `[]` in JSON, on the names-only fallback (`IntentSourceEngines`), where it was nil (`null`): nothing is listed by manifest there, as in the Python reference. Its doc comment says so instead of promising one tag per language on every path. (CodeRabbit on #65.)
- `ClosestLanguage`'s doc comment is attached to it again; it sat above `UsualForm`'s.
- The parity contract's `home-link` reason describes the new bound, and the evidence hashes of `home-link`, `connection-admission`, `listing` and `runtime` are refreshed.

## 0.14.1 — 2026-11-06

- `golang.org/x/net` v0.62.0 (indirect), up from v0.61.0, for CVE-2026-78661 (#86). No API, wire or parity-contract change.

## 0.14.0 — 2026-11-03

Chat apps: a hub owner connects a chat network (Telegram and Discord; Matrix, Mattermost and Twitch in beta) to their hub, and the hub answers there. This release covers the owner routes under `/v1/bridges`. New API only; nothing is removed and no signature changes.

- `ControlPlane` gains `ListChatAppConnectors`, `GetBridgeNetworks`, `ListBridges`, `CreateBridge`, `GetBridge`, `UpdateBridge`, `ReplaceBridgeToken`, `ReplaceBridgeSecrets`, `ListBridgeChats`, `SendBridgeTestMessage`, `GetBridgeAvatar` and `DeleteBridge` (new file `chat_apps.go`). They answer typed values (`ChatAppConnector`, `BridgeNetworks` with its `BridgeAllowance`, `Bridge` with its `BridgeStatus` and `BridgeHold`, `BridgeChatList`, `BridgeTestResult`, `BridgeAvatar`) whose JSON names are the API's. Reads need `hubs:read`, writes `hubs:write`.
- The enums (`BridgeState`, `BridgeSessionScope`, `BridgePhase`, `BridgeStatusReason`, `BridgeStatusSource`, `BridgeHoldReason`, `ChatAppStatus`, `ChatAppHelper`) are string types with constants, and a value the API adds later is kept as sent instead of failing the decode. Optional members the API leaves out read as nil or "", and the API's defaults apply (`ChatAppSecretField.Required` and `BridgeChat.Selectable` true, `ChatAppOptions.SessionScopes` person and chat, `BridgeTestResult.Language` "en").
- `CreateBridge` takes a `BridgeCreate`: exactly one of `Token` and `Secrets`, or nothing is sent; `Settings` when non-nil. It sends `connector`, not the older `network`. `UpdateBridge` takes a `BridgeUpdate` and sends only what it sets (a non-nil `Allowed`, `Settings` or pointer, a non-empty `SessionScope`); `ETag`, when set, goes out as `If-Match`, and a stale one answers 412. Pause and resume are `Paused: new(true)` and `Paused: new(false)`.
- `SecretValue` wraps a credential. Every fmt verb, `json.Marshal`, `MarshalText` and `log/slog` print `[REDACTED]`, the marker the SDK already uses; `Reveal` reads the value, and the SDK calls it only to build the request body. Tests check that a token reaches the request body and nothing else: not an error, not a printed `BridgeCreate`, not a log line.
- `APIError.Fields` names the settings or secrets a refusal is about (`bridge_settings_invalid`, `bridge_secrets_invalid`, the `bridge_url_*` codes), read from the body's `fields` or from inside an object `detail`, string entries only; nil otherwise. It never holds values. The plan and auth refusals match `ErrPlan` and `ErrAuth` as before, and `RetryAfter` reads a 429's `Retry-After`.
- A control-plane response whose body ends before its declared length is now an `*APIError` ("incomplete response body", with the status) instead of the partial bytes, so a picture or a JSON body cut short never passes as a complete answer. `GetBridgeAvatar` reads at most 4 MiB (the API serves at most 1 MiB) and refuses a larger body ("response body too large") rather than reading it into memory.
- The parity contract declares the new `chat-apps` capability (`chat_apps.go`, `control.go`, `errors.go`; tests `chat_apps_test.go`) and refreshes the evidence hashes of every capability whose files changed.

## 0.13.0 — 2026-10-11

The Python reference's 0.9.7 round (reference digest `5b973a65ab58…`, see thalovant/thalovant-python-sdk#105). New API only; nothing is removed and no signature changes.

- `Client.Intents` lists in the hub's own language when the asked one has no sentences. A hub loads its `lang` and `secondary_langs` (en-US and fr-FR on most public hubs, plus de-DE on Story Time) and lists nothing under any other tag, so a phone set to es-ES, ja-JP, or de-DE on most hubs got zero rows, then zero rows again in the usual form, and showed an empty list from a hub that answers those people in its own language. Measured on all 13 public hubs on 2026-10-11. Now, when the language and its usual form both leave it without an enabled template row and `Nearest` is on (the default), `Intents` asks `ovos.intent.list` once more with no `lang` at all, in the payload or the context (`include_definitions` only when describing). The reply lists every language the hub loaded; rows are grouped by their `lang`, `fr-fr` and `fr-FR` as one group under the first spelling seen, in the hub's order. It is asked at most once per `Intents` call, and only when some language still needs it, so a language the hub loaded never asks it.
- `NearestHubLanguage(asked, listings, hubLang)` is the choice on its own, over the new `HubLanguageRows` in the hub's order. Only a language with an enabled template row is a candidate. The same language comes first (`ClosestLanguage`, so `en-CA` takes `en-GB` when the hub loaded no `en-US`), then the hub's own language, which `Intents` reads from `context.session.lang` on the reply to the language-less listing (a request naming no language is answered in a session the hub opened in its default language: `en-US` on Daily Desk, which lists `fr-FR` first), and only when the hub listed it. Otherwise the language with the most enabled template rows wins, the first listed on a tie. `ListedIn` records the chosen tag in the hub's spelling. Keyword rows the asked language had are kept only when nothing with sentences is found. Two asked languages that land on one listing share one key, the first spelling used, so an inventory for `en-us` and `ja-JP` keeps its phrases under `en-us` alone. A timeout, a `*PolicyDeniedError`, or `ok: false` on the language-less listing leaves the result exactly as it was; a cancelled context is still returned. `Nearest: false` never asks it.
- Fallback skills come from `ovos.skills.list` (`EventSkillsList`, `EventSkillsListResponse`). `ovos.skills.fallback.list` came from ovos-core #951 and #988 reverted it the same day, so no hub ever answered it: every `IntentsWithCapabilities` call spent the full 1.5 s on it and reported fallbacks as unknown, and a skill with no intents never showed up (Daily Desk's `thalovant-skill-memory`, Ops Copilot's `custos-fallback`). A fallback is now a row whose `capabilities` contain `FallbackCapability` (`"fallback"`) and whose `session_id` is absent, empty, or `"default"`; a row from another session belongs to somebody else, and a malformed row (no string `skill_id`, `capabilities` not a list) is skipped while the rest are kept. The list is sorted by skill id with no duplicates. The probe keeps its 1.5 s cap (now `FallbackProbeTimeout`) and its meaning: nil (`FallbacksKnown` false) for a timeout, a denial, `ok: false`, or a `skills` that is not a list; a non-nil empty slice when the hub says there are none. `ovos.skills.fallback.list` is no longer sent. `EventFallbackList` and `EventFallbackListResponse` stay exported so code that imports them keeps compiling. A connection needs `ovos.skills.list` on its allow-list; until thalovant-api grants it, the hub answers `hive.policy.denied` and fallbacks stay unknown, as before.
- `HubFallback` gains `PriorityKnown`, and it is always false. `ovos.skills.list` has no order field and the query that had one was reverted, so no priority is invented: `Priority` is 0 and is not a rank, and the list is in skill id order, which says nothing about who is tried first. Making `Priority` a `*int64` would have matched Python's `None` more literally, but it breaks every caller that reads `Priority` as a number or builds a `HubFallback` literal with one, so `Priority int64` stays and the new flag says whether to read it. JSON follows the other SDKs: `HubFallback` marshals `{"skill_id", "priority": null}` while the priority is unknown, and unmarshalling a non-null number sets `PriorityKnown`. The unexported priority parser is gone, since nothing has a priority to parse.
- Intent queries with no language (the language-less listing, the fallback probe) leave `lang` out of the context instead of sending `"lang": ""`.
- The parity contract acknowledges the new reference digest, and its `runtime` capability now declares `intents.go`, `fallbacks.go` and `intents_test.go`, where this behaviour and its tests live.

## 0.12.7 and 0.12.8 — 2026-10-11

- Security floor: `go.mod` now says `go 1.26.9`, up from `go 1.26.0`. GO-2026-6611 (CVE-2026-78669) is an HTTP/2 denial of service, a peer that opens many streams and then sends many small SETTINGS frames carrying `SETTINGS_INITIAL_WINDOW_SIZE`, and it is fixed in three places: `golang.org/x/net` v0.60.0 (already required), the standard library's `net/http` in Go 1.26.9, and `net/http/internal/http2` in Go 1.27.2. The SDK talks HTTP through `net/http`, so the module requirement alone could not keep a build off a vulnerable standard library. A consumer whose toolchain is older than 1.26.9 now has the go command download go1.26.9 (the default `GOTOOLCHAIN=auto`), or fails with `go.mod requires go >= 1.26.9` under `GOTOOLCHAIN=local`; upgrade the local toolchain to 1.26.9 or 1.27.2. The directive cannot exclude Go 1.27.0 and 1.27.1, which are also vulnerable, so build with 1.27.2 or newer on that line. CI's minimum-version leg and the release, auto-release and published-smoke workflows now pin Go 1.26.9 instead of `1.26.x`/`1.26`, so tagged builds and their tests use a patched standard library. No API, wire or parity-contract change.
- The `HubIntentInventory.ListedIn` and `IntentOptions.Nearest` doc comments describe the retry below: `ListedIn` can differ from `Languages` after a keyword-only listing too, not only after an empty one. Comments only.
- `Client.Intents` asks the language's usual form when the first listing has no sentences, not only when it is empty. `Nearest` retried `en-CA` as `en-us` only on an empty listing, and a listing of keyword intents alone is not empty but has nothing a person can say: the Pop Culture hub lists five Source Scout keyword intents under `en-CA` (that skill registers keyword vocabulary in 63 regional tags) and 141 intents with sentences under `en-US`, so a phone in Toronto was shown five names. Now the usual form is asked whenever the first listing has no enabled template row, and its listing is taken when it has one, or when the first was empty. A listing with a template row is still asked once, and keyword names are never traded for other names. A retry that times out or fails with `ErrRuntime` (`ok: false`, or a `*PolicyDeniedError`) keeps the names the first listing gave; after an empty first listing it returns that error, where this port used to swallow it and return an empty inventory, so it now matches the Python reference. Any other retry error, and a cancelled context, is returned as it is. This follows the Python reference (thalovant-python-sdk#103).
- `IntentOptions.Nearest` reaches `Client.Intents`. `intentOptions` merged every option but `Nearest`, so `Nearest: false` was silently ignored and the retry in the usual form could not be turned off.

## 0.12.4 — 2026-10-06

The Python reference's 0.9.5 round (reference digest `3f37b7453397…`).

- `Identity.EndpointFor(ProtocolWSS)` falls back to `DefaultMaster` when the identity names no WSS endpoint and the master is a `wss://` or `ws://` URL, in any case. It used to return `""`, so `NewClient` chose HTTPS polling for every identity a setup link wrote. The API's identify payload carried `default_master` and no `data_plane_endpoints` (thalovant-api#408 now adds both `data_plane_endpoints` and `protocols`). An explicit WSS endpoint still wins, and an `https://` or `http://` master still gives none. The master is never read as an MQTT endpoint. Whether WSS is used at all is still `SupportsProtocol`'s call, so a hub whose `protocols.wss.enabled` is false keeps HTTPS. This matches the Python reference, Node, Kotlin, .NET and Swift. Existing identity files need no re-pairing.

## 0.12.1 — 2026-09-30

The Python reference's 0.9.2 round.

- `Reply.Claimed` treated any fallback-tier reply as "nothing understood," because OVOS's fallback priority band (`match_low`, 90-101) holds a skill that genuinely vocabulary-gates its own narrow answers in the same tier as the fleet's generic catch-all -- the pipeline id alone cannot tell them apart. A skill's own speak event may now carry `meta={"thalovant_claimed": true}` (thalovant-skillkit's `speak_to`/`emit_speech` already thread a `meta` map onto that message) to positively assert a genuine claim; `Claimed` checks it first, before the existing pipeline-tier heuristic. It is opt-in and additive: only a literal `true` under the new `ThalovantClaimedMetaKey` ("thalovant_claimed") counts, only the skill's own speak event is read (never another correlated event this reply collected, `ovos.utterance.handled` included), it can never turn a failed or unhandled reply into a claimed one, and a reply that never sets it -- including the fleet's own generic catch-all -- is judged exactly as before. No existing signature changes.
- The speak-only scoping was tightened in review, before merge: CodeRabbit caught the wider (any-event) version scanning too much on the embedded-C and Node ports, confirmed to apply here too. `hasAssertedClaim` now filters to `EventSpeak`/`EventOvosUtteranceSpeak` only, matching the same two constants `client.go` already uses to recognize a speak-type event -- never the wider media-events set, which also includes a skill sound clip with no meaning as a claim. Eight `reply-claim-vectors.json` cases cover the assertion and its edges (false, a string, a second event, a failed reply, a non-fallback reply, a correlated non-speak event carrying the assertion); all 24 cases pass. Reference digest `841f3bbeec23…`.

## 0.12.0 — 2026-09-28

The Python reference's 0.9.1 round. Everything is new API; no existing call changes its signature or the error it matches.

- `DeviceLoginOptions.ClientID` and `BeginDeviceLoginWithOptions` sign in as a registered app: `client_id` is sent when set and left out when empty, and `HomeAssistantClientID` is Home Assistant's, `thalovant-home-assistant`. The approval screen then shows the platform's own name for the app as verified, and an id the API does not know is a 400 `unknown_client` `*APIError`. `BeginDeviceLogin(ctx, scopes, clientName)` keeps its signature. `LoginWithBrowser` sends `ClientID` too.
- `DescribeDeviceLogin(ctx, userCode)` reads a pending device sign-in as its approver sees it (`GET /v1/auth/device/codes/{user_code}`, signed in): a `DeviceLoginRequest` with `Scopes`, `ClientName`, `ClientID`, `DeviceName`, `ExpiresAt` and `ClientVerified`, which is true only when the API says so and names the app. An unknown, expired or answered code is a 404.
- `APIError.GoString` keeps `%#v` from printing `Problem`, which can hold what the request sent (a validation error's `input`): it shows the status, the code and the display line.
- A close with a refusal code inside the settle window is a refusal only while the hub has sent nothing that decrypted under the new session's keys. Any frame counts -- JSON or binary, a chunk, the hub's encrypted HELLO -- since a hub refuses a key before it says anything; a close after one is a drop.
- A hub that pinned another client key for the connection closes the link the moment the XX handshake that showed this one ends. That is now its own kind of refusal: a `*ClientKeyRejectedError`, matching the new `ErrClientKeyRejected` and still `ErrHubRefused` and `ErrConnection`, whose `KeyFolder` names where this client's key is and `OtherKeyFolder` where another program reading the same identity likely keeps its own ("re-pair, or share the key folder"). After KK the same close stays a plain refusal. `LinkSupervisor` gives up at once on the new `LinkClientKeyRejected` outcome, and `HubSession.Run` returns the error.
- `Client.Connect` reads a close that lands between the transport finishing its handshake and `Connect` looking at it as the transport read it, as `HubSession`'s settle window reads one a moment later. On a slow machine a rejected key, or a refusal, used to come out of that window as a plain `ErrConnection` ("transport returned before authenticated readiness").
- Over HTTPS polling, a request answered 401 or 403 while this client sends the last frames of an XX handshake, or right after, before the hub has sent anything, is the same verdict. `HTTPTransport.ClosedRefused` now reports an HTTP refusal, so `HubSession`'s settle window reads it as it reads a WebSocket close. MQTT cannot see the case: a broker relays no refusal.
- Where the key lives: an identity read from a file (`IdentityFromFile`, `IdentityFromConfig`) now knows it (`Identity.SourcePath()`, never serialized), and a transport with no `NoiseStateDir` keeps its key and hub pins in that file's directory, so every program reading one identity presents one key. The usual `~/.config/thalovant` files keep the directory they always used, so nothing moves. For an identity file elsewhere, the first connection copies the key and pins from the old default -- never moves them, and only when that key has met the hub being dialled -- so no device is handed a new key and locked out. The pins go first and the key last, and a copy that fails writing fails that connection rather than going on with a key of the transport's own, so the next one copies again. A directory the process cannot write falls back to the old default.
- A reply withdrawn while it waits for the send path leaves the link as it was, and a frame already being written is finished whole on every transport. A caller that stops waiting mid-write gets `ErrTimeout` at once while the frame goes on being written; before, the WebSocket closed the connection under it and HTTP/MQTT failed the session. The wait for HTTP's and MQTT's Noise channel can now be withdrawn too. A send that finished after its caller left no longer tears the link down; one whose write then fails retires the session on every transport, as the WebSocket already did, so `Healthcheck` stops reporting it up.
- A compressed part of a WIRE-1 frame inflates to at most 32 MiB, and one that would inflate further, or is truncated, is refused.
- Declares the parity contract's new `link-carriers` capability and runs `link-carrier-vectors.json` over the real HTTPS polling and MQTT transports against loopback TLS hubs, one connect as a kept link makes it. The handshake cases of `link-keeping-vectors.json` now connect the same way (the handshake, then the settle window), with the new `client_key_changed`, `client_key_changed_pinned_here` and `closed_after_first_frame` situations, and the home-link `queued` cases run over a real WSS link. All 159 recorded cases and all nine vector digests equal the reference's (`d33dc2be8b00`).

## 0.11.0 — 2026-09-27

- Link a Home Assistant to a hub, the four capabilities the Python SDK 0.9 adds to the parity contract: sign in on a device one step at a time, create a connection of a named kind, wait for the hub to admit it, and answer the hub's requests on the data plane while keeping the link up.
- Device sign-in one step at a time, for a caller that shows the code on its own screen and polls on its own schedule. `BeginDeviceLogin` returns a `DeviceAuthorization` and prints and opens nothing; `PollDeviceLogin` asks once. Nobody having approved yet is a `*DeviceLoginPendingError` whose `Interval` says when to ask again, and a `slow_down` lengthens the authorization's `Interval` by five seconds for good. Expiry and denial are `*DeviceLoginExpiredError` and `*DeviceLoginDeniedError`, which still match `ErrDeviceCodeExpired` and `ErrDeviceAccessDenied`; all three reach the `*APIError` the API answered with. The approved token is kept on `AccessToken` and its id on the new `ControlPlane.TokenID`, and `RevokeAPIToken` revokes it -- a token may always revoke itself -- and forgets it. Revoking the token in use is idempotent: a token already revoked cannot authenticate its own revoke, so the API's 401 counts as revoked, and revoking again sends nothing. A revoke forgets the token only while it is still the one revoked, so a sign-in that finished meanwhile keeps its token -- even on another goroutine: a sign-in stores the token and its id together, and a revoke checks and clears them, under one lock that is never held across a request. A token answer that is not a JSON object is an error with no status, like one with no token. `HomeAssistantScopes()` is what a Home Assistant link signs in with. An empty scope list is left out, as none is: the API refuses `[]`. `DeviceAuthorization` and `APIToken` print with the device code and the token redacted. `LoginWithBrowser` runs its loop over the same poll. Every sign-in -- password, native, browser, device poll -- sets `TokenID` from its own answer, so a password sign-in clears the id a device login left instead of leaving `RevokeAPIToken` aimed at a token the control plane no longer holds.
- `BootstrapIdentityOptions.ConnectionType` creates a connection of that kind (`ConnectionTypeHomeAssistant` and the other known kinds are constants). The API must say it made that kind: when it makes an ordinary connection instead, the SDK deletes it and returns an `*UnsupportedConnectionTypeError`, as it does when the API answers 422 about the field. A 422 counts as that only when its detail or code, or a validation error's location or message, names the field: a validation error echoes the request, which always carries the kind, so a 422 about any other field is an ordinary `*APIError`. `BootstrapIdentityResult.Operation` is the operation that admits the connection, and `ClientID()` and `ConnectionType()` read the answer.
- `ErrAuth`, `ErrPlan` and `ErrAlreadyLinked` tell apart the refusals a caller acts on differently: sign in again (401, 423, or 403 "Insufficient scopes"), change the plan (402, or 403 `plan_limit`), or the hub already holds its Home Assistant link (409 `home_assistant_already_linked`, with `APIError.LinkedClientID()` naming the connection that holds it). They match through `errors.Is` on the `*APIError` every control-plane call already returned, so no caller sees a different error type.
- `APIError.RetryAfter` is how long the API asked the caller to wait: the body's `retry_after_seconds` (at the top or inside a `detail` object), else the `Retry-After` header, else `RateLimit-Reset`, which is all the API's own rate limiter sends with its plain-text 429.
- A control-plane request that never got an answer -- DNS, the connection, TLS, a proxy -- now also matches `ErrAPIUnreachable` and `ErrConnection`, beside the `ErrAPI` it always matched; its message is unchanged.
- `GetClient` and `DeleteClient`. `DeleteClient` reads the etag when none is given, reads it again and retries once when the connection changed underneath (412), and counts a connection already gone (404) as deleted.
- `WaitForAdmission` follows the admission operation, about ninety seconds. `ready` returns nil; a failed one is an `*AdmissionFailedError` with the operation's `ErrorCode`; no operation or a 404 is admitted at once. A 5xx is ridden out, and a 429 waits what the API asks (a 429 asking for longer than the time left is a timeout at once). A 401 or 403 is the API's own `*APIError`, not a failed admission, since the token is the trouble; any other refusal of the wait is an `*AdmissionFailedError` whose `Err` is the `*APIError` with its status, code and detail, and whose `ErrorCode` stays empty. An API out of reach ends the wait with that error as it is. No read runs past the deadline. Running out of time is an `*AdmissionTimeoutError`, which matches both `ErrConnection` and `ErrTimeout` because the connection may still be admitted later. An operation link on another origin -- scheme, host and port, the default port spelled out -- is refused and never fetched, so the token goes nowhere else.
- `AnswerHomeRequests` answers `thalovant.home.request` with at most one `thalovant.home.response`, sent as a reply along the route the request came, and never after the hub's 10 seconds counted from the request's arrival (`HomeAnswerOptions.HubTimeout`). The handler runs on its own goroutine with 9 seconds or what is left of the 10, whichever is less, and the reply gets what it left: a reply is never started after the bound, and one still waiting to be sent then is withdrawn. The answer goes out at the deadline even when the handler ignores its context. A handler that fails or panics is answered as `failed_to_handle`, one still running at its deadline as `timeout`, and an answer outside the contract as `unknown`, with empty speech so the hub speaks its own sentence in the device's language. `AnswerHomeRequest` answers one and returns a nil payload when there was no time left; `HomeResponse` is the payload on its own.
- Speech goes out as plain text, made the same way in every SDK: `StripSSML` removes only real tags, comments and processing instructions, so "5 < 6 and 7 > 3" stays whole and an unclosed "<b" is text (display text gets the same fix); `DecodeReferences` decodes numeric references, the five XML entities and `&nbsp;` and nothing else, where `html.UnescapeString`'s table differs from every other SDK's; and `PlainSpeech` collapses every run of Unicode White_Space and trims it from the ends (U+001C..U+001F are not white space, and stay).
- `Client.Reply` and `HubSession.Reply` answer an event back along its route (OVOS-MSG-1 §5.2), from a deep copy of the context the hub sent; `ReplyContext` is the rule on its own. A request with a destination and no source gets a reply with no destination, rather than one addressed back to its own sender.
- `HubSession` keeps a link by itself. `Run` makes the first attempt at once and then does what a `LinkSupervisor` decides: a drop dials again at once, a failure waits the 10-to-120-second ladder, a refusal is "not admitted yet" for 10 minutes (`WithRefusalGrace`) before `Run` returns it, and a changed hub key ends `Run` at once. `On` registers a handler for one event type that keeps working across reconnects and is never called after its unsubscribe returned, `OnStateChange` reports the link going up and down, and `Connect` makes one attempt that must stay up for a settle window (750 ms, `WithSettle`). `NewHubSession` takes these as options after the policy, so existing calls compile unchanged. `LinkSupervisor` is the decision on its own, as a pure function of the outcomes and their times.
- Refusals and changed keys are told apart. A handshake answer that does not authenticate under the connection's password (a wrong password), a WebSocket upgrade answered 401 or 403, and a close with no status, 1000, 1005 or 1008 during the handshake or within 750 ms after it all match `ErrHubRefused`; any other close is a drop. A hub whose Noise key is not the pinned one matches the new `ErrHubKeyChanged`, a connection error that is not a refusal; the pin is never replaced. On every transport -- WSS, HTTP and MQTT -- a KK handshake that fails is followed at once, inside the same connect, by one XX handshake, since only XX tells a changed password from a changed hub key; the pin is still checked when it completes. Over HTTP a request answered 401 or 403 is a refusal too, as a WebSocket upgrade answered so is; MQTT has no refusal signal from the hub, so there only an answer that does not authenticate leads to XX. A close that lands between the end of a WSS handshake and Connect returning is read at once, as it would be a moment later. `WSSTransport.ClosedRefused` and `Client.ClosedRefused` report whether the last connection ended in a refusal.
- Declares the parity contract's `device-login`, `connection-kinds`, `connection-admission` and `home-link` capabilities, run against the Python reference's five vector files over a loopback HTTP server, a loopback WSS hub with real Noise, and the pure close and supervisor rules, and recorded in `contracts/conformance-results.json`; every case produces what the reference produced.

## 0.10.6 — 2026-09-26

- `*APIError` carries what the API said, not only the line built from it. `Problem` is the whole error body decoded, when it is a JSON object; `Code` is its machine-readable code; `ProblemDetail` is its sentence whole, exactly as sent. The line `Error()` prints was the only place any of this reached a caller, and it is cut at 256 runes: a `platform_image_required` refusal names every image each refused key may be instead, which is longer than that, so the list a caller needed was the part cut off -- and `refused_images`, `allowed_images` and `allowed_repositories` never reached anybody at all. The same held for every structured refusal, `plan_limit`'s `resource`, `limit` and `used` included. `StatusCode`, `Detail` and the message are unchanged, the message still never repeats a value the body echoed back from the request, and an `APIError` literal with only `StatusCode` and `Detail` reads as it always did.
- `Code` and `ProblemDetail` are also read from inside a `detail` that is itself an object -- FastAPI's own envelope, which the API's Problem+JSON handler normally lifts. A code or detail that is not a string, or is only whitespace, is absent (`""`).
- Every non-2xx control-plane response -- any call, the read half of a guarded configuration merge, and the device sign-in's token poll -- builds its error in one place that decodes the body once.
- Declares the parity contract's new `api-errors` capability, run against the Python reference's `api-error-vectors.json`: thirteen responses served by a loopback HTTP server and read back through `GetHub`, with the results recorded in `contracts/conformance-results.json`.

## 0.10.4 — 2026-09-21

- `UsualForm` returns the lower-case usual form whenever the tag is not already that exact string: `UsualForm("en-US")` and `UsualForm("en_US")` are `"en-us", true`, and only `"en-us"` itself is `"", false`. It used to compare with `SameLanguage`, which folds case, so the canonical BCP 47 spelling counted as "already usual" and `Intents` never retried it, although the hub's manifest is keyed by the exact lower-case tag and listed nothing under `en-US`. Ported from the Python reference; `usual_form_test.go` pins both spellings. (This entry was missing when 0.10.4 shipped.)

## 0.10.1 — 2026-09-18

- A refusal ends an ask at once instead of letting it run to the deadline. The hub sends `hive.policy.denied` the instant it refuses, with no request id, and the request-id gate dropped it: the ask waited out its whole budget, and a caller told somebody their hub "did not answer in time" about a question it had refused and explained. A denial with no request id is now taken when it names the type this ask sent and this ask is the only utterance the client has out; with a second ask or a query in flight either could be the one refused, so neither takes it.
- An ask returns `*PolicyDeniedError` rather than a bare `ErrRuntime` wrap, with `Quota` -- `Period`, `Limit`, `Used`, `ResetAfter` -- for a spent `intent_quota_exceeded` and a message that fits the refusal. `PolicyCodeACL`, `PolicyCodeQuotaExceeded` and `PolicyCodeBackendUnavailable` name the three codes. Both new errors still match `errors.Is(err, ErrRuntime)`.
- An unmatched intent returns the new `*UnansweredError`: the hub understood and has nothing for it, which is not a failure.
- `Allowed` holds only non-blank, trimmed strings.
- `UnansweredError.Said` carries what the person said. Both event names put the input in the event's text; the old read of `reason`/`error` left it empty.
- A fire-and-forget utterance is recorded immediately before the publish, so the grace window is not spent waiting for transport ownership; a publish that errors keeps its record, because the transport can fail after the hub already holds the frame. The slice is pruned as entries are added, so a client that only ever sends does not keep them for its lifetime.
- A refusal on a quota the hub sent no numbers for says a quota has run out, rather than claiming "all questions used".
- A float count outside the `int64` range reads as 0 rather than being converted, with an exclusive upper bound: `math.MaxInt64` as a `float64` rounds up to 1<<63, which `int64` cannot represent and Go leaves to the implementation.
- README documents the refusal surface: the three codes, `Quota`, `UnansweredError`, and when an uncorrelated denial is this ask's.
- A fire-and-forget utterance -- `SendUtterance`, `SendAction`, `SendCode`, or `Emit` of `recognizer_loop:utterance` -- counts as in flight for 10 s after it is sent, so a refusal of it cannot end an unrelated ask. The ask publishes its own through an internal path, so it never counts itself.
- `Quota` counts are `int64` and never negative: no narrowing conversion, and a negative limit, usage or reset time reads as 0.
- Declares the parity contract's new `refusal` capability, run against the Python reference's `refusal-vectors.json`.

## 0.10.0 — 2026-09-16

- Speak the rest of the HiveMind protocol. A hub relays more than this client's conversation, and the five hive kinds -- `broadcast`, `propagate`, `escalate`, `intercom`, `rendezvous` -- fell off the end of `dispatchNoiseMessage` with no case and no log line. `ListenHive` subscribes to one kind, and `Propagate`, `Escalate` and `Broadcast` send. A refusal is a disconnection rather than an error: a hub's HELLO says nothing about what a client may do, so nothing can check first.
- Receive binary frames. This is how a hub answers `speak:synth`: it renders the utterance and sends the audio back, so a client with no synthesiser of its own can still speak, and it is how a file arrives. `DecodeHiveBinaryFrame` read the WIRE-1 header and then JSON-parsed the payload, so a frame carrying raw audio failed; it now reads the four payload-type bits and hands over the clip untouched, and `ListenBinary` delivers it. Checked against `binary-frames.json` -- hivemind-bus-client's own encoder output, not frames this SDK built for itself.

## 0.9.1 — 2026-09-13

- Expose advisory reply claim status and first-seen pipeline/skill identifiers, with shared conformance for fallback, mixed stages, legacy hubs and malformed stamps. Existing reply construction remains compatible.

## 0.9.0 — 2026-09-13

- Add managed hub sessions with persistent subscriptions, bounded background retry backoff, terminal close, and no automatic replay of admitted requests. Preferred-origin selection delegates address binding and failed-attempt cleanup to the transport builder.
- Add presentable skill/intent inventories, regional example selection, tri-state catalogue locale support, and private best-effort inventory caches. Explicit language order survives JSON serialization across SDKs; invalid cache records become misses.
- Match Python question detection, including unnamed-locale patterns and Unicode question marks, with shared executable conformance vectors.
- Require reviewed Python reference and consumer evidence in PR and publishing parity checks, with scheduled fresh-dependency conformance checks.

## 0.8.1 — 2026-09-12

- Refresh bundled listing rules to thalovant-languages 0.2.1, matching Python 0.6.8 across 270 languages. Preserve regional inheritance and the corrected French/Spanish trailing-word behavior.
- Regenerate public-reference cases for every shipped locale, including Spanish questions and French complete phrases.

## 0.8.0 — 2026-09-12

- Add locale-aware intent sentence rendering, canonical slot examples, complete-phrase ranking and OVOS-compatible regional language selection.
- Snapshot custom language rules, preserve the selected locale and count unique rendered examples toward limits.
- Bound question regex evaluation by deadline and backtracking stack size; failures leave bare lines instead of guessing punctuation.

## 0.7.0 — 2026-09-12

- Preserve JSON number precision in both caller deltas and stored configuration snapshots during conditional merges.

- Match Python 0.6.3 request hints, location construction, ordered embedded audio replies, strict bounded hex decoding, and speakable intent examples with original phrase priority.
- Default runtime configuration updates to revision-guarded deep merges. Retry only HTTP 412 (three attempts maximum); fail before writing against older servers. Explicit replacement remains available. Merging now requires both hubs:read and hubs:write scopes, plus a paid plan.
- Add regression coverage for conflict preservation, retry limits, unsupported revisions, audio bounds, caller context preservation, and example ranking.
- `AskOptions` and `Reply` gain fields. Migrate positional struct literals to keyed fields when upgrading; `RuntimeGroupConfigOptions` retains its existing fields.

## 0.6.0 — 2026-09-12

- Add hub-addressed skill listing, history, install, update and removal.
- Add optional bounded polling and explicit operation resumption without repeating accepted writes.
- Document shared-runtime scope, authorization and cancellation behavior.

## 0.5.4

- Refuse malformed saved Noise pins, including null or empty values, before loading or modifying trust. Preserve corrupt files for explicit diagnosis; never silently treat them as first contact.
- Reject invalid pin inputs before writing. Valid pins require a nonempty node ID and exactly 32 bytes of hexadecimal key material; verified rotation still requires `ForgetNoisePin`.

## 0.5.3 - 2026-09-09

- Reject concurrent Ask calls sharing a request ID and concurrent Query calls
  sharing a query ID before publication; release reservations with collectors.
- Reject empty hub ETags locally before any update or delete request.
- Preserve describe timeouts when prior replies contain no definitions; fully
  answered empty inventories and usable partial definitions remain supported.
- Correct caller idempotency-key reuse, error handling, and 429 retry metadata
  documentation; add historical review regression coverage.

## 0.5.2

- Recover HTTP cleanup after a lost success response by recognizing the upstream exact already-disconnected acknowledgment, while retaining admission responsibility until that confirmation.
- Reject contradictory or explicitly failed disconnect acknowledgments; retain replica affinity and authenticated pins through a cleanup retry and KK reconnect.
- Add real TLS/Noise lost-response and malformed/refused acknowledgment regressions.

## 0.5.1

- Start Ask settlement on the first nonempty speech, keep fixed empty/settlement windows within the original deadline, and return collected speech at that deadline.
- Collect replies while an admitted send retires, retaining transport cleanup ownership and never replaying application requests.
- Freeze Query immediately on policy denial or explicit query timeout; a soft intent miss cannot invalidate speech already collected.
- Require the matching request ID for Ask, accept runtime-replaced session IDs, and reject unrelated or uncorrelated ambient replies.
- Add race-tested regressions for fixed settlement, deadline-clipped speech, hard terminal replies during suspended sends, cleanup ownership, and soft-failure recovery.

## 0.5.0

- Require Go 1.26 or newer and use patched `golang.org/x/crypto` 0.56.0.
- Add `WaitForEvent` and filtered `Listen` with correlation/predicate options, bounded buffers, event limits, explicit timeout/disconnect/overflow errors, and cancellation that unsubscribes immediately.
- Validate device verification URLs before display/browser launch and keep URLs as direct arguments without a command shell.
- Refuse control-plane redirects and require HTTPS for credential-bearing requests except literal local development endpoints, including injected HTTP clients. Sanitize transport errors without retaining query strings or arbitrary credential-bearing causes.
- Include connection diagnostics in caller deadlines, preserve SDK timeout classification, drain queued reply fragments at settlement, and retain HTTP teardown ownership after caller cancellation.
- Add `IntentsWithCapabilities`, `ListFallbacks`, and `HubIntentCapabilities.MayAnswer`: optional fallback discovery distinguishes unknown from known-empty and shares one bounded connect/send/reply budget. Silent unified listings use engine manifests by default.
- Add independent bounded subscriptions on all built-in transports so concurrent Ask, Query and inventory calls retain their own replies. Slow observers fail explicitly with `ErrEventOverflow`.
- Add `AskWithOptions` for delayed speech and fragment settlement, preserving existing request option literals. Soft intent misses can recover with later speech; hard denials retain partial-reply failure information.
- Enforce authenticated connection readiness, bounded connection/send/close callers, retained cleanup ownership, cancellable queued operations and generation-specific WSS teardown. Failed HTTP admission cleanup must succeed before a new admission.
- Publish complete Noise identity/trust files atomically under a crash-released cross-process lock; reject conflicting pins and unexpected trust-file types.
- Expand CI to race tests on Linux/macOS/Windows, minimum/current Go, vulnerability checks and parser fuzzing, including real TLS/Noise transport and process-crash regressions.

## 0.4.5

- Reset this transport object's prior HTTP admission before reconnecting after
  a failed session. The listener does not issue a fresh Noise offer while the
  old peer is still registered. Keep the static key and hub pin for XX-to-KK
  recovery; a first connection does not disconnect an unknown existing peer.

## 0.4.4

- Implement the deployed HiveMind v3 Noise exchange for HTTP and MQTT, including
  persistent static keys, hub pin continuity, encrypted HELLO and bus traffic,
  chunked messages, and XXpsk2/KKpsk0 reconnects. Capability offers alone never
  mark a transport ready; plaintext sends cannot bypass the session.
- HTTP retains replica affinity cookies, uses the binary send/poll endpoints,
  and rejects error JSON even when the HTTP status is 200.
- MQTT serializes encrypted publishes and fails closed on broker disconnects.
  Call `Connect` again to resubscribe and negotiate a new session. Add
  `TLSConfig` for broker trust roots, plus `NoiseStateDir` and `RemoteStaticKey`
  on both HTTP and MQTT.
- WSS no longer drops the trusted hub key after a failed KK exchange and clears
  session/readiness on disconnect or read failure.
- Validate real local TLS HTTP and MQTT round trips, same-object reconnects,
  concurrent chunked messages, wrong passwords, tampering/replay, HTTP errors,
  plaintext rejection, and key-change rejection.

## 0.4.1

- **Security.** Move `golang.org/x/crypto` from `v0.44.0` to `v0.55.0`, which
  also lifts `golang.org/x/net` to `v0.57.0`. `v0.44.0` carries
  CVE-2026-56854 (critical) and nine highs. The `0.4.0` root module resolved
  `golang.org/x/net v0.47.0`, which carried five more; this was newer than
  the `v0.46.0` required by `x/crypto v0.44.0`. `0.4.0` shipped with all fifteen.

  The pin came from adding the Noise dependencies for v3: `go get
  golang.org/x/crypto@latest` wanted `v0.56.0`, which raises the `go`
  directive to `1.26`, so I pinned back to keep `go 1.25.0` — and pinned far
  further back than that needed. `v0.55.0` builds on `go 1.25.0` unchanged, so
  the module's stated Go floor is untouched.

## 0.4.0

- **Breaking.** `wss` connections now perform the HiveMind v3 Noise handshake,
  and only that. A HiveMind-core 5.x hub accepts no other key exchange, so this
  release requires one; against an older hub the connection is refused rather
  than downgraded. `Noise_XXpsk2_25519_ChaChaPoly_SHA256` on first contact and
  `Noise_KKpsk0_...` once the hub's static key is pinned, with
  `25519_AESGCM_SHA256` supported where a hub prefers it.
- **Breaking.** `Identity.CryptoKey` is gone, along with the AES helpers
  (`EncryptAsJSON`, `DecryptFromJSON`, `EncryptAsBinary`, `DecryptBinary`,
  `RuntimeCryptoKey`) and the pre-shared handshake on all three transports.
  Hubs no longer issue a crypto key and v3 derives its pre-shared key from the
  `password`, so the field named a credential that no longer exists.
  `crypto_key` is still read from an existing identity file and ignored, and it
  stays in the bootstrap redaction list so an older payload carrying one does
  not leak it. `https` and `mqtt` now rely on TLS for confidentiality, as they
  already did for everything the crypto key did not cover.
- The Noise pre-shared key is derived from `password` with argon2id
  (`time_cost=3`, 64 MiB, `parallelism=1`), salted with SHA-256 of the hub's
  node id. A transport caches it per hub, so only the first connection pays the
  few hundred milliseconds.
- Two files persist beside the SDK config file, both `0600`: `noise_key` (this
  client's static X25519 key) and `noise_pins.json` (the hub keys it has
  pinned). `WSSTransport.NoiseStateDir` overrides the location, and
  `LoadOrCreateNoiseKey`, `LoadNoisePin`, `SaveNoisePin` and `ForgetNoisePin`
  are exported for callers that manage the state themselves.
- Trust on first use: the first hub key seen for a node id is pinned, and a
  later connection presenting a different key is refused with an error naming
  `ForgetNoisePin`, rather than silently re-pinned. A failed `KKpsk0` handshake
  drops the stale pin, because `KK` needs each side to hold the other's key and
  the failure is as likely to mean the hub no longer has this client's.
- `WSSTransport.RemoteStaticKey()` reports the hub's static key for the current
  session.
- A `wss` connection that the hub refuses now fails with the close reason
  instead of running out the handshake clock: a wrong password reported as a
  twenty second timeout hid what had actually happened.
- `WSSTransport.sendCleartext` reads the connection once under the lock.
  `readLoop` calls it during the handshake while `Connect`'s timeout branch can
  be running `Disconnect`, which clears it -- an unsynchronized read raced that
  and could dereference nil. `Disconnect` now captures and clears under one
  lock and closes outside it, and `Connect` sets it under the lock.
- A read loop is bound to the connection attempt that started it. `Disconnect`
  does not wait for it to exit, so a loop from a previous attempt could
  overwrite `lastError` and close the *new* `readDone`, aborting a fresh
  handshake with a stale error.
- **Breaking.** `HTTPTransport.Connect` now refuses a hub endpoint that is not
  `https://`, for the same reason as the MQTT change below: TLS is the only
  confidentiality left on that hop, and the access key travels in the
  `authorization` query.
- `CreateClientIdentity` drops `cryptoKey` and `crypto_key` from a
  caller-supplied `opts.Spec` rather than merging them into the request. The
  generated-secret redaction covers only what the SDK mints, so a legacy value
  passed in by a caller could otherwise be echoed back inside an `ApiError`.
- **Breaking.** `MQTTTransport.Connect` now refuses a broker whose identity does
  not enable TLS. Removing the crypto key took the separate payload cipher with
  it, so TLS is the only confidentiality left on that hop; without it every
  message and the broker password would travel in the clear. Use an `mqtts://`
  endpoint, or set `tls: true` on the identity's `mqtt` block.
- Messages larger than one Noise transport message are chunked at 65000 bytes
  and reassembled by the peer, with reassembly capped at 32 MiB. Any transport
  message that fails to decrypt, and any malformed chunk sequence, drops the
  session rather than the frame.

## 0.3.14

- `ListIntents` returns an error wrapping `ErrRuntime` when the hub answers
  `ovos.intent.list` with `ok: false`, instead of reading the missing
  `intents` key as an empty list. A refused listing is not an empty hub, and
  reporting it as no intents showed a person a device that can do nothing;
  the error carries the hub's own `error` text. `Intents` fails the same way
  and does not fall back to the engines' manifests, which answer a policy
  denial rather than a query the hub could not produce. `DescribeIntent`
  keeps returning an empty list for `ok: false`, which is a real answer: the
  hub does not know that registration. Reported by the Kotlin port's review.
- Say in the README that a connection needs `ovos.intent.describe` only when
  definitions are asked for (the default); listing alone needs
  `ovos.intent.list`.

## 0.3.13

- Add the intent inventory: `client.Intents(ctx, languages)` reads the hub
  runtime's intent manifest (OVOS-INTENT-4 §10) over the client's own session
  and returns a `HubIntentInventory` — every intent each skill registered, per
  language, with the sentences a person says to reach it as the skill's locale
  files wrote them, `{slot}` placeholders included. No control-plane credential
  is involved. `client.ListIntents(ctx, lang)` and
  `client.DescribeIntent(ctx, skillID, intentName, lang)` expose the two
  underlying queries (`ovos.intent.list` / `ovos.intent.describe`) as
  `[]IntentRegistration` and `[]IntentDefinition`. All three take optional
  `IntentOptions{Timeout, Describe, Fallback, IncludeDefinitions}`; `Describe`
  and `Fallback` are `*bool` that default to true when nil, like
  `DeviceLoginOptions.OpenBrowser`.
- Export the result model: `HubIntentInventory` (`Languages`, `Skills`,
  `Source`, `Denied`, `Intents()`, `HasPhrases()`), `HubSkillIntents`
  (`SkillID`, `Intents`, `Languages()`) and `HubIntent` (`SkillID`, `Name`,
  `Engine`, `Enabled`, `Languages`, `Phrases`, `ID()`, `PhrasesFor(lang)`,
  `Examples(lang, limit)`), with snake_case JSON tags so `json.Marshal` matches
  the Python SDK's `as_dict()`. `Examples` prefers whole sentences to ones with
  a slot, shorter first. `SameLanguage` compares tags case-insensitively with
  `_` and `-` folded, and `IntentSourceManifest` / `IntentSourceEngines` name
  the two sources.
- Queries are correlated by `context.request_id` like `Ask`, and a reply
  delivered more than once is taken once. Describes are sent together and
  matched by request id, or by the definition's own `skill_id`/`intent_name`/
  `lang` for a hub that does not echo the id; a describe the hub does not
  answer in time leaves that intent without sentences rather than failing the
  inventory.
- Add `PolicyDeniedError` (wrapping `ErrRuntime`, retrieved with `errors.As`),
  returned at once from the hub's `hive.policy.denied` with `DeniedType`,
  `Code`, `Reason` and the `Allowed` list, instead of waiting for a timeout.
  `Intents` falls back to the engines' own manifests
  (`intent.service.adapt.manifest.get` / `intent.service.padatious.manifest.get`)
  when `ovos.intent.list` is refused, unless `IntentOptions.Fallback` is false;
  the result then carries names only, `Source` set to `engine-manifests` and
  `Denied` naming the refused query.
- A runtime that attaches each row's `definition` to `ovos.intent.list` when
  asked with `include_definitions` is used as such; one that does not is
  described row by row.
- Send describes in batches of at most `DescribeBatch` (32), each batch its
  own window with its own deadline, instead of putting every request in
  flight at once. A hub with 69 intents in two languages is 138 requests and,
  with every reply delivered twice, 276 inbound events — more than a
  transport's reply channel holds (`BusEvents` buffers 32), so replies past
  its capacity stall the transport's read loop and the inventory comes back
  missing sentences. Reported by the Rust port's review and settled in the
  Python reference at 0.4.38. A hub that answers nothing now fails after one
  batch rather than holding every request open.
- Keep partial results across describe batches: a batch that received no
  reply contributes nothing, and the call fails only when no batch produced a
  definition. Batches are contiguous slices of the work, so one unresponsive
  skill can own a whole batch — a skill with more than 32 intents would
  otherwise turn the entire inventory into a timeout, while the same skill
  with fewer only lost its sentences. A hub silent from the start still fails
  at the first batch, and a refusal still stops the call whenever it arrives.
  Reported by the Rust port's review and settled in the Python reference at
  0.4.39.
- Four readings settled with the Python reference (0.4.37) so every SDK reads
  the same: `HasPhrases()` is true only when at least one intent carries at
  least one sentence; `Intents` trims each language tag and asks a language
  repeated under another spelling (`en-us`, `en-US`, `en_us`) once, keeping
  the first spelling given; an intent registered under both engines in one
  language keeps the template row's sentences whichever order the rows
  arrive in, and the first row names its engine; on the names-only fallback
  the first engine to name an intent decides its engine (adapt is asked
  before padatious).
- Add the event-name constants `EventIntentList`, `EventIntentListResponse`,
  `EventIntentDescribe`, `EventIntentDescribeResponse`,
  `EventAdaptManifestGet`, `EventAdaptManifest`, `EventPadatiousManifestGet`
  and `EventPadatiousManifest`.
- No existing signature changed.

## Unreleased

### Breaking

- Remove the admin analytics path from `GetAnalyticsOverview`. The
  `AnalyticsOverviewOptions.Admin` and `AnalyticsOverviewOptions.OwnerID` fields
  are gone, and the call always targets `GET /v1/analytics/overview`; the
  `GET /v1/admin/analytics/overview` branch and its `owner_id` query are
  removed. This SDK serves non-admin customers, so callers that set `Admin` or
  `OwnerID` must drop them.
- Migrate the MQTT data-plane topic scheme to `<topic_prefix>/in|out|status`.
  `MqttBrokerCredentials.TopicPrefix` is now the full plaintext base
  (`hivemind/<hub-id>/<access-key>`) and the transport appends the fixed
  suffixes: publish requests go to `<prefix>/in`, subscribe replies to
  `<prefix>/out`, and the retained presence/LWT to `<prefix>/status`. The old
  `<base>/c2s|s2c|status/<access-key>` scheme, the `HashTopics` hashing, and the
  hub-id fallback are removed. `MqttTopicSet` renames its `C2S`/`S2C` fields to
  `Inbound`/`Outbound`, and `MqttBrokerCredentials` drops the `HubID`,
  `C2STopic`, `S2CTopic`, `StatusTopic`, and `HashTopics` fields (and their
  `hub_id`/`c2s_topic`/`s2c_topic`/`status_topic`/`hash_topics` JSON keys and
  `MQTT_*` environment variables). `TopicPrefix` is now required:
  `MQTTTopicsForIdentity` errors when it is empty.

### Fixed

- Recognise `ovos.intent.unmatched` as a terminal failure event (#22). OVOS
  renamed the "no intent matched" bus event from the legacy Mycroft
  `complete_intent_failure`, which was the only name in the failure-event set, so
  an utterance matching no intent was never treated as terminal and the
  interaction/query loop waited out its full timeout instead of failing promptly.
  Both names are now recognised; the legacy name is retained for older runtimes.

### Security

- Redact secrets from human-facing formatting. `Identity`,
  `MqttBrokerCredentials`, and `ControlPlane` now implement `String()`, so the
  `%v`, `%s`, and `%+v` verbs print `[REDACTED]` in place of the access key,
  password, crypto key, MQTT username/password, control-plane access token, and
  any userinfo embedded in data-plane endpoint URLs. This changes formatted
  output only: `json.Marshal` — the wire protocol and the persisted identity
  file — still serializes the real secret values.
- `BootstrapIdentityResult.Summary(false)` — the default — now redacts the
  secrets echoed in the raw `hub` and `client` maps (the `initial_identify`
  access key/password/crypto key/MQTT credentials, the `initial_identify_token`,
  and the spec's `apiKey`/`password`/`cryptoKey`), matching how it already
  redacts the identity. `Summary(true)` is unchanged and still returns every
  secret in the clear; never log it.
- Strip the `?authorization=` query from data-plane transport errors before they
  are stored in `LastError`, so `ConnectionInfo()`/`Healthcheck()` and their
  JSON no longer leak the data-plane access key when a connection fails.
- Surface only an allowlist of non-secret JSON message fields (`detail`,
  `message`, `error`, ...) in control-plane HTTP errors, bounded and
  single-line, and omit any other body. A failed `POST /v1/clients` response can
  echo the just-sent `apiKey`/`password`/`cryptoKey`, so arbitrary response body
  text is never interpolated into an error string.
- Document the inverted boolean polarity of the two `Map` methods
  (`HubDataPlaneEndpoints.Map(redactCredentials)` redacts when true;
  `MqttBrokerCredentials.Map(includeSecrets)` reveals when true) and the
  protocol-mandated appearance of the access key in the MQTT client ID and topic
  segments.
- Redact `MqttBrokerCredentials.TopicPrefix` from human-facing formatting. Since
  the topic migration `TopicPrefix` carries `hivemind/<hub-id>/<access-key>`,
  `MqttBrokerCredentials.String()` — and the `Identity.String()` that embeds it —
  now print `[REDACTED]` for it instead of the raw prefix, closing a `%v`/`%s`/
  `%+v` path that leaked the same access key the username/password redaction
  already hides. `json.Marshal` still serializes the real prefix.
- Harden `topic_prefix` validation in `MQTTTopicsForIdentity`. The prefix is
  trimmed of surrounding whitespace before its leading/trailing `/`, a
  whitespace-only prefix is now rejected with the existing topic_prefix error,
  and a prefix containing an MQTT wildcard (`#` or `+`) or an ASCII control
  character (`< 0x20`, including NUL) is rejected, so a malformed or
  subscription-widening base can no longer build the `<prefix>/in|out|status`
  topic set. The topic_prefix error also drops its trailing period (staticcheck
  ST1005).

## 0.3.6

- Add hub provisioning to the control plane: `CreateHub`, `UpdateHub`,
  `DeleteHub`, `ReleaseHub`, `SetHubRating`, `ClearHubRating`, and
  `GetHubRuntimeCapabilities`. `CreateHub` always sends an `Idempotency-Key`
  header, generating one unless `HubCreateOptions.IdempotencyKey` is set.
  `UpdateHub` and `DeleteHub` take `etag` as a required argument, not an
  option, because the API rejects a missing or stale `If-Match` with HTTP 412.
- Add runtime-group management: `ListRuntimeGroups`, `GetRuntimeGroup`,
  `CreateRuntimeGroup`, `UpdateRuntimeGroup`, `GetRuntimeGroupConfig`,
  `UpdateRuntimeGroupConfig`, `ReleaseRuntimeGroup`, and `DeleteRuntimeGroup`.
  These routes read no `If-Match` and no idempotency header, so the SDK sends
  neither. Configuration is merged rather than replaced, and personas are
  replaced only when `RuntimeGroupConfigOptions.Personas` is set.
- Add skill discovery and installation: `ListMarketplaceSkills`,
  `ListRuntimeGroupMarketplace`, `ListRuntimeGroupInventory`,
  `InstallRuntimeGroupSkill`, and `UninstallRuntimeGroupSkill`. A zero-value
  `RuntimeGroupSkillInstallOptions` installs an active skill from the
  marketplace catalog.
- Export the option types the new calls take: `HubCreateOptions`,
  `ReleaseOptions`, `RuntimeGroupConfigOptions`,
  `RuntimeGroupSkillInstallOptions`, `MarketplaceSkillListOptions`,
  `RuntimeGroupMarketplaceOptions`, and `RuntimeGroupInventoryOptions`. False
  and empty options are omitted from the query string, and an unset
  `ReleaseOptions` sends an empty JSON body so the API applies the workspace
  release policy.
- Accept the camelCase spellings of the hub and runtime-group body keys
  (`runtimeGroupId`, `ownerId`, `capacityProfile`, `isLocked`,
  `cloneFromDefault`) and rename them to snake_case before sending, so a
  camelCase payload is no longer silently dropped by the API's request model.
- Document the plan and scope requirements: the provisioning writes need a paid
  plan and `hubs:write` (HTTP 402 on the free plan), hub ratings need
  `hubs:write` but no paid plan, the marketplace catalog needs only `hubs:read`
  and is not paid-gated, and `ListRuntimeGroupInventory` and
  `ListRuntimeGroupMarketplace` need `hubs:inspect`. `ListRuntimeGroupInventory` reports a pending source instead
  of the HTTP 409 `GetHubRuntimeCapabilities` returns when nothing is
  reporting.
- No existing signature changed.

## 0.3.5

- Derive both user agents from a single exported `Version` constant in
  `version.go` instead of hand-maintained literals. `DefaultUserAgent` and
  `DefaultControlUserAgent` keep their names, exportedness, constant-ness, and
  values; they are now `"ThalovantGoSDK/" + Version`, resolved at compile time,
  so the data-plane and control-plane copies can no longer disagree.
- Pin the user agents in tests against the derived value rather than a version
  literal, require `Version` to equal the `VERSION` file, and fail the suite if
  any `.go` source hard-codes a `ThalovantGoSDK/<version>` literal again.
- Stop rewriting `constants.go` during the automatic release bump, which no
  longer contains a version, and make the remaining `VERSION` and `version.go`
  replacements fail loudly when their target literal is absent. A silent no-op
  in that step is what left the control-plane user agent at
  `ThalovantGoSDK/0.3.0` after 0.3.1.

## 0.3.4

- Document the two HTTP 429 responses the control plane returns for
  token-authenticated calls: `token_rate_limited` (the plan's per-minute
  request rate, 60 requests per minute on the free plan) and
  `token_quota_exceeded` (the plan's daily or monthly call quota, reported in
  `quota`, `limit`, and `used`). Both carry a `Retry-After` header and a
  matching `retry_after_seconds`, both are returned as errors wrapping
  `ErrAPI`, `Retry-After` is authoritative, and the SDK does not retry
  automatically.

## 0.3.3

- Add browser device-flow sign-in: `ControlPlane.LoginWithBrowser` and
  `DeviceLoginOptions` request a device authorization, present the
  verification URI and user code (custom `Prompt` supported), open the
  browser on a best-effort basis, and poll `/v1/auth/device/token` honoring
  the server `interval` and `slow_down` backoff until the request is
  approved, denied (`ErrDeviceAccessDenied`), expired
  (`ErrDeviceCodeExpired`), timed out (`ErrTimeout`, 15 minutes by default),
  or the context is cancelled. The approved durable API token is stored on
  the `ControlPlane` exactly like `Login`.
- Document direct API-token auth for CI and other non-interactive use: pass a
  pre-provisioned token to `NewDefaultControlPlane`/`NewControlPlane` (for
  example from a `THALOVANT_API_TOKEN` environment variable) instead of
  calling a login method.

## 0.3.2

- Add MFA login support: `LoginOptions` and `ControlPlane.LoginWithOptions` send
  optional `otp_code`/`recovery_code` fields, omitting them when empty. The
  existing `Login` signature is unchanged.
- Realign the control-plane user agent with the module release; it had been
  left at `ThalovantGoSDK/0.3.0` since 0.3.1.

## 0.3.1

- Bump the `go-routine-updates` dependency group: `golang.org/x/net` 0.55.0 to
  0.57.0 and `golang.org/x/sync` 0.17.0 to 0.22.0 (both indirect).

## 0.3.0

- Raise the supported toolchain floor to Go 1.25, the oldest upstream-supported Go release.
- Upgrade `golang.org/x/net` to 0.55.0 to remediate four high-severity dependency findings.

## 0.2.17

- Avoid overflow-prone capacity arithmetic when encoding caller-controlled binary payloads.
- Give CI and release-guard workflows explicit read-only repository permissions.
- Keep data-plane and control-plane user-agent versions aligned with the module release.

## 0.2.16

- Add `OperationResource` and `ControlPlane.GetOperation` for durable command polling.
