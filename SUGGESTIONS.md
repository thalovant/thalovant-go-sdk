Last Edit: Claude Opus 5.5 - 2026-11-08 - Motive: Propose shared usual-form vectors after fixing UsualForm's drift from the reference.

# Suggestions

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
