package thalovant

// Browser sign-in beyond the shared vectors: what is printed and opened, the
// generated secrets, the listener's own edges, the goroutines it runs, and
// what never reaches an error. browser_login_vectors_test.go runs the
// contract every SDK shares; these are this SDK's own, as the reference's
// tests/test_browser_login.py is the Python SDK's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var unitNativeConfig = map[string]any{
	"authorization_endpoint":           "https://dash.thalovant.com/authorize",
	"token_endpoint":                   "/v1/auth/native/token",
	"device_authorization_endpoint":    "/v1/auth/device/authorize",
	"sdk_client_id":                    "thalovant-sdk",
	"loopback_redirect_uri":            "http://127.0.0.1:{port}/callback",
	"code_challenge_methods_supported": []any{"S256"},
}

var unitToken = map[string]any{"access_token": "tvpat_unit_token", "token_type": "bearer", "scopes": []any{"hubs:read"},
	"expires_at": "2027-11-08T12:00:00Z", "token_id": "tok-unit", "ai_disclosure": "AI"}

// unitBrowserAPI answers the configuration and the token routes; token, when
// set, answers the exchange.
type unitBrowserAPI struct {
	mu       sync.Mutex
	requests []string
	token    func(body map[string]any) (int, any)
	server   *httptest.Server
}

func newUnitBrowserAPI(t *testing.T, token func(body map[string]any) (int, any)) *unitBrowserAPI {
	t.Helper()
	api := &unitBrowserAPI{token: token}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		api.mu.Lock()
		api.requests = append(api.requests, r.Method+" "+r.URL.Path)
		api.mu.Unlock()
		answer := func(status int, body any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		}
		switch r.URL.Path {
		case "/v1/auth/native/config":
			answer(200, unitNativeConfig)
		case "/v1/auth/native/token":
			if api.token == nil {
				answer(200, unitToken)
				return
			}
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			answer(api.token(body))
		default:
			w.WriteHeader(599)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *unitBrowserAPI) sent() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.requests...)
}

// urlParts reads the port, the state and the query out of an authorization
// URL.
func urlParts(t *testing.T, target string) (int, string, url.Values) {
	t.Helper()
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	redirect, err := url.Parse(query.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(redirect.Port(), "%d", &port); err != nil {
		t.Fatalf("no port in %q", query.Get("redirect_uri"))
	}
	return port, query.Get("state"), query
}

// startSignIn runs BrowserLogin on its own goroutine, handing the URL over
// through opts.Prompt when it has none of its own.
func startSignIn(ctx context.Context, plane *ControlPlane, opts BrowserLoginOptions) (<-chan string, <-chan signInOutcome) {
	opened := make(chan string, 1)
	if opts.Prompt == nil {
		opts.Prompt = func(target string) {
			select {
			case opened <- target:
			default:
			}
		}
	}
	done := make(chan signInOutcome, 1)
	go func() {
		token, err := plane.BrowserLogin(ctx, opts)
		done <- signInOutcome{token, err}
	}()
	return opened, done
}

func receive[T any](t *testing.T, from <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-from:
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// captureStdout swaps os.Stdout for a pipe until the returned function is
// called, which restores it and answers what was printed.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	printed := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(reader)
		printed <- string(raw)
	}()
	restored := false
	restore := func() string {
		if restored {
			return ""
		}
		restored = true
		os.Stdout = original
		_ = writer.Close()
		text := <-printed
		_ = reader.Close()
		return text
	}
	t.Cleanup(func() { restore() })
	return restore
}

// swapOpenBrowser replaces the platform browser for one test.
func swapOpenBrowser(t *testing.T, replacement func(string) error) {
	t.Helper()
	original := openBrowser
	openBrowser = replacement
	t.Cleanup(func() { openBrowser = original })
}

// assertNoLoopbackGoroutines fails when any goroutine the listener runs is
// still alive. close waits for every one of them, so they are gone the moment
// BrowserLogin returns, give or take the instant a goroutine takes to exit
// after its last deferred call.
func assertNoLoopbackGoroutines(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		buffer := make([]byte, 1<<20)
		stacks := string(buffer[:runtime.Stack(buffer, true)])
		if !strings.Contains(stacks, ".(*loopbackListener).") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a loopback listener goroutine outlived the sign-in:\n%s", stacks)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// -- what the person sees ---------------------------------------------------------

func TestBrowserLoginPrintsTheURLAndDoesNotWaitForTheOpener(t *testing.T) {
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	swapOpenBrowser(t, func(string) error { t.Error("the platform browser was opened"); return nil })
	printed := captureStdout(t)
	opened := make(chan string, 1)
	release := make(chan struct{})
	returned := make(chan struct{})
	done := make(chan signInOutcome, 1)
	go func() {
		token, err := plane.BrowserLogin(context.Background(), BrowserLoginOptions{
			DeviceName: new("unit"),
			// A console browser holds the call until it exits.
			Opener: func(target string) {
				opened <- target
				<-release
				close(returned)
			},
		})
		done <- signInOutcome{token, err}
	}()
	target := receive(t, opened, "the browser to be opened")
	port, state, _ := urlParts(t, target)
	if answer := browserTab(t, port, "GET", "/callback?code=c&state="+state, nil); answer.status != 200 || !strings.Contains(answer.body, loopbackSignedInText) {
		t.Fatalf("the tab got %d %q", answer.status, answer.body)
	}
	outcome := receive(t, done, "the sign-in to end while the opener still runs")
	close(release)
	receive(t, returned, "the opener to return")
	if outcome.err != nil || outcome.token["access_token"] != "tvpat_unit_token" || plane.TokenID != "tok-unit" {
		t.Fatalf("sign-in = %v, %v", outcome.token, outcome.err)
	}
	want := "Opening your browser to sign in to Thalovant. If it does not open, visit:\n" + target + "\n"
	if text := printed(); !strings.Contains(text, want) {
		t.Errorf("printed %q, want %q", text, want)
	}
	assertNoLoopbackGoroutines(t)
}

func TestBrowserLoginWithoutOpenBrowserOnlyShowsTheURL(t *testing.T) {
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	swapOpenBrowser(t, func(string) error { t.Error("the browser was opened"); return nil })
	printed := captureStdout(t)
	open := false
	// Nobody comes back to the port, so the sign-in expires.
	_, err := plane.BrowserLogin(context.Background(), BrowserLoginOptions{
		OpenBrowser: &open, Timeout: 200 * time.Millisecond,
		Opener: func(string) { t.Error("the opener was called") },
	})
	var expired *DeviceLoginExpiredError
	if !errors.As(err, &expired) || expired.APIError != nil || !errors.Is(err, ErrDeviceCodeExpired) || !errors.Is(err, ErrAPI) {
		t.Fatalf("err = %v, want an expired sign-in with no status", err)
	}
	if !strings.Contains(err.Error(), "not completed within 200ms") {
		t.Errorf("err = %q", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("a browser sign-in's timeout is expired, not ErrTimeout")
	}
	if text := printed(); !strings.HasPrefix(text, "To sign in to Thalovant, visit:\nhttps://dash.thalovant.com/authorize?response_type=code&") {
		t.Errorf("printed %q", text)
	}
}

func TestBrowserLoginOpensThePlatformBrowserAndItsFailureChangesNothing(t *testing.T) {
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	var mu sync.Mutex
	var launched []string
	swapOpenBrowser(t, func(target string) error {
		mu.Lock()
		launched = append(launched, target)
		mu.Unlock()
		return errors.New("no runnable browser")
	})
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{})
	target := receive(t, opened, "the URL")
	port, state, _ := urlParts(t, target)
	browserTab(t, port, "GET", "/callback?code=c&state="+state, nil)
	if outcome := receive(t, done, "the sign-in"); outcome.err != nil || outcome.token["token_id"] != "tok-unit" {
		t.Fatalf("sign-in = %v, %v", outcome.token, outcome.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(launched) != 1 || launched[0] != target {
		t.Errorf("the platform browser got %v, want the URL once", launched)
	}
}

func TestDefaultDeviceNameIsThisSDKOnThisHost(t *testing.T) {
	original := hostName
	t.Cleanup(func() { hostName = original })
	for _, tc := range []struct {
		host string
		err  error
		want string
	}{
		{host: "workstation", want: "thalovant-go-sdk on workstation"},
		{host: "  spaced  ", want: "thalovant-go-sdk on spaced"},
		{host: "", want: "thalovant-go-sdk"},
		{err: errors.New("no hostname"), want: "thalovant-go-sdk"},
	} {
		hostName = func() (string, error) { return tc.host, tc.err }
		if got := DefaultDeviceName(); got != tc.want {
			t.Errorf("host %q: DefaultDeviceName() = %q, want %q", tc.host, got, tc.want)
		}
	}
	hostName = func() (string, error) { return strings.Repeat("é", 300), nil }
	if got := []rune(DefaultDeviceName()); len(got) != MaxDeviceNameLength {
		t.Errorf("a long name has %d code points, want %d", len(got), MaxDeviceNameLength)
	}
}

func TestTheDefaultDeviceNameReachesTheConsentPage(t *testing.T) {
	original := hostName
	hostName = func() (string, error) { return "workstation", nil }
	t.Cleanup(func() { hostName = original })
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	open := false
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open, Timeout: 200 * time.Millisecond})
	_, _, query := urlParts(t, receive(t, opened, "the URL"))
	if got := query.Get("device_name"); got != "thalovant-go-sdk on workstation" {
		t.Errorf("device_name = %q", got)
	}
	if query.Has("scope") {
		t.Errorf("scope sent when none was asked for: %q", query.Get("scope"))
	}
	if outcome := receive(t, done, "the sign-in"); !errors.Is(outcome.err, ErrDeviceCodeExpired) {
		t.Errorf("err = %v", outcome.err)
	}
}

// -- the secrets ----------------------------------------------------------------

func TestEveryVerifierAndStateIsFreshAndWellFormed(t *testing.T) {
	verifiers, states := map[string]bool{}, map[string]bool{}
	verifierShape := regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`)
	// 24 random bytes: 192 bits, well over the 128 the contract asks for.
	stateShape := regexp.MustCompile(`^[A-Za-z0-9\-_]{32}$`)
	for range 50 {
		verifier, err := NewVerifier()
		if err != nil {
			t.Fatal(err)
		}
		state, err := NewState()
		if err != nil {
			t.Fatal(err)
		}
		if !verifierShape.MatchString(verifier) || !stateShape.MatchString(state) {
			t.Fatalf("verifier %q or state %q is malformed", verifier, state)
		}
		verifiers[verifier], states[state] = true, true
	}
	if len(verifiers) != 50 || len(states) != 50 {
		t.Errorf("%d verifiers and %d states from 50 draws", len(verifiers), len(states))
	}
}

func TestAnEchoedCodeAndVerifierNeverReachTheError(t *testing.T) {
	var mu sync.Mutex
	sent := map[string]string{}
	api := newUnitBrowserAPI(t, func(body map[string]any) (int, any) {
		// A validation error lists each field's input, as FastAPI's 422
		// does, and a secret can sit in a key too.
		mu.Lock()
		defer mu.Unlock()
		detail := []any{}
		for key, value := range body {
			sent[key] = fmt.Sprint(value)
			detail = append(detail, map[string]any{"loc": []any{"body", key}, "msg": "bad", "input": value})
		}
		return 422, map[string]any{"detail": detail, "seen:" + fmt.Sprint(body["code"]): "x",
			"code": "echo-" + fmt.Sprint(body["code"]), "fields": []any{"code_verifier=" + fmt.Sprint(body["code_verifier"])}}
	})
	plane := NewControlPlane(api.server.URL, "")
	open := false
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
	port, state, _ := urlParts(t, receive(t, opened, "the URL"))
	answer := browserTab(t, port, "GET", "/callback?code=secret-code-value&state="+state, nil)
	if answer.status != 200 || !strings.Contains(answer.body, loopbackNotCompletedText) {
		t.Fatalf("the tab got %d %q", answer.status, answer.body)
	}
	err := receive(t, done, "the sign-in").err
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 422 {
		t.Fatalf("err = %v, want the 422", err)
	}
	problem, _ := json.Marshal(apiErr.Problem)
	shown := displayForms(err)
	shown["%#v"] = fmt.Sprintf("%#v", err)
	shown["Problem"] = string(problem)
	shown["ProblemDetail"] = apiErr.ProblemDetail
	shown["Code"] = apiErr.Code
	shown["Fields"] = strings.Join(apiErr.Fields, ",")
	mu.Lock()
	secrets := []string{sent["code"], sent["code_verifier"], state}
	mu.Unlock()
	for form, text := range shown {
		for _, secret := range secrets {
			if secret == "" || strings.Contains(text, secret) {
				t.Errorf("%s carries a secret (or a secret was never sent)", form)
			}
		}
	}
	if !strings.Contains(string(problem), redactedSecret) {
		t.Errorf("the problem was not redacted: %s", problem)
	}
	if plane.AccessToken != "" {
		t.Error("a refused exchange left a token")
	}
}

func TestACleartextAPIIsRefusedBeforeAnythingIsAskedOrOpened(t *testing.T) {
	swapOpenBrowser(t, func(string) error { t.Error("the browser was opened"); return nil })
	plane := NewControlPlane("http://control.example.test", "")
	plane.HTTPClient = &http.Client{Transport: browserTransport(func(r *http.Request) (*http.Response, error) {
		t.Errorf("a request was sent: %s %s", r.Method, r.URL.Path)
		return nil, errors.New("no network")
	})}
	opener := func(string) { t.Error("the opener was called") }
	if _, err := plane.BrowserLogin(context.Background(), BrowserLoginOptions{Opener: opener}); !errors.Is(err, ErrAPI) || !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("BrowserLogin err = %v", err)
	}
	if _, err := plane.InteractiveLogin(context.Background(), InteractiveLoginOptions{Method: LoginMethodBrowser, Opener: opener}); !errors.Is(err, ErrAPI) || !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("InteractiveLogin(browser) err = %v", err)
	}
	// Auto counts a cleartext API as one without browser sign-in: the device
	// login it falls back to is refused by the control plane's own HTTPS rule.
	becomeMachine(t, map[string]any{"platform": "macos", "env": map[string]any{}})
	if _, err := plane.InteractiveLogin(context.Background(), InteractiveLoginOptions{Opener: opener}); !errors.Is(err, ErrAPI) || !strings.Contains(err.Error(), "require HTTPS") {
		t.Errorf("InteractiveLogin(auto) err = %v", err)
	}
}

type browserTransport func(*http.Request) (*http.Response, error)

func (f browserTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInteractiveLoginRefusesAMethodItDoesNotKnow(t *testing.T) {
	plane := NewControlPlane("http://127.0.0.1:9", "")
	plane.HTTPClient = &http.Client{Transport: browserTransport(func(r *http.Request) (*http.Response, error) {
		t.Errorf("a request was sent: %s %s", r.Method, r.URL.Path)
		return nil, errors.New("no network")
	})}
	_, err := plane.InteractiveLogin(context.Background(), InteractiveLoginOptions{Method: "password"})
	if !errors.Is(err, ErrAPI) || !strings.Contains(err.Error(), "LoginMethodAuto") {
		t.Errorf("err = %v", err)
	}
}

// -- the chooser's own edges --------------------------------------------------------

func TestInteractiveLoginDeviceOpensTheVerificationPageWithTheOpener(t *testing.T) {
	becomeMachine(t, map[string]any{"platform": "macos", "env": map[string]any{}})
	swapOpenBrowser(t, func(string) error { t.Error("the platform browser was opened"); return nil })
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			_, _ = io.WriteString(w, `{"device_code":"dc","user_code":"WDJB-MJHT","verification_uri":"https://dash.thalovant.com/device","verification_uri_complete":"https://dash.thalovant.com/device?code=WDJB-MJHT","interval":0,"expires_in":900}`)
		case "/v1/auth/device/token":
			_, _ = io.WriteString(w, `{"access_token":"tvpat_device","token_type":"bearer","scopes":[],"token_id":"tok-device"}`)
		default:
			w.WriteHeader(599)
		}
	}))
	defer server.Close()
	plane := NewControlPlane(server.URL, "")
	opened := make(chan string, 1)
	printed := captureStdout(t)
	token, err := plane.InteractiveLogin(context.Background(), InteractiveLoginOptions{
		Method: LoginMethodDevice, DeviceName: new("unit"), Opener: func(target string) { opened <- target },
	})
	printed()
	if err != nil || token["access_token"] != "tvpat_device" || plane.TokenID != "tok-device" {
		t.Fatalf("sign-in = %v, %v", token, err)
	}
	if got := receive(t, opened, "the verification page"); got != "https://dash.thalovant.com/device?code=WDJB-MJHT" {
		t.Errorf("opened %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(requests, ",") != "POST /v1/auth/device/authorize,POST /v1/auth/device/token" {
		t.Errorf("requests = %v", requests)
	}
}

func TestInteractiveLoginCancelledWhileAskingForTheConfigurationIsNoDeviceLogin(t *testing.T) {
	becomeMachine(t, map[string]any{"platform": "windows", "env": map[string]any{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/v1/auth/native/config" {
			cancel()
			<-r.Context().Done()
			return
		}
		w.WriteHeader(599)
	}))
	defer server.Close()
	plane := NewControlPlane(server.URL, "")
	_, err := plane.InteractiveLogin(ctx, InteractiveLoginOptions{Opener: func(string) { t.Error("opened") }})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(requests, ",") != "GET /v1/auth/native/config" {
		t.Errorf("requests = %v", requests)
	}
}

// -- the listener's own edges ---------------------------------------------------------

func TestCancellingTheSignInClosesThePort(t *testing.T) {
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	ctx, cancel := context.WithCancel(context.Background())
	open := false
	opened, done := startSignIn(ctx, plane, BrowserLoginOptions{OpenBrowser: &open})
	port, _, _ := urlParts(t, receive(t, opened, "the URL"))
	cancel()
	if err := receive(t, done, "the sign-in").err; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if !refusedPort(port) {
		t.Error("the port still listens after the sign-in was cancelled")
	}
	assertNoLoopbackGoroutines(t)
}

func TestAConnectionThatSendsNothingDoesNotHoldTheSignIn(t *testing.T) {
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	open := false
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
	port, state, _ := urlParts(t, receive(t, opened, "the URL"))
	idle, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	started := time.Now()
	if answer := browserTab(t, port, "GET", "/callback?code=c&state="+state, nil); answer.status != 200 {
		t.Fatalf("status %d", answer.status)
	}
	if err := receive(t, done, "the sign-in").err; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took >= 900*time.Millisecond {
		t.Errorf("an idle connection held the sign-in for %s", took)
	}
	assertNoLoopbackGoroutines(t)
}

func TestARequestThatIsNotOneIs400AndTheWaitGoesOn(t *testing.T) {
	vectors := loadBrowserVectors(t)
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	open := false
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
	port, state, _ := urlParts(t, receive(t, opened, "the URL"))
	for name, request := range map[string][]byte{
		"no request line": []byte("hello\r\n\r\n"),
		"a head over 16 KiB": []byte("GET /callback?code=" + strings.Repeat("a", loopbackMaxRequestHead+10) +
			" HTTP/1.1\r\n\r\n"),
	} {
		answer := rawTab(t, port, request, nil)
		if answer.status != 400 {
			t.Errorf("%s: status %d, want 400", name, answer.status)
		}
		checkPageHeaders(t, answer, vectors)
	}
	select {
	case outcome := <-done:
		t.Fatalf("the sign-in ended on a bad request: %v", outcome.err)
	default:
	}
	// No Host header: the reference reads the request line alone.
	answer := rawTab(t, port, []byte("GET /callback?code=c&state="+state+" HTTP/1.1\r\n\r\n"), nil)
	if answer.status != 200 {
		t.Fatalf("status %d", answer.status)
	}
	if outcome := receive(t, done, "the sign-in"); outcome.err != nil || outcome.token["access_token"] != "tvpat_unit_token" {
		t.Fatalf("sign-in = %v, %v", outcome.token, outcome.err)
	}
}

// waitForConnections waits until the listener holds n connections.
func waitForConnections(t *testing.T, listener *loopbackListener, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		listener.mu.Lock()
		held := len(listener.conns)
		listener.mu.Unlock()
		if held >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener holds %d connections, want %d", held, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestASecondAnswerAfterTheFirstIsTurnedAway(t *testing.T) {
	listener := newLoopbackListener("the-state-of-this-sign-in")
	port, err := listener.start()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.close()
	// Both connected before the first is accepted, so the second still
	// reaches the listener after the port has stopped listening.
	first, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	waitForConnections(t, listener, 2)
	request := []byte("GET /callback?code=c&state=the-state-of-this-sign-in HTTP/1.1\r\n\r\n")
	if _, err := first.Write(request); err != nil {
		t.Fatal(err)
	}
	accepted, err := listener.wait(context.Background(), 5*time.Second)
	if err != nil || accepted == nil || accepted.code != "c" {
		t.Fatalf("accepted %v, %v", accepted, err)
	}
	if !refusedPort(port) {
		t.Error("the port still listens after a callback was accepted")
	}
	if _, err := second.Write(request); err != nil {
		t.Fatal(err)
	}
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if raw, _ := io.ReadAll(second); !strings.HasPrefix(string(raw), "HTTP/1.1 400 ") {
		t.Errorf("the second answer got %q", raw)
	}
	listener.finish(true)
	_ = first.SetReadDeadline(time.Now().Add(5 * time.Second))
	if raw, _ := io.ReadAll(first); !strings.Contains(string(raw), loopbackSignedInText) {
		t.Errorf("the accepted tab got %q", raw)
	}
}

func TestTheWaitIsNeverCutShort(t *testing.T) {
	listener := newLoopbackListener("s")
	if _, err := listener.start(); err != nil {
		t.Fatal(err)
	}
	defer listener.close()
	started := time.Now()
	if accepted, err := listener.wait(context.Background(), 250*time.Millisecond); accepted != nil || err != nil {
		t.Fatalf("wait = %v, %v", accepted, err)
	}
	if took := time.Since(started); took < 250*time.Millisecond {
		t.Errorf("the wait ended after %s", took)
	}
}

func TestTheListenerBindsLoopbackOnly(t *testing.T) {
	listener := newLoopbackListener("s")
	if _, err := listener.start(); err != nil {
		t.Fatal(err)
	}
	defer listener.close()
	address, ok := listener.listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) || address.IP.To4() == nil {
		t.Errorf("bound %v", listener.listener.Addr())
	}
	if address.Port < 1024 || address.Port > 65535 {
		t.Errorf("bound port %d", address.Port)
	}
}

func TestAConnectionThatClosesBeforeItsAnswerChangesNothing(t *testing.T) {
	listener := newLoopbackListener("the-state")
	port, err := listener.start()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.close()
	// Gone before sending a whole request head.
	gone, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = gone.Write([]byte("GET /callback?code=c&st"))
	_ = gone.Close()
	if accepted, _ := listener.wait(context.Background(), 100*time.Millisecond); accepted != nil {
		t.Fatalf("a half request was accepted: %v", accepted)
	}
	// Nothing was accepted, so this sign-in's own answer still is; the
	// sign-in then ends without a token, so its tab is told it was not
	// completed.
	answer := browserTab(t, port, "GET", "/callback?code=c&state=the-state", func() {
		time.Sleep(50 * time.Millisecond)
		listener.finish(false)
	})
	if answer.status != 200 || !strings.Contains(answer.body, loopbackNotCompletedText) {
		t.Errorf("the tab got %d %q", answer.status, answer.body)
	}
	if accepted, _ := listener.wait(context.Background(), 0); accepted == nil || accepted.code != "c" {
		t.Errorf("accepted %v", accepted)
	}
}

// -- what a callback says -------------------------------------------------------------

func TestCallbackValuesAreDecodedAsPythonsParseQslDecodesThem(t *testing.T) {
	// Expectations from CPython 3.14's urllib.parse.parse_qsl(query,
	// keep_blank_values=True), which is how the reference reads a callback.
	for query, want := range map[string][][2]string{
		"code=a%2Bb%2Fc%3D%2541&state=x": {{"code", "a+b/c=%41"}, {"state", "x"}},
		"a=1;b=2":                        {{"a", "1;b=2"}},
		"a&&b=&=c":                       {{"a", ""}, {"b", ""}, {"", "c"}},
		"c=%zz%4":                        {{"c", "%zz%4"}},
		"c=%E2%82x":                      {{"c", "�x"}},
		"c=%F0%80%80":                    {{"c", "���"}},
		"c=%ff%fe":                       {{"c", "��"}},
		"c=a+b":                          {{"c", "a b"}},
		"c=%C3%A9t%C3%A9":                {{"c", "été"}},
		"c=%%41":                         {{"c", "%A"}},
		"c=é%41":                         {{"c", "éA"}},
		"c=%e2%82%ac%ED%A0%80":           {{"c", "€���"}},
	} {
		if got := formPairs(query); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("formPairs(%q) = %q, want %q", query, got, want)
		}
	}
	// A request line is read as Latin-1, as the reference reads it: a raw
	// byte 0xE9 is "é", not a broken UTF-8 sequence.
	method, target, ok := requestLine([]byte("GET /callback?code=\xe9&state=s HTTP/1.1"))
	if !ok || method != "GET" {
		t.Fatalf("requestLine = %q %q %v", method, target, ok)
	}
	if status, callback := classifyRequest(method, target, "s"); status != 200 || callback.code != "é" {
		t.Errorf("classifyRequest = %d %v", status, callback)
	}
}

// Only RFC 6749 section 4.1.2.1's codes are kept; anything else is a value
// somebody else chose (the vectors' callbacks section pins the state cases).
func TestAFailedCallbackNamesOnlyAnRFC6749Error(t *testing.T) {
	for errorParam, want := range map[string]string{
		"invalid_request":              "invalid_request",
		"unauthorized_client":          "unauthorized_client",
		"unsupported_response_type":    "unsupported_response_type",
		"invalid_scope":                "invalid_scope",
		"server_error":                 "server_error",
		"temporarily_unavailable":      "temporarily_unavailable",
		"bad%20code":                   "",
		"Server_Error":                 "",
		strings.Repeat("x", 81):        "",
		"invalid_request:scope.denied": "",
	} {
		api := newUnitBrowserAPI(t, nil)
		plane := NewControlPlane(api.server.URL, "")
		open := false
		opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
		port, state, _ := urlParts(t, receive(t, opened, "the URL"))
		browserTab(t, port, "GET", "/callback?error="+errorParam+"&state="+state, nil)
		err := receive(t, done, "the sign-in").err
		var failed *BrowserLoginFailedError
		if !errors.As(err, &failed) || failed.Code != want || !errors.Is(err, ErrAPI) {
			t.Errorf("error=%s: err = %v, want Code %q", errorParam, err, want)
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			t.Errorf("error=%s: a failed callback carries an HTTP status", errorParam)
		}
	}
}

// -- the remaining edges ------------------------------------------------------------

func TestThisMachineIsNamedAsTheBrowserRuleNamesIt(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "macos", "windows": "windows", "linux": "linux",
		"android": "linux", "ios": "ios", "freebsd": "freebsd"} {
		if got := platformName(goos); got != want {
			t.Errorf("platformName(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestTheBrowserRuleReadsThisProcessByDefault(t *testing.T) {
	becomeMachine(t, map[string]any{"platform": "linux", "env": map[string]any{}})
	if CanOpenBrowser(nil, "") {
		t.Error("Linux without a display has a browser")
	}
	t.Setenv("DISPLAY", ":0")
	if !CanOpenBrowser(nil, "") {
		t.Error("Linux with DISPLAY has none")
	}
	t.Setenv("SSH_TTY", "/dev/pts/1")
	if CanOpenBrowser(nil, "") {
		t.Error("over SSH there is one")
	}
}

func TestAnAuthorizationEndpointNoBrowserCanOpenIsUnusable(t *testing.T) {
	for _, endpoint := range []string{
		"https://dash.thalovant.com/author\x07ize", "https://dash.thalovant.com /authorize",
		"https://dash.thalovant.com:99999/authorize", "https://[::1/authorize", "https://dash.thalovant.com/authorize?",
		"https:///authorize", "javascript://dash.thalovant.com/authorize", "https://dash.thalovant.com/\u0085authorize",
	} {
		payload := map[string]any{}
		for key, value := range unitNativeConfig {
			payload[key] = value
		}
		payload["authorization_endpoint"] = endpoint
		if _, err := ParseNativeSignInConfig(payload); !errors.Is(err, ErrAPI) {
			t.Errorf("%q is usable", endpoint)
		}
	}
	for _, endpoint := range []string{"https://dash.thalovant.com:8443/authorize", "http://[::1]:5173/authorize", "HTTPS://Dash.Thalovant.com/authorize"} {
		payload := map[string]any{}
		for key, value := range unitNativeConfig {
			payload[key] = value
		}
		payload["authorization_endpoint"] = endpoint
		if _, err := ParseNativeSignInConfig(payload); err != nil {
			t.Errorf("%q is unusable: %v", endpoint, err)
		}
	}
	if _, err := ParseNativeSignInConfig(nil); !errors.Is(err, ErrAPI) {
		t.Error("a configuration that is not an object is usable")
	}
}

func TestAnAnswerThatIsNotJSONIsAnErrorWithNoStatus(t *testing.T) {
	for _, route := range []string{"config", "token"} {
		t.Run(route, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/v1/auth/native/"+route:
					w.Header().Set("Content-Type", "text/html")
					_, _ = io.WriteString(w, "<html>not json</html>")
				case r.URL.Path == "/v1/auth/native/config":
					_ = json.NewEncoder(w).Encode(unitNativeConfig)
				default:
					w.WriteHeader(599)
				}
			}))
			defer server.Close()
			plane := NewControlPlane(server.URL, "")
			open := false
			opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
			if route == "token" {
				port, state, _ := urlParts(t, receive(t, opened, "the URL"))
				if answer := browserTab(t, port, "GET", "/callback?code=c&state="+state, nil); answer.status != 200 {
					t.Fatalf("status %d", answer.status)
				}
			}
			err := receive(t, done, "the sign-in").err
			var apiErr *APIError
			if !errors.Is(err, ErrAPI) || errors.As(err, &apiErr) {
				t.Errorf("err = %#v, want an ErrAPI error with no status", err)
			}
		})
	}
}

func TestNoGoroutineOutlivesTheSignInOnAnyPath(t *testing.T) {
	for name, play := range map[string]func(t *testing.T, port int, state string, cancel context.CancelFunc){
		"approved": func(t *testing.T, port int, state string, _ context.CancelFunc) {
			browserTab(t, port, "GET", "/callback?code=c&state="+state, nil)
		},
		"denied": func(t *testing.T, port int, state string, _ context.CancelFunc) {
			browserTab(t, port, "GET", "/callback?error=access_denied&state="+state, nil)
		},
		"failed": func(t *testing.T, port int, state string, _ context.CancelFunc) {
			browserTab(t, port, "GET", "/callback?state="+state, nil)
		},
		"refused exchange": func(t *testing.T, port int, state string, _ context.CancelFunc) {
			browserTab(t, port, "GET", "/callback?code=refuse&state="+state, nil)
		},
		"expired with an idle connection": func(t *testing.T, port int, _ string, _ context.CancelFunc) {
			if idle, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
				t.Cleanup(func() { _ = idle.Close() })
			}
		},
		"cancelled": func(_ *testing.T, _ int, _ string, cancel context.CancelFunc) { cancel() },
	} {
		t.Run(name, func(t *testing.T) {
			api := newUnitBrowserAPI(t, func(body map[string]any) (int, any) {
				if body["code"] == "refuse" {
					return 400, map[string]any{"detail": "invalid_grant"}
				}
				return 200, unitToken
			})
			plane := NewControlPlane(api.server.URL, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			open := false
			opened, done := startSignIn(ctx, plane, BrowserLoginOptions{OpenBrowser: &open, Timeout: 300 * time.Millisecond})
			port, state, _ := urlParts(t, receive(t, opened, "the URL"))
			play(t, port, state, cancel)
			receive(t, done, "the sign-in")
			if !refusedPort(port) {
				t.Error("the port still listens")
			}
			assertNoLoopbackGoroutines(t)
		})
	}
	t.Run("a panicking prompt", func(t *testing.T) {
		api := newUnitBrowserAPI(t, nil)
		plane := NewControlPlane(api.server.URL, "")
		var port int
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the prompt's panic did not reach the caller")
				}
			}()
			_, _ = plane.BrowserLogin(context.Background(), BrowserLoginOptions{Prompt: func(target string) {
				port, _, _ = urlParts(t, target)
				panic("prompt")
			}})
		}()
		if port == 0 || !refusedPort(port) {
			t.Error("the port still listens after the prompt panicked")
		}
		assertNoLoopbackGoroutines(t)
	})
}

// -- review follow-ups (#90) ------------------------------------------------------------

func TestAPromptThatCancelsTheSignInOpensNothing(t *testing.T) {
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	swapOpenBrowser(t, func(string) error { t.Error("the platform browser was opened"); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var port int
	_, err := plane.BrowserLogin(ctx, BrowserLoginOptions{
		Prompt: func(target string) {
			port, _, _ = urlParts(t, target)
			cancel()
		},
		Opener: func(string) { t.Error("the opener was called after the sign-in was cancelled") },
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
	if port == 0 || !refusedPort(port) {
		t.Error("the port still listens")
	}
	if sent := api.sent(); strings.Join(sent, ",") != "GET /v1/auth/native/config" {
		t.Errorf("requests = %v", sent)
	}
	assertNoLoopbackGoroutines(t)
}

func TestAnErrorThatCarriesTheStateIsNotRepeated(t *testing.T) {
	for name, errorParam := range map[string]func(state string) string{
		"the state itself":      func(state string) string { return state },
		"the state inside one":  func(state string) string { return "x:" + state + ":y" },
		"a well-formed one too": func(string) string { return "server_error" },
	} {
		t.Run(name, func(t *testing.T) {
			api := newUnitBrowserAPI(t, nil)
			plane := NewControlPlane(api.server.URL, "")
			open := false
			opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
			port, state, _ := urlParts(t, receive(t, opened, "the URL"))
			value := errorParam(state)
			browserTab(t, port, "GET", "/callback?error="+url.QueryEscape(value)+"&state="+state, nil)
			err := receive(t, done, "the sign-in").err
			var failed *BrowserLoginFailedError
			if !errors.As(err, &failed) {
				t.Fatalf("err = %v", err)
			}
			for form, text := range displayForms(err) {
				if strings.Contains(text, state) {
					t.Errorf("%s carries the state", form)
				}
			}
			if strings.Contains(value, state) {
				if failed.Code != "" {
					t.Errorf("Code = %q, want none", failed.Code)
				}
			} else if failed.Code != value {
				t.Errorf("Code = %q, want %q", failed.Code, value)
			}
		})
	}
}

func TestTheConfigurationParserReadsATypedMethodList(t *testing.T) {
	withMethods := func(methods any) map[string]any {
		payload := map[string]any{}
		for key, value := range unitNativeConfig {
			payload[key] = value
		}
		payload["code_challenge_methods_supported"] = methods
		return payload
	}
	if _, err := ParseNativeSignInConfig(withMethods([]string{"plain", "S256"})); err != nil {
		t.Errorf("[]string{plain, S256} is unusable: %v", err)
	}
	for _, methods := range []any{[]string{"plain"}, "S256", []any{"s256"}, nil} {
		if _, err := ParseNativeSignInConfig(withMethods(methods)); !errors.Is(err, ErrAPI) {
			t.Errorf("%#v is usable", methods)
		}
	}
}

func TestEndpointsNoRequestCanUseAreUnusable(t *testing.T) {
	configWith := func(key, value string) map[string]any {
		payload := map[string]any{}
		for name, member := range unitNativeConfig {
			payload[name] = member
		}
		payload[key] = value
		return payload
	}
	for key, values := range map[string][]string{
		"authorization_endpoint": {"https://dash.thalovant.com:0/authorize", "https://dash.thalovant.com:00/authorize"},
		"token_endpoint":         {"/v1/%zz", "/v1/auth/native/token%", "/v1/%G0"},
	} {
		for _, value := range values {
			if _, err := ParseNativeSignInConfig(configWith(key, value)); !errors.Is(err, ErrAPI) {
				t.Errorf("%s %q is usable", key, value)
			}
		}
	}
	for key, value := range map[string]string{
		"authorization_endpoint": "https://dash.thalovant.com:1/authorize",
		"token_endpoint":         "/v1/auth/native%2Ftoken",
	} {
		if _, err := ParseNativeSignInConfig(configWith(key, value)); err != nil {
			t.Errorf("%s %q is unusable: %v", key, value, err)
		}
	}
}

// replayRecorder notes, for each request, whether the transport could send
// its body a second time.
type replayRecorder struct {
	mu        sync.Mutex
	rewinds   map[string]bool
	transport http.RoundTripper
}

func (r *replayRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.rewinds[request.Method+" "+request.URL.Path] = request.GetBody != nil
	r.mu.Unlock()
	return r.transport.RoundTrip(request)
}

func TestTheExchangeIsNeverReplayedByTheTransport(t *testing.T) {
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	recorder := &replayRecorder{rewinds: map[string]bool{}, transport: http.DefaultTransport}
	plane.HTTPClient = &http.Client{Transport: recorder}
	open := false
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
	port, state, _ := urlParts(t, receive(t, opened, "the URL"))
	browserTab(t, port, "GET", "/callback?code=c&state="+state, nil)
	if outcome := receive(t, done, "the sign-in"); outcome.err != nil {
		t.Fatal(outcome.err)
	}
	// Any other request with a body keeps its GetBody, so the transport's
	// usual retries are unchanged there.
	_, _ = plane.BeginDeviceLogin(context.Background(), []string{"hubs:read"}, "unit")
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if rewind, sent := recorder.rewinds["POST /v1/auth/native/token"]; !sent || rewind {
		t.Errorf("the exchange was sent %v with a rewindable body %v, want sent once without", sent, rewind)
	}
	if !recorder.rewinds["POST /v1/auth/device/authorize"] {
		t.Error("another POST lost its rewindable body")
	}
}

func TestARefusedExchangeKeepsNoTokenAndNoHalfSecret(t *testing.T) {
	const code = "secret-code-value-abcdefghijklmnop"
	escaped := func(text string) string {
		var out strings.Builder
		for _, r := range text {
			fmt.Fprintf(&out, `\u%04x`, r)
		}
		return out.String()
	}
	for name, answer := range map[string]string{
		// A token in a refusal, at the top, nested, and in the sentence.
		"a token in the answer": `{"detail":"invalid_grant","access_token":"tvpat_leaked_in_a_refusal","error_description":"was tvpat_leaked_in_a_refusal","nested":{"refresh_token":"rt_leaked","items":["rt_leaked"]}}`,
		// The code spelled with escapes, placed across the display limit.
		"an escaped code across the limit": `{"detail":"` + strings.Repeat("x", 240) + " " + escaped(code) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth/native/config" {
					_ = json.NewEncoder(w).Encode(unitNativeConfig)
					return
				}
				w.WriteHeader(400)
				_, _ = io.WriteString(w, answer)
			}))
			defer server.Close()
			plane := NewControlPlane(server.URL, "")
			open := false
			opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
			port, state, _ := urlParts(t, receive(t, opened, "the URL"))
			browserTab(t, port, "GET", "/callback?code="+code+"&state="+state, nil)
			err := receive(t, done, "the sign-in").err
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
				t.Fatalf("err = %v", err)
			}
			problem, _ := json.Marshal(apiErr.Problem)
			shown := displayForms(err)
			shown["%#v"] = fmt.Sprintf("%#v", err)
			shown["Problem"] = string(problem)
			shown["ProblemDetail"] = apiErr.ProblemDetail
			for form, text := range shown {
				for _, secret := range []string{"tvpat_leaked_in_a_refusal", "rt_leaked", code[:8], state} {
					if strings.Contains(text, secret) {
						t.Errorf("%s carries %q", form, secret)
					}
				}
			}
			if plane.AccessToken != "" {
				t.Error("a refused exchange kept a token")
			}
		})
	}
}

func TestTheListenerHandlesABoundedNumberOfConnections(t *testing.T) {
	listener := newLoopbackListener("the-state")
	port, err := listener.start()
	if err != nil {
		t.Fatal(err)
	}
	// Even if an assertion stops the test early, so that no goroutine of
	// this listener outlives it into another test's goroutine check. close
	// may run twice; the explicit one below stays before that check.
	t.Cleanup(listener.close)
	// More connections than the bound, none of them sending anything.
	var idle []net.Conn
	t.Cleanup(func() {
		for _, conn := range idle {
			_ = conn.Close()
		}
	})
	for range loopbackMaxConnections + 6 {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, conn)
	}
	waitForConnections(t, listener, loopbackMaxConnections)
	time.Sleep(100 * time.Millisecond)
	listener.mu.Lock()
	held := len(listener.conns)
	listener.mu.Unlock()
	if held != loopbackMaxConnections {
		t.Fatalf("the listener handles %d connections, want at most %d", held, loopbackMaxConnections)
	}
	// Each one that closes makes room for one that waited.
	for _, conn := range idle {
		_ = conn.Close()
	}
	answer := browserTab(t, port, "GET", "/callback?code=c&state=the-state", func() { listener.finish(true) })
	if answer.status != 200 || !strings.Contains(answer.body, loopbackSignedInText) {
		t.Errorf("the tab got %d %q once the idle connections closed", answer.status, answer.body)
	}
	listener.close()
	assertNoLoopbackGoroutines(t)
}

// -- the producer's final round (thalovant-python-sdk#113 at 39e31d8) ---------------------

// hostRecorder notes every host a request was sent to, and answers none.
type hostRecorder struct {
	mu    sync.Mutex
	hosts []string
}

func (h *hostRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.hosts = append(h.hosts, request.URL.Host)
	h.mu.Unlock()
	return nil, errors.New("not sent")
}

// TestNoRequestLeavesTheAPIOrigin mirrors test_no_request_leaves_the_api_origin.
// This SDK appends a path to APIURL rather than joining it, so the
// reference's paths stay on the API's own host here; what can leave is a
// ControlPlane built by hand whose APIURL has no trailing slash.
func TestNoRequestLeavesTheAPIOrigin(t *testing.T) {
	for _, path := range []string{"/https://elsewhere.example/v1/x", "/elsewhere.example:443/v1/x", "https://elsewhere.example/v1/x"} {
		recorder := &hostRecorder{}
		plane := NewControlPlane("https://api.thalovant.com", "tvpat_must_stay_home")
		plane.HTTPClient = &http.Client{Transport: recorder}
		_, _ = plane.request(context.Background(), http.MethodGet, path, nil, nil, true)
		_, _, _, _ = plane.sendNoReplay(context.Background(), http.MethodPost, path, map[string]any{"code": "c"})
		recorder.mu.Lock()
		for _, host := range recorder.hosts {
			if host != "api.thalovant.com" {
				t.Errorf("%s reached %s", path, host)
			}
		}
		recorder.mu.Unlock()
	}
	recorder := &hostRecorder{}
	plane := &ControlPlane{APIURL: "https://api.thalovant.com", AccessToken: "tvpat_must_stay_home", HTTPClient: &http.Client{Transport: recorder}}
	_, err := plane.request(context.Background(), http.MethodGet, ".elsewhere.example/v1/x", nil, nil, true)
	var apiErr *APIError
	if !errors.Is(err, ErrAPI) || errors.As(err, &apiErr) || !strings.Contains(err.Error(), "origin") {
		t.Errorf("err = %v, want a no-status origin refusal", err)
	}
	if _, _, _, err := plane.sendNoReplay(context.Background(), http.MethodPost, ".elsewhere.example/v1/x", map[string]any{"code": "c"}); !strings.Contains(fmt.Sprint(err), "origin") {
		t.Errorf("sendNoReplay err = %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "tvpat_must_stay_home") {
		t.Error("the refusal names the token")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.hosts) != 0 {
		t.Errorf("requests were sent to %v", recorder.hosts)
	}
}

// TestTheExchangeNeverLeavesTheAPIOrigin mirrors
// test_the_exchange_never_leaves_the_api_origin: a configuration built past
// ParseNativeSignInConfig's own check is refused again before anything is sent.
func TestTheExchangeNeverLeavesTheAPIOrigin(t *testing.T) {
	recorder := &hostRecorder{}
	plane := &ControlPlane{APIURL: "https://api.thalovant.com", HTTPClient: &http.Client{Transport: recorder}}
	config := NativeSignInConfig{AuthorizationEndpoint: "https://dash.thalovant.com/authorize",
		TokenEndpoint: ".collector.example/v1/auth/native/token", SDKClientID: SDKClientID}
	_, err := plane.exchangeBrowserCode(context.Background(), config, "c", strings.Repeat("v", 43), "http://127.0.0.1:1/callback", "s")
	var apiErr *APIError
	if !strings.Contains(fmt.Sprint(err), "origin") || errors.As(err, &apiErr) {
		t.Errorf("err = %v, want a no-status origin refusal", err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.hosts) != 0 {
		t.Errorf("the exchange was sent to %v", recorder.hosts)
	}
}

// TestTheAcceptedTabWaitsAsLongAsTheSignInTakes mirrors
// test_the_accepted_tab_waits_as_long_as_the_sign_in_takes.
func TestTheAcceptedTabWaitsAsLongAsTheSignInTakes(t *testing.T) {
	listener := newLoopbackListener("the-state")
	port, err := listener.start()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.close()
	started := time.Now()
	tab := make(chan tabAnswer, 1)
	go func() { tab <- browserTab(t, port, "GET", "/callback?code=c&state=the-state", nil) }()
	if accepted, err := listener.wait(context.Background(), 5*time.Second); accepted == nil || err != nil {
		t.Fatalf("accepted %v, %v", accepted, err)
	}
	time.Sleep(300 * time.Millisecond)
	select {
	case answer := <-tab:
		t.Fatalf("the tab was answered before the sign-in ended: %q", answer.body)
	default:
	}
	listener.finish(true)
	answer := receive(t, tab, "the tab's page")
	if !strings.Contains(answer.body, loopbackSignedInText) || time.Since(started) < 300*time.Millisecond {
		t.Errorf("the tab got %q after %s", answer.body, time.Since(started))
	}
}

// TestClosingTheListenerAnswersATabThatIsStillWaiting mirrors
// test_closing_the_listener_answers_a_tab_that_is_still_waiting.
func TestClosingTheListenerAnswersATabThatIsStillWaiting(t *testing.T) {
	listener := newLoopbackListener("the-state")
	port, err := listener.start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listener.close) // the explicit close below is the one tested
	tab := make(chan tabAnswer, 1)
	go func() { tab <- browserTab(t, port, "GET", "/callback?code=c&state=the-state", nil) }()
	if accepted, _ := listener.wait(context.Background(), 5*time.Second); accepted == nil {
		t.Fatal("nothing was accepted")
	}
	listener.close()
	if answer := receive(t, tab, "the tab's page"); !strings.Contains(answer.body, loopbackNotCompletedText) {
		t.Errorf("the tab got %q", answer.body)
	}
	assertNoLoopbackGoroutines(t)
}

// TestAHeadRequestGetsTheHeadersAndNoBody mirrors
// test_a_head_request_gets_the_headers_and_no_body.
func TestAHeadRequestGetsTheHeadersAndNoBody(t *testing.T) {
	listener := newLoopbackListener("the-state")
	port, err := listener.start()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.close()
	answer := rawTab(t, port, []byte("HEAD /callback?code=c&state=the-state HTTP/1.1\r\n\r\n"), nil)
	if answer.status != 404 || answer.body != "" || answer.headers["Content-Type"] != "text/html; charset=utf-8" {
		t.Fatalf("HEAD got %d %v %q", answer.status, answer.headers, answer.body)
	}
	if want := strconv.Itoa(len(loopbackPage(loopbackNotFoundText))); answer.headers["Content-Length"] != want {
		t.Errorf("Content-Length %q, want %s (what a GET would have carried)", answer.headers["Content-Length"], want)
	}
}

// TestAMessageCutToLengthKeepsNoPartOfASecret mirrors
// test_a_message_cut_to_length_keeps_no_part_of_a_secret, at this SDK's
// 256-rune display limit.
func TestAMessageCutToLengthKeepsNoPartOfASecret(t *testing.T) {
	var mu sync.Mutex
	var verifier string
	api := newUnitBrowserAPI(t, func(body map[string]any) (int, any) {
		mu.Lock()
		defer mu.Unlock()
		verifier = fmt.Sprint(body["code_verifier"])
		// The verifier starts ten runes before the display line's cut.
		return 400, map[string]any{"detail": strings.Repeat("a", maxServerErrorDetail-10) + verifier}
	})
	plane := NewControlPlane(api.server.URL, "")
	open := false
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
	port, state, _ := urlParts(t, receive(t, opened, "the URL"))
	browserTab(t, port, "GET", "/callback?code=c&state="+state, nil)
	err := receive(t, done, "the sign-in").err
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	prefix := verifier[:10]
	mu.Unlock()
	problem, _ := json.Marshal(apiErr.Problem)
	for _, shown := range []string{err.Error(), fmt.Sprintf("%#v", err), apiErr.Detail, apiErr.ProblemDetail, string(problem)} {
		if strings.Contains(shown, prefix) {
			t.Errorf("%q keeps part of the verifier", shown)
		}
	}
	if want := strings.Repeat("a", maxServerErrorDetail-10) + redactedSecret; apiErr.Detail != want {
		t.Errorf("Detail = %q, want %q", apiErr.Detail, want)
	}
}

// TestScrubbingIsOnePassLongestFirstAndLeavesItsMarkerAlone mirrors
// test_scrubbing_is_one_pass_longest_first_and_leaves_its_marker_alone.
func TestScrubbingIsOnePassLongestFirstAndLeavesItsMarkerAlone(t *testing.T) {
	hidden := []string{"abc", "abcdef", "c"}
	if got := scrubText("xx-abc-abcdef-[redacted]", hidden); got != "xx-[redacted]-[redacted]-[redacted]" {
		t.Errorf("scrubText = %q", got)
	}
	once := scrubSecrets(map[string]any{"seen:abcdef": []any{"c", map[string]any{"deep": "abc"}}}, hidden)
	want := map[string]any{"seen:[redacted]": []any{"[redacted]", map[string]any{"deep": "[redacted]"}}}
	if fmt.Sprint(once) != fmt.Sprint(want) {
		t.Errorf("scrubSecrets = %v, want %v", once, want)
	}
	if again := scrubSecrets(once, hidden); fmt.Sprint(again) != fmt.Sprint(once) {
		t.Errorf("scrubbing twice changed %v into %v", once, again)
	}
	if got := scrubText("nothing hidden", nil); got != "nothing hidden" {
		t.Errorf("scrubText with nothing hidden = %q", got)
	}
}

// requestBodyError is a transport failure that keeps the request it failed.
type requestBodyError struct{ body string }

func (e *requestBodyError) Error() string { return "connection reset: " + e.body }

// TestATransportFailureDuringTheExchangeCarriesNothingOfTheRequest mirrors
// test_a_transport_failure_during_the_exchange_carries_nothing_of_the_request.
func TestATransportFailureDuringTheExchangeCarriesNothingOfTheRequest(t *testing.T) {
	config, _ := json.Marshal(unitNativeConfig)
	plane := NewControlPlane("http://127.0.0.1:9", "")
	plane.HTTPClient = &http.Client{Transport: browserTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/auth/native/config" {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(string(config))), Request: r}, nil
		}
		raw, _ := io.ReadAll(r.Body)
		return nil, &requestBodyError{body: string(raw)}
	})}
	open := false
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
	port, state, _ := urlParts(t, receive(t, opened, "the URL"))
	browserTab(t, port, "GET", "/callback?code=transport-code&state="+state, nil)
	err := receive(t, done, "the sign-in").err
	var (
		apiErr    *APIError
		transport *requestBodyError
	)
	if err == nil || errors.As(err, &apiErr) || errors.As(err, &transport) {
		t.Fatalf("err = %#v, want a no-status error with no cause", err)
	}
	for form, text := range displayForms(err) {
		for _, secret := range []string{"transport-code", "code_verifier", state} {
			if strings.Contains(text, secret) {
				t.Errorf("%s carries %q", form, secret)
			}
		}
	}
}

// sendWhileReading sends request while reading the answer, as a browser
// does, and says how the connection ended.
func sendWhileReading(t *testing.T, port int, request []byte) ([]byte, error) {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		_, _ = conn.Write(request)
	}()
	data, readErr := io.ReadAll(conn)
	<-sent
	return data, readErr
}

// TestAnEarlyAnswerIsNotLostToAReset mirrors
// test_an_early_answer_is_not_lost_to_a_reset.
func TestAnEarlyAnswerIsNotLostToAReset(t *testing.T) {
	for name, request := range map[string][]byte{
		// A head far beyond the listener's limit: most of it is never read.
		"oversized head": []byte("GET /callback?code=" + strings.Repeat("a", 600_000) + " HTTP/1.1\r\nHost: x\r\n\r\n"),
		// A body nobody asked for, behind a 404.
		"unread body": []byte("POST /callback HTTP/1.1\r\nHost: x\r\nContent-Length: 600000\r\n\r\n" + strings.Repeat("b", 600_000)),
	} {
		t.Run(name, func(t *testing.T) {
			listener := newLoopbackListener("the-state")
			port, err := listener.start()
			if err != nil {
				t.Fatal(err)
			}
			defer listener.close()
			data, err := sendWhileReading(t, port, request)
			line, _, _ := strings.Cut(string(data), "\r\n")
			if line != "HTTP/1.1 400 Bad Request" && line != "HTTP/1.1 404 Not Found" {
				t.Errorf("the answer was %q", line)
			}
			if err != nil {
				t.Errorf("the connection ended with %v", err)
			}
		})
	}
}

// TestDrainingAConnectionIsBounded mirrors test_draining_a_connection_is_bounded.
func TestDrainingAConnectionIsBounded(t *testing.T) {
	original := loopbackDrainTimeout
	loopbackDrainTimeout = 200 * time.Millisecond
	t.Cleanup(func() { loopbackDrainTimeout = original })
	listener := newLoopbackListener("the-state")
	port, err := listener.start()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.close()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /favicon.ico HTTP/1.1\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	// The answer ends: our side is shut.
	if raw, _ := io.ReadAll(conn); !strings.HasPrefix(string(raw), "HTTP/1.1 404 ") {
		t.Fatalf("got %q", raw)
	}
	time.Sleep(500 * time.Millisecond) // ours stays open
	listener.mu.Lock()
	left := len(listener.conns)
	listener.mu.Unlock()
	if left != 0 {
		t.Errorf("%d connections still drained past the bound", left)
	}
}

// -- second review of #90 ------------------------------------------------------------

func TestClosingTheListenerTwiceIsHarmless(t *testing.T) {
	listener := newLoopbackListener("the-state")
	if _, err := listener.start(); err != nil {
		t.Fatal(err)
	}
	listener.close()
	listener.close()
	assertNoLoopbackGoroutines(t)
}

// refusedExchangeError signs in against an API whose exchange answers 400
// with answer (built from the request body), and returns the error.
func refusedExchangeError(t *testing.T, code string, answer func(body map[string]any) any) error {
	t.Helper()
	api := newUnitBrowserAPI(t, func(body map[string]any) (int, any) { return 400, answer(body) })
	plane := NewControlPlane(api.server.URL, "")
	open := false
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
	port, state, _ := urlParts(t, receive(t, opened, "the URL"))
	browserTab(t, port, "GET", "/callback?code="+code+"&state="+state, nil)
	return receive(t, done, "the sign-in").err
}

// shownForms is every place an APIError's text can be read.
func shownForms(t *testing.T, err error) map[string]string {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	problem, _ := json.Marshal(apiErr.Problem)
	shown := displayForms(err)
	shown["%#v"] = fmt.Sprintf("%#v", err)
	shown["Problem"] = string(problem)
	shown["ProblemDetail"] = apiErr.ProblemDetail
	shown["Code"] = apiErr.Code
	return shown
}

func TestATokenThatCarriesTheMarkerIsStillRedactedWhole(t *testing.T) {
	const token = "tvpat_left[redacted]right_tail"
	err := refusedExchangeError(t, "c", func(map[string]any) any {
		return map[string]any{"detail": "invalid_grant " + token, "access_token": token, "nested": []any{token}}
	})
	for form, text := range shownForms(t, err) {
		for _, part := range []string{"tvpat_left", "right_tail"} {
			if strings.Contains(text, part) {
				t.Errorf("%s keeps %q of the token", form, part)
			}
		}
	}
	// One pass still leaves a marker that is already there alone.
	hidden := []string{token, "c"}
	if once := scrubText("x "+token+" [redacted]", hidden); once != "x [redacted] [redacted]" || scrubText(once, hidden) != once {
		t.Errorf("scrubText = %q", once)
	}
}

func TestALargeOrTokenStuffedAnswerIsRedactedOrOmitted(t *testing.T) {
	// A token of 700 KB, under the size bound: redacted whole, quickly.
	big := "tvpat_" + strings.Repeat("x", 700_000)
	started := time.Now()
	err := refusedExchangeError(t, "c", func(map[string]any) any {
		return map[string]any{"detail": "invalid_grant", "access_token": big, "echo": big}
	})
	for form, text := range shownForms(t, err) {
		if strings.Contains(text, "tvpat_xxxx") {
			t.Errorf("%s keeps the large token", form)
		}
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("redacting a large token took %s", took)
	}
	// As many distinct tokens as the bound allows: every one redacted.
	err = refusedExchangeError(t, "c", func(map[string]any) any {
		items := []any{}
		for index := range maxRefusedAnswerTokens {
			items = append(items, map[string]any{"access_token": fmt.Sprintf("tvpat_many_%03d", index)})
		}
		return map[string]any{"detail": "invalid_grant", "items": items}
	})
	for form, text := range shownForms(t, err) {
		if strings.Contains(text, "tvpat_many_") {
			t.Errorf("%s keeps one of the tokens", form)
		}
	}
	// Past either bound the answer is not echoed at all.
	for name, answer := range map[string]func(map[string]any) any{
		"too many tokens": func(map[string]any) any {
			items := []any{}
			for index := range maxRefusedAnswerTokens + 1 {
				items = append(items, map[string]any{"access_token": fmt.Sprintf("tvpat_many_%03d", index)})
			}
			return map[string]any{"detail": "invalid_grant", "items": items}
		},
		"too large": func(map[string]any) any {
			return map[string]any{"detail": "invalid_grant", "padding": strings.Repeat("p", maxRefusedAnswerBytes)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := refusedExchangeError(t, "c", answer)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || apiErr.Problem != nil || apiErr.Detail != serverErrorOmitted {
				t.Errorf("err = %#v, want a 400 whose answer is omitted", err)
			}
			if strings.Contains(fmt.Sprint(err), "tvpat_many_") {
				t.Error("a token reached the error")
			}
		})
	}
}

func TestAnOversizedHeadRequestGetsHeadersAndNoBody(t *testing.T) {
	listener := newLoopbackListener("the-state")
	port, err := listener.start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listener.close)
	request := []byte("HEAD /callback?code=" + strings.Repeat("a", loopbackMaxRequestHead+10) + " HTTP/1.1\r\n\r\n")
	data, err := sendWhileReading(t, port, request)
	if err != nil {
		t.Fatalf("the connection ended with %v", err)
	}
	answer := parseTabAnswer(t, data)
	if answer.status != 400 || answer.body != "" || answer.headers["Content-Type"] != "text/html; charset=utf-8" {
		t.Errorf("an oversized HEAD got %d %v %q", answer.status, answer.headers, answer.body)
	}
	if want := strconv.Itoa(len(loopbackPage(loopbackWrongStateText))); answer.headers["Content-Length"] != want {
		t.Errorf("Content-Length %q, want %s", answer.headers["Content-Length"], want)
	}
	// An oversized GET still carries its page.
	data, _ = sendWhileReading(t, port, []byte("GET /callback?code="+strings.Repeat("a", loopbackMaxRequestHead+10)+" HTTP/1.1\r\n\r\n"))
	if answer := parseTabAnswer(t, data); answer.status != 400 || !strings.Contains(answer.body, loopbackWrongStateText) {
		t.Errorf("an oversized GET got %d %q", answer.status, answer.body)
	}
}

func TestADeniedCallbackWithAnotherErrorFailsAsAccessDenied(t *testing.T) {
	status, callback := classifyRequest("GET", "/callback?error=access_denied&error=server_error&state=s", "s")
	if status != 200 || callback.outcome != callbackFailed || callback.errorParam != "access_denied" {
		t.Fatalf("classifyRequest = %d %+v", status, callback)
	}
	api := newUnitBrowserAPI(t, nil)
	plane := NewControlPlane(api.server.URL, "")
	open := false
	opened, done := startSignIn(context.Background(), plane, BrowserLoginOptions{OpenBrowser: &open})
	port, state, _ := urlParts(t, receive(t, opened, "the URL"))
	browserTab(t, port, "GET", "/callback?error=access_denied&error=server_error&state="+state, nil)
	err := receive(t, done, "the sign-in").err
	var failed *BrowserLoginFailedError
	if !errors.As(err, &failed) || failed.Code != "access_denied" || errors.Is(err, ErrDeviceAccessDenied) {
		t.Errorf("err = %v, want a failure with Code access_denied, not a denial", err)
	}
}
