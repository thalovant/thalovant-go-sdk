Last Edit: Claude Opus 5.5 - 2026-11-06 - Motive: Upgrade golang.org/x/net to 0.62.0 for CVE-2026-78661.

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
