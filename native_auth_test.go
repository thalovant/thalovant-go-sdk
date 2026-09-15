package thalovant

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

// The authorization-code grant, which every client that needed it wrote for
// itself until this existed. What is tested is what is a security bug when
// wrong and looks fine when wrong.

func query(t *testing.T, raw string) url.Values {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("authorization URL does not parse: %v", err)
	}
	return parsed.Query()
}

func begin(t *testing.T, opts NativeSignInOptions) NativeSignIn {
	t.Helper()
	begun, err := BeginNativeSignIn(opts)
	if err != nil {
		t.Fatalf("BeginNativeSignIn: %v", err)
	}
	return begun
}

func TestTheChallengeIsTheS256OfTheVerifier(t *testing.T) {
	begun := begin(t, NativeSignInOptions{ClientID: "thalovant-cli", RedirectURI: "http://127.0.0.1:8765/"})
	sum := sha256.Sum256([]byte(begun.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := query(t, begun.AuthorizationURL).Get("code_challenge"); got != want {
		t.Fatalf("code_challenge = %q, want %q", got, want)
	}
	if got := ChallengeFor(begun.Verifier); got != want {
		t.Fatalf("ChallengeFor = %q, want %q", got, want)
	}
}

func TestTheVerifierNeverReachesTheBrowser(t *testing.T) {
	begun := begin(t, NativeSignInOptions{ClientID: "app", RedirectURI: "app://auth"})
	if strings.Contains(begun.AuthorizationURL, begun.Verifier) {
		t.Fatal("the verifier is in the URL handed to the browser")
	}
}

func TestOnlyS256IsOffered(t *testing.T) {
	begun := begin(t, NativeSignInOptions{ClientID: "app", RedirectURI: "app://auth"})
	if got := query(t, begun.AuthorizationURL).Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
}

func TestEveryAttemptGetsItsOwnVerifierAndState(t *testing.T) {
	first := begin(t, NativeSignInOptions{ClientID: "app", RedirectURI: "app://auth"})
	second := begin(t, NativeSignInOptions{ClientID: "app", RedirectURI: "app://auth"})
	if first.Verifier == second.Verifier || first.State == second.State {
		t.Fatal("two attempts shared a secret")
	}
}

func TestTheRequestCarriesWhatTheAuthorizeEndpointMatchesOn(t *testing.T) {
	begun := begin(t, NativeSignInOptions{
		ClientID:    "thalovant-cli",
		RedirectURI: "http://127.0.0.1:8765/",
		Scopes:      []string{"hubs:read", "clients:write"},
	})
	parameters := query(t, begun.AuthorizationURL)
	for name, want := range map[string]string{
		"client_id":     "thalovant-cli",
		"redirect_uri":  "http://127.0.0.1:8765/",
		"response_type": "code",
		"scope":         "hubs:read clients:write",
		"state":         begun.State,
	} {
		if got := parameters.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.HasPrefix(begun.AuthorizationURL, "https://dash.thalovant.com/authorize?") {
		t.Fatalf("unexpected authorization URL: %s", begun.AuthorizationURL)
	}
}

func TestTheDefaultScopesAreTheThreeAFreePlanMayMint(t *testing.T) {
	begun := begin(t, NativeSignInOptions{ClientID: "app", RedirectURI: "app://auth"})
	if got := query(t, begun.AuthorizationURL).Get("scope"); got != strings.Join(DefaultNativeScopes, " ") {
		t.Fatalf("scope = %q", got)
	}
}

func TestARedirectAnsweringADifferentAttemptIsRefused(t *testing.T) {
	begun := begin(t, NativeSignInOptions{ClientID: "app", RedirectURI: "app://auth"})
	if _, ok := begun.CodeFrom("app://auth?code=abc&state=somebody-elses"); ok {
		t.Fatal("a redirect for another attempt was accepted")
	}
	code, ok := begun.CodeFrom("app://auth?code=abc&state=" + begun.State)
	if !ok || code != "abc" {
		t.Fatalf("CodeFrom = %q, %v", code, ok)
	}
}

func TestNoCodeOrAnErrorInsteadIsNotSuccess(t *testing.T) {
	begun := begin(t, NativeSignInOptions{ClientID: "app", RedirectURI: "app://auth"})
	for _, redirect := range []string{
		"app://auth?state=" + begun.State,
		"app://auth?code=&state=" + begun.State,
		"app://auth?error=access_denied&state=" + begun.State,
		"app://auth",
	} {
		if _, ok := begun.CodeFrom(redirect); ok {
			t.Fatalf("%q was treated as success", redirect)
		}
	}
}

func TestACodeWithEscapedCharactersSurvivesTheRoundTrip(t *testing.T) {
	begun := begin(t, NativeSignInOptions{ClientID: "app", RedirectURI: "app://auth"})
	code, ok := begun.CodeFrom("app://auth?code=a%2Bb%2Fc%3D&state=" + begun.State)
	if !ok || code != "a+b/c=" {
		t.Fatalf("CodeFrom = %q, %v", code, ok)
	}
}

func TestAnEmptyClientOrRedirectIsRefusedHereRatherThanAtTheAPI(t *testing.T) {
	if _, err := BeginNativeSignIn(NativeSignInOptions{ClientID: "", RedirectURI: "app://auth"}); err == nil {
		t.Fatal("an empty ClientID was accepted")
	}
	if _, err := BeginNativeSignIn(NativeSignInOptions{ClientID: "app", RedirectURI: "  "}); err == nil {
		t.Fatal("an empty RedirectURI was accepted")
	}
}

func TestAThalovantURLIsRecognisedBySchemeAndHost(t *testing.T) {
	for _, ours := range []string{"https://dash.thalovant.com/authorize?x=1", "https://thalovant.com"} {
		if !IsThalovantURL(ours) {
			t.Fatalf("%s should be recognised", ours)
		}
	}
	for _, foreign := range []string{
		"http://dash.thalovant.com",
		// The one that matters: a lookalike host ending in the same letters.
		"https://dash.thalovant.com.evil.test",
		"https://notthalovant.com",
		"nonsense",
	} {
		if IsThalovantURL(foreign) {
			t.Fatalf("%s should not be recognised", foreign)
		}
	}
}
