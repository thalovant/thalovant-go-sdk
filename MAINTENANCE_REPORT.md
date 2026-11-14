Last Edit: Claude Sonnet 5.5 - 2026-11-12 - Motive: Intercom review fixes, round 2 (CodeRabbit PR 92): urgent-scope code before the generic scope refusal, the accepted announcement kept on a malformed POST answer, docs aligned with the API; Python digest a0722fec2c54.

## 2026-11-14 — connector-connections (0.17.0; Python digest `8687c8429e28`)

Ported the `connector-connections` capability from thalovant-python-sdk 0.13.0 (depends on thalovant-api #521, not deployed; additive and safe against the old API): `connections.go` (`ConnectionTypeConnector`, `Serves`, `ServesWarning`, replay detection, secret scrub), `control.go` (`CreateClientIdentity` reads the create's response headers, returns `*CredentialsNotReplayedError` on a replay and `*ConnectionTypeUnavailableError` on `connection_type_unavailable`; `BootstrapIdentityResult.Replayed`), `errors.go` (six sentinels, `ConnectionCode*`, accessors `Known`, `LinkClientID`, `Links`, `Settings`, status-and-code matching in `refusal()`), `connector_connections_test.go` (15 tests mirroring the reference's cases), `VERSION`/`version.go` to 0.17.0 (0.16.0 is published), the parity record. Verification: `gofmt -l .` empty, `go vet ./...` clean, `go test -count=1 ./...` 521 passed, 0 failed, 1 skipped (`TestNoiseInteropAgainstLiveHub`), 15 more than before; parity check verified. Not verified against a live API.

Transparency Report: AI model Claude Sonnet 5.5; actions: ported the connector-connections behaviour, tests and parity record from the Python reference; oversight: none yet, review pending.

## 2026-11-12 — Intercom review fixes, round 2 (CodeRabbit PR 92; Python digest `a0722fec2c54`)

**Code**: `refusal()` (`errors.go`) matches an intercom code before the generic 403 "Insufficient scopes" refusal, so `errors.Is(err, ErrUrgentScopeRequired)` is true for a 403 that carries both (it is still an `ErrAuth`); `Announce` (`intercom.go`) returns the partly decoded announcement (its `ID` when the answer named one) with the "unexpected announcement shape" error instead of an empty one, so a caller can read the accepted announcement with `GetAnnouncement` before sending again. **Docs**: the README example checks the error and an empty list, and picks a speaker with `CanPlay`; it cancels only after a successful `Announce`; the FAQ says to create the key before the first call and reuse that exact key (with `Wait` false, which makes no status read), that a `failed` record is not proof nothing was spoken, and that `errors.Is(err, ErrIntercomDisabled)` also matches the platform and unavailable kinds (check those first); the constant-key paragraph names both outcomes; `CancelAnnouncement` is documented as open to the owner, a manager, a plain member, an administrator and an impersonated session (`thalovant-api` `app/routes/intercom.py` `cancel_announcement`); `ErrTooManyTargets` says naming the same room or group does not help. **One suggestion declined**: the review asked to remove "`intercom:send` implies `intercom:read`" (Node PR 112 comment; the same text is in each README), but the API grants it (`thalovant-api` `app/auth/utils.py:232` `expand_granted_scopes`, applied to every token at `app/auth/deps.py:236`; `CONSOLE_API_NOTES.md` 9.1), so the opposite statements ("`wait` needs `intercom:read` as well", "a send-only token fails the first poll") were the defect and were removed everywhere in this repo.

Tests: `TestIntercomAnAnswerWithAnIDButNoStatusKeepsTheID` and `TestIntercomTheUrgentScopeCodeBeatsTheGenericInsufficientScopesRefusal` (`intercom_test.go`). Verification: `go test ./...` (Go 1.26, linux/amd64: 506 top-level tests, 505 passed, 0 failed, 1 skipped (`TestNoiseInteropAgainstLiveHub`, needs a live hub); two of them are new), `go vet ./...`, `gofmt -l .` clean; `scripts/check-sdk-parity.py` against the Python reference (digest `a0722fec2c54`).

Transparency Report: AI model Claude Sonnet 5.5; actions: fixed the review findings, verified the scope and cancel rules against the API source, added tests, refreshed the parity record; oversight: none yet, review pending.

## 2026-11-11 — Intercom final API (platform security review; Python digest `69badc4fb072`)

Ported the reference's final-API round (thalovant-python-sdk, `src/thalovant/intercom.py`, `docs/intercom-porting.md`). `intercom.go`: scope constants (`IntercomUrgentScope` beside read and send), `IntercomSpeaker.Kind`/`CanPlay`/`PolicySynced` with the API's defaults (`UnmarshalJSON`), `IntercomSpeakerList` (`ListIntercomSpeakers` now returns it: a signature change on an unreleased branch), `IntercomDelivery.Detail` and `KnownDeliveryDetail`, `Announcement.Replayed` read from `Idempotency-Replayed` by `controlIntoHeader` and kept after `Wait`, `IntercomUnavailablePlatform` and the other reason constants, the `IntercomCode*` problem-code constants, and the API's validation: `AnnouncementText` (no `<` `>`, categories Cc/Cf/Cs/Co/Zl/Zp except U+200C/U+200D, invalid UTF-8, no echo), every target list at most 50 names of 1-64 characters, `Lang` pattern, `validIdempotencyKey` (128 printable ASCII), `validIntercomTimezone` (`time.LoadLocation` with a shape-only fallback probed with `America/New_York`) and the DND rule (enabled needs a zone and `Start != End`). `errors.go`: sentinels for the 13 new codes with the reference's hierarchy (`ErrUrgentScopeRequired` is an `ErrAuth`, `ErrUrgentBusy` an `ErrRateLimited`, `ErrIntercomUnavailable` and `ErrIntercomPlatformDisabled` kinds of `ErrIntercomDisabled`), accessors `AnnouncementStatus`, `Reason`, `UrgentScope`, `SpeakerKind`, `NotSpeakers`, `Matched`, `RefusedClientRefs`, and `refusal()` deciding the intercom codes by code whatever the status. Deviations from the reference: no NFC normalisation (`golang.org/x/text` is not a dependency; `AUDIT.md`), `Detail` is `""` where Python has `None`, DND and speaker updates are closed structs so there are no unknown keys to refuse. `VERSION` stays 0.16.0 (unreleased; the convention bumps once per release). Docs: README, QUICK_FACTS, FAQ, AUDIT, SUGGESTIONS, CHANGELOG; `intercom:urgent` and what `delivered` means are said everywhere the scopes and statuses are; examples keep a fresh `thalovant.NewRequestID()` key.

Verification: `gofmt -l .` empty; `go vet ./...` clean, also with `GOOS=windows` and `GOOS=darwin`; `go test -count=1 ./...` (Go 1.26, linux/amd64): 504 top-level tests, 503 passed, 0 failed, 1 skipped (`TestNoiseInteropAgainstLiveHub`, needs a live hub), 45 more than before this round (`intercom_test.go`). `-race` was not run: the machine was under a load average above 50 from other work. `contracts/sdk-parity.json` acknowledges `69badc4fb072703e675ce50ae3a09765bf49cbf65efcc2fef5befb46f2b32a8c` and the evidence hashes of `intercom.go`, `errors.go` and `intercom_test.go` were refreshed after the final test run; `check-sdk-parity.py --workspace /tmp/ic-work --consumer thalovant-go-sdk` passes. Not verified against a live API.

Transparency Report: AI model Claude Sonnet 5.5; actions: ported the final intercom API from the Python reference to Go (code, tests, docs, parity record); oversight: none yet, review pending.

## 2026-11-11 — Intercom amendment 2 (client-side security review, contract section J)

Added `ControlPlane.CancelAnnouncement` (`POST .../announcements/{id}/cancel`), `Announcement.HubConfirmed` (`*bool`, tolerant decoding through `UnmarshalJSON`), `AnnouncementCancelled`, `ErrNotCancellable` (409 `not_cancellable`) and `APIError.AnnouncementID()` for the 502 (`errors.go`). `validIntercomID` (`intercom.go`) refuses `.`, `..`, empty ids and ids holding `/`, `?`, `#`, a backslash or a control character before any request, for hub, client and announcement ids (review L1). `AnnounceOptions` now implements `String`, `GoString`, `Format` and `slog.LogValuer` so the text never prints (review L4). The SDK sets no request timeout, so the announce timeout item needs no code; the docs say to give `Announce` at least 20 s (review M2). Docs: every example uses a fresh `thalovant.NewRequestID()` key; README, FAQ and QUICK_FACTS say that the same key replays the stored result and never re-sends, that a replayed `failed` stays `failed`, that a deliberate second send needs a new key, and to read the announcement after a 502 (review H3); the wording no longer says "nothing listens" or "stops every announcement at once" (J7). Tests (`intercom_test.go`): cancel route/body/scope/409, `HubConfirmed`, `AnnouncementID`, unsafe ids on every call, a replayed failed announcement, redaction under every verb and slog, and a docs guard. Verification: `gofmt`, `go vet`, `go test ./...`, `scripts/check-sdk-parity.py --consumer thalovant-go-sdk`. Not done (not in section J): L5, L6, a `Replayed` field (`SUGGESTIONS.md`, `AUDIT.md`).

Transparency Report: AI model Claude Sonnet 5.5; actions: implemented contract section J for the Go SDK (code, tests, docs, parity record); oversight: none yet, review pending.

## 2026-11-11 — intercom review fixes (CodeRabbit, PR 92; Python digest `66bfb1ac5879`)

`Announce` rejects a negative `WaitTimeout` or `PollInterval` before the POST (zero stays "the default"); each poll runs under a context bounded by the time left (`context.WithDeadline`), no read starts once the sleep used the deadline, a read cut by the deadline returns the latest `sending` answer, and a poll that fails for another reason returns the last accepted `Announcement` with the error instead of an empty one. `IntercomDNDUpdate.Enabled` is a required `*bool`: the API replaces the whole DND object and stores a missing `enabled` as false (`app/schemas/intercom.py`, `Dnd`), so making it optional would not preserve the stored value; a nil is refused. README: the example lists the speakers, addresses the speaker by its `ClientID` and sets its room before targeting it. Docs: `Wait` needs `intercom:read` too, a poll can fail after the POST (reuse the idempotency key), `ErrPlan` is 402 and 403 `plan_limit`, and `ErrHubUnreachable` no longer says a retry is safe. Verification: `gofmt`, `go vet ./...`, `go test -race -count=1 ./...`, parity check against the Python reference.

Transparency Report: AI model Claude Sonnet 5.5; actions: fixed the review findings, added tests, refreshed the parity record; oversight: none yet, review pending.

## 2026-11-11 — intercom (0.16.0)

Ported the `intercom` capability from thalovant-python-sdk 0.12.0 (digest `690784dbb9af`, since moved to `66bfb1ac5879`): `intercom.go`, seven `ControlPlane` methods, three error kinds in `errors.go`, `intercom_test.go` (loopback fake API), `VERSION`/`version.go` to 0.16.0, the parity record, README, QUICK_FACTS, FAQ, AUDIT and CHANGELOG. The `errors.go` evidence hash of the other capabilities was refreshed after `go test ./...` passed: the change is additive.

Transparency Report: AI model Claude Sonnet 5.5; actions: wrote the module, methods, tests, docs and parity record from the contract and the Python reference; oversight: none yet, review pending.

## 2026-11-08 — second review of #90 (final-contract commit 291b49b)

CodeRabbit opened nine threads on the final-contract commit. Seven are fixed, two are answered as the cross-SDK contract and reported to the rollout lead. Nothing the 119 shared vectors decide changed: the recorded conformance file is byte-identical to the committed one, and equal to the reference's.

- **Fixed:** redaction is a `secretScrubber` built once per answer with no regular expression, linear per value, and a refused answer over 1 MiB or with more than 64 distinct tokens is omitted rather than echoed; a token that carries `[redacted]` is matched whole (a match is skipped only when it lies wholly inside an existing marker); an oversized `HEAD` gets its 400 without a body; `BrowserLoginFailedError` documents `access_denied` sent with another error; two listener tests close their listener on an early failure; the parity reason says 119 cases; the README says no scopes leaves `scope` to the API's default.
- **Answered as the contract:** hexadecimal hosts (`0x100000000`) stay usable, as the reference's `_real_host` has them, and the ahead-of-reference capability is the rollout order the contract sets, gated by the Python checker in the Python repository.
- **Parity:** evidence refreshed only for the changed files (`browser_login.go`, `loopback.go`, `browser_login_test.go` in `browser-login`); `control.go` and the reference digests are unchanged.
- **Verification:** `gofmt -l .` empty; `go vet ./...` clean for linux, windows and darwin; `THALOVANT_CONFORMANCE_OUT=... go test -race -count=1 -timeout=5m -v ./...`: 448 top-level tests pass, 0 fail, 1 skipped (`TestNoiseInteropAgainstLiveHub`), no data race; the 119 recorded `browser-login-vectors.json` digests equal the reference's; `check-sdk-parity.py --consumer thalovant-go-sdk` verified against the reference's `main` (44b6e2b) and against 39e31d8. Each fix's test fails with the fix reverted.

Transparency Report. AI Model: Claude Opus 5.5 (claude-opus-5-5). Actions Taken: read each thread against the code, the final reference and its brief; wrote the fixes and six tests; checked each test against a reverted fix; replied on and resolved every thread. Oversight: the rollout lead asked for the round and decides on the two contract items; hosted CI runs on the pull request. Not merged, tagged or released.

## 2026-11-08 — browser sign-in against the final reference (Python 39e31d8)

The producer (thalovant/thalovant-python-sdk#113) is final at 39e31d8, reference digest `6dfb3bb31ac7590591febef1c4d8372984ca6cdad4700381f869db30c38452df`. This ports every row of its porting brief's "What changed since 49744ee385bf" table.

- **Vectors:** `testdata/browser-login-vectors.json` re-vendored byte for byte (sha256 `b0c73f808c4f26328c27dd4d04eebfbbc3e7c0c81b8d63a666f63737d0259e73`, 119 cases). All 119 run and are recorded; each digest equals the reference's `contracts/conformance-results.json`, the 98 cases of the first round record exactly what they did, and no other vector file's results moved.
- **Ported:** (1) no control-plane request off the API's origin (`sendRequest`, `control.go`); (2) `token_endpoint` without a colon and with whole escapes only; (3) `authorization_endpoint` on port 1-65535 with a host, as written, that is a bracketed IPv6 address, IPv4 or ASCII labels (`dashboardHost`); (4) only RFC 6749's seven codes kept from a callback (`oauthErrors`, `loopback.go`); (5) only a missing device name gets the default: `DeviceName` is now `*string` on both options (unreleased API, so nothing breaks); (6) no 60-second bound on the accepted tab; (7) HEAD answers without a body; (8) one-pass, longest-first redaction before formatting, never inside `[redacted]`; (9) every answered connection drained (write side shut, at most 2 s / 1 MiB) before it closes; (10) the "delivers nothing to the sender" wording in the README, CHANGELOG and package comment. `sendNoReplay` stays. Rows 6-9 have unit tests mirroring the Python tests the brief names; rows 1-5 and 7-9 were each checked by reverting the fix and seeing its test fail (row 6 cannot be, in the reference either).
- **Parity:** `reference_digests` keeps `57c7e71f6456…` and replaces the interim `49744ee385bf…` with `6dfb3bb31ac7…`. Evidence refreshed for the files that changed: the `browser-login` entry (`browser_login.go`, `loopback.go`, `control.go`, both test files, the vectors) and `control.go` in `api-errors`, `chat-apps`, `connection-admission`, `connection-kinds`, `control` and `device-login`, whose tests and vectors pass unchanged with the origin check.
- **Verification:** `gofmt -l .` empty; `go vet ./...` clean for linux, windows and darwin; `go test -race -count=1 -timeout=5m -v ./...`: 443 top-level tests pass, 0 fail, 1 skipped (`TestNoiseInteropAgainstLiveHub`), no data race; `check-sdk-parity.py --consumer thalovant-go-sdk` verified against the reference's `main` (44b6e2b) and against 39e31d8.

Transparency Report. AI Model: Claude Opus 5.5 (claude-opus-5-5). Actions Taken: read the brief's change table and the reference's diff since e3999cb (`native_auth.py`, `_loopback.py`, `control.py`, its tests); ported each row and wrote the mirroring tests; re-vendored the vectors and re-recorded them; refreshed the evidence of the changed files; updated these records. Oversight: the rollout lead sent the final reference and the rows to port and merges; hosted CI runs on the pull request. Not merged, tagged or released.

## 2026-11-08 — review of #90 (browser sign-in)

CodeRabbit left 14 threads on #90. Twelve are fixed, each with a test that fails when the fix is reverted (checked one by one); two are answered on their threads and left as the reference and the porting brief have them. Nothing a shared vector decides changed: all 98 recorded `browser-login-vectors.json` digests, and every other recorded result, are identical to the committed ones.

- **Fixed:** the exchange is sent at most once (`sendNoReplay` in `control.go`, no `GetBody`, since HTTP/2 replays a written body after a peer `PROTOCOL_ERROR`); a refused exchange's body is redacted after decoding and before its display line is cut, including any `access_token`, `refresh_token` or `id_token` it carries (`redactExchangeAnswer`); a consent-page `error` carrying the state is not repeated in `Code`; a ctx done by the time `Prompt` returns opens nothing; the listener handles at most 64 connections at once; an authorization endpoint on port 0, a token endpoint with a malformed escape, and a `[]string` method list are handled; the goroutine guarantee, the incomplete-head note and the printing rule are corrected in the doc comments, CHANGELOG, README and FAQ.
- **Answered, not changed:** the accepted tab's 60-second wait (the porting brief bounds it; reported to the rollout lead), and `Prompt` on the calling goroutine (the reference calls its prompt synchronously, as the print it replaces).
- **Live check:** API #501 is deployed. `GET https://api.thalovant.com/v1/auth/native/config` answered 200 with `authorization_endpoint` `https://dash.thalovant.com/authorize`, `token_endpoint` `/v1/auth/native/token`, `sdk_client_id` `thalovant-sdk` and `["S256"]`; `ParseNativeSignInConfig` accepts it, and a `BrowserLogin` against it with nothing opened built the consent URL on a loopback redirect and expired after 1 s as it should, holding no token.
- **Parity:** `browser-login` evidence and the `control.go` hash in the six capabilities that list it refreshed from the files the passing run tested; the reference digests are unchanged (`57c7e71f6456…`, `49744ee385bf…`), pending the producer's final one.
- **Verification:** `gofmt -l .` empty; `go vet ./...` clean for linux, windows and darwin; `go test -race -count=1 -timeout=5m -v ./...`: 433 top-level tests pass, 0 fail, 1 skipped (`TestNoiseInteropAgainstLiveHub`), no data race; `check-sdk-parity.py --consumer thalovant-go-sdk` verified against the reference's `main` (44b6e2b) and PR head (e3999cb).

Transparency Report. AI Model: Claude Opus 5.5 (claude-opus-5-5). Actions Taken: read each thread against the code, the Go 1.26 transport source and the porting brief; wrote the fixes and seven tests; checked each test against a reverted fix; ran the live configuration check; replied on and resolved every thread. Oversight: the rollout lead asked for the review round and decides on the two contract items; hosted CI runs on the pull request. Not merged, tagged or released.

## 2026-11-08 — browser sign-in (0.15.0)

Ports the Python SDK's browser sign-in (thalovant/thalovant-python-sdk#113 at e3999cb, porting brief `docs/browser-login-porting.md`): the authorization code grant with PKCE on a loopback redirect, and the auto/browser/device chooser. The owner's goal is that a sign-in can be approved only by whoever started it; a device code can be typed in anywhere, while with a loopback redirect a link or code sent to someone on another computer delivers nothing to the sender. Device login is unchanged and is the fallback.

- **Added:** `browser_login.go` (`BrowserLogin`, `InteractiveLogin`, `LoginMethod`, the options, `NativeSignInConfig`/`ParseNativeSignInConfig`, `BrowserAuthorizationURL`, `LoopbackRedirectURI`, `NewState`, `CanOpenBrowser`, `DefaultDeviceName`, `BrowserLoginFailedError`, the exchange and the secret scrubbing), `loopback.go` (the listener, `classifyRequest`, the `parse_qsl`-compatible decoding, the pages), `loopback_windows.go` (`SO_EXCLUSIVEADDRUSE`) and `loopback_other.go`; tests `browser_login_vectors_test.go` (all 98 vector cases, recorded) and `browser_login_test.go` (25 edge tests); `testdata/browser-login-vectors.json`, byte-identical to the reference (sha256 `9058adf2ad30fd8b7f255882ce718865170d0d4b84d36f8fbfa23bde51d1651e`).
- **Changed:** `control.go`: `LoginWithBrowser` delegates to the new unexported `loginWithDevice`, which takes an opener, and `openWith` opens with the caller's opener on its own goroutine or with `openBrowser` as before. `VERSION` and `version.go` to 0.15.0. `contracts/sdk-parity.json`: the candidate reference digest `49744ee385bf…` added beside `57c7e71f6456…`; the new `browser-login` capability with its implementation and test hashes; the `control.go` hash refreshed in `api-errors`, `chat-apps`, `connection-admission`, `connection-kinds`, `control` and `device-login`, after the full suite passed on the change, since none of their behaviour changed (device-login's reason says why the file moved). `contracts/conformance-results.json` regenerated by the suite: the `browser-login-vectors.json` block is new, and every other block is identical to the committed one. README ("Signing in through the browser"), CHANGELOG (0.15.0), QUICK_FACTS, FAQ, AGENTS.md (the sign-in rule), AUDIT and SUGGESTIONS.
- **Rationale for the listener:** the porting brief suggests `net/http`'s server for Go, but its behaviour rules decide otherwise. `net/http` answers a head it cannot read with its own text/plain 400 (and one over its limit with 431), without the page headers every answer must carry; it refuses an HTTP/1.1 request without `Host` before any handler runs, which the reference serves; and `Server.Close` does not wait for its connection goroutines. `loopback.go` therefore reads the request head itself, as the reference does over asyncio streams, counts every goroutine, and waits for them in `close`.
- **Rationale for the errors:** denied and expired are the device-login types, as the brief requires, wrapped so their messages say "browser sign-in" (`errors.As` and `errors.Is` reach them unchanged). A callback without a code needs "no status, but a code", which `*APIError` cannot express, hence `*BrowserLoginFailedError`.
- **Verification:** `test -z "$(gofmt -l .)"` passes; `go vet ./...` is clean, also with `GOOS=windows` and `GOOS=darwin`; `THALOVANT_CONFORMANCE_OUT=... go test -race -count=1 -timeout=5m -v ./...` passes: 426 top-level tests, 0 failures, 1 skipped (`TestNoiseInteropAgainstLiveHub`, needs a live hub), no data race. The 98 recorded `browser-login-vectors.json` case digests and the file digest equal the reference's `contracts/conformance-results.json`. `python3 check-sdk-parity.py --workspace <ws> --consumer thalovant-go-sdk` verifies the record against the reference's `main` (44b6e2b) and against the PR head (e3999cb). Two mutations were caught by the new tests (see AUDIT.md). `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` finds no reachable vulnerability and `go mod verify` passes. Not run here: the macOS and Windows test legs, which CI runs on the pull request.

Transparency Report. AI Model: Claude Opus 5.5 (claude-opus-5-5). Actions Taken: read the contract, the porting brief, the Python implementation, tests and vectors; wrote `browser_login.go`, `loopback.go`, the two platform files, the `control.go` delegation and both test files; vendored the vectors; regenerated the conformance record from the suite's own run; updated the parity record with hashes of the files the passing run tested; wrote the documentation and these records. Oversight: a lead agent coordinates the cross-SDK rollout and merges; the owner set the goal and the contract; hosted CI (the full OS matrix and the parity gate) runs on the pull request. Not merged, tagged or released by this change.

## 2026-11-08 — release 0.14.2

#88 (b4d5e55) was merged with `[skip ci]`, so no workflow ran on that push and no release was cut; v0.14.1 stayed the latest tag. `auto-release.yml` has no manual trigger and `release.yml` needs `VERSION` already bumped, so the release goes the way v0.14.1 did: this commit dates the CHANGELOG's Unreleased section as 0.14.2 (and adds the missing 0.14.1 entry), and on its push to `main` the Auto Release plan finds `home.go`, `intents.go` and `language_matching.go` changed since v0.14.1, bumps `VERSION` and `version.go` to 0.14.2 itself and publishes v0.14.2.

- **Changed:** `CHANGELOG.md` (0.14.2 and 0.14.1 headings), `QUICK_FACTS.md` (version), this file. No code.
- **Verification:** #88's CI passed on its head ed36579, whose tree is b4d5e55's (`e18417b9`); this PR's CI runs the full matrix on the same Go tree, and the release job runs `gofmt`, `go vet` and `go test` again on the bump commit.

Transparency Report. AI Model: Claude Opus 5.5 (claude-opus-5-5). Actions Taken: read the release workflows, wrote the CHANGELOG headings and these notes, merged once CI was green. Oversight: release authorised by the owner ("fix all the coderabbit issues", 2026-11-08); hosted CI gates the merge.

## 2026-11-08 — CodeRabbit review-body items on merged PRs

Every "Nitpick", "Outside diff range" and "Duplicate" item in CodeRabbit's review bodies on #23, #24, #29, #31, #48, #65, #69 and #70, checked against `main` at b160343. No inline thread on those PRs was still open.

| PR | Item | Disposition |
| --- | --- | --- |
| #23 | Reject an empty `etag` locally in `UpdateHub`/`DeleteHub` | Fixed since c2effa9 (#42): both refuse a blank etag with `ErrAPI` before any request (`TestEmptyHubETagFailsBeforeAnyRequest`) |
| #24 | Assert that `owner_id` is absent from the analytics query | Fixed since c2effa9 (#42), `TestControlPlaneGetsAnalyticsOverview` |
| #29 | The describe batch has no bound on requests in flight | Fixed before merge, in #29's own squash 2fe5972: `describeMany` sends at most `DescribeBatch` (32) at once |
| #31 | Remove the unused `protocolV3` | Fixed since c2effa9 (#42) |
| #48 | `session.go` and `inventory.go` missing from the auto-release paths | Fixed before merge, in 0215ebb: both listed, and every root `.go` file that is not a test counts |
| #48 | The scratch-symlink assertion could not fail | Fixed before merge, in 0215ebb: the symlink sits on the final cache path |
| #65 | `ListedIn` nil on the engine fallback | Fixed here, as the reference does it: empty (`[]`), not the asked languages, since nothing was listed by manifest; doc comment corrected |
| #65 | `Nearest: false` dropped by the option merge | Fixed since e2bb00f (#79, 0.12.7) |
| #65 | `en-x` read as `en` | Matches the Python reference (`_parse` stops at a singleton the same way), so not changed. Running both on 4,453 tags found two real differences, already listed in AUDIT.md, and both are fixed here: an empty subtag, and languages known through their macrolanguage |
| #69 | Keep the reply inside the 10 s bound | Fixed before merge, in #69's squash 60c6da3: one deadline from arrival, shared by the handler and the reply |
| #69 | A `Replier` that ignores its context leaves a goroutine per request | Fixed here: at most four replies still running past the bound, counted per subscription or, for calls on their own, per `Replier`; then none is started and the error says so; `Replier` doc states the contract |
| #69 | A handler that ignores its context leaves a goroutine per request | Fixed here: at most four handlers still running past their time; then a request is answered `timeout` without starting one, as the reference does with its four handler threads |
| #70 | HTTP: retire the session when an admitted write fails after its caller left | Fixed before merge, in #70's squash 5715299: `sendRetiring` with the channel-guarded `failSession` (`TestAWriteThatFailsAfterItsCallerLeftRetiresTheSession`) |
| #70 | MQTT: fail the generation in the same case | Fixed before merge, in 5715299: `failGeneration`, guarded by generation, same test |

- **Changed:** `home.go` (`homeOverrunLimit`, `homeBounds`, `replierOverruns`, `homeRun`; `AnswerHomeRequest` now calls `answerHomeRequest` with shared counts, replies kept apart per `Replier`, and `AnswerHomeRequests` passes its own), `intents.go` (`inventoryFromNames`, the `ListedIn` comment), `language_matching.go` (`UsualForm`). Tests: `home_link_test.go` (seven new tests, and the context-deaf Replier test now releases its goroutine so `-count=N` runs do not share it), `intents_test.go`, `usual_form_test.go`. CHANGELOG under Unreleased; parity reason and hashes in `contracts/sdk-parity.json`.
- **Verification:** `gofmt -l .` empty, `go vet ./...` clean, `go test -race -count=1 -timeout=5m ./...` passes (391 top-level tests pass, 1 is skipped: the live-hub interop test), and the new home tests pass 20 times in a row under `-race` and 3 times under `-shuffle=on`. Each new test fails with its fix taken out. The recorded conformance results are byte-for-byte what this change produces. The reference's parity checker (`check-sdk-parity.py --consumer thalovant-go-sdk`) verifies the acceptance and fails with the old evidence hashes.
- **Review on the fix PR (#88):** CodeRabbit asked for the shared reply count to be kept per `Replier` (`replierOverruns`, keyed by the `Replier` itself so only the same pointer or an equal value shares a count, an uncomparable value not counted, an entry dropped at zero) and for `TestASubscriptionCountsItsOwnStuckHandlers` to wait for other tests' slow handlers before reading the shared count; both done.
- **Release:** none cut here; the change touches `home.go`, `intents.go` and `language_matching.go`, so it is release-relevant for the next patch.

Transparency Report. AI Model: Claude Opus 5.5 (claude-opus-5-5). Actions Taken: read each review item and the current code, ran the Python reference's `usual_form` and this port's `UsualForm` on the same tags, wrote the fixes, tests and this entry. Oversight: hosted CI gates the merge; no human review before merge.

## 2026-11-06 — golang.org/x/net 0.62.0

Upgraded for CVE-2026-78661. Verification: `go build ./...` and `go test ./...` pass with Go 1.26.10.

Transparency Report. AI Model: Claude Opus 5.5. Actions Taken: ran `go get` and `go mod tidy`, then built and tested. Oversight: requested by the owner ("make sure all is perfect"); no human review before merge.

# Maintenance Report

## 2026-11-03 — CodeRabbit follow-up of #83

- `TestChatAppReadRoutesAskWhatTheAPIServes` (`chat_apps_test.go`) now asserts what `GetBridge` decodes (`Network`, `ETag`, `Bot.Username`), not only that it returned no error, as CodeRabbit's last review-body item on #83 asked. Test-only: no release. The `chat-apps` test evidence hash in `contracts/sdk-parity.json` is refreshed.
- Verification: `go vet ./...` and `go test -count=1 ./...` pass; the parity checker verifies the acceptance.
- Transparency: Claude Opus 5.5 (claude-opus-5-5) wrote the assertion and this entry; hosted CI gates the merge.

## 2026-11-03: chat apps (0.14.0)

- **Change:** added the owner routes under `/v1/bridges` to `ControlPlane` (`chat_apps.go`), typed
  models with string enums that keep unknown values, `SecretValue` for credentials, and
  `APIError.Fields` (`errors.go`, filled in `apiErrorFromResponse` in `control.go`). README section
  "Chat apps", CHANGELOG entry, version 0.14.0 in `VERSION` and `version.go`, and a `chat-apps`
  entry in `contracts/sdk-parity.json` with refreshed hashes for every capability whose evidence
  files changed (`control.go`, `errors.go`).
- **Review round:** `send` now returns an `*APIError` for a body cut short instead of the partial
  bytes, and `GetBridgeAvatar` reads at most 4 MiB (`sendWithin` in `control.go`).
- **Why:** hub owners can connect chat networks to their hubs, and every SDK exposes the same
  surface (the Python SDK is the parity reference).
- **Release:** the PR bumps the minor version itself, so the auto-release publishes v0.14.0 on merge
  instead of a patch bump.

### Verification

- `test -z "$(gofmt -l .)"`, `go vet ./...`: clean.
- `go test -race -count=1 ./...`: 384 top-level tests pass, 1 skipped (live hub only); 16 of them are
  new in `chat_apps_test.go`.
- README examples compiled in a scratch module against this tree.
- `check-sdk-parity.py --consumer thalovant-go-sdk`: verified; `chat-apps` reported as not yet
  known to the reference (expected until the Python reference declares it).

### Transparency Report

- **AI Model:** Claude Opus 5.5 (claude-opus-5-5).
- **Actions Taken:** wrote `chat_apps.go` and `chat_apps_test.go`, added `APIError.Fields`, bumped the
  version, and wrote the CHANGELOG, README and parity entries and these repository notes.
- **Oversight:** automated tests and CodeRabbit review; merged by the repository owner's delegated
  agent.

## 2026-11-03 — CodeRabbit follow-ups (#65, #66, #68, #73)

Four review threads were still unresolved on merged pull requests. Each was checked against `main` (f8ec1b3).

| Old PR | Finding | Outcome |
| --- | --- | --- |
| #65 | `ListIntents` forwards `en-US` unchanged and `UsualForm` suppresses the retry. | Already fixed in d61624f (#66, released in v0.10.4). `UsualForm` compares bytes (`language_matching.go`), so `Intents` retries `en-US` as `en-us`; `usual_form_test.go` pins it. `ListIntents` sends the tag as given, as the Python reference does. |
| #66 | Document `UsualForm`'s canonical spelling and byte comparison in the public artifacts. | The godoc already said the form is lower case because the manifest is keyed that way, and the parity contract acknowledged the reference digest. The CHANGELOG had no entry for v0.10.4. **Added here.** |
| #68 | `%#v` exposes `APIError.Problem`. | Already fixed in 5715299 (#70): `APIError.GoString()` (`errors.go`), tested in `api_error_vectors_test.go`. |
| #73 | The docs.thalovant.com Go page documented v0.9.3. | Already fixed on the docs site (#202 and the version syncs after it). The Go page installs with `go get` and points to Supported Versions, which lists v0.13.1. |

Verification: documentation-only change (CHANGELOG.md, this file, AUDIT.md). `CHANGELOG.md` is not a release path for `auto-release.yml`, so merging cuts no release. `go test ./...` was run on `main` while probing `UsualForm`.

## Transparency Report

- **AI Model**: Claude Opus 5.5.
- **Actions Taken**: Verified four CodeRabbit threads against current code, the Python reference and docs.thalovant.com; added the missing v0.10.4 CHANGELOG entry; probed `UsualForm` on edge tags against the reference (see AUDIT.md); wrote this report and AUDIT.md.
- **Oversight**: Pull-request CI. No human reviewed this change before the pull request was opened.
