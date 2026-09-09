# Changelog

## 0.5.2

- Recover HTTP cleanup after a lost success response by recognizing the upstream exact already-disconnected acknowledgment, while retaining admission responsibility until that confirmation.
- Reject contradictory or explicitly failed disconnect acknowledgments; retain replica affinity and authenticated pins through a cleanup retry and KK reconnect.
- Add real TLS/Noise lost-response and malformed/refused acknowledgment regressions.

## 0.5.1

- Start Ask settlement on the first nonempty speech, keep fixed empty/settlement windows within the original deadline, and return collected speech at that deadline.
- Collect replies while an admitted send retires, retaining transport cleanup ownership and never replaying application requests.
- Freeze Query immediately on policy denial or explicit query timeout; a soft intent miss cannot invalidate speech already collected.
- Require the matching request ID for Ask, accept runtime-replaced session IDs, and reject unrelated or uncorrelated ambient replies.
- Add race-tested regressions for fixed settlement, deadline-clipped speech, hard terminal replies during suspended sends, cleanup ownership, and soft-failure recovery.

## 0.5.0

- Require Go 1.26 or newer and use patched `golang.org/x/crypto` 0.56.0.
- Add `WaitForEvent` and filtered `Listen` with correlation/predicate options, bounded buffers, event limits, explicit timeout/disconnect/overflow errors, and cancellation that unsubscribes immediately.
- Validate device verification URLs before display/browser launch and keep URLs as direct arguments without a command shell.
- Refuse control-plane redirects and require HTTPS for credential-bearing requests except literal local development endpoints, including injected HTTP clients. Sanitize transport errors without retaining query strings or arbitrary credential-bearing causes.
- Include connection diagnostics in caller deadlines, preserve SDK timeout classification, drain queued reply fragments at settlement, and retain HTTP teardown ownership after caller cancellation.
- Add `IntentsWithCapabilities`, `ListFallbacks`, and `HubIntentCapabilities.MayAnswer`: optional fallback discovery distinguishes unknown from known-empty and shares one bounded connect/send/reply budget. Silent unified listings use engine manifests by default.
- Add independent bounded subscriptions on all built-in transports so concurrent Ask, Query and inventory calls retain their own replies. Slow observers fail explicitly with `ErrEventOverflow`.
- Add `AskWithOptions` for delayed speech and fragment settlement, preserving existing request option literals. Soft intent misses can recover with later speech; hard denials retain partial-reply failure information.
- Enforce authenticated connection readiness, bounded connection/send/close callers, retained cleanup ownership, cancellable queued operations and generation-specific WSS teardown. Failed HTTP admission cleanup must succeed before a new admission.
- Publish complete Noise identity/trust files atomically under a crash-released cross-process lock; reject conflicting pins and unexpected trust-file types.
- Expand CI to race tests on Linux/macOS/Windows, minimum/current Go, vulnerability checks and parser fuzzing, including real TLS/Noise transport and process-crash regressions.

## 0.4.5

- Reset this transport object's prior HTTP admission before reconnecting after
  a failed session. The listener does not issue a fresh Noise offer while the
  old peer is still registered. Keep the static key and hub pin for XX-to-KK
  recovery; a first connection does not disconnect an unknown existing peer.

## 0.4.4

- Implement the deployed HiveMind v3 Noise exchange for HTTP and MQTT, including
  persistent static keys, hub pin continuity, encrypted HELLO and bus traffic,
  chunked messages, and XXpsk2/KKpsk0 reconnects. Capability offers alone never
  mark a transport ready; plaintext sends cannot bypass the session.
- HTTP retains replica affinity cookies, uses the binary send/poll endpoints,
  and rejects error JSON even when the HTTP status is 200.
- MQTT serializes encrypted publishes and fails closed on broker disconnects.
  Call `Connect` again to resubscribe and negotiate a new session. Add
  `TLSConfig` for broker trust roots, plus `NoiseStateDir` and `RemoteStaticKey`
  on both HTTP and MQTT.
- WSS no longer drops the trusted hub key after a failed KK exchange and clears
  session/readiness on disconnect or read failure.
- Validate real local TLS HTTP and MQTT round trips, same-object reconnects,
  concurrent chunked messages, wrong passwords, tampering/replay, HTTP errors,
  plaintext rejection, and key-change rejection.

## 0.4.1

- **Security.** Move `golang.org/x/crypto` from `v0.44.0` to `v0.55.0`, which
  also lifts `golang.org/x/net` to `v0.57.0`. `v0.44.0` carries
  CVE-2026-56854 (critical) and nine highs, and the `x/net` it pulled carried
  five more. `0.4.0` shipped with all fifteen.

  The pin came from adding the Noise dependencies for v3: `go get
  golang.org/x/crypto@latest` wanted `v0.56.0`, which raises the `go`
  directive to `1.26`, so I pinned back to keep `go 1.25.0` — and pinned far
  further back than that needed. `v0.55.0` builds on `go 1.25.0` unchanged, so
  the module's stated Go floor is untouched.

## 0.4.0

- **Breaking.** `wss` connections now perform the HiveMind v3 Noise handshake,
  and only that. A HiveMind-core 5.x hub accepts no other key exchange, so this
  release requires one; against an older hub the connection is refused rather
  than downgraded. `Noise_XXpsk2_25519_ChaChaPoly_SHA256` on first contact and
  `Noise_KKpsk0_...` once the hub's static key is pinned, with
  `25519_AESGCM_SHA256` supported where a hub prefers it.
- **Breaking.** `Identity.CryptoKey` is gone, along with the AES helpers
  (`EncryptAsJSON`, `DecryptFromJSON`, `EncryptAsBinary`, `DecryptBinary`,
  `RuntimeCryptoKey`) and the pre-shared handshake on all three transports.
  Hubs no longer issue a crypto key and v3 derives its pre-shared key from the
  `password`, so the field named a credential that no longer exists.
  `crypto_key` is still read from an existing identity file and ignored, and it
  stays in the bootstrap redaction list so an older payload carrying one does
  not leak it. `https` and `mqtt` now rely on TLS for confidentiality, as they
  already did for everything the crypto key did not cover.
- The Noise pre-shared key is derived from `password` with argon2id
  (`time_cost=3`, 64 MiB, `parallelism=1`), salted with SHA-256 of the hub's
  node id. A transport caches it per hub, so only the first connection pays the
  few hundred milliseconds.
- Two files persist beside the SDK config file, both `0600`: `noise_key` (this
  client's static X25519 key) and `noise_pins.json` (the hub keys it has
  pinned). `WSSTransport.NoiseStateDir` overrides the location, and
  `LoadOrCreateNoiseKey`, `LoadNoisePin`, `SaveNoisePin` and `ForgetNoisePin`
  are exported for callers that manage the state themselves.
- Trust on first use: the first hub key seen for a node id is pinned, and a
  later connection presenting a different key is refused with an error naming
  `ForgetNoisePin`, rather than silently re-pinned. A failed `KKpsk0` handshake
  drops the stale pin, because `KK` needs each side to hold the other's key and
  the failure is as likely to mean the hub no longer has this client's.
- `WSSTransport.RemoteStaticKey()` reports the hub's static key for the current
  session.
- A `wss` connection that the hub refuses now fails with the close reason
  instead of running out the handshake clock: a wrong password reported as a
  twenty second timeout hid what had actually happened.
- `WSSTransport.sendCleartext` reads the connection once under the lock.
  `readLoop` calls it during the handshake while `Connect`'s timeout branch can
  be running `Disconnect`, which clears it -- an unsynchronized read raced that
  and could dereference nil. `Disconnect` now captures and clears under one
  lock and closes outside it, and `Connect` sets it under the lock.
- A read loop is bound to the connection attempt that started it. `Disconnect`
  does not wait for it to exit, so a loop from a previous attempt could
  overwrite `lastError` and close the *new* `readDone`, aborting a fresh
  handshake with a stale error.
- **Breaking.** `HTTPTransport.Connect` now refuses a hub endpoint that is not
  `https://`, for the same reason as the MQTT change below: TLS is the only
  confidentiality left on that hop, and the access key travels in the
  `authorization` query.
- `CreateClientIdentity` drops `cryptoKey` and `crypto_key` from a
  caller-supplied `opts.Spec` rather than merging them into the request. The
  generated-secret redaction covers only what the SDK mints, so a legacy value
  passed in by a caller could otherwise be echoed back inside an `ApiError`.
- **Breaking.** `MQTTTransport.Connect` now refuses a broker whose identity does
  not enable TLS. Removing the crypto key took the separate payload cipher with
  it, so TLS is the only confidentiality left on that hop; without it every
  message and the broker password would travel in the clear. Use an `mqtts://`
  endpoint, or set `tls: true` on the identity's `mqtt` block.
- Messages larger than one Noise transport message are chunked at 65000 bytes
  and reassembled by the peer, with reassembly capped at 32 MiB. Any transport
  message that fails to decrypt, and any malformed chunk sequence, drops the
  session rather than the frame.

## 0.3.14

- `ListIntents` returns an error wrapping `ErrRuntime` when the hub answers
  `ovos.intent.list` with `ok: false`, instead of reading the missing
  `intents` key as an empty list. A refused listing is not an empty hub, and
  reporting it as no intents showed a person a device that can do nothing;
  the error carries the hub's own `error` text. `Intents` fails the same way
  and does not fall back to the engines' manifests, which answer a policy
  denial rather than a query the hub could not produce. `DescribeIntent`
  keeps returning an empty list for `ok: false`, which is a real answer: the
  hub does not know that registration. Reported by the Kotlin port's review.
- Say in the README that a connection needs `ovos.intent.describe` only when
  definitions are asked for (the default); listing alone needs
  `ovos.intent.list`.

## 0.3.13

- Add the intent inventory: `client.Intents(ctx, languages)` reads the hub
  runtime's intent manifest (OVOS-INTENT-4 §10) over the client's own session
  and returns a `HubIntentInventory` — every intent each skill registered, per
  language, with the sentences a person says to reach it as the skill's locale
  files wrote them, `{slot}` placeholders included. No control-plane credential
  is involved. `client.ListIntents(ctx, lang)` and
  `client.DescribeIntent(ctx, skillID, intentName, lang)` expose the two
  underlying queries (`ovos.intent.list` / `ovos.intent.describe`) as
  `[]IntentRegistration` and `[]IntentDefinition`. All three take optional
  `IntentOptions{Timeout, Describe, Fallback, IncludeDefinitions}`; `Describe`
  and `Fallback` are `*bool` that default to true when nil, like
  `DeviceLoginOptions.OpenBrowser`.
- Export the result model: `HubIntentInventory` (`Languages`, `Skills`,
  `Source`, `Denied`, `Intents()`, `HasPhrases()`), `HubSkillIntents`
  (`SkillID`, `Intents`, `Languages()`) and `HubIntent` (`SkillID`, `Name`,
  `Engine`, `Enabled`, `Languages`, `Phrases`, `ID()`, `PhrasesFor(lang)`,
  `Examples(lang, limit)`), with snake_case JSON tags so `json.Marshal` matches
  the Python SDK's `as_dict()`. `Examples` prefers whole sentences to ones with
  a slot, shorter first. `SameLanguage` compares tags case-insensitively with
  `_` and `-` folded, and `IntentSourceManifest` / `IntentSourceEngines` name
  the two sources.
- Queries are correlated by `context.request_id` like `Ask`, and a reply
  delivered more than once is taken once. Describes are sent together and
  matched by request id, or by the definition's own `skill_id`/`intent_name`/
  `lang` for a hub that does not echo the id; a describe the hub does not
  answer in time leaves that intent without sentences rather than failing the
  inventory.
- Add `PolicyDeniedError` (wrapping `ErrRuntime`, retrieved with `errors.As`),
  returned at once from the hub's `hive.policy.denied` with `DeniedType`,
  `Code`, `Reason` and the `Allowed` list, instead of waiting for a timeout.
  `Intents` falls back to the engines' own manifests
  (`intent.service.adapt.manifest.get` / `intent.service.padatious.manifest.get`)
  when `ovos.intent.list` is refused, unless `IntentOptions.Fallback` is false;
  the result then carries names only, `Source` set to `engine-manifests` and
  `Denied` naming the refused query.
- A runtime that attaches each row's `definition` to `ovos.intent.list` when
  asked with `include_definitions` is used as such; one that does not is
  described row by row.
- Send describes in batches of at most `DescribeBatch` (32), each batch its
  own window with its own deadline, instead of putting every request in
  flight at once. A hub with 69 intents in two languages is 138 requests and,
  with every reply delivered twice, 276 inbound events — more than a
  transport's reply channel holds (`BusEvents` buffers 32), so replies past
  its capacity stall the transport's read loop and the inventory comes back
  missing sentences. Reported by the Rust port's review and settled in the
  Python reference at 0.4.38. A hub that answers nothing now fails after one
  batch rather than holding every request open.
- Keep partial results across describe batches: a batch that received no
  reply contributes nothing, and the call fails only when no batch produced a
  definition. Batches are contiguous slices of the work, so one unresponsive
  skill can own a whole batch — a skill with more than 32 intents would
  otherwise turn the entire inventory into a timeout, while the same skill
  with fewer only lost its sentences. A hub silent from the start still fails
  at the first batch, and a refusal still stops the call whenever it arrives.
  Reported by the Rust port's review and settled in the Python reference at
  0.4.39.
- Four readings settled with the Python reference (0.4.37) so every SDK reads
  the same: `HasPhrases()` is true only when at least one intent carries at
  least one sentence; `Intents` trims each language tag and asks a language
  repeated under another spelling (`en-us`, `en-US`, `en_us`) once, keeping
  the first spelling given; an intent registered under both engines in one
  language keeps the template row's sentences whichever order the rows
  arrive in, and the first row names its engine; on the names-only fallback
  the first engine to name an intent decides its engine (adapt is asked
  before padatious).
- Add the event-name constants `EventIntentList`, `EventIntentListResponse`,
  `EventIntentDescribe`, `EventIntentDescribeResponse`,
  `EventAdaptManifestGet`, `EventAdaptManifest`, `EventPadatiousManifestGet`
  and `EventPadatiousManifest`.
- No existing signature changed.

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

### Fixed

- Recognise `ovos.intent.unmatched` as a terminal failure event (#22). OVOS
  renamed the "no intent matched" bus event from the legacy Mycroft
  `complete_intent_failure`, which was the only name in the failure-event set, so
  an utterance matching no intent was never treated as terminal and the
  interaction/query loop waited out its full timeout instead of failing promptly.
  Both names are now recognised; the legacy name is retained for older runtimes.

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
- Redact `MqttBrokerCredentials.TopicPrefix` from human-facing formatting. Since
  the topic migration `TopicPrefix` carries `hivemind/<hub-id>/<access-key>`,
  `MqttBrokerCredentials.String()` — and the `Identity.String()` that embeds it —
  now print `[REDACTED]` for it instead of the raw prefix, closing a `%v`/`%s`/
  `%+v` path that leaked the same access key the username/password redaction
  already hides. `json.Marshal` still serializes the real prefix.
- Harden `topic_prefix` validation in `MQTTTopicsForIdentity`. The prefix is
  trimmed of surrounding whitespace before its leading/trailing `/`, a
  whitespace-only prefix is now rejected with the existing topic_prefix error,
  and a prefix containing an MQTT wildcard (`#` or `+`) or an ASCII control
  character (`< 0x20`, including NUL) is rejected, so a malformed or
  subscription-widening base can no longer build the `<prefix>/in|out|status`
  topic set. The topic_prefix error also drops its trailing period (staticcheck
  ST1005).

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
