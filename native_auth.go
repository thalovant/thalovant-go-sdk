package thalovant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The authorization-code grant with PKCE (RFC 7636), for a client that can
// open a browser.
//
// LoginWithBrowser is the device grant, and it exists for something that
// cannot open one: somebody reads a code off one screen and types it into
// another. A desktop tool can open the browser itself and be handed the
// answer back, and asking its user to copy a code between two windows on the
// same machine is a worse experience than the one every other tool offers.
//
//	begun, err := BeginNativeSignIn(NativeSignInOptions{
//	    ClientID: "my-app", RedirectURI: "http://127.0.0.1:8765/"})
//	browser.Open(begun.AuthorizationURL)
//	code, ok := begun.CodeFrom(redirect)    // verifies state
//	plane.CompleteNativeSignIn(ctx, code, begun.Verifier, "my-app", redirectURI)
//
// begun.Verifier never leaves the process and never enters the browser. That
// is what PKCE is for: a code intercepted by whatever else claimed the
// redirect is useless without it.

// DefaultDashboardURL is where a person approves the request.
const DefaultDashboardURL = "https://dash.thalovant.com"

// DefaultNativeScopes are the three a phone needs; also the three a free plan
// may mint.
var DefaultNativeScopes = []string{"hubs:read", "clients:read", "clients:write"}

// NativeSignInOptions carries the inputs for BeginNativeSignIn. RedirectURI
// must be one the API has registered for ClientID; the authorization endpoint
// matches it exactly and refuses anything else, so it cannot be turned into an
// open redirect.
type NativeSignInOptions struct {
	ClientID     string
	RedirectURI  string
	Scopes       []string
	DashboardURL string
}

// NativeSignIn is one sign-in attempt in progress. Keep it until the browser
// comes back; it holds the two secrets that make the round trip safe.
type NativeSignIn struct {
	// AuthorizationURL is opened in a browser.
	AuthorizationURL string
	// State proves the redirect answers this attempt and not a replayed one.
	State string
	// Verifier is never sent to the browser. Exchanged with the code, once.
	Verifier string
}

func base64URL(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func randomURLSafe(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("%w: could not generate a sign-in secret", ErrAPI)
	}
	return base64URL(raw), nil
}

// NewVerifier returns a PKCE verifier: 64 random bytes, base64url, no padding.
func NewVerifier() (string, error) { return randomURLSafe(64) }

// ChallengeFor returns the S256 challenge for a verifier.
func ChallengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64URL(sum[:])
}

// IsThalovantURL reports whether a URL belongs to Thalovant, for a caller that
// wants to show where it is about to send somebody. Scheme and host only: a
// display check, not an authorization one.
func IsThalovantURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "thalovant.com" || strings.HasSuffix(host, ".thalovant.com")
}

// BeginNativeSignIn starts a sign-in, returning the URL to open and the
// secrets to keep.
func BeginNativeSignIn(opts NativeSignInOptions) (NativeSignIn, error) {
	clientID := strings.TrimSpace(opts.ClientID)
	redirectURI := strings.TrimSpace(opts.RedirectURI)
	if clientID == "" {
		return NativeSignIn{}, fmt.Errorf("%w: ClientID is required to start a sign-in", ErrAPI)
	}
	if redirectURI == "" {
		return NativeSignIn{}, fmt.Errorf("%w: RedirectURI is required to start a sign-in", ErrAPI)
	}
	verifier, err := NewVerifier()
	if err != nil {
		return NativeSignIn{}, err
	}
	state, err := randomURLSafe(24)
	if err != nil {
		return NativeSignIn{}, err
	}
	scopes := opts.Scopes
	if scopes == nil {
		scopes = DefaultNativeScopes
	}
	dashboard := strings.TrimRight(opts.DashboardURL, "/")
	if dashboard == "" {
		dashboard = DefaultDashboardURL
	}
	query := url.Values{
		"client_id":      {clientID},
		"redirect_uri":   {redirectURI},
		"response_type":  {"code"},
		"code_challenge": {ChallengeFor(verifier)},
		// S256 only. plain is refused by the API, and offering it here would
		// only give a caller a way to ask for the weaker one.
		"code_challenge_method": {"S256"},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
	}
	return NativeSignIn{
		AuthorizationURL: dashboard + "/authorize?" + query.Encode(),
		State:            state,
		Verifier:         verifier,
	}, nil
}

// CodeFrom returns the authorization code out of the redirect the browser came
// back with. ok is false when it is not an answer to this attempt.
//
// A bool rather than an error on a state mismatch, a missing code, or an
// error= response: all three mean "do not continue", and a caller that handles
// them alike cannot accidentally treat one of them as success.
func (s NativeSignIn) CodeFrom(redirect string) (code string, ok bool) {
	parsed, err := url.Parse(redirect)
	if err != nil {
		return "", false
	}
	found, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", false
	}
	if found.Get("state") != s.State {
		return "", false
	}
	value := found.Get("code")
	return value, value != ""
}

// CompleteNativeSignIn exchanges an authorization code for a scoped access
// token and stores it.
//
// The verifier is sent here and nowhere else; it never entered the browser,
// which is what makes an intercepted code useless to whoever intercepted it.
//
// A code presented twice revokes the token the first exchange minted
// (RFC 9700), so retrying a failed exchange with the same code destroys the
// token it is trying to obtain. Start again from BeginNativeSignIn.
func (c *ControlPlane) CompleteNativeSignIn(ctx context.Context, code string, verifier string, clientID string, redirectURI string) (map[string]any, error) {
	payload := map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"code_verifier": verifier,
		"client_id":     clientID,
		"redirect_uri":  redirectURI,
	}
	token, err := c.request(ctx, http.MethodPost, "/v1/auth/native/token", payload, nil, false)
	if err != nil {
		return nil, err
	}
	accessToken, _ := token["access_token"].(string)
	if accessToken == "" {
		return nil, fmt.Errorf("%w: token response did not include access_token", ErrAPI)
	}
	c.AccessToken = accessToken
	return token, nil
}
