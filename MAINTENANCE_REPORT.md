Last Edit: Claude Opus 5.5 - 2026-11-03 - Motive: Record the chat apps change (0.14.0) and its verification.

# Maintenance Report

## 2026-11-03: chat apps (0.14.0)

- **Change:** added the owner routes under `/v1/bridges` to `ControlPlane` (`chat_apps.go`), typed
  models with string enums that keep unknown values, `SecretValue` for credentials, and
  `APIError.Fields` (`errors.go`, filled in `apiErrorFromResponse` in `control.go`). README section
  "Chat apps", CHANGELOG entry, version 0.14.0 in `VERSION` and `version.go`, and a `chat-apps`
  entry in `contracts/sdk-parity.json` with refreshed hashes for every capability whose evidence
  files changed (`control.go`, `errors.go`).
- **Why:** hub owners can connect chat networks to their hubs, and every SDK exposes the same
  surface (the Python SDK is the parity reference).
- **Release:** the PR bumps the minor version itself, so the auto-release publishes v0.14.0 on merge
  instead of a patch bump.

### Verification

- `test -z "$(gofmt -l .)"`, `go vet ./...`: clean.
- `go test -race -count=1 ./...`: 381 top-level tests pass, 1 skipped (live hub only); 13 of them are
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
