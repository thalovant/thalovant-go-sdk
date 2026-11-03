Last Edit: Claude Opus 5.5 - 2026-11-03 - Motive: Proposals noted while adding chat apps (0.14.0).

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
