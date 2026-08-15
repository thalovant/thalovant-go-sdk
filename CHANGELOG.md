# Changelog

## Unreleased

### Breaking

- Remove the admin analytics path from `GetAnalyticsOverview`. The
  `AnalyticsOverviewOptions.Admin` and `AnalyticsOverviewOptions.OwnerID` fields
  are gone, and the call always targets `GET /v1/analytics/overview`; the
  `GET /v1/admin/analytics/overview` branch and its `owner_id` query are
  removed. This SDK serves non-admin customers, so callers that set `Admin` or
  `OwnerID` must drop them.
- Migrate the MQTT data-plane topic scheme to `<topic_prefix>/in|out|status`.
  `MqttBrokerCredentials.TopicPrefix` is now the full plaintext base
  (`hivemind/<hub-id>/<access-key>`) and the transport appends the fixed
  suffixes: publish requests go to `<prefix>/in`, subscribe replies to
  `<prefix>/out`, and the retained presence/LWT to `<prefix>/status`. The old
  `<base>/c2s|s2c|status/<access-key>` scheme, the `HashTopics` hashing, and the
  hub-id fallback are removed. `MqttTopicSet` renames its `C2S`/`S2C` fields to
  `Inbound`/`Outbound`, and `MqttBrokerCredentials` drops the `HubID`,
  `C2STopic`, `S2CTopic`, `StatusTopic`, and `HashTopics` fields (and their
  `hub_id`/`c2s_topic`/`s2c_topic`/`status_topic`/`hash_topics` JSON keys and
  `MQTT_*` environment variables). `TopicPrefix` is now required:
  `MQTTTopicsForIdentity` errors when it is empty.

### Security

- Redact secrets from human-facing formatting. `Identity`,
  `MqttBrokerCredentials`, and `ControlPlane` now implement `String()`, so the
  `%v`, `%s`, and `%+v` verbs print `[REDACTED]` in place of the access key,
  password, crypto key, MQTT username/password, control-plane access token, and
  any userinfo embedded in data-plane endpoint URLs. This changes formatted
  output only: `json.Marshal` — the wire protocol and the persisted identity
  file — still serializes the real secret values.
- `BootstrapIdentityResult.Summary(false)` — the default — now redacts the
  secrets echoed in the raw `hub` and `client` maps (the `initial_identify`
  access key/password/crypto key/MQTT credentials, the `initial_identify_token`,
  and the spec's `apiKey`/`password`/`cryptoKey`), matching how it already
  redacts the identity. `Summary(true)` is unchanged and still returns every
  secret in the clear; never log it.
- Strip the `?authorization=` query from data-plane transport errors before they
  are stored in `LastError`, so `ConnectionInfo()`/`Healthcheck()` and their
  JSON no longer leak the data-plane access key when a connection fails.
- Surface only an allowlist of non-secret JSON message fields (`detail`,
  `message`, `error`, ...) in control-plane HTTP errors, bounded and
  single-line, and omit any other body. A failed `POST /v1/clients` response can
  echo the just-sent `apiKey`/`password`/`cryptoKey`, so arbitrary response body
  text is never interpolated into an error string.
- Document the inverted boolean polarity of the two `Map` methods
  (`HubDataPlaneEndpoints.Map(redactCredentials)` redacts when true;
  `MqttBrokerCredentials.Map(includeSecrets)` reveals when true) and the
  protocol-mandated appearance of the access key in the MQTT client ID and topic
  segments.

## 0.3.6

- Add hub provisioning to the control plane: `CreateHub`, `UpdateHub`,
  `DeleteHub`, `ReleaseHub`, `SetHubRating`, `ClearHubRating`, and
  `GetHubRuntimeCapabilities`. `CreateHub` always sends an `Idempotency-Key`
  header, generating one unless `HubCreateOptions.IdempotencyKey` is set.
  `UpdateHub` and `DeleteHub` take `etag` as a required argument, not an
  option, because the API rejects a missing or stale `If-Match` with HTTP 412.
- Add runtime-group management: `ListRuntimeGroups`, `GetRuntimeGroup`,
  `CreateRuntimeGroup`, `UpdateRuntimeGroup`, `GetRuntimeGroupConfig`,
  `UpdateRuntimeGroupConfig`, `ReleaseRuntimeGroup`, and `DeleteRuntimeGroup`.
  These routes read no `If-Match` and no idempotency header, so the SDK sends
  neither. Configuration is merged rather than replaced, and personas are
  replaced only when `RuntimeGroupConfigOptions.Personas` is set.
- Add skill discovery and installation: `ListMarketplaceSkills`,
  `ListRuntimeGroupMarketplace`, `ListRuntimeGroupInventory`,
  `InstallRuntimeGroupSkill`, and `UninstallRuntimeGroupSkill`. A zero-value
  `RuntimeGroupSkillInstallOptions` installs an active skill from the
  marketplace catalog.
- Export the option types the new calls take: `HubCreateOptions`,
  `ReleaseOptions`, `RuntimeGroupConfigOptions`,
  `RuntimeGroupSkillInstallOptions`, `MarketplaceSkillListOptions`,
  `RuntimeGroupMarketplaceOptions`, and `RuntimeGroupInventoryOptions`. False
  and empty options are omitted from the query string, and an unset
  `ReleaseOptions` sends an empty JSON body so the API applies the workspace
  release policy.
- Accept the camelCase spellings of the hub and runtime-group body keys
  (`runtimeGroupId`, `ownerId`, `capacityProfile`, `isLocked`,
  `cloneFromDefault`) and rename them to snake_case before sending, so a
  camelCase payload is no longer silently dropped by the API's request model.
- Document the plan and scope requirements: the provisioning writes need a paid
  plan and `hubs:write` (HTTP 402 on the free plan), hub ratings need
  `hubs:write` but no paid plan, the marketplace catalog needs only `hubs:read`
  and is not paid-gated, and the group-scoped inventory reads need
  `hubs:inspect`. `ListRuntimeGroupInventory` reports a pending source instead
  of the HTTP 409 `GetHubRuntimeCapabilities` returns when nothing is
  reporting.
- No existing signature changed.

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
