package thalovant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Browser sign-in: the authorization code grant with PKCE (RFC 7636, S256
// only) on a loopback redirect (RFC 8252 section 7.3), for a program on a
// machine with a browser.
//
// A device code can be read off one screen and typed in anywhere, which is
// what device-code phishing is. With a loopback redirect the browser that
// approves hands the code to its own computer's 127.0.0.1, so a link or code
// sent to someone on another computer delivers nothing to the sender. That is
// all it promises: a forwarded callback port (ssh -L, a port-forwarding tool)
// or another user of the same computer can still complete a sign-in.
// BrowserLogin signs a program in that way, and InteractiveLogin uses it
// whenever the machine can show a browser, with device login (LoginWithBrowser)
// as the fallback for one that cannot.
//
// The behaviour is the shared contract in browser-login-vectors.json, whose
// reference is the Python SDK's browser_login and interactive_login.

const (
	// SDKClientID is the SDKs' own registered app, which a program signs in
	// as through the browser. GET /v1/auth/native/config names it as
	// sdk_client_id, and BrowserLogin uses what the API names.
	SDKClientID = "thalovant-sdk"
	// LoopbackCallbackPath is the one path the loopback listener answers.
	LoopbackCallbackPath = "/callback"
	// DefaultBrowserLoginTimeout bounds how long BrowserLogin waits for the
	// browser to come back.
	DefaultBrowserLoginTimeout = 300 * time.Second
	// MaxDeviceNameLength is the longest device name the consent page shows
	// and the token is named by, in Unicode code points. The API refuses a
	// longer one, so BrowserLogin cuts it to this length.
	MaxDeviceNameLength = 120

	// sdkDeviceLabel is the name this SDK gives itself in DefaultDeviceName.
	sdkDeviceLabel = "thalovant-go-sdk"
)

// LoginMethod is how InteractiveLogin signs a person in.
type LoginMethod string

const (
	// LoginMethodAuto signs in through the browser when this machine can show
	// one (CanOpenBrowser) and the API offers browser sign-in, and with a
	// device login otherwise. It is what an empty LoginMethod means.
	LoginMethodAuto LoginMethod = "auto"
	// LoginMethodBrowser always signs in through the browser (BrowserLogin),
	// whatever the machine. An API without browser sign-in is an error; it
	// never becomes a device login.
	LoginMethodBrowser LoginMethod = "browser"
	// LoginMethodDevice always signs in with a device login
	// (LoginWithBrowser), and never asks for the browser sign-in
	// configuration.
	LoginMethodDevice LoginMethod = "device"
)

// BrowserLoginOptions carries the optional inputs of BrowserLogin.
type BrowserLoginOptions struct {
	// Scopes are what the token will carry. Empty strings are dropped; none
	// left (nil or empty) asks for the API's default, hubs:read and
	// clients:write. Any non-admin scope may be asked for; the API refuses
	// admin or unknown scopes when the person approves.
	Scopes []string
	// DeviceName is what the consent page and the token show. Only nil asks
	// for DefaultDeviceName ("thalovant-go-sdk on <hostname>"); a pointer to
	// "" leaves device_name out, which keeps the hostname off the consent
	// page. It is cut to MaxDeviceNameLength code points. Go 1.26's
	// new("name") makes the pointer.
	DeviceName *string
	// OpenBrowser defaults to true when nil. False only shows the URL.
	OpenBrowser *bool
	// Timeout bounds the wait for the browser to come back, from the moment
	// the URL is shown; DefaultBrowserLoginTimeout when zero or negative.
	Timeout time.Duration
	// Prompt, when set, receives the authorization URL in place of the
	// message printed to stdout: nothing is printed, so show the URL there.
	// It does not stop the browser from opening. It is called on the calling
	// goroutine, as the printed message is written (and as the Python
	// reference calls its prompt), so a panic in it reaches the caller. The
	// listener already accepts callbacks while it runs; the timeout counts
	// from when it returns, so it should return once the URL is shown. A ctx
	// done by then ends the sign-in before the browser is opened.
	Prompt func(url string)
	// Opener opens the URL in place of the platform's browser (open on
	// macOS, url.dll's FileProtocolHandler on Windows, xdg-open elsewhere). It
	// is called on a goroutine of its own, since a console browser can hold
	// the call until it exits, and BrowserLogin does not wait for it to
	// return. Opening can fail without a word; the URL is always shown too.
	Opener func(url string)
}

// InteractiveLoginOptions carries the optional inputs of InteractiveLogin.
type InteractiveLoginOptions struct {
	// Method picks the sign-in; "" is LoginMethodAuto.
	Method LoginMethod
	// Scopes are what the token will carry, for either method; none asks for
	// the API's default.
	Scopes []string
	// DeviceName names this machine on the consent page: BrowserLogin's
	// DeviceName, or a device login's ClientName. Only nil asks for
	// DefaultDeviceName; a pointer to "" sends none to either method.
	DeviceName *string
	// OpenBrowser defaults to true when nil, for either method.
	OpenBrowser *bool
	// Timeout bounds the wait; zero or negative uses each method's own
	// default: DefaultBrowserLoginTimeout for the browser,
	// DefaultDeviceLoginTimeout for a device login.
	Timeout time.Duration
	// Opener opens a URL in place of the platform's browser, for either
	// method: the consent page, or a device login's verification page. See
	// BrowserLoginOptions.Opener.
	Opener func(url string)
}

// NativeSignInConfig is where a program signs in through the browser, as GET
// /v1/auth/native/config says. The API reads it from its own settings, so a
// staging or self-hosted API sends the browser to its own dashboard.
type NativeSignInConfig struct {
	// AuthorizationEndpoint is the dashboard page the browser opens.
	AuthorizationEndpoint string
	// TokenEndpoint is the path on the API the code is exchanged at.
	TokenEndpoint string
	// SDKClientID is the registered app the program signs in as.
	SDKClientID string
	// DeviceAuthorizationEndpoint is the device login's path, when the API
	// named one; "" otherwise. BrowserLogin does not read it.
	DeviceAuthorizationEndpoint string
}

// BrowserAuthorization is what BrowserAuthorizationURL puts in the address
// a browser sign-in opens.
type BrowserAuthorization struct {
	// ClientID is the configuration's SDKClientID; "" uses SDKClientID.
	ClientID string
	// RedirectURI is LoopbackRedirectURI of the port the listener bound.
	RedirectURI string
	// CodeChallenge is ChallengeFor(verifier).
	CodeChallenge string
	// State is NewState's value for this sign-in.
	State string
	// Scopes: empty strings are dropped, the rest joined by one space, and
	// scope is left out when nothing is left.
	Scopes []string
	// DeviceName is cut to MaxDeviceNameLength code points and left out when
	// empty.
	DeviceName string
}

// BrowserLoginFailedError is a browser sign-in the browser came back from
// without a code: the consent page named an error other than access_denied,
// or sent no single, non-empty code. It matches ErrAPI. The API was not
// asked, so there is no HTTP status.
type BrowserLoginFailedError struct {
	// Code is the error the consent page named when it is one of the codes
	// RFC 6749 section 4.1.2.1 defines (invalid_request, unauthorized_client,
	// unsupported_response_type, invalid_scope, server_error,
	// temporarily_unavailable); "" for anything else, which could be the
	// state or a code and is never repeated.
	Code string
}

func (e *BrowserLoginFailedError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%v: the browser came back without a sign-in code (%s)", ErrAPI, e.Code)
	}
	return fmt.Sprintf("%v: the browser came back without a sign-in code", ErrAPI)
}

// Unwrap makes a BrowserLoginFailedError match ErrAPI.
func (e *BrowserLoginFailedError) Unwrap() error { return ErrAPI }

// browserDeniedError is a sign-in the person refused on the consent page,
// told in a browser sign-in's own words. errors.As reaches the
// *DeviceLoginDeniedError inside it, whose APIError is nil since the API was
// not asked, and errors.Is matches ErrDeviceAccessDenied and ErrAPI.
type browserDeniedError struct {
	denied *DeviceLoginDeniedError
}

func (e *browserDeniedError) Error() string {
	return fmt.Sprintf("%v: the sign-in request was denied in the browser", ErrDeviceAccessDenied)
}

func (e *browserDeniedError) Unwrap() error { return e.denied }

// browserExpiredError is a browser sign-in nobody completed in time.
// errors.As reaches the *DeviceLoginExpiredError inside it (APIError nil),
// and errors.Is matches ErrDeviceCodeExpired and ErrAPI.
type browserExpiredError struct {
	wait    time.Duration
	expired *DeviceLoginExpiredError
}

func (e *browserExpiredError) Error() string {
	return fmt.Sprintf("%v: the browser sign-in was not completed within %s; call BrowserLogin again to start a new one",
		ErrDeviceCodeExpired, e.wait)
}

func (e *browserExpiredError) Unwrap() error { return e.expired }

// BrowserLogin signs in through the browser and stores the API token.
//
// It reads GET /v1/auth/native/config for where to send the browser, listens
// on 127.0.0.1 on a port the system picks, shows the consent page's URL
// (printed as "Opening your browser to sign in to Thalovant. If it does not
// open, visit:" and the URL, or given to opts.Prompt), opens the browser
// unless opts.OpenBrowser is false, and waits for the browser to come back to
// http://127.0.0.1:<port>/callback. A tab that answers with another sign-in's
// state is turned away (400) and the wait goes on. The code is then
// exchanged, once, with its PKCE verifier, and the accepted tab is told how
// the sign-in ended.
//
// It returns the token answer and keeps its access_token and token_id on
// this ControlPlane exactly as a device login does (AccessToken, TokenID), so
// RevokeAPIToken revokes it the same way. Errors:
//
//   - denied on the consent page: errors.Is ErrDeviceAccessDenied
//     (a *DeviceLoginDeniedError, APIError nil);
//   - no answer within opts.Timeout: errors.Is ErrDeviceCodeExpired
//     (a *DeviceLoginExpiredError, APIError nil);
//   - another error from the consent page, or no code: a
//     *BrowserLoginFailedError;
//   - a code the API will not exchange: an *APIError (400, detail
//     invalid_grant); a token answer that is not a JSON object with an
//     access_token is an ErrAPI error with no status;
//   - an API without browser sign-in answers the configuration 404, an
//     *APIError; InteractiveLogin falls back to a device login for it;
//   - ctx done while waiting, or by the time Prompt returns (the browser is
//     then not opened): ctx.Err().
//
// The API URL must be https, or http on a loopback address while developing:
// a code and its verifier never cross the network in cleartext, so anything
// else is refused before anything is asked or opened. Neither the code, the
// verifier, the state nor a token appears in an error. The exchange is sent
// at most once: its body is never replayed by the HTTP transport, since a
// code presented twice revokes the token the first exchange minted.
//
// When BrowserLogin returns, the port is closed and every goroutine the
// loopback listener ran (its accept loop and one per connection) has
// returned. Two may still be running, neither of them the listener's: a
// caller's own Opener call, which runs on its own goroutine and is not
// waited for, and, with no Opener, the platform launcher's wait for the
// process it started (open, xdg-open or rundll32), which ends when that
// process exits, as it always has for device login.
func (c *ControlPlane) BrowserLogin(ctx context.Context, opts BrowserLoginOptions) (map[string]any, error) {
	// Before anything is asked or opened.
	if err := requireSecureTokenExchange(c.APIURL); err != nil {
		return nil, err
	}
	config, err := c.nativeSignInConfig(ctx)
	if err != nil {
		return nil, err
	}
	return c.browserLogin(ctx, config, opts)
}

// InteractiveLogin signs a person in the best way this machine allows, and
// stores the API token.
//
// LoginMethodAuto (the default) signs in through the browser (BrowserLogin)
// when this machine can show one (CanOpenBrowser: never in CI or over SSH; on
// Linux and the BSDs only with a display) and the API answers GET
// /v1/auth/native/config with a usable configuration. Otherwise it uses a
// device login (LoginWithBrowser), whose code can be approved from any
// browser: a 404, any other failure, an unusable answer and a cleartext API
// URL all count, and nothing has been opened yet, so that is not a downgrade.
// LoginMethodBrowser and LoginMethodDevice force one. Once the browser has
// been sent to the consent page, the browser sign-in's own outcome stands,
// whether denied, expired or a refused exchange: it never turns into a
// device login.
//
// It returns the token answer, from either method, and keeps the token as
// both do. An unknown Method is an ErrAPI error, before anything is sent.
func (c *ControlPlane) InteractiveLogin(ctx context.Context, opts InteractiveLoginOptions) (map[string]any, error) {
	method := opts.Method
	if method == "" {
		method = LoginMethodAuto
	}
	switch method {
	case LoginMethodAuto, LoginMethodBrowser, LoginMethodDevice:
	default:
		return nil, fmt.Errorf("%w: Method must be LoginMethodAuto, LoginMethodBrowser or LoginMethodDevice, not %q", ErrAPI, method)
	}
	// Only a missing name gets the default: an empty one is the caller's way
	// of sending none, to the consent page or to a device login.
	name := DefaultDeviceName()
	if opts.DeviceName != nil {
		name = *opts.DeviceName
	}
	var config *NativeSignInConfig
	switch {
	case method == LoginMethodBrowser:
		if err := requireSecureTokenExchange(c.APIURL); err != nil {
			return nil, err
		}
		found, err := c.nativeSignInConfig(ctx)
		if err != nil {
			return nil, err
		}
		config = &found
	case method == LoginMethodAuto && CanOpenBrowser(nil, ""):
		if requireSecureTokenExchange(c.APIURL) == nil {
			found, err := c.nativeSignInConfig(ctx)
			switch {
			case err == nil:
				config = &found
			case ctx.Err() != nil:
				// Cancelled, not unusable: a device login would fail the
				// same way.
				return nil, err
			}
			// An older API (404), or one that cannot say: the browser has
			// not been opened, so a device login is not a downgrade.
		}
	}
	if config == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = DefaultDeviceLoginTimeout
		}
		return c.loginWithDevice(ctx, DeviceLoginOptions{
			Scopes: opts.Scopes,
			// "" is left out of the device authorization request.
			ClientName:  name,
			OpenBrowser: opts.OpenBrowser,
			Timeout:     timeout,
		}, opts.Opener)
	}
	return c.browserLogin(ctx, *config, BrowserLoginOptions{
		Scopes:      opts.Scopes,
		DeviceName:  &name,
		OpenBrowser: opts.OpenBrowser,
		Timeout:     opts.Timeout,
		Opener:      opts.Opener,
	})
}

// browserLogin is one browser sign-in against config: listen, show and open
// the URL, wait, exchange, and answer the tab.
func (c *ControlPlane) browserLogin(ctx context.Context, config NativeSignInConfig, opts BrowserLoginOptions) (map[string]any, error) {
	if err := requireSecureTokenExchange(c.APIURL); err != nil {
		return nil, err
	}
	verifier, err := NewVerifier()
	if err != nil {
		return nil, err
	}
	state, err := NewState()
	if err != nil {
		return nil, err
	}
	listener := newLoopbackListener(state)
	// Bound before the URL is built: the redirect names the port.
	port, err := listener.start()
	if err != nil {
		return nil, err
	}
	// On every path, a panicking Prompt included: stop listening, give an
	// accepted tab its page, close every connection, and wait for every
	// goroutine the listener runs.
	defer listener.close()
	redirectURI, err := LoopbackRedirectURI(port)
	if err != nil {
		return nil, err
	}
	deviceName := DefaultDeviceName()
	if opts.DeviceName != nil {
		deviceName = *opts.DeviceName
	}
	authorizationURL := BrowserAuthorizationURL(config.AuthorizationEndpoint, BrowserAuthorization{
		ClientID:      config.SDKClientID,
		RedirectURI:   redirectURI,
		CodeChallenge: ChallengeFor(verifier),
		State:         state,
		Scopes:        opts.Scopes,
		DeviceName:    deviceName,
	})
	if err := presentBrowserLogin(ctx, authorizationURL, opts.Prompt, opts.OpenBrowser == nil || *opts.OpenBrowser, opts.Opener); err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultBrowserLoginTimeout
	}
	callback, err := listener.wait(ctx, timeout)
	if err != nil {
		return nil, err
	}
	if callback == nil {
		return nil, &browserExpiredError{wait: timeout, expired: &DeviceLoginExpiredError{}}
	}
	switch callback.outcome {
	case callbackDenied:
		return nil, &browserDeniedError{denied: &DeviceLoginDeniedError{}}
	case callbackFailed:
		// errorParam is one of RFC 6749's codes or nothing (classifyRequest):
		// a value the page chose could be the state or a code.
		return nil, &BrowserLoginFailedError{Code: callback.errorParam}
	}
	// A code is single-use and lives 60 seconds: a failed exchange is never
	// retried, since presenting a code twice revokes the token the first
	// exchange minted (RFC 9700).
	token, err := c.exchangeBrowserCode(ctx, config, callback.code, verifier, redirectURI, state)
	if err != nil {
		return nil, err
	}
	listener.finish(true)
	return token, nil
}

// presentBrowserLogin shows the URL, since opening a browser can fail
// without a word, and opens it. A ctx done by the time the URL is shown -- a
// Prompt can take a while, and can cancel the sign-in itself -- ends the
// sign-in there, so no browser is sent to a consent page whose callback is
// already closing.
func presentBrowserLogin(ctx context.Context, target string, prompt func(string), open bool, opener func(string)) error {
	switch {
	case prompt != nil:
		prompt(target)
	case open:
		fmt.Printf("Opening your browser to sign in to Thalovant. If it does not open, visit:\n%s\n", target)
	default:
		fmt.Printf("To sign in to Thalovant, visit:\n%s\n", target)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if open {
		openWith(opener, target)
	}
	return nil
}

// nativeSignInConfig reads GET /v1/auth/native/config: where to send the
// browser, and where to exchange the code.
func (c *ControlPlane) nativeSignInConfig(ctx context.Context) (NativeSignInConfig, error) {
	status, raw, header, err := c.send(ctx, http.MethodGet, "/v1/auth/native/config", nil, nil, false)
	if err != nil {
		return NativeSignInConfig{}, err
	}
	if status == http.StatusNotFound {
		// The API's own fields stay as they were sent; only the display line
		// says what the 404 means.
		apiErr := apiErrorFromResponse(status, raw, header)
		apiErr.Detail = "this Thalovant API does not offer browser sign-in; sign in with a device login instead"
		return NativeSignInConfig{}, apiErr
	}
	if status < 200 || status > 299 {
		return NativeSignInConfig{}, apiErrorFromResponse(status, raw, header)
	}
	// A 2xx this SDK cannot use is an error with no status, as a token
	// answer's is: the API did not refuse.
	payload, decodeErr := decodeControlJSON(raw)
	if decodeErr != nil {
		return NativeSignInConfig{}, fmt.Errorf("%w: the API answered an unusable browser sign-in configuration: it is not a JSON object", ErrAPI)
	}
	return ParseNativeSignInConfig(payload)
}

// ParseNativeSignInConfig reads GET /v1/auth/native/config's answer and
// refuses one a browser sign-in cannot safely use, with an ErrAPI error:
//
//   - an authorization_endpoint that is not https (or http on localhost,
//     127.0.0.1 or ::1), has no host, carries credentials, a query (or a bare
//     ?), a fragment, whitespace or control characters, or a port no browser
//     can open (0, or above 65535);
//   - a token_endpoint that is not a path on the API: one leading /, not //,
//     no ?, #, \ or whitespace, and no malformed percent escape (a request
//     to it could not be built after the person approved);
//   - an sdk_client_id that is not 1-64 of A-Z a-z 0-9 . _ -;
//   - code_challenge_methods_supported that is not a list holding "S256"
//     ([]any as decoded from JSON, or []string from a caller).
//
// A nil payload is not an object. Other members are ignored, and
// loopback_redirect_uri is never used: the redirect is LoopbackRedirectURI.
func ParseNativeSignInConfig(payload map[string]any) (NativeSignInConfig, error) {
	unusable := func(why string) (NativeSignInConfig, error) {
		return NativeSignInConfig{}, fmt.Errorf("%w: the API answered an unusable browser sign-in configuration: %s", ErrAPI, why)
	}
	if payload == nil {
		return unusable("it is not a JSON object")
	}
	authorization, _ := payload["authorization_endpoint"].(string)
	if !usableAuthorizationEndpoint(authorization) {
		return unusable("no usable authorization_endpoint")
	}
	token, _ := payload["token_endpoint"].(string)
	if !usableTokenEndpoint(token) {
		return unusable("no usable token_endpoint")
	}
	clientID, _ := payload["sdk_client_id"].(string)
	if !sdkClientIDPattern.MatchString(clientID) {
		return unusable("no usable sdk_client_id")
	}
	offersS256 := false
	// anySlice reads a decoded []any and a caller's []string alike; anything
	// that is not a list, "S256" on its own included, reads as none.
	for _, method := range anySlice(payload["code_challenge_methods_supported"]) {
		if method == "S256" {
			offersS256 = true
		}
	}
	if !offersS256 {
		return unusable("it does not offer S256")
	}
	device, _ := payload["device_authorization_endpoint"].(string)
	return NativeSignInConfig{
		AuthorizationEndpoint:       authorization,
		TokenEndpoint:               token,
		SDKClientID:                 clientID,
		DeviceAuthorizationEndpoint: device,
	}, nil
}

var sdkClientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// unsafeInURL reports a whitespace or control character, C1 included.
func unsafeInURL(r rune) bool {
	return unicode.IsSpace(r) || r < 32 || (r >= 127 && r <= 159)
}

// usableAuthorizationEndpoint: https, or http on a loopback host while
// developing; a host a browser can open (dashboardHost); no credentials,
// query, fragment, whitespace or control characters; a port from 1 to 65535
// when one is given.
func usableAuthorizationEndpoint(value string) bool {
	if value == "" || strings.IndexFunc(value, unsafeInURL) >= 0 || strings.ContainsAny(value, "?#") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.Opaque != "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || !validPort(parsed.Port()) || !dashboardHost(value) {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return true
	case "http":
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

// hostLabel is one DNS label as a URL host carries it: ASCII letters, digits
// and inner hyphens, 1-63 characters.
var hostLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// dashboardHost reports whether the host of value, as written (url.Parse
// would decode a %XX in it), names something a browser can reach: an IPv6
// address between brackets; an IPv4 address, which a host whose last label
// is a number has to be; or a DNS name of hostLabel labels (an international
// name in its xn-- form) of at most 253 characters. "%", "_", an empty label
// and anything else that does not parse to a hostname are refused, as the
// reference's _real_host refuses them.
func dashboardHost(value string) bool {
	_, rest, found := strings.Cut(value, "://")
	if !found {
		return false
	}
	authority, _, _ := strings.Cut(rest, "/")
	if strings.ContainsAny(authority, "@%") {
		return false
	}
	if strings.HasPrefix(authority, "[") {
		end := strings.Index(authority, "]")
		if end < 0 {
			return false
		}
		address, err := netip.ParseAddr(authority[1:end])
		return err == nil && address.Is6()
	}
	host := authority
	if colon := strings.LastIndex(host, ":"); colon >= 0 {
		host = host[:colon]
	}
	host = strings.ToLower(host)
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Is4()
	}
	labels := strings.Split(host, ".")
	if last := labels[len(labels)-1]; last != "" && strings.Trim(last, "0123456789") == "" {
		return false // numeric like an IPv4 address, and not one
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range labels {
		if !hostLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// validPort is "" or a decimal port from 1 to 65535, written in ASCII digits
// (leading zeros allowed, as the reference reads one). Port 0 is not one a
// browser can open, so an API naming it has no usable dashboard and
// LoginMethodAuto signs in with a device login instead of waiting out a
// consent page that never loads.
func validPort(port string) bool {
	if port == "" {
		return true
	}
	number := 0
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return false
		}
		if number = number*10 + int(digit-'0'); number > 65535 {
			return false
		}
	}
	return number >= 1
}

// usableTokenEndpoint: a path on the API, with one leading slash, every "%"
// followed by two hexadecimal digits, and no colon, query, fragment,
// backslash, whitespace or control character.
//
// No colon, because what follows the slash could read as a URL of its own
// ("/https://collector.example/token", "/collector.example:443/token") to a
// URL joiner, and the exchange carries the code and the verifier; this SDK
// appends the path instead, and refuses any request off the API's origin
// besides. A broken escape ("/v1/%zz") cannot be sent as written; better
// found out now, while a device login can still stand in, than after
// somebody approved.
func usableTokenEndpoint(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return false
	}
	if strings.IndexFunc(value, func(r rune) bool { return strings.ContainsRune(`:?#\`, r) || unsafeInURL(r) }) >= 0 {
		return false
	}
	return wholeEscapes(value)
}

// wholeEscapes reports whether every "%" in text is followed by two
// hexadecimal digits.
func wholeEscapes(text string) bool {
	for index := 0; index < len(text); index++ {
		if text[index] != '%' {
			continue
		}
		if index+2 >= len(text) {
			return false
		}
		if _, ok := hexValue(text[index+1]); !ok {
			return false
		}
		if _, ok := hexValue(text[index+2]); !ok {
			return false
		}
	}
	return true
}

// NewState returns an OAuth state: 24 random bytes (192 bits) from the
// operating system, base64url without padding (32 characters). It ties the
// answer the browser brings back to the request that sent it.
func NewState() (string, error) { return randomURLSafe(24) }

// LoopbackRedirectURI is http://127.0.0.1:<port>/callback, the one redirect a
// browser sign-in is answered on: the IP literal, never localhost, which a
// hosts file or a resolver can send elsewhere; no query and no trailing
// slash. The API matches it exactly and only for a port from 1024 to 65535,
// which an ephemeral port always is; any other port is an ErrAPI error.
func LoopbackRedirectURI(port int) (string, error) {
	if port < 1024 || port > 65535 {
		return "", fmt.Errorf("%w: a loopback redirect needs a port from 1024 to 65535, not %d", ErrAPI, port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, LoopbackCallbackPath), nil
}

// BrowserAuthorizationURL is the address a browser sign-in opens:
// {authorizationEndpoint}?response_type=code&client_id=&redirect_uri=
// &code_challenge=&code_challenge_method=S256&state=&scope=&device_name=, in
// that order, every value percent-encoded as RFC 3986 has it (A-Z a-z 0-9 -
// . _ ~ as they are, every other UTF-8 byte as %XX, a space as %20). scope is
// left out when no non-empty scope is asked for, and the API then grants its
// default (hubs:read clients:write); device_name is cut to
// MaxDeviceNameLength code points and left out when empty.
func BrowserAuthorizationURL(authorizationEndpoint string, a BrowserAuthorization) string {
	clientID := a.ClientID
	if clientID == "" {
		clientID = SDKClientID
	}
	parameters := [][2]string{
		{"response_type", "code"},
		{"client_id", clientID},
		{"redirect_uri", a.RedirectURI},
		{"code_challenge", a.CodeChallenge},
		// S256 only: plain puts the verifier in the browser.
		{"code_challenge_method", "S256"},
		{"state", a.State},
	}
	scopes := make([]string, 0, len(a.Scopes))
	for _, scope := range a.Scopes {
		if scope != "" {
			scopes = append(scopes, scope)
		}
	}
	if len(scopes) > 0 {
		parameters = append(parameters, [2]string{"scope", strings.Join(scopes, " ")})
	}
	if a.DeviceName != "" {
		parameters = append(parameters, [2]string{"device_name", truncateCodePoints(a.DeviceName, MaxDeviceNameLength)})
	}
	query := make([]string, len(parameters))
	for index, parameter := range parameters {
		query[index] = parameter[0] + "=" + queryValue(parameter[1])
	}
	return authorizationEndpoint + "?" + strings.Join(query, "&")
}

// queryValue percent-encodes as RFC 3986 has it. url.QueryEscape writes a
// space as "+"; a literal "+" is already "%2B", so the swap is safe.
func queryValue(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

// truncateCodePoints cuts text to at most limit code points, never inside one.
func truncateCodePoints(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// hostName is os.Hostname; tests replace it.
var hostName = os.Hostname

// DefaultDeviceName is "thalovant-go-sdk on <hostname>", what the consent
// page and the token show when the caller names nothing, cut to
// MaxDeviceNameLength code points; "thalovant-go-sdk" when the host has no
// name.
func DefaultDeviceName() string {
	host, err := hostName()
	if err != nil {
		host = ""
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return sdkDeviceLabel
	}
	return truncateCodePoints(sdkDeviceLabel+" on "+host, MaxDeviceNameLength)
}

// browserPlatform is this machine as the browser rule names it; tests
// replace it.
var browserPlatform = func() string { return platformName(runtime.GOOS) }

// platformName maps a GOOS to the browser rule's names: "macos", "windows",
// "linux", or the GOOS itself. Android is "linux" (a phone has no DISPLAY, so
// LoginMethodAuto picks a device login; the app signs in through its own
// flow), and iOS stays "ios", which the rule treats like Linux without a
// display.
func platformName(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	case "android":
		return "linux"
	}
	return goos
}

// CanOpenBrowser reports whether a machine can show a browser to the person
// signing in.
//
// Never in CI (CI set) or over SSH (SSH_CONNECTION or SSH_TTY set): the
// browser would open on another machine, or nowhere. Otherwise yes on
// "macos" and "windows"; on any other platform (Linux, the BSDs) only with a
// display (DISPLAY or WAYLAND_DISPLAY). A variable counts as set when it has
// a non-empty value: CI=false is set, an empty DISPLAY is not.
//
// env nil reads this process's environment; platform "" is this machine
// ("macos", "windows", "linux", or runtime.GOOS otherwise).
func CanOpenBrowser(env map[string]string, platform string) bool {
	present := func(name string) bool {
		if env == nil {
			return os.Getenv(name) != ""
		}
		return env[name] != ""
	}
	if present("CI") || present("SSH_CONNECTION") || present("SSH_TTY") {
		return false
	}
	if platform == "" {
		platform = browserPlatform()
	}
	if platform == "macos" || platform == "windows" {
		return true
	}
	return present("DISPLAY") || present("WAYLAND_DISPLAY")
}

// exchangeBrowserCode sends POST {token_endpoint} with exactly code,
// code_verifier, client_id and redirect_uri, once, and keeps the token.
//
// Read as a device-login token answer is: a 2xx whose body is not a JSON
// object with an access_token is an error with no status -- the API did not
// refuse, its answer could not be used -- and any other failure carries the
// api-errors fields (invalid_grant for every way an exchange fails), with
// the code, the verifier, the state and any token the answer carried taken
// out.
func (c *ControlPlane) exchangeBrowserCode(ctx context.Context, config NativeSignInConfig, code, verifier, redirectURI, state string) (map[string]any, error) {
	if err := requireSecureTokenExchange(c.APIURL); err != nil {
		return nil, err
	}
	payload := map[string]any{
		"code":          code,
		"code_verifier": verifier,
		"client_id":     config.SDKClientID,
		"redirect_uri":  redirectURI,
	}
	// At most once: a code presented twice revokes the token the first
	// exchange minted (RFC 9700), so the transport may not replay this body.
	status, raw, header, err := c.sendNoReplay(ctx, http.MethodPost, config.TokenEndpoint, payload)
	if err != nil {
		// send never keeps the transport's own error, which could name the
		// request.
		return nil, err
	}
	if status < 200 || status > 299 {
		clean, hidden := redactExchangeAnswer(raw, []string{code, verifier, state})
		return nil, withoutSecrets(apiErrorFromResponse(status, clean, header), hidden)
	}
	token, decodeErr := decodeControlJSON(raw)
	if decodeErr != nil {
		return nil, fmt.Errorf("%w: the browser sign-in token response was not a JSON object", ErrAPI)
	}
	if _, err := c.acceptToken(token); err != nil {
		return nil, err
	}
	return token, nil
}

// redactedSecret replaces a secret an API echoed back.
const redactedSecret = "[redacted]"

// answerTokenKeys name the members whose values are credentials wherever an
// error answer carries them.
var answerTokenKeys = map[string]bool{"access_token": true, "refresh_token": true, "id_token": true}

// redactExchangeAnswer takes the secrets out of a refused exchange's body
// before the error is built from it, and answers the secrets it found.
//
// A JSON body is decoded first, so that a secret it spells with escapes
// ("\u0061...") is matched as the text it decodes to, and is redacted before
// the display line is joined and cut at its limit, where half a secret would
// no longer match. Every value under access_token, refresh_token or id_token
// is a secret too, wherever it sits: an answer that refuses the exchange has
// no business carrying one. Numbers keep their exact spelling. A body that
// is not JSON is redacted as text; it never reaches the display line.
func redactExchangeAnswer(raw []byte, hidden []string) ([]byte, []string) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body any
	if err := decoder.Decode(&body); err != nil {
		return redactAll(raw, hidden), hidden
	}
	if _, err := decoder.Token(); err != io.EOF {
		// Trailing data: not one JSON value, so not a problem body either.
		return redactAll(raw, hidden), hidden
	}
	found := append([]string{}, hidden...)
	var collect func(any)
	collect = func(item any) {
		switch typed := item.(type) {
		case map[string]any:
			for key, member := range typed {
				if text, ok := member.(string); ok && answerTokenKeys[key] && text != "" {
					found = append(found, text)
				}
				collect(member)
			}
		case []any:
			for _, member := range typed {
				collect(member)
			}
		}
	}
	collect(body)
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(scrubSecrets(body, found)); err != nil {
		return redactAll(raw, found), found
	}
	return bytes.TrimRight(out.Bytes(), "\n"), found
}

// redactAll takes every value in hidden out of a body as text.
func redactAll(raw []byte, hidden []string) []byte {
	return []byte(scrubText(string(raw), hidden))
}

// scrubText replaces every value in hidden within text with [redacted], in
// one pass, the longest value first, so that a value which is part of
// another cannot break the longer one up before it is matched; and never
// inside a [redacted] already there (whose letters a short value can hold),
// so scrubbing twice changes nothing. This is the reference's _scrub.
func scrubText(text string, hidden []string) string {
	values := make([]string, 0, len(hidden))
	for _, value := range hidden {
		if value != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return text
	}
	sort.SliceStable(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = regexp.QuoteMeta(value)
	}
	// Go's regexp takes the first alternative that matches at the leftmost
	// position, as Python's re does, so longest first wins.
	pattern := regexp.MustCompile(strings.Join(quoted, "|"))
	parts := strings.Split(text, redactedSecret)
	for index, part := range parts {
		parts[index] = pattern.ReplaceAllLiteralString(part, redactedSecret)
	}
	return strings.Join(parts, redactedSecret)
}

// scrubSecrets is item with every value in hidden taken out of every string
// in it, the keys of its objects included.
func scrubSecrets(item any, hidden []string) any {
	switch typed := item.(type) {
	case string:
		return scrubText(typed, hidden)
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, member := range typed {
			clean[scrubText(key, hidden)] = scrubSecrets(member, hidden)
		}
		return clean
	case []any:
		clean := make([]any, len(typed))
		for index, member := range typed {
			clean[index] = scrubSecrets(member, hidden)
		}
		return clean
	}
	return item
}

// withoutSecrets is apiErr with every value in hidden taken out of every
// field, the problem's keys included. An API that echoes a request back, as a
// FastAPI 422 lists each field's input, would otherwise hand the code and the
// verifier to whoever logs the error. The returned copy is the only error
// that leaves; nothing keeps the original.
func withoutSecrets(apiErr *APIError, hidden []string) *APIError {
	clean := *apiErr
	clean.Detail = scrubText(apiErr.Detail, hidden)
	clean.Code = scrubText(apiErr.Code, hidden)
	clean.ProblemDetail = scrubText(apiErr.ProblemDetail, hidden)
	if apiErr.Problem != nil {
		clean.Problem, _ = scrubSecrets(apiErr.Problem, hidden).(map[string]any)
	}
	if apiErr.Fields != nil {
		clean.Fields = make([]string, len(apiErr.Fields))
		for index, field := range apiErr.Fields {
			clean.Fields[index] = scrubText(field, hidden)
		}
	}
	return &clean
}
