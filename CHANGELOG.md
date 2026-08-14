# Changelog

## 0.3.5

- Derive both user agents from a single exported `Version` constant in
  `version.go` instead of hand-maintained literals. `DefaultUserAgent` and
  `DefaultControlUserAgent` keep their names, exportedness, constant-ness, and
  values; they are now `"ThalovantGoSDK/" + Version`, resolved at compile time,
  so the data-plane and control-plane copies can no longer disagree.
- Pin the user agents in tests against the derived value rather than a version
  literal, require `Version` to equal the `VERSION` file, and fail the suite if
  any `.go` source hard-codes a `ThalovantGoSDK/<version>` literal again.
- Stop rewriting `constants.go` during the automatic release bump, which no
  longer contains a version, and make the remaining `VERSION` and `version.go`
  replacements fail loudly when their target literal is absent. A silent no-op
  in that step is what left the control-plane user agent at
  `ThalovantGoSDK/0.3.0` after 0.3.1.

## 0.3.4

- Document the two HTTP 429 responses the control plane returns for
  token-authenticated calls: `token_rate_limited` (the plan's per-minute
  request rate, 60 requests per minute on the free plan) and
  `token_quota_exceeded` (the plan's daily or monthly call quota, reported in
  `quota`, `limit`, and `used`). Both carry a `Retry-After` header and a
  matching `retry_after_seconds`, both are returned as errors wrapping
  `ErrAPI`, `Retry-After` is authoritative, and the SDK does not retry
  automatically.

## 0.3.3

- Add browser device-flow sign-in: `ControlPlane.LoginWithBrowser` and
  `DeviceLoginOptions` request a device authorization, present the
  verification URI and user code (custom `Prompt` supported), open the
  browser on a best-effort basis, and poll `/v1/auth/device/token` honoring
  the server `interval` and `slow_down` backoff until the request is
  approved, denied (`ErrDeviceAccessDenied`), expired
  (`ErrDeviceCodeExpired`), timed out (`ErrTimeout`, 15 minutes by default),
  or the context is cancelled. The approved durable API token is stored on
  the `ControlPlane` exactly like `Login`.
- Document direct API-token auth for CI and other non-interactive use: pass a
  pre-provisioned token to `NewDefaultControlPlane`/`NewControlPlane` (for
  example from a `THALOVANT_API_TOKEN` environment variable) instead of
  calling a login method.

## 0.3.2

- Add MFA login support: `LoginOptions` and `ControlPlane.LoginWithOptions` send
  optional `otp_code`/`recovery_code` fields, omitting them when empty. The
  existing `Login` signature is unchanged.
- Realign the control-plane user agent with the module release; it had been
  left at `ThalovantGoSDK/0.3.0` since 0.3.1.

## 0.3.1

- Bump the `go-routine-updates` dependency group: `golang.org/x/net` 0.55.0 to
  0.57.0 and `golang.org/x/sync` 0.17.0 to 0.22.0 (both indirect).

## 0.3.0

- Raise the supported toolchain floor to Go 1.25, the oldest upstream-supported Go release.
- Upgrade `golang.org/x/net` to 0.55.0 to remediate four high-severity dependency findings.

## 0.2.17

- Avoid overflow-prone capacity arithmetic when encoding caller-controlled binary payloads.
- Give CI and release-guard workflows explicit read-only repository permissions.
- Keep data-plane and control-plane user-agent versions aligned with the module release.

## 0.2.16

- Add `OperationResource` and `ControlPlane.GetOperation` for durable command polling.
