Last Edit: Claude Opus 5.5 - 2026-11-08 - Motive: Audit browser sign-in for 0.15.0, with the fixes from CodeRabbit's review of #90.

## 2026-11-06 — CVE-2026-78661 in golang.org/x/net

HIGH, found by the dependency scan in infra-manifests' `contracts` gate at v0.61.0, fixed in v0.62.0. `go.mod` now requires v0.62.0. The scan reads this repository's `go.mod`, and it blocks every infra-manifests pull request until the version moves.

# Audit

Known issues, technical debt and their status, from test runs and code reading. Each entry names the code it concerns.

Evidence: `go test -race -count=1 -timeout=5m ./...` on 2026-11-08 (Go 1.26.10, linux/amd64): 433
top-level tests pass, 0 fail, 1 skipped (`TestNoiseInteropAgainstLiveHub`, which needs a live hub).
`gofmt -l .` is empty and `go vet ./...` is clean for linux, windows and darwin. The parity check
(`check-sdk-parity.py --consumer thalovant-go-sdk`) verifies the acceptance record against the
Python reference's main (44b6e2b, digest `57c7e71f6456`) and its browser sign-in PR head (e3999cb,
digest `49744ee385bf`), and every recorded `browser-login-vectors.json` case digest equals the
reference's.

## Verified in this change (browser sign-in, 0.15.0)

- All 98 cases of `testdata/browser-login-vectors.json` (byte-identical to the reference, sha256
  `9058adf2ad30…`) run in `browser_login_vectors_test.go`, and the 24 callback cases run a second
  time through a real listener (`TestBrowserLoginCallbackVectorsOverTheListener`). `sign_ins` and
  `chooser` run for real: a loopback httptest API checks every request, a raw TCP client plays the
  browser, and the exchange's verifier is checked against the URL's challenge before it is recorded.
- No secret reaches an error: the vectors check every printed form of every error against the
  synthetic secrets, the generated verifier and the state (`assertBrowserNoSecrets`); a 422 that
  echoes the code and the verifier, in values and in a key, is redacted in every field
  (`TestAnEchoedCodeAndVerifierNeverReachTheError`; `withoutSecrets` and `redactAll` in
  `browser_login.go`).
- No goroutine outlives a sign-in: the listener counts its accept loop and every connection
  (`loopbackListener.goroutines` in `loopback.go`) and `close` waits for them; checked by stack on
  every path, a panicking `Prompt` included (`TestNoGoroutineOutlivesTheSignInOnAnyPath`), and an
  idle connection does not hold the sign-in (`TestAConnectionThatSendsNothingDoesNotHoldTheSignIn`).
  Two mutations were run and caught: calling the `Opener` synchronously
  (`TestBrowserLoginPrintsTheURLAndDoesNotWaitForTheOpener` fails), and not closing the port on
  acceptance (`TestASecondAnswerAfterTheFirstIsTurnedAway` fails).
- Callback values decode as CPython's `parse_qsl` decodes them, checked against its output for 12
  queries (`TestCallbackValuesAreDecodedAsPythonsParseQslDecodesThem`; `formPairs`, `formUnquote`,
  `utf8Replacing` in `loopback.go`).
- Device login is unchanged: `LoginWithBrowser` delegates to `loginWithDevice` (`control.go`), and
  `device-login-vectors.json` and the existing `TestControlPlaneLoginWithBrowser*` tests pass
  unchanged; every other recorded vector result is identical to the committed record.

## Fixed from the review of #90 (CodeRabbit, 2026-11-08)

Each with a test that fails without the fix (checked by reverting the fix and re-running it):

- **The exchange could be replayed by the HTTP transport.** HTTP/2 replays a written body on a
  stream the server reset with `PROTOCOL_ERROR` (golang/go#47635), and a code presented twice
  revokes the first token. The exchange now goes through `sendNoReplay` (`control.go`), with no
  `GetBody` (`TestTheExchangeIsNeverReplayedByTheTransport`).
- **A refused exchange could keep a token, or half an escaped secret.** `redactExchangeAnswer`
  (`browser_login.go`) decodes the body, redacts the code, the verifier, the state and every
  `access_token`/`refresh_token`/`id_token` value, and only then is the display line built and cut
  (`TestARefusedExchangeKeepsNoTokenAndNoHalfSecret`).
- **A consent-page `error` carrying the state was repeated in `BrowserLoginFailedError.Code`**
  (`browserLogin`; `TestAnErrorThatCarriesTheStateIsNotRepeated`). The Python reference repeats it
  too (`_browser_login` in `control.py`).
- **A `Prompt` that cancelled the sign-in still opened the browser** (`presentBrowserLogin`;
  `TestAPromptThatCancelsTheSignInOpensNothing`).
- **Unbounded connections.** The listener handles at most `loopbackMaxConnections` (64) at once
  (`loopback.go`, `serve`; `TestTheListenerHandlesABoundedNumberOfConnections`).
- **Configurations no request can use were usable:** an authorization endpoint on port 0, a token
  endpoint with a malformed escape (`/v1/%zz`), and a `[]string` method list was refused
  (`validPort`, `usableTokenEndpoint`, `ParseNativeSignInConfig`; `TestEndpointsNoRequestCanUseAreUnusable`,
  `TestTheConfigurationParserReadsATypedMethodList`). The Python reference accepts port 0 and the
  malformed escape.
- **Documentation:** the goroutine guarantee now names the platform launcher's wait, the CHANGELOG
  says a connection closing before a whole head is closed without an answer, and the README and
  FAQ say the URL is not printed when `Prompt` is set.

Not changed, with the reason on the thread: the accepted tab's 60-second wait for its page (the
porting brief bounds it), and `Prompt` running on the calling goroutine (the reference calls its
prompt synchronously, as the print it replaces).

## Verified in 0.14.0 (chat apps)

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

- **An `Opener` that never returns keeps its goroutine.** `BrowserLogin` calls
  `BrowserLoginOptions.Opener` on its own goroutine (`openWith` in `control.go`) and does not wait
  for it, since a console browser can hold the call until it exits; Go cannot stop it. The default
  opener (`openBrowser`) returns once the process has started, and its `cmd.Wait` goroutine lives
  as long as the launcher process, as it always has for device login.
- **`ParseNativeSignInConfig` reads the dashboard address with `url.Parse`**, which refuses a path
  with a malformed escape (`https://dash.example/%zz`) that Python's `urlsplit` accepts. Go then
  calls the configuration unusable, the safe side; no vector or API answer has such an address
  (`usableAuthorizationEndpoint` in `browser_login.go`).
- **`*BrowserLoginFailedError` is Go's own type.** The reference raises its API error with no
  status and the page's error as `code`; Go's `APIError.StatusCode` is an `int` with no "none", so
  a callback without a code is a separate type that matches `ErrAPI` (`browser_login.go`).
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

- **Stuck home handlers and replies left a goroutine per request** (CodeRabbit on #69). Fixed on 2026-11-08 (Unreleased): `answerHomeRequest` and `runHomeHandler` in `home.go` count the handlers and the replies still running past their time (`homeBounds`, `homeRun`); at four of either, a request is answered `timeout` without starting a handler, or no reply is started and the call returns `ErrTimeout`. Replies are counted per subscription, or per `Replier` for calls on their own (`replierOverruns`, `replierKey`). Tests: `TestHandlersThatNeverReturnCannotLeaveAGoroutinePerRequest`, `TestRepliesThatNeverReturnCannotLeaveAGoroutinePerRequest`, `TestAStuckReplierHoldsUpOnlyItsOwnAnswers`, `TestStuckAndWorkingReplierValuesOfOneTypeAreCountedApart`, `TestAReplierThatCannotBeComparedIsNotCounted`, `TestASubscriptionCountsItsOwnStuckHandlers`, and `TestHandlersThatHonourTheirTimeAreNotLimited`, which checks that handlers honouring their context are not capped (`home_link_test.go`).
- **`UsualForm` differed from the Python reference on two kinds of tag** (found 2026-11-03, raised again by CodeRabbit's singleton item on #65). Fixed on 2026-11-08 (Unreleased) in `UsualForm` (`language_matching.go`): an empty subtag (`"en-"`, `"en--US"`, `"en-US-"`) has no usual form, and a language known through its macrolanguage keeps its own code (`"arb"` to `arb-eg`, `"cmn"` to `cmn-cn`). Pinned in `usual_form_test.go`; a run of both implementations over 4,453 tags found no difference. Dangling singletons (`"en-u"`, `"en-x"`) matched the reference before and still do.
- **`ListedIn` was nil on the names-only fallback** (CodeRabbit on #65). Now `[]` (`inventoryFromNames` in `intents.go`), as in the reference, which leaves `listed_in` empty when nothing was listed by manifest; `TestTheFallbackListsNamesAndSaysWhatWasRefused` checks both the slice and the JSON.
- **`UsualForm` suppressed its own retry for `en-US`** (CodeRabbit on #65). Fixed in d61624f (#66, v0.10.4): byte comparison in `UsualForm` (`language_matching.go`), pinned in `usual_form_test.go`. The CHANGELOG entry was missing and is added now.
- **`%#v` printed `APIError.Problem`** (CodeRabbit on #68). Fixed in 5715299 (#70): `APIError.GoString()` in `errors.go`, tested in `api_error_vectors_test.go`.

## Open

- **Drop the older reference digest** (`57c7e71f6456…`) from `contracts/sdk-parity.json` once the
  Python SDK's `main` carries `49744ee385bf…` (thalovant/thalovant-python-sdk#113).
- **The Rust port's `usual_form` still answers the way Go did** on the two kinds of tag above (noted 2026-11-03). No shared vector covers `usual_form`, so the parity gate sees no difference; see SUGGESTIONS.md.
- **The home-goroutine bound is Go's own.** The reference caps plain handlers at four threads running at once; Go caps only the handlers and replies still running past their time, so handlers that honour their context are never refused. The count is checked when a request arrives, so requests already running when the fourth runs over can still add to it before new ones are refused. A `Replier` value that cannot be compared (a struct holding a slice) is not counted for calls of `AnswerHomeRequest` on their own, since it cannot be told apart from another value; pointers, which `Client` and `HubSession` are, always are.
