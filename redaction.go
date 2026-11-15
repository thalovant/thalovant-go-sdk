package thalovant

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// This file closes the gaps the String methods leave. A type with only String
// still prints its fields for %#v, for verbs a Stringer is not asked about (%d,
// which writes a string field as "%!d(string=...)"), and under log/slog's JSON
// handler. Format answers every verb, GoString answers %#v, and LogValue
// answers slog, so no fmt verb or logger prints a secret from these types.
// json.Marshal of Identity and MqttBrokerCredentials is unchanged: it is the
// identity-file and wire form and must keep the real values.

// writeRedacted writes plain for every verb, quoted for %q, and goSyntax for %#v.
func writeRedacted(state fmt.State, verb rune, plain string, goSyntax string) {
	switch {
	case verb == 'v' && state.Flag('#'):
		_, _ = io.WriteString(state, goSyntax)
	case verb == 'q':
		_, _ = io.WriteString(state, strconv.Quote(plain))
	default:
		_, _ = io.WriteString(state, plain)
	}
}

// GoString keeps %#v from printing the access key, the password or the broker
// credentials.
func (i Identity) GoString() string { return "thalovant." + i.String() }

// Format redacts the secrets under every fmt verb.
func (i Identity) Format(state fmt.State, verb rune) {
	writeRedacted(state, verb, i.String(), i.GoString())
}

// LogValue makes log/slog record the redacted form, never the secrets.
func (i Identity) LogValue() slog.Value { return slog.StringValue(i.String()) }

// GoString keeps %#v from printing the broker username and password.
func (m MqttBrokerCredentials) GoString() string { return "thalovant." + m.String() }

// Format redacts the broker credentials under every fmt verb.
func (m MqttBrokerCredentials) Format(state fmt.State, verb rune) {
	writeRedacted(state, verb, m.String(), m.GoString())
}

// LogValue makes log/slog record the redacted form, never the secrets.
func (m MqttBrokerCredentials) LogValue() slog.Value { return slog.StringValue(m.String()) }

// GoString keeps %#v from printing the bearer token.
func (c ControlPlane) GoString() string { return "thalovant." + c.String() }

// Format redacts the access token under every fmt verb.
func (c ControlPlane) Format(state fmt.State, verb rune) {
	writeRedacted(state, verb, c.String(), c.GoString())
}

// LogValue makes log/slog record the redacted form, never the token.
func (c ControlPlane) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Format redacts the access token under every fmt verb.
func (t APIToken) Format(state fmt.State, verb rune) {
	writeRedacted(state, verb, t.String(), t.GoString())
}

// LogValue makes log/slog record the redacted form, never the token.
func (t APIToken) LogValue() slog.Value { return slog.StringValue(t.String()) }

// Format redacts the device code under every fmt verb.
func (a DeviceAuthorization) Format(state fmt.State, verb rune) {
	writeRedacted(state, verb, a.String(), a.GoString())
}

// LogValue makes log/slog record the redacted form, never the device code.
func (a DeviceAuthorization) LogValue() slog.Value { return slog.StringValue(a.String()) }

// String renders the sign-in attempt with the state and the PKCE verifier
// redacted.
func (s NativeSignIn) String() string {
	return fmt.Sprintf("NativeSignIn{AuthorizationURL:%q State:%s Verifier:%s RedirectURI:%q}",
		s.AuthorizationURL, redactSecret(s.State), redactSecret(s.Verifier), s.RedirectURI)
}

// GoString keeps %#v from printing the state and the verifier.
func (s NativeSignIn) GoString() string { return "thalovant." + s.String() }

// Format redacts the state and the verifier under every fmt verb.
func (s NativeSignIn) Format(state fmt.State, verb rune) {
	writeRedacted(state, verb, s.String(), s.GoString())
}

// LogValue makes log/slog record the redacted form, never the verifier.
func (s NativeSignIn) LogValue() slog.Value { return slog.StringValue(s.String()) }

// String renders the options with the auth token and claims redacted.
func (o ClientContextOptions) String() string {
	claims := ""
	if len(o.AuthClaims) > 0 {
		claims = secretPlaceholder
	}
	return fmt.Sprintf("ClientContextOptions{UserID:%q UserName:%q AuthToken:%s AuthProvider:%q AuthClaims:%s Roles:%q Platform:%q Source:%q Destination:%q Channel:%q DeviceID:%q Locale:%q SessionID:%q}",
		o.UserID, o.UserName, redactSecret(o.AuthToken), o.AuthProvider, claims, o.Roles,
		o.Platform, o.Source, o.Destination, o.Channel, o.DeviceID, o.Locale, o.SessionID)
}

// GoString keeps %#v from printing the auth token and claims.
func (o ClientContextOptions) GoString() string { return "thalovant." + o.String() }

// Format redacts the auth token under every fmt verb.
func (o ClientContextOptions) Format(state fmt.State, verb rune) {
	writeRedacted(state, verb, o.String(), o.GoString())
}

// LogValue makes log/slog record the redacted form, never the token.
func (o ClientContextOptions) LogValue() slog.Value { return slog.StringValue(o.String()) }

// String renders the result with the identity's secrets and the freshly minted
// secrets inside the raw hub and client answers redacted (the client answer
// carries initial_identify and the spec's apiKey and password).
func (r BootstrapIdentityResult) String() string {
	endpoint := "<nil>"
	if r.Endpoint != nil {
		endpoint = fmt.Sprintf("{Protocol:%s Endpoint:%q}", r.Endpoint.Protocol, redactEndpointCredentials(r.Endpoint.Endpoint))
	}
	operation := "<nil>"
	if r.Operation != nil {
		operation = fmt.Sprintf("{ID:%q Kind:%q Status:%q}", r.Operation.ID, r.Operation.Kind, r.Operation.Status)
	}
	return fmt.Sprintf("BootstrapIdentityResult{Identity:%s Hub:%v Client:%v Endpoint:%s Operation:%s Replayed:%t}",
		r.Identity.String(), redactBootstrapSecrets(r.Hub), redactBootstrapSecrets(r.Client), endpoint, operation, r.Replayed)
}

// GoString keeps %#v from printing the identity and the raw client answer.
func (r BootstrapIdentityResult) GoString() string { return "thalovant." + r.String() }

// Format redacts the secrets under every fmt verb.
func (r BootstrapIdentityResult) Format(state fmt.State, verb rune) {
	writeRedacted(state, verb, r.String(), r.GoString())
}

// LogValue makes log/slog record the redacted form, never the secrets.
func (r BootstrapIdentityResult) LogValue() slog.Value { return slog.StringValue(r.String()) }

// MarshalJSON encodes Summary(false): the identity without its secrets and the
// hub and client answers with the minted secrets replaced. Keep the secrets with
// json.Marshal(result.Identity), which is the identity-file form.
func (r BootstrapIdentityResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.Summary(false))
}
