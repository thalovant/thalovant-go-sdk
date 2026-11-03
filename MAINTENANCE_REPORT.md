Last Edit: Claude Opus 5.5 - 2026-11-03 - Motive: Record the CodeRabbit follow-ups to #65, #66, #68 and #73.

# Maintenance Report

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
