Last Edit: Claude Sonnet 5.5 - 2026-11-11 - Motive: Intercom (0.16.0): Announce, speakers and settings on the control plane.

# Suggestions

## Shared intercom vectors

- **Problem:** `intercom` has no conformance vectors; text rules (control characters, trimming, rune count) are proved by each SDK's own tests.
- **Proposal:** an `intercom-vectors.json` in the Python reference (validation and error-kind cases), claimed by the capability.
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
