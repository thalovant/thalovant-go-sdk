Last Edit: Claude Sonnet 5.5 - 2026-11-11 - Motive: Intercom final API: `Replayed` and the caps are done; shared vectors matter more; NFC; `message` in `ProblemDetail`.

# Suggestions

## Scrub `input` and `ctx` from `Problem` on intercom calls

- **Problem:** `APIError.Problem` keeps the whole API body; a regression in the API's error middleware would put the announcement text there (review L5).
- **Proposal:** in `Announce`, drop `input` and `ctx` from `Problem` before returning.
- **Impact:** low; defence in depth.

## NFC-normalise the announcement text

- **Problem:** the API counts the 280 characters after NFC; this SDK counts as sent (no normaliser in the standard library, `golang.org/x/text` is not a dependency), so a decomposed text can be refused locally that the API accepts.
- **Proposal:** either add `golang.org/x/text/unicode/norm` (one more module in the supply chain, already an indirect neighbour of `golang.org/x/net`) or drop the local count and let the API's 422 decide.
- **Impact:** low effort; closes the one place the Go rule is stricter than the reference.

## `ProblemDetail` does not read `message`

- **Problem:** the final intercom problems carry their sentence in `message`; `ProblemDetail` reads `detail` only, so it is `""` for them.
- **Proposal:** fall back to `message` when `detail` is absent, in every SDK at once, with a case in `api-error-vectors.json` (a shared-vector change, so a coordinated round).
- **Impact:** low effort; touches every consumer's error type.

## Shared intercom vectors

- **Problem:** `intercom` has no conformance vectors; the text rule (NFC, 1-280, `<` `>`, control/bidi/format characters, lone surrogates), the do-not-disturb rule (valid IANA zone, `start != end`) and the problem-code to error-kind mapping are proved by each SDK's own tests and can drift between five implementations on different Unicode and timezone databases (the Go port already differs on NFC).
- **Proposal:** an `intercom-vectors.json` in the Python reference (text, target, DND and error-kind cases), claimed by the capability.
- **Impact:** low effort; one more file for every consumer.

## A typed result for the sign-ins

- **Problem:** `BrowserLogin`, `InteractiveLogin` and `LoginWithBrowser` answer the raw token map
  (`map[string]any`), as the porting brief asks for this round, so a caller re-reads
  `access_token`, `scopes`, `expires_at` and `ai_disclosure` by hand. `PollDeviceLogin` already
  answers an `*APIToken`.
- **Proposal:** once the reference adds `ai_disclosure` to its token type, add
  `BrowserLoginToken`-style typed variants (or an `APIToken.AIDisclosure`) beside the map ones,
  without changing the existing signatures.
- **Impact:** low effort; fewer field-name mistakes in CLIs.

## Retire the older reference digest after the Python merge

- **Problem:** `contracts/sdk-parity.json` accepts both `57c7e71f6456…` and `6dfb3bb31ac7…` during
  the browser-login rollout.
- **Proposal:** once thalovant-python-sdk#113 is on `main`, drop the older digest in a small
  follow-up and re-run the parity check.
- **Impact:** trivial; keeps the acceptance record honest about what it reviewed.

## Document browser sign-in on docs.thalovant.com

- **Problem:** the README's "Signing in through the browser" section is the only prose for
  `InteractiveLogin` and `BrowserLogin`; the Go page on docs.thalovant.com still describes device
  login as the way to sign in without a password.
- **Proposal:** add the chooser, the browser rule, the options and the errors to the Go SDK page in
  the same release train (AGENTS.md asks for it).
- **Impact:** low effort; owner or docs-repository step.

## Bound control-plane response bodies

- **Problem:** `ControlPlane.send` reads JSON responses without a size cap (`control.go`, `sendWithin` with limit 0). Only `GetBridgeAvatar` passes a cap (4 MiB). A misconfigured endpoint or proxy could stream far more into memory on the other routes.
- **Proposal:** give `send` a generous default cap (for example 16 MiB) through `sendWithin`, and refuse a larger body with an `*APIError`, as the avatar read does.
- **Impact:** low effort; removes a memory-exhaustion path for every control-plane call.

## Typed models for the other control-plane reads

- **Problem:** most control-plane methods (`GetHub`, `ListHubs`, `ListHubSkills`, ...) answer `map[string]any`, so every caller re-reads the same fields (see `HubDisplayName` in `hubs.go`). Chat apps and `GetOperation` answer typed values.
- **Proposal:** add typed variants next to the map ones (`GetHubResource`, ...) using `controlInto` from `chat_apps.go`, without changing the existing signatures.
- **Impact:** medium effort; fewer field-name mistakes in apps, and compile-time checks when the API adds members.

## Fields on hand-built APIError values

- **Problem:** `APIError.refusal()` reads `Code` from `Problem` when a caller built the error by hand, but `Fields` is only filled by `apiErrorFromResponse`.
- **Proposal:** if hand-built errors matter to callers, add an accessor that falls back to `problemFieldNames(e.Problem)`.
- **Impact:** small; only affects code that constructs `APIError` literals (mostly tests).

## Shared vectors for `usual_form`

- **Problem:** `UsualForm` (`language_matching.go`) drifted from the Python reference's `usual_form` on empty subtags and macrolanguage members, and the Rust port still answers the old way. `language-matching-vectors.json` covers `closest_lang` only, so the parity gate could not see either difference; it took running both implementations on the same tags.
- **Proposal:** record `usual_form` over a fixed tag list in the reference (the shared data's language, alias, macrolanguage and likely-subtag keys, the matching vectors' tags, and malformed ones; 4,453 tags here) as a new shared vector file, and have every port assert it, the way `TestUsualForm` pins a handful of cases now.
- **Impact:** medium. Which language a listing is retried in decides what a hub appears to offer; one shared file would catch this class of drift in every port at once.
