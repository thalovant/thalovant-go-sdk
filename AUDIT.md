Last Edit: Claude Opus 5.5 - 2026-11-03 - Motive: Start the audit log with what the CodeRabbit follow-up pass found.

# Audit

Known issues, technical debt and their status, from test runs and code reading. Each entry names the code it concerns.

## Fixed

- **`UsualForm` suppressed its own retry for `en-US`** (CodeRabbit on #65). Fixed in d61624f (#66, v0.10.4): byte comparison in `UsualForm` (`language_matching.go`), pinned in `usual_form_test.go`. The CHANGELOG entry was missing and is added now.
- **`%#v` printed `APIError.Problem`** (CodeRabbit on #68). Fixed in 5715299 (#70): `APIError.GoString()` in `errors.go`, tested in `api_error_vectors_test.go`.

## Open

- **`UsualForm` differs from the Python reference on two kinds of tag** (found by running both on the same inputs, 2026-11-03; not changed here):
  - A tag with an empty subtag (`"en-"`, `"en--US"`, `"en-US-"`): the reference returns `None`; `UsualForm` returns `"en-us", true`.
  - A language CLDR knows only through its macrolanguage (`"arb"`, `"cmn"`): the reference returns `arb-eg` / `cmn-cn`; `UsualForm` returns `"", false` because `matching.Likely` has no direct entry.
  - The Rust port answers the same as Go. No shared vector covers `usual_form`, so the parity gate sees neither difference.
  - Dangling singletons (`"en-u"`) already match the reference (`en-us`).
