Last Edit: Claude Opus 5.5 - 2026-11-03 - Motive: Chat apps audit notes (0.14.0), merged with the CodeRabbit follow-up audit from main.

# Audit

Known issues, technical debt and their status, from test runs and code reading. Each entry names the code it concerns.

Evidence: `go test -race -count=1 ./...` on 2026-11-03 (Go 1.26.10, linux/amd64): 384 top-level tests
pass, 0 fail, 1 skipped (`TestNoiseInteropAgainstLiveHub`, which needs a live hub). `gofmt -l .` is
empty and `go vet ./...` is clean. The parity check (`check-sdk-parity.py --consumer
thalovant-go-sdk`) verifies the acceptance record and reports `chat-apps` as a capability the
reference does not know yet, which is expected until the Python reference declares it.

## Verified in this change

- Secrets stay out of output: `SecretValue` (`chat_apps.go`, `type SecretValue`) prints the marker
  for every fmt verb, JSON, text and slog, including inside caller structs with an unexported field
  (`TestSecretValuePrintsTheMarkerEverywhere`). Request bodies carry the value and errors do not
  (`TestChatAppSecretsReachOnlyTheRequestBody`, `TestChatAppTransportFailureNamesNoSecret`).
- `APIError.Fields` (`errors.go`, `problemFieldNames`) reads strings only, top level first, then an
  object `detail` (`TestProblemFieldNamesFollowTheCodeRules`); the shared api-error vectors still
  pass (`api_error_vectors_test.go`), and `APIError.GoString` still leaves `Problem` out.
- Plan and auth mapping is unchanged for the bridge routes: 402 and 403 `plan_limit` match `ErrPlan`,
  401 and 403 "Insufficient scopes" match `ErrAuth`, 429 sets `RetryAfter`
  (`TestChatAppRefusalsCarryCodeFieldsAndTheUsualKinds`).

## Known issues and debt

- **Unbounded JSON reads.** `ControlPlane.send` calls `sendWithin` with no cap (`control.go`), so JSON
  responses are read whole. `GetBridgeAvatar` passes a 4 MiB cap (`maxBridgeAvatarBytes` in
  `chat_apps.go`, `TestChatAppAvatarReadIsBounded`). See SUGGESTIONS.md.
- **Fixed in this change:** `send` used to drop the `io.ReadAll` error, so a body cut short was
  returned as complete. It is now an `*APIError` "incomplete response body"
  (`TestChatAppBodyCutShortIsNotAnAnswer`).
- **`Fields` only on parsed errors.** `APIError.refusal()` reads `Code` from `Problem` for a
  hand-built error; `Fields` is set only by `apiErrorFromResponse` (`control.go`). Hand-built errors
  carry no `Fields`.
- **SecretValue equality.** The value sits behind a pointer so fmt cannot print it through an
  unexported field; as a result two `SecretValue`s made from the same text are not `==`
  (documented on the type).
- **Older API releases.** The connectors route, `PUT .../secrets` and the bridge's `connector`,
  `settings` and `secrets_set` members are live since 2026-11-03 (checked against the deployed
  OpenAPI document). `Bridge.Connector` stays a pointer and the others decode as empty, so the SDK
  still reads an older answer (`TestBridgeKeepsUnknownValuesAndToleratesMissingOptionals`).

## Fixed

- **`UsualForm` suppressed its own retry for `en-US`** (CodeRabbit on #65). Fixed in d61624f (#66, v0.10.4): byte comparison in `UsualForm` (`language_matching.go`), pinned in `usual_form_test.go`. The CHANGELOG entry was missing and is added now.
- **`%#v` printed `APIError.Problem`** (CodeRabbit on #68). Fixed in 5715299 (#70): `APIError.GoString()` in `errors.go`, tested in `api_error_vectors_test.go`.

## Open

- **`UsualForm` differs from the Python reference on two kinds of tag** (found by running both on the same inputs, 2026-11-03; not changed here):
  - A tag with an empty subtag (`"en-"`, `"en--US"`, `"en-US-"`): the reference returns `None`; `UsualForm` returns `"en-us", true`.
  - A language CLDR knows only through its macrolanguage (`"arb"`, `"cmn"`): the reference returns `arb-eg` / `cmn-cn`; `UsualForm` returns `"", false` because `matching.Likely` has no direct entry.
  - The Rust port answers the same as Go. No shared vector covers `usual_form`, so the parity gate sees neither difference.
  - Dangling singletons (`"en-u"`) already match the reference (`en-us`).
