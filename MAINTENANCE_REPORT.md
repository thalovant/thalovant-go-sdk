Last Edit: Claude Opus 5.5 - 2026-11-08 - Motive: Check every CodeRabbit review-body item on #23, #24, #29, #31, #48, #65, #69 and #70 against main and fix the ones still open.

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
| #69 | A `Replier` that ignores its context leaves a goroutine per request | Fixed here: at most four replies still running past the bound; then none is started and the error says so; `Replier` doc states the contract |
| #69 | A handler that ignores its context leaves a goroutine per request | Fixed here: at most four handlers still running past their time; then a request is answered `timeout` without starting one, as the reference does with its four handler threads |
| #70 | HTTP: retire the session when an admitted write fails after its caller left | Fixed before merge, in #70's squash 5715299: `sendRetiring` with the channel-guarded `failSession` (`TestAWriteThatFailsAfterItsCallerLeftRetiresTheSession`) |
| #70 | MQTT: fail the generation in the same case | Fixed before merge, in 5715299: `failGeneration`, guarded by generation, same test |

- **Changed:** `home.go` (`homeOverrunLimit`, `homeBounds`, `homeRun`; `AnswerHomeRequest` now calls `answerHomeRequest` with shared counts and `AnswerHomeRequests` passes its own), `intents.go` (`inventoryFromNames`, the `ListedIn` comment), `language_matching.go` (`UsualForm`). Tests: `home_link_test.go` (four new tests, and the context-deaf Replier test now releases its goroutine so `-count=N` runs do not share it), `intents_test.go`, `usual_form_test.go`. CHANGELOG under Unreleased; parity reason and hashes in `contracts/sdk-parity.json`.
- **Verification:** `gofmt -l .` empty, `go vet ./...` clean, `go test -race -count=1 -timeout=5m ./...` passes (388 top-level tests pass, 1 is skipped: the live-hub interop test), and the new home tests pass 20 times in a row under `-race`. Each new test fails with its fix taken out. The recorded conformance results are byte-for-byte what this change produces. The reference's parity checker (`check-sdk-parity.py --consumer thalovant-go-sdk`) verifies the acceptance and fails with the old evidence hashes.
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
