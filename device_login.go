package thalovant

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HomeAssistantScopes are the scopes a Home Assistant link signs in with:
// enough to find the account's hubs and to create, read and delete the one
// connection it holds. They are also everything a Free plan can approve.
// Each call returns a fresh slice.
func HomeAssistantScopes() []string {
	return []string{"hubs:read", "clients:read", "clients:write"}
}

// DeviceAuthorization is a started device sign-in (RFC 8628): what to show a
// person, and what to poll with. Show VerificationURI and UserCode, or open
// VerificationURIComplete, which carries the code; then call PollDeviceLogin
// every Interval.
//
// DeviceCode is the secret half and never needs showing; String() redacts it.
// Keep the whole value to resume polling later, in this process or another.
type DeviceAuthorization struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	// VerificationURIComplete is the verification URL with the code in it, or
	// "" when the API sent none.
	VerificationURIComplete string
	// Interval is how long to wait between two polls. A slow_down answer
	// lengthens it on the authorization that was polled.
	Interval time.Duration
	// ExpiresIn is how long the code stays valid from when it was issued.
	ExpiresIn time.Duration
}

// String renders the authorization with its device code redacted.
func (a DeviceAuthorization) String() string {
	return fmt.Sprintf(
		"DeviceAuthorization{DeviceCode:%s UserCode:%q VerificationURI:%q VerificationURIComplete:%q Interval:%s ExpiresIn:%s}",
		redactSecret(a.DeviceCode), a.UserCode, a.VerificationURI, a.VerificationURIComplete, a.Interval, a.ExpiresIn,
	)
}

// GoString keeps %#v from printing the device code.
func (a DeviceAuthorization) GoString() string { return a.String() }

// APIToken is an API token a device sign-in minted: the credential and what it
// may do. There is no refresh token; a device-login token lives 365 days. Keep
// TokenID to revoke it with RevokeAPIToken. String() redacts AccessToken.
type APIToken struct {
	AccessToken string
	TokenType   string
	Scopes      []string
	// ExpiresAt is when the token stops working; zero when the API did not say.
	ExpiresAt time.Time
	// TokenID names the token for RevokeAPIToken; "" when the API did not say.
	TokenID string
}

// String renders the token with its access token redacted.
func (t APIToken) String() string {
	expires := ""
	if !t.ExpiresAt.IsZero() {
		expires = t.ExpiresAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("APIToken{AccessToken:%s TokenType:%q Scopes:%q ExpiresAt:%s TokenID:%q}",
		redactSecret(t.AccessToken), t.TokenType, t.Scopes, expires, t.TokenID)
}

// GoString keeps %#v from printing the access token.
func (t APIToken) GoString() string { return t.String() }

// BeginDeviceLogin starts a device sign-in: a code for a person to approve in a
// browser, and the interval to poll at. It is LoginWithBrowser one step at a
// time, for a caller that runs its own loop -- a setup screen that shows the
// code and polls on its own schedule -- and it prints and opens nothing.
//
// scopes are what the token will carry; nil lets the API choose its default
// (hubs:read and clients:write). HomeAssistantScopes is what a Home Assistant
// link asks for. clientName, when set, names the device on the approval page.
//
// A verification URL that is not HTTP(S), has no host, or carries credentials
// is refused: it is about to be opened in a browser.
func (c *ControlPlane) BeginDeviceLogin(ctx context.Context, scopes []string, clientName string) (*DeviceAuthorization, error) {
	payload := map[string]any{}
	if scopes != nil {
		payload["scopes"] = scopes
	}
	if strings.TrimSpace(clientName) != "" {
		payload["client_name"] = clientName
	}
	grant, err := c.request(ctx, http.MethodPost, "/v1/auth/device/authorize", payload, nil, false)
	if err != nil {
		return nil, err
	}
	return deviceAuthorizationFromGrant(grant)
}

// maxDurationSeconds is the most seconds a time.Duration holds (about 292
// years), so a number off the wire never overflows the conversion.
const maxDurationSeconds = float64(math.MaxInt64/int64(time.Second)) - 1

// deviceAuthorizationFromGrant reads POST /v1/auth/device/authorize and
// refuses URLs a browser should not open.
func deviceAuthorizationFromGrant(grant map[string]any) (*DeviceAuthorization, error) {
	deviceCode, _ := grant["device_code"].(string)
	userCode, _ := grant["user_code"].(string)
	verificationURI, _ := grant["verification_uri"].(string)
	if deviceCode == "" || userCode == "" || verificationURI == "" {
		return nil, fmt.Errorf("%w: device authorization response was incomplete", ErrAPI)
	}
	if err := validateBrowserURL(verificationURI); err != nil {
		return nil, err
	}
	complete, _ := grant["verification_uri_complete"].(string)
	if complete != "" {
		if err := validateBrowserURL(complete); err != nil {
			return nil, err
		}
	}
	interval := defaultDevicePollInterval
	if raw, ok := grant["interval"].(float64); ok && raw >= 0 && raw <= maxDurationSeconds {
		interval = time.Duration(raw * float64(time.Second))
	}
	expiresIn := 900 * time.Second
	if raw, ok := grant["expires_in"].(float64); ok && raw >= 0 && raw == math.Trunc(raw) && raw <= maxDurationSeconds {
		expiresIn = time.Duration(raw) * time.Second
	}
	return &DeviceAuthorization{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: complete,
		Interval:                interval,
		ExpiresIn:               expiresIn,
	}, nil
}

// PollDeviceLogin asks once whether a device sign-in was approved.
//
// On approval it returns the token and keeps it on this ControlPlane
// (AccessToken and TokenID), exactly like Login. Otherwise it returns:
//
//   - *DeviceLoginPendingError (errors.Is ErrDeviceLoginPending): nobody has
//     approved yet; poll again after its Interval. A slow_down answer lengthens
//     authorization.Interval by five seconds, for good, as RFC 8628 §3.5 asks,
//     so keep polling with the same authorization;
//   - *DeviceLoginExpiredError (errors.Is ErrDeviceCodeExpired): start again;
//   - *DeviceLoginDeniedError (errors.Is ErrDeviceAccessDenied);
//   - any other refusal as an *APIError carrying what the API said.
//
// All of them match ErrAPI. Neither the device code nor the token ever appears
// in an error.
func (c *ControlPlane) PollDeviceLogin(ctx context.Context, authorization *DeviceAuthorization) (*APIToken, error) {
	if authorization == nil || authorization.DeviceCode == "" {
		return nil, fmt.Errorf("%w: polling a device sign-in needs the authorization BeginDeviceLogin returned", ErrAPI)
	}
	token, err := c.deviceTokenOnce(ctx, authorization.DeviceCode, &authorization.Interval)
	if err != nil {
		return nil, err
	}
	return c.acceptToken(token)
}

// deviceTokenOnce sends one POST /v1/auth/device/token: the token, or why there
// is none yet. interval is the authorization's polling interval; slow_down
// lengthens it in place.
func (c *ControlPlane) deviceTokenOnce(ctx context.Context, deviceCode string, interval *time.Duration) (map[string]any, error) {
	status, raw, err := c.send(ctx, http.MethodPost, "/v1/auth/device/token", map[string]any{"device_code": deviceCode}, nil, false)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if status >= 200 && status <= 299 {
		result, decodeErr := decodeControlJSON(raw)
		if decodeErr != nil {
			return nil, &APIError{StatusCode: status, Detail: "invalid JSON response"}
		}
		return result, nil
	}
	apiErr := apiErrorFromResponse(status, raw)
	errorCode := ""
	if status == http.StatusBadRequest && apiErr.Problem != nil {
		errorCode, _ = apiErr.Problem["error"].(string)
	}
	switch errorCode {
	case "authorization_pending", "slow_down":
		if errorCode == "slow_down" {
			// RFC 8628 §3.5: every slow_down adds five seconds, for good.
			*interval += deviceSlowDownIncrement
		}
		return nil, &DeviceLoginPendingError{Interval: *interval, APIError: apiErr}
	case "access_denied":
		return nil, &DeviceLoginDeniedError{APIError: apiErr}
	case "expired_token":
		return nil, &DeviceLoginExpiredError{APIError: apiErr}
	}
	return nil, apiErr
}

// acceptToken keeps a sign-in's token on the ControlPlane, with the id it came
// with or none, and reads what it may do. Every sign-in goes through here, so
// TokenID always names the token in AccessToken -- or nothing, for a password
// sign-in's session token -- and never one signed in with before: otherwise a
// later RevokeAPIToken("") would revoke a token this ControlPlane no longer
// holds.
func (c *ControlPlane) acceptToken(token map[string]any) (*APIToken, error) {
	accessToken, _ := token["access_token"].(string)
	if accessToken == "" {
		return nil, fmt.Errorf("%w: token response did not include access_token", ErrAPI)
	}
	c.AccessToken = accessToken
	tokenID, _ := token["token_id"].(string)
	c.TokenID = tokenID
	c.revokedOwn = false
	result := &APIToken{AccessToken: accessToken, TokenID: tokenID, Scopes: []string{}}
	result.TokenType, _ = token["token_type"].(string)
	for _, scope := range anySlice(token["scopes"]) {
		if text, ok := scope.(string); ok {
			result.Scopes = append(result.Scopes, text)
		}
	}
	if expires, ok := token["expires_at"].(string); ok && expires != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, expires); err == nil {
			result.ExpiresAt = parsed
		}
	}
	return result, nil
}

// RevokeAPIToken revokes an API token; with tokenID "" the one this
// ControlPlane signed in with (TokenID). A token may always revoke itself,
// whatever its scopes. Revoking the token in use forgets it here too, so a
// later call fails locally rather than with an HTTP 401.
//
// Revoking the token in use is idempotent. A token already revoked, or
// expired, cannot authenticate its own revoke, so the API answers 401; the
// token is dead either way, so that counts as revoked and the token is
// forgotten. Revoking it again then sends nothing and returns nil, until the
// next sign-in. Revoking another token by id is not idempotent: the API's own
// answer, such as a 404 for a token it does not know, is returned as usual.
func (c *ControlPlane) RevokeAPIToken(ctx context.Context, tokenID string) error {
	target := strings.TrimSpace(tokenID)
	if target == "" {
		target = c.TokenID
	}
	if target == "" {
		if c.revokedOwn && c.AccessToken == "" {
			return nil // already revoked and forgotten: revoking again changes nothing
		}
		return fmt.Errorf("%w: no API token id to revoke: pass one, or sign in with a device login first", ErrAPI)
	}
	own := target == c.TokenID
	if _, err := c.request(ctx, http.MethodDelete, "/v1/auth/api-tokens/"+url.PathEscape(target), nil, nil, true); err != nil {
		if !own || !isAPIStatus(err, http.StatusUnauthorized) {
			return err
		}
	}
	if own {
		c.AccessToken = ""
		c.TokenID = ""
		c.revokedOwn = true
	}
	return nil
}

// devicePending reads the wait out of a pending poll.
func devicePending(err error) (time.Duration, bool) {
	var pending *DeviceLoginPendingError
	if errors.As(err, &pending) {
		return pending.Interval, true
	}
	return 0, false
}
