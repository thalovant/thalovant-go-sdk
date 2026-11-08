package thalovant

// Browser sign-in against browser-login-vectors.json: the Python SDK's
// contracts/conformance/browser-login-vectors.json, vendored here unchanged
// under testdata/ and pinned by the parity contract.
//
// The pure sections (PKCE, the redirect, the authorization URL, the
// configuration, the browser rule, what a callback means) are run against
// the functions that decide them, and every callback case runs through a
// real listener too. sign_ins and chooser are run for real: a loopback
// httptest server serves each case's exchanges in order and checks every
// request the SDK sends, and a raw TCP client plays the browser against the
// port the SDK bound. What the SDK produced is recorded under
// <section>/<name>, shaped exactly as the reference's runner
// (tests/test_browser_login_vectors.py) shapes it, and compared with the
// case. The Go SDK has one form of each call (blocking, with a context), so
// there is one run of each case where Python runs a sync and an async one.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const browserVectorFile = "browser-login-vectors.json"

// syntheticBrowserState is the state the pure callback cases are run with.
const syntheticBrowserState = "Xx0-_yLq7m3Vw9ZtR2cKp4HdN8sFjB6a"

var (
	unreservedVerifier = regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`)
	stateAlphabet      = regexp.MustCompile(`^[A-Za-z0-9\-_]{22,}$`)
	challengeShape     = regexp.MustCompile(`^[A-Za-z0-9\-_]{43}$`)
)

func loadBrowserVectors(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/browser-login-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors map[string]any
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

// browserCases runs each case of one section, recording what it produced.
func browserCases(t *testing.T, vectors map[string]any, section string, run func(t *testing.T, spec map[string]any) any) {
	t.Helper()
	cases := anySlice(vectors[section])
	if len(cases) == 0 {
		t.Fatalf("%s has no %s cases", browserVectorFile, section)
	}
	for _, raw := range cases {
		spec := mapValue(raw)
		name := fmt.Sprint(spec["name"])
		t.Run(name, func(t *testing.T) {
			produced := run(t, spec)
			recordConformance(t, browserVectorFile, section+"/"+name, produced)
			assertProduced(t, produced, spec["expect"])
		})
	}
}

func TestBrowserLoginVectorsRunEverySection(t *testing.T) {
	vectors := loadBrowserVectors(t)
	want := map[string]bool{"pkce": true, "redirect_uris": true, "authorization_urls": true, "config": true,
		"environment": true, "callbacks": true, "sign_ins": true, "chooser": true}
	for key, value := range vectors {
		cases := anySlice(value)
		if len(cases) == 0 {
			continue
		}
		if _, named := mapValue(cases[0])["name"]; named && !want[key] {
			t.Errorf("section %s is not run", key)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("sections missing from the vectors: %v", want)
	}
	first := mapValue(anySlice(vectors["pkce"])[0])
	if first["verifier"] != "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk" ||
		mapValue(first["expect"])["challenge"] != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Error("the first PKCE case is not RFC 7636 Appendix B")
	}
	if vectors["client_id"] != SDKClientID || vectors["redirect_uri"] != "http://127.0.0.1:{port}/callback" {
		t.Errorf("constants differ: %v %v", vectors["client_id"], vectors["redirect_uri"])
	}
	if milliseconds(vectors["default_timeout_ms"]) != DefaultBrowserLoginTimeout {
		t.Errorf("default timeout %v, want %v ms", DefaultBrowserLoginTimeout, vectors["default_timeout_ms"])
	}
	if vectors["device_name_max_code_points"] != float64(MaxDeviceNameLength) {
		t.Errorf("MaxDeviceNameLength = %d, want %v", MaxDeviceNameLength, vectors["device_name_max_code_points"])
	}
	pages := mapValue(vectors["pages"])
	if pages["signed_in"] != loopbackSignedInText || pages["not_completed"] != loopbackNotCompletedText {
		t.Errorf("page texts differ: %v", pages)
	}
	headers := map[string]any{}
	for _, header := range loopbackPageHeaders {
		headers[header[0]] = header[1]
	}
	if !reflect.DeepEqual(headers, mapValue(vectors["page_headers"])) {
		t.Errorf("page headers %v, want %v", headers, vectors["page_headers"])
	}
}

// -- the pure sections ----------------------------------------------------------

func TestBrowserLoginPKCEVectors(t *testing.T) {
	browserCases(t, loadBrowserVectors(t), "pkce", func(_ *testing.T, spec map[string]any) any {
		return map[string]any{"challenge": ChallengeFor(fmt.Sprint(spec["verifier"]))}
	})
}

func TestBrowserLoginRedirectURIVectors(t *testing.T) {
	browserCases(t, loadBrowserVectors(t), "redirect_uris", func(_ *testing.T, spec map[string]any) any {
		port, _ := spec["port"].(float64)
		redirect, err := LoopbackRedirectURI(int(port))
		if err != nil {
			return map[string]any{"error": "invalid_port"}
		}
		return map[string]any{"redirect_uri": redirect}
	})
}

func TestBrowserLoginAuthorizationURLVectors(t *testing.T) {
	browserCases(t, loadBrowserVectors(t), "authorization_urls", func(t *testing.T, spec map[string]any) any {
		input := mapValue(spec["input"])
		port, _ := input["port"].(float64)
		redirect, err := LoopbackRedirectURI(int(port))
		if err != nil {
			t.Fatal(err)
		}
		var scopes []string
		for _, scope := range anySlice(input["scopes"]) {
			scopes = append(scopes, fmt.Sprint(scope))
		}
		return map[string]any{"url": BrowserAuthorizationURL(fmt.Sprint(input["authorization_endpoint"]), BrowserAuthorization{
			ClientID:      fmt.Sprint(input["client_id"]),
			RedirectURI:   redirect,
			CodeChallenge: fmt.Sprint(input["code_challenge"]),
			State:         fmt.Sprint(input["state"]),
			Scopes:        scopes,
			DeviceName:    fmt.Sprint(input["device_name"]),
		})}
	})
}

func TestBrowserLoginConfigVectors(t *testing.T) {
	browserCases(t, loadBrowserVectors(t), "config", func(t *testing.T, spec map[string]any) any {
		config, err := ParseNativeSignInConfig(mapValue(spec["body"]))
		if err != nil {
			if !errors.Is(err, ErrAPI) {
				t.Errorf("an unusable configuration is not ErrAPI: %v", err)
			}
			return map[string]any{"usable": false}
		}
		return map[string]any{"usable": true, "authorization_endpoint": config.AuthorizationEndpoint,
			"token_endpoint": config.TokenEndpoint, "sdk_client_id": config.SDKClientID}
	})
}

func TestBrowserLoginEnvironmentVectors(t *testing.T) {
	browserCases(t, loadBrowserVectors(t), "environment", func(_ *testing.T, spec map[string]any) any {
		env := map[string]string{}
		for name, value := range mapValue(spec["env"]) {
			env[name] = fmt.Sprint(value)
		}
		return map[string]any{"browser": CanOpenBrowser(env, fmt.Sprint(spec["platform"]))}
	})
}

// callbackOutcomeRecord is a callback as the vectors spell it.
func callbackOutcomeRecord(status int, callback *loopbackCallback) map[string]any {
	switch {
	case callback == nil:
		return map[string]any{"status": status, "outcome": "ignored"}
	case callback.outcome == callbackCode:
		return map[string]any{"status": status, "outcome": "code", "code": callback.code}
	case callback.outcome == callbackDenied:
		return map[string]any{"status": status, "outcome": "denied"}
	}
	return map[string]any{"status": status, "outcome": "failed", "error": absentIfEmpty(callback.errorParam)}
}

func TestBrowserLoginCallbackVectors(t *testing.T) {
	browserCases(t, loadBrowserVectors(t), "callbacks", func(_ *testing.T, spec map[string]any) any {
		request := mapValue(spec["request"])
		target := strings.ReplaceAll(fmt.Sprint(request["target"]), "{state}", syntheticBrowserState)
		return callbackOutcomeRecord(classifyRequest(fmt.Sprint(request["method"]), target, syntheticBrowserState))
	})
}

// The same cases through a real socket: the listener answers as the rule
// says, with the page headers on every answer. Not recorded, as in the
// reference: the classifier's record is the case's.
func TestBrowserLoginCallbackVectorsOverTheListener(t *testing.T) {
	vectors := loadBrowserVectors(t)
	for _, raw := range anySlice(vectors["callbacks"]) {
		spec := mapValue(raw)
		t.Run(fmt.Sprint(spec["name"]), func(t *testing.T) {
			listener := newLoopbackListener(syntheticBrowserState)
			port, err := listener.start()
			if err != nil {
				t.Fatal(err)
			}
			defer listener.close()
			request := mapValue(spec["request"])
			target := strings.ReplaceAll(fmt.Sprint(request["target"]), "{state}", syntheticBrowserState)
			answer := browserTab(t, port, fmt.Sprint(request["method"]), target, func() {
				time.Sleep(50 * time.Millisecond)
				listener.finish(false)
			})
			checkPageHeaders(t, answer, vectors)
			accepted, err := listener.wait(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			assertProduced(t, callbackOutcomeRecord(answer.status, accepted), spec["expect"])
		})
	}
}

// -- a browser, and an API, on loopback -----------------------------------------

// tabAnswer is what a browser tab was answered with.
type tabAnswer struct {
	status  int
	headers map[string]string
	body    string
}

// page names which of the vectors' pages the body shows, or nil.
func (a tabAnswer) page(vectors map[string]any) any {
	for key, text := range mapValue(vectors["pages"]) {
		if strings.Contains(a.body, fmt.Sprint(text)) {
			return key
		}
	}
	return nil
}

// browserTab sends one request on its own connection, as a browser tab sends
// it, and reads the answer to the end. beforeReading, when set, runs between
// the two.
func browserTab(t *testing.T, port int, method, target string, beforeReading func()) tabAnswer {
	t.Helper()
	request := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nConnection: close\r\n\r\n", method, target, port)
	return rawTab(t, port, []byte(request), beforeReading)
}

func rawTab(t *testing.T, port int, request []byte, beforeReading func()) tabAnswer {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("the listener refused the tab: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("writing the request: %v", err)
	}
	if beforeReading != nil {
		beforeReading()
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	return parseTabAnswer(t, raw)
}

func parseTabAnswer(t *testing.T, raw []byte) tabAnswer {
	t.Helper()
	head, body, _ := strings.Cut(string(raw), "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	fields := strings.Split(lines[0], " ")
	if len(fields) < 2 {
		t.Fatalf("not an HTTP answer: %q", lines[0])
	}
	var status int
	if _, err := fmt.Sscanf(fields[1], "%d", &status); err != nil {
		t.Fatalf("no status in %q", lines[0])
	}
	headers := map[string]string{}
	for _, line := range lines[1:] {
		name, value, _ := strings.Cut(line, ":")
		headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return tabAnswer{status: status, headers: headers, body: body}
}

func checkPageHeaders(t *testing.T, answer tabAnswer, vectors map[string]any) {
	t.Helper()
	for name, value := range mapValue(vectors["page_headers"]) {
		if answer.headers[name] != value {
			t.Errorf("header %s = %q, want %q", name, answer.headers[name], value)
		}
	}
	// Self-contained: nothing for the page to load.
	for _, reference := range []string{"src=", "href=", "http"} {
		if strings.Contains(answer.body, reference) {
			t.Errorf("the page carries %q: %s", reference, answer.body)
		}
	}
}

// refusedPort reports whether a new connection to the port is refused. A
// listening loopback port accepts at once, so a short timeout tells the two
// apart; Windows retries a refused loopback connect for about two seconds
// before it says so, and that wait would only slow the suite down.
func refusedPort(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

// signing is what the runner learnt from the URL the SDK built.
type signing struct {
	endpoint  string
	pairs     [][2]string
	port      int
	state     string
	challenge string
}

func readSigning(t *testing.T, raw string) *signing {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the authorization URL is not a URL: %v", err)
	}
	found := &signing{endpoint: parsed.Scheme + "://" + parsed.Host + parsed.Path}
	values := map[string]string{}
	for _, piece := range strings.Split(parsed.RawQuery, "&") {
		key, value, _ := strings.Cut(piece, "=")
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			t.Fatalf("%s is not percent-encoded: %v", key, err)
		}
		found.pairs = append(found.pairs, [2]string{key, decoded})
		values[key] = decoded
	}
	redirect, err := url.Parse(values["redirect_uri"])
	if err != nil {
		t.Fatalf("redirect_uri: %v", err)
	}
	if _, err := fmt.Sscanf(redirect.Port(), "%d", &found.port); err != nil {
		t.Fatalf("redirect_uri has no port: %q", values["redirect_uri"])
	}
	found.state, found.challenge = values["state"], values["code_challenge"]
	if !stateAlphabet.MatchString(found.state) {
		t.Errorf("state %q is not at least 128 bits of base64url", found.state)
	}
	if !challengeShape.MatchString(found.challenge) {
		t.Errorf("code_challenge %q is not 43 base64url characters", found.challenge)
	}
	if values["redirect_uri"] != fmt.Sprintf("http://127.0.0.1:%d/callback", found.port) || found.port < 1024 || found.port > 65535 {
		t.Errorf("redirect_uri %q", values["redirect_uri"])
	}
	return found
}

// normalized puts the generated port, state and challenge back as their
// placeholders.
func (s *signing) normalized(text string) string {
	text = strings.ReplaceAll(text, fmt.Sprintf("http://127.0.0.1:%d/callback", s.port), "http://127.0.0.1:{port}/callback")
	text = strings.ReplaceAll(text, s.state, "{state}")
	return strings.ReplaceAll(text, s.challenge, "{challenge}")
}

func (s *signing) record() map[string]any {
	query := make([]any, len(s.pairs))
	for index, pair := range s.pairs {
		query[index] = []any{pair[0], s.normalized(pair[1])}
	}
	return map[string]any{"endpoint": s.endpoint, "query": query}
}

// browserAPI serves a case's exchanges in order and checks each request
// against its own, as the reference's ScriptedApi does.
type browserAPI struct {
	mu         sync.Mutex
	exchanges  []any
	index      int
	sent       []string
	mismatches []string
	signing    *signing
	exchange   map[string]any
	verifier   string
	server     *httptest.Server
}

func newBrowserAPI(t *testing.T, exchanges []any) *browserAPI {
	t.Helper()
	api := &browserAPI{exchanges: exchanges}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	t.Cleanup(api.server.Close)
	return api
}

func (a *browserAPI) setSigning(found *signing) {
	a.mu.Lock()
	a.signing = found
	a.mu.Unlock()
}

// tokenBody is the exchange body with the generated values put back as
// placeholders, once they are proven: exactly four fields, and a verifier of
// 43-128 unreserved characters whose S256 challenge is the URL's.
func (a *browserAPI) tokenBody(body any) any {
	fields, ok := body.(map[string]any)
	if !ok || a.signing == nil {
		return body
	}
	if len(fields) != 4 || fields["code"] == nil || fields["code_verifier"] == nil || fields["client_id"] == nil || fields["redirect_uri"] == nil {
		a.mismatches = append(a.mismatches, fmt.Sprintf("the exchange must carry exactly four fields, not %d", len(fields)))
	}
	clean := map[string]any{}
	for key, value := range fields {
		if text, isText := value.(string); isText {
			clean[key] = a.signing.normalized(text)
		} else {
			clean[key] = value
		}
	}
	verifier, _ := fields["code_verifier"].(string)
	sum := sha256.Sum256([]byte(verifier))
	if unreservedVerifier.MatchString(verifier) && base64.RawURLEncoding.EncodeToString(sum[:]) == a.signing.challenge {
		clean["code_verifier"] = "{verifier}"
		a.verifier = verifier
	} else {
		a.mismatches = append(a.mismatches, "the code_verifier is not the one the challenge was made from")
	}
	a.exchange = clean
	return clean
}

func (a *browserAPI) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, r.Method+" "+r.URL.Path)
	if a.index >= len(a.exchanges) {
		a.mismatches = append(a.mismatches, "unexpected "+r.Method+" "+r.URL.Path)
		w.WriteHeader(599)
		_, _ = w.Write([]byte("{}"))
		return
	}
	exchange := mapValue(a.exchanges[a.index])
	a.index++
	expected := mapValue(exchange["request"])
	if r.Method != expected["method"] || r.URL.Path != expected["path"] {
		a.mismatches = append(a.mismatches, fmt.Sprintf("%s %s != %v %v", r.Method, r.URL.Path, expected["method"], expected["path"]))
	}
	var body any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			a.mismatches = append(a.mismatches, "body is not JSON")
		}
	}
	_, carriesVerifier := mapValue(body)["code_verifier"]
	if (r.URL.Path == expected["path"] && expected["path"] == "/v1/auth/native/token") || carriesVerifier {
		body = a.tokenBody(body)
	}
	if want, ok := expected["json"]; ok && !reflect.DeepEqual(body, want) {
		a.mismatches = append(a.mismatches, fmt.Sprintf("body %v != %v", body, want))
	}
	if want, ok := expected["authorization"]; ok && r.Header.Get("Authorization") != want {
		a.mismatches = append(a.mismatches, "wrong Authorization header")
	}
	response := mapValue(exchange["response"])
	answer, _ := response["body"].(string)
	if answer != "" {
		w.Header().Set("Content-Type", fmt.Sprint(response["content_type"]))
	}
	status, _ := response["status"].(float64)
	w.WriteHeader(int(status))
	_, _ = w.Write([]byte(answer))
}

func (a *browserAPI) check(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.mismatches) != 0 {
		t.Errorf("requests did not match the case: %v", a.mismatches)
	}
	if a.index != len(a.exchanges) {
		t.Errorf("used %d of %d exchanges: %v", a.index, len(a.exchanges), a.sent)
	}
}

// browserResult is an outcome as the vectors spell it.
func browserResult(t *testing.T, plane *ControlPlane, token map[string]any, err error) map[string]any {
	t.Helper()
	status := func(apiErr *APIError) any {
		if apiErr == nil {
			return nil
		}
		return apiErr.StatusCode
	}
	var (
		denied  *DeviceLoginDeniedError
		expired *DeviceLoginExpiredError
		failed  *BrowserLoginFailedError
		apiErr  *APIError
	)
	switch {
	case err == nil:
		if plane.AccessToken != token["access_token"] || plane.TokenID != token["token_id"] {
			t.Error("the approved token was not kept")
		}
		return map[string]any{"outcome": "approved", "token_type": token["token_type"], "scopes": token["scopes"],
			"expires_at": token["expires_at"], "token_id": token["token_id"]}
	case errors.As(err, &denied):
		if !errors.Is(err, ErrDeviceAccessDenied) || !errors.Is(err, ErrAPI) {
			t.Errorf("denied does not match its sentinels: %v", err)
		}
		return map[string]any{"outcome": "denied", "status": status(denied.APIError)}
	case errors.As(err, &expired):
		if !errors.Is(err, ErrDeviceCodeExpired) || !errors.Is(err, ErrAPI) {
			t.Errorf("expired does not match its sentinels: %v", err)
		}
		return map[string]any{"outcome": "expired", "status": status(expired.APIError)}
	case errors.As(err, &failed):
		if !errors.Is(err, ErrAPI) {
			t.Errorf("a failed sign-in does not match ErrAPI: %v", err)
		}
		return map[string]any{"outcome": "error", "status": nil, "code": absentIfEmpty(failed.Code)}
	case errors.As(err, &apiErr):
		return map[string]any{"outcome": "error", "status": apiErr.StatusCode, "code": absentIfEmpty(apiErr.Code),
			"detail": absentIfEmpty(apiErr.ProblemDetail)}
	}
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("browser sign-in returned an error outside ErrAPI: %T %v", err, err)
	}
	return map[string]any{"outcome": "error", "status": nil, "code": nil}
}

// assertBrowserNoSecrets checks that no secret reaches any printed form of
// the error: the case's synthetic ones, and the verifier and state this
// sign-in generated.
func assertBrowserNoSecrets(t *testing.T, err error, vectors map[string]any, api *browserAPI) {
	t.Helper()
	hidden := []string{}
	for _, secret := range anySlice(vectors["message_excludes"]) {
		hidden = append(hidden, fmt.Sprint(secret))
	}
	api.mu.Lock()
	if api.verifier != "" {
		hidden = append(hidden, api.verifier)
	}
	if api.signing != nil {
		hidden = append(hidden, api.signing.state)
	}
	api.mu.Unlock()
	shown := displayForms(err)
	shown["%#v"] = fmt.Sprintf("%#v", err)
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		problem, _ := json.Marshal(apiErr.Problem)
		shown["Problem"] = string(problem)
		shown["ProblemDetail"] = apiErr.ProblemDetail
		shown["Code"] = apiErr.Code
		shown["Fields"] = strings.Join(apiErr.Fields, ",")
	}
	for form, text := range shown {
		for _, secret := range hidden {
			if strings.Contains(text, secret) {
				t.Errorf("%s of %T carries a secret", form, err)
			}
		}
	}
}

type signInOutcome struct {
	token map[string]any
	err   error
}

// runBrowserSignIn starts a sign-in, plays the browser on the URL it hands
// over, and records what the reference's _sign_in records.
func runBrowserSignIn(t *testing.T, vectors, spec map[string]any, start func(plane *ControlPlane, handOver func(string)) (map[string]any, error)) (map[string]any, *browserAPI) {
	t.Helper()
	api := newBrowserAPI(t, anySlice(spec["exchanges"]))
	plane := NewControlPlane(api.server.URL, "")
	opened := make(chan string, 1)
	handOver := func(target string) {
		select {
		case opened <- target:
		default:
		}
	}
	done := make(chan signInOutcome, 1)
	go func() {
		token, err := start(plane, handOver)
		done <- signInOutcome{token, err}
	}()
	produced := map[string]any{}
	var (
		target  string
		outcome *signInOutcome
	)
	select {
	case target = <-opened:
	case ended := <-done:
		outcome = &ended
		select {
		case target = <-opened:
		default:
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sign-in neither opened a URL nor ended")
	}
	var found *signing
	if target != "" {
		found = readSigning(t, target)
		api.setSigning(found)
		produced["authorization_url"] = found.record()
		browser := []any{}
		for _, raw := range anySlice(spec["requests"]) {
			request := mapValue(raw)
			answer := browserTab(t, found.port, fmt.Sprint(request["method"]),
				strings.ReplaceAll(fmt.Sprint(request["target"]), "{state}", found.state), nil)
			checkPageHeaders(t, answer, vectors)
			step := map[string]any{"status": answer.status}
			if answer.status == 200 {
				step["page"] = answer.page(vectors)
			}
			browser = append(browser, step)
		}
		produced["browser"] = browser
	}
	if outcome == nil {
		select {
		case ended := <-done:
			outcome = &ended
		case <-time.After(10 * time.Second):
			t.Fatal("the sign-in did not end")
		}
	}
	if outcome.err != nil {
		assertBrowserNoSecrets(t, outcome.err, vectors, api)
	}
	if found != nil {
		produced["closed"] = refusedPort(found.port)
	}
	api.mu.Lock()
	if api.exchange != nil {
		produced["exchange"] = api.exchange
	}
	api.mu.Unlock()
	result := browserResult(t, plane, outcome.token, outcome.err)
	if mapValue(spec["call"])["then"] == "revoke" && result["outcome"] == "approved" {
		if err := plane.RevokeAPIToken(context.Background(), ""); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if plane.AccessToken != "" || plane.TokenID != "" {
			t.Error("the revoked token is still held")
		}
		// Idempotent: revoking again sends nothing. The scripted API flags
		// any request past the case's exchanges.
		if err := plane.RevokeAPIToken(context.Background(), ""); err != nil {
			t.Fatalf("revoking again: %v", err)
		}
		result = map[string]any{"outcome": "revoked"}
	}
	produced["result"] = result
	return produced, api
}

// callDeviceName is a case's device_name: nil when the case names none (the
// default then applies), and the string, empty included, when it names one.
func callDeviceName(call map[string]any) *string {
	if name, ok := call["device_name"].(string); ok {
		return &name
	}
	return nil
}

func callScopes(call map[string]any) []string {
	var scopes []string
	for _, scope := range anySlice(call["scopes"]) {
		scopes = append(scopes, fmt.Sprint(scope))
	}
	return scopes
}

func TestBrowserLoginSignInVectors(t *testing.T) {
	vectors := loadBrowserVectors(t)
	browserCases(t, vectors, "sign_ins", func(t *testing.T, spec map[string]any) any {
		call := mapValue(spec["call"])
		open := false
		produced, api := runBrowserSignIn(t, vectors, spec, func(plane *ControlPlane, handOver func(string)) (map[string]any, error) {
			return plane.BrowserLogin(context.Background(), BrowserLoginOptions{
				Scopes:      callScopes(call),
				DeviceName:  callDeviceName(call),
				OpenBrowser: &open,
				Timeout:     milliseconds(call["timeout_ms"]),
				Prompt:      handOver,
				Opener:      func(string) { t.Error("the browser was opened with OpenBrowser false") },
			})
		})
		api.check(t)
		return produced
	})
}

// becomeMachine makes this process a case's machine: the variables the
// browser rule reads, and its platform. An empty variable is not set, so
// t.Setenv(name, "") clears one for the test.
func becomeMachine(t *testing.T, spec map[string]any) {
	t.Helper()
	for _, name := range []string{"CI", "SSH_CONNECTION", "SSH_TTY", "DISPLAY", "WAYLAND_DISPLAY"} {
		t.Setenv(name, "")
	}
	for name, value := range mapValue(spec["env"]) {
		t.Setenv(name, fmt.Sprint(value))
	}
	original := browserPlatform
	platform := fmt.Sprint(spec["platform"])
	browserPlatform = func() string { return platform }
	t.Cleanup(func() { browserPlatform = original })
}

func TestBrowserLoginChooserVectors(t *testing.T) {
	vectors := loadBrowserVectors(t)
	browserCases(t, vectors, "chooser", func(t *testing.T, spec map[string]any) any {
		becomeMachine(t, spec)
		call := mapValue(spec["call"])
		produced, api := runBrowserSignIn(t, vectors, spec, func(plane *ControlPlane, handOver func(string)) (map[string]any, error) {
			return plane.InteractiveLogin(context.Background(), InteractiveLoginOptions{
				Method:     LoginMethod(fmt.Sprint(call["method"])),
				DeviceName: callDeviceName(call),
				Timeout:    milliseconds(call["timeout_ms"]),
				// The browser the SDK opens. A device login opens its
				// verification page too; only the consent page is this
				// runner's to answer.
				Opener: func(target string) {
					if strings.Contains(target, "response_type=code") {
						handOver(target)
					}
				},
			})
		})
		api.check(t)
		method := "browser"
		api.mu.Lock()
		for _, line := range api.sent {
			if line == "POST /v1/auth/device/authorize" {
				method = "device"
			}
		}
		api.mu.Unlock()
		if _, opened := produced["browser"]; method == "browser" && opened && produced["closed"] != true {
			t.Error("the port is still open after the sign-in ended")
		}
		return map[string]any{"method": method, "result": produced["result"]}
	})
}
