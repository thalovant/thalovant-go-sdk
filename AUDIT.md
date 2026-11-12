Last Edit: Claude Sonnet 5.5 - 2026-11-11 - Motive: Intercom final API: L6 and the replayed flag are fixed; new open items (no NFC, timezone database, Unicode version, ListIntercomSpeakers return type, detail vs message).

## 2026-11-11 — Intercom (0.16.0): `AnnounceOptions` printed the text (security review L4), now redacted

`AnnounceOptions` (`intercom.go`) is a plain struct, so `%v`, `%+v`, `%#v`, `%d` or a `log/slog` handler printed `Text`. It now implements `String`, `GoString`, `Format` and `slog.LogValuer`, which print `<N chars>` (`intercom_test.go`, `TestIntercomAnnounceOptionsNeverPrintTheText`). Still open: `json.Marshal(options)` prints the field, because a `MarshalJSON` that hid it would make a caller who stores the options lose the text silently; nothing in the SDK marshals the struct (`announceBody` builds a map).

## 2026-11-11 — Intercom (0.16.0): `APIError.Problem` keeps the whole API body (review L5)

The API strips `input` and `ctx` from `/intercom/` errors, so the text does not reach `Problem` today; a regression would. Not in contract section J; proposed in `SUGGESTIONS.md`.

Test run behind these entries: `go test -count=1 ./...` (Go 1.26, linux/amd64, 2026-11-11): 504 top-level tests, 503 passed, 0 failed, 1 skipped (`TestNoiseInteropAgainstLiveHub`); `gofmt` and `go vet` (linux, windows, darwin) clean.

## 2026-11-11 — Intercom (0.16.0), final API: this SDK does not NFC-normalise the text

`AnnouncementText` (`intercom.go`) trims, counts runes and refuses `<`, `>`, controls, bidirectional and other format characters, invalid UTF-8, private use and line/paragraph separators, as the API does; but the API (and the Python and Node SDKs) NFC-normalise first and count after. The Go standard library has no normaliser and `golang.org/x/text` is not a dependency (`go.mod`), so a decomposed text (`e` + U+0301 per letter) counts double here and can be refused locally that the API would accept once composed. The reverse, a text that NFC lengthens, is refused by the API (422), never sent short. Pinned by `TestIntercomTheTextIsTrimmedAndCountedAsSentWithoutNormalisation`. Documented in `README.md` and `FAQ.md`.

## 2026-11-11 — Intercom (0.16.0), final API: the timezone and Unicode checks use this build's databases

`validIntercomTimezone` (`intercom.go`) asks `time.LoadLocation`, so a name is valid when this machine's tz database (or `time/tzdata`, if the program imports it) knows it; the API asks its own, case-sensitively on Linux. Where no database exists (a minimal container without `/usr/share/zoneinfo`, Windows without a Go install) every name looks unknown, so a probe with `America/New_York` finds that out and only the shape is checked; the API decides (`TestIntercomTheZoneShapeIsTheFallbackOnlyWhereThereIsNoTimezoneDatabase`). `unicode.In` uses the Unicode version of the Go release (`go.mod` says go 1.26.9): a character assigned later than the API's Unicode can pass here and be refused there, or the reverse. The API's answer stays authoritative; the 422 is the fallback.

## 2026-11-11 — Intercom (0.16.0), final API: `ListIntercomSpeakers` returns an `IntercomSpeakerList`

It returned `[]IntercomSpeaker`. The list now carries `PolicySynced` and `UnavailableReason` beside the items, as `AnnouncementList` does, so the return type changed (the branch is unreleased; nothing on `main` calls it). `IntercomSpeaker.PolicySynced` is a `*bool` (nil on list items, set on the PATCH answer) and `IntercomDelivery.Detail` is a plain string where the other SDKs have null (`""` is "none").

## 2026-11-11 — Intercom (0.16.0), final API: `APIError.ProblemDetail` does not read `message`

The final intercom problems carry their sentence in `message`; `problemFields` (`errors.go`) reads `detail` only, so `ProblemDetail` is `""` for them (the sentence is in `Problem["message"]` and in `Error()`). Moving that is a shared api-error change for every SDK; proposed in `SUGGESTIONS.md`.

## 2026-11-11 — Intercom (0.16.0), final API: fixed in this round

- **Rooms, groups and the key length** (was L6): `intercomTargetsBody` caps every list at 50 names of 64 characters and `validIdempotencyKey` caps the key at 128 printable ASCII characters (`TestIntercomATargetListHoldsAtMost50NamesOfAtMost64Characters`, `TestIntercomAnIdempotencyKeyIsAtMost128PrintableASCIICharacters`).
- **A replayed announcement is marked** (`Announcement.Replayed`, from `Idempotency-Replayed: true`, kept after `Wait`; `TestIntercomAReplayedAnnouncementSaysSoAndAFirstAnswerDoesNot`, `TestIntercomAReplayedSendingAnnouncementStaysReplayedAfterWait`).
- **The text rule is the API's** (was: 1-280 and control characters only; `TestIntercomATextTheAPIWouldRefuseIsRefusedFirstAndNeverEchoed`).
- **Quiet hours** that are on need a real IANA zone and `Start != End` (`TestIntercomAnUnusableDoNotDisturbIsRefusedBeforeAnythingIsSent`).
- **`hub_unreachable` and `not_cancellable` decide by code**, whatever the status, as in the reference (`TestIntercomTheCodesDecideWhateverTheStatus`); the 13 new codes have typed refusals (`TestIntercomEachNewProblemCodeIsItsOwnTypedRefusal`).

## 2026-11-11 — Intercom (0.16.0): a poll that fails after the POST

`ControlPlane.Announce` with `Wait` (`intercom.go`) sends the POST, then polls `GetAnnouncement`, which needs `intercom:read`. A token with only `intercom:send`, a 429 or a network error fails a poll after the announcement was accepted. `Announce` now returns the last accepted `Announcement` with the error, so its ID is not lost (`intercom_test.go`, `TestIntercomAFailedPollKeepsTheAcceptedAnnouncement`); a retry still needs the same `IdempotencyKey` to avoid speaking it again.

## 2026-11-11 — Intercom (0.16.0): the text is in the request body

`announceBody` (`intercom.go`) builds the body that holds the text and `ControlPlane.Announce` sends it; nothing logs or returns it, and `Announcement` never carries it (`intercom_test.go`, `TestIntercomAnnounceSendsTheContractBody`). It is not redacted from an `*APIError` the API echoes it in (`APIError.Problem` keeps the body, `Error()` never prints it); the contract says the API never echoes it. The announcement listing shape (`items`, `next_cursor`) is taken from the contract; `data` and `cursor` are read too. Not exercised against a live API.

## 2026-11-06 — CVE-2026-78661 in golang.org/x/net

HIGH, found by the dependency scan in infra-manifests' `contracts` gate at v0.61.0, fixed in v0.62.0. `go.mod` now requires v0.62.0. The scan reads this repository's `go.mod`, and it blocks every infra-manifests pull request until the version moves.

# Audit

Known issues, technical debt and their status, from test runs and code reading. Each entry names the code it concerns.

Evidence: `go test -race -count=1 -timeout=5m ./...` on 2026-11-08 (Go 1.26.10, linux/amd64): 448
top-level tests pass, 0 fail, 1 skipped (`TestNoiseInteropAgainstLiveHub`, which needs a live hub).
`gofmt -l .` is empty and `go vet ./...` is clean for linux, windows and darwin. The parity check
(`check-sdk-parity.py --consumer thalovant-go-sdk`) verifies the acceptance record against the
Python reference's main (44b6e2b, digest `57c7e71f6456`) and its final browser sign-in PR head
(39e31d8, digest `6dfb3bb31ac7`), and every recorded `browser-login-vectors.json` case digest (119)
equals the reference's.

## Verified in this change (browser sign-in, 0.15.0)

- All 119 cases of `testdata/browser-login-vectors.json` (byte-identical to the reference at
  39e31d8, sha256 `b0c73f808c4f…`) run in `browser_login_vectors_test.go`; the 98 cases of the
  first round record exactly the digests they did. The 27 callback cases run a second
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

Not changed in that round, with the reason on the thread: the accepted tab's 60-second wait for
its page, which the porting brief bounded then, and `Prompt` running on the calling goroutine (the
reference calls its prompt synchronously, as the print it replaces).

## Fixed from CodeRabbit's second review of #90 (final-contract commit)

- **Redaction recompiled a pattern per string and could not bound its work** (`scrubText`). The
  new `secretScrubber` (`browser_login.go`) is built once per answer, uses no regular expression,
  finds each value's next occurrence once (linear per value), and an answer over
  `maxRefusedAnswerBytes` (1 MiB) or with more than `maxRefusedAnswerTokens` (64) distinct tokens
  is omitted rather than echoed (`TestALargeOrTokenStuffedAnswerIsRedactedOrOmitted`).
- **A token carrying `[redacted]` escaped redaction**, since scrubbing split on the marker first.
  A match is now skipped only when it lies wholly inside an existing marker
  (`TestATokenThatCarriesTheMarkerIsStillRedactedWhole`). The reference's `_scrub` splits first
  and has the same gap.
- **An oversized `HEAD` got a 400 with a body** (`handle`, `readRequestHead`;
  `TestAnOversizedHeadRequestGetsHeadersAndNoBody`). The reference sends a body there too.
- Docs and hygiene: `BrowserLoginFailedError` documents `access_denied` sent with another error
  (`TestADeniedCallbackWithAnotherErrorFailsAsAccessDenied`); two listener tests close their
  listener on an early failure; the parity reason says 119 cases; the README says an empty
  `Scopes` leaves `scope` to the API's default.
- Answered, not changed (cross-SDK contract): hexadecimal hosts such as `0x100000000` stay usable,
  as the reference's `_real_host` has them, and the producer's own release gate is the Python
  checker's.

## Ported from the reference's final round (39e31d8)

The brief's "What changed since 49744ee385bf" table, each row with its vector cases or a test
mirroring the Python one it names; every row with an observable behaviour was checked by reverting
it and seeing its test fail:

1. No control-plane request leaves the API's origin (`sendRequest` in `control.go`, `urlOrigin`):
   `TestNoRequestLeavesTheAPIOrigin`, `TestTheExchangeNeverLeavesTheAPIOrigin`. Go appends a path
   to `APIURL` instead of joining it, so the reference's `/https://...` paths stay on the API's
   host here; what the check stops is a hand-built `APIURL` without its trailing slash.
2. `token_endpoint`: no colon, whole escapes only (`usableTokenEndpoint`, `wholeEscapes`): five
   `config` cases.
3. `authorization_endpoint` openable: port 1-65535, host as written (`dashboardHost`): nine
   `config` cases.
4. Only RFC 6749's codes kept from a callback (`oauthErrors` in `loopback.go`): three `callbacks`
   cases and `sign_ins/an error that repeats the state never reaches the error`.
5. Only a missing device name gets the default (`DeviceName *string`): `sign_ins/an empty device
   name keeps the hostname off the consent page`, `chooser/an empty device name is not sent to a
   device login either`.
6. No 60-second bound on the accepted tab (`handle`): `TestTheAcceptedTabWaitsAsLongAsTheSignInTakes`,
   `TestClosingTheListenerAnswersATabThatIsStillWaiting`. Like the reference's, a 300 ms test
   cannot tell a 60-second bound from none, so this row was not checked by reverting it.
7. HEAD answers with headers and no body (`loopbackResponse`): `TestAHeadRequestGetsTheHeadersAndNoBody`.
8. Redaction before formatting, one pass, longest first, never inside `[redacted]` (`scrubText`,
   `redactExchangeAnswer`): `TestAMessageCutToLengthKeepsNoPartOfASecret`,
   `TestScrubbingIsOnePassLongestFirstAndLeavesItsMarkerAlone`,
   `TestATransportFailureDuringTheExchangeCarriesNothingOfTheRequest`.
9. Drain before closing (`reply`: `CloseWrite`, then at most 2 s / 1 MiB): `TestAnEarlyAnswerIsNotLostToAReset`,
   `TestDrainingAConnectionIsBounded`.
10. Wording: README, CHANGELOG and the package comment say a link or code sent to someone on
    another computer delivers nothing to the sender.

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
  Python SDK's `main` carries `6dfb3bb31ac7…` (thalovant/thalovant-python-sdk#113).
- **The Rust port's `usual_form` still answers the way Go did** on the two kinds of tag above (noted 2026-11-03). No shared vector covers `usual_form`, so the parity gate sees no difference; see SUGGESTIONS.md.
- **The home-goroutine bound is Go's own.** The reference caps plain handlers at four threads running at once; Go caps only the handlers and replies still running past their time, so handlers that honour their context are never refused. The count is checked when a request arrives, so requests already running when the fourth runs over can still add to it before new ones are refused. A `Replier` value that cannot be compared (a struct holding a slice) is not counted for calls of `AnswerHomeRequest` on their own, since it cannot be told apart from another value; pointers, which `Client` and `HubSession` are, always are.
