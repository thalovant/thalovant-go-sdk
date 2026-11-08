Last Edit: Claude Opus 5.5 - 2026-11-08 - Motive: Record the UsualForm and home-goroutine fixes from CodeRabbit's review-body items on #65 and #69.

## 2026-11-06 — CVE-2026-78661 in golang.org/x/net

HIGH, found by the dependency scan in infra-manifests' `contracts` gate at v0.61.0, fixed in v0.62.0. `go.mod` now requires v0.62.0. The scan reads this repository's `go.mod`, and it blocks every infra-manifests pull request until the version moves.

# Audit

Known issues, technical debt and their status, from test runs and code reading. Each entry names the code it concerns.

Evidence: `go test -race -count=1 -timeout=5m ./...` on 2026-11-08 (Go 1.26.10, linux/amd64): 388
top-level tests pass, 0 fail, 1 skipped (`TestNoiseInteropAgainstLiveHub`, which needs a live hub).
`gofmt -l .` is empty and `go vet ./...` is clean. The parity check (`check-sdk-parity.py --consumer
thalovant-go-sdk`, reference at bd4061f) verifies the acceptance record.

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

- **Stuck home handlers and replies left a goroutine per request** (CodeRabbit on #69). Fixed on 2026-11-08 (Unreleased): `answerHomeRequest` and `runHomeHandler` in `home.go` count the handlers and the replies still running past their time (`homeBounds`, `homeRun`); at four of either, a request is answered `timeout` without starting a handler, or no reply is started and the call returns `ErrTimeout`. Tests: `TestHandlersThatNeverReturnCannotLeaveAGoroutinePerRequest`, `TestRepliesThatNeverReturnCannotLeaveAGoroutinePerRequest`, `TestASubscriptionCountsItsOwnStuckHandlers`, and `TestHandlersThatHonourTheirTimeAreNotLimited`, which checks that handlers honouring their context are not capped (`home_link_test.go`).
- **`UsualForm` differed from the Python reference on two kinds of tag** (found 2026-11-03, raised again by CodeRabbit's singleton item on #65). Fixed on 2026-11-08 (Unreleased) in `UsualForm` (`language_matching.go`): an empty subtag (`"en-"`, `"en--US"`, `"en-US-"`) has no usual form, and a language known through its macrolanguage keeps its own code (`"arb"` to `arb-eg`, `"cmn"` to `cmn-cn`). Pinned in `usual_form_test.go`; a run of both implementations over 4,453 tags found no difference. Dangling singletons (`"en-u"`, `"en-x"`) matched the reference before and still do.
- **`ListedIn` was nil on the names-only fallback** (CodeRabbit on #65). Now `[]` (`inventoryFromNames` in `intents.go`), as in the reference, which leaves `listed_in` empty when nothing was listed by manifest; `TestTheFallbackListsNamesAndSaysWhatWasRefused` checks both the slice and the JSON.
- **`UsualForm` suppressed its own retry for `en-US`** (CodeRabbit on #65). Fixed in d61624f (#66, v0.10.4): byte comparison in `UsualForm` (`language_matching.go`), pinned in `usual_form_test.go`. The CHANGELOG entry was missing and is added now.
- **`%#v` printed `APIError.Problem`** (CodeRabbit on #68). Fixed in 5715299 (#70): `APIError.GoString()` in `errors.go`, tested in `api_error_vectors_test.go`.

## Open

- **The Rust port's `usual_form` still answers the way Go did** on the two kinds of tag above (noted 2026-11-03). No shared vector covers `usual_form`, so the parity gate sees no difference; see SUGGESTIONS.md.
- **The home-goroutine bound is Go's own.** The reference caps plain handlers at four threads running at once; Go caps only the handlers and replies still running past their time, so handlers that honour their context are never refused. The count is checked when a request arrives, so requests already running when the fourth runs over can still add to it before new ones are refused.
