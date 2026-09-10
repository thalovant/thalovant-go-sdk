package thalovant

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMalformedSavedNoisePinsCannotResetTrust(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"hub":null}`, `{"hub":""}`, `{"hub":" "}`, `{"hub":"aa"}`, `{"hub":"` + strings.Repeat("gg", 32) + `"}`, `{"hub":false}`} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, NoisePinsFilename)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadNoisePin(dir, "hub"); !errors.Is(err, ErrIdentity) {
				t.Errorf("invalid stored pin accepted: %v", err)
			}
			for _, write := range []func() error{func() error { return SaveNoisePin(dir, "hub", strings.Repeat("ab", 32)) }, func() error { return pinNoisePeer(dir, "hub", strings.Repeat("ab", 32)) }, func() error { return ForgetNoisePin(dir, "hub") }} {
				if err := write(); !errors.Is(err, ErrIdentity) {
					t.Errorf("malformed trust operation accepted: %v", err)
				}
				after, _ := os.ReadFile(path)
				if string(after) != raw {
					t.Error("malformed trust file was rewritten")
				}
			}
		})
	}
}

func TestInvalidNoisePinInputsCannotPoisonStore(t *testing.T) {
	for _, key := range []string{"", " ", "ab", strings.Repeat("gg", 32)} {
		dir := t.TempDir()
		if err := SaveNoisePin(dir, "hub", key); !errors.Is(err, ErrIdentity) {
			t.Errorf("invalid input accepted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, NoisePinsFilename)); !os.IsNotExist(err) {
			t.Error("invalid input created trust file")
		}
	}
}

func TestHTTPNoiseRejectsMalformedPinBeforeAuthenticatedHello(t *testing.T) {
	for _, raw := range []string{`{"test-hub":null}`, `{"test-hub":""}`} {
		fixture := newHTTPNoiseFixture(t)
		transport := fixture.transport(t)
		path := filepath.Join(transport.NoiseStateDir, NoisePinsFilename)
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := transport.Connect(context.Background()); !errors.Is(err, ErrIdentity) {
			t.Errorf("malformed pin handshake: %v", err)
		}
		if transport.IsHandshakeComplete() {
			t.Error("malformed pin reached authenticated readiness")
		}
		after, _ := os.ReadFile(path)
		if string(after) != raw {
			t.Error("handshake rewrote malformed trust")
		}
		fixture.mu.Lock()
		hellos := fixture.responder.helloCount
		fixture.mu.Unlock()
		if hellos != 0 {
			t.Error("client identity was published after malformed pin")
		}
	}
}

func TestEquivalentHexNoisePinsPreserveStoredBytes(t *testing.T) {
	for _, saved := range []string{strings.Repeat("AB", 32), strings.Repeat("aB", 32)} {
		dir := t.TempDir()
		if err := SaveNoisePin(dir, "hub", saved); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, NoisePinsFilename)
		before, _ := os.ReadFile(path)
		if err := SaveNoisePin(dir, "hub", strings.ToLower(saved)); err != nil {
			t.Errorf("equivalent setter rejected: %v", err)
		}
		if err := pinNoisePeer(dir, "hub", strings.ToLower(saved)); err != nil {
			t.Errorf("equivalent peer rejected: %v", err)
		}
		if err := SaveNoisePin(dir, "hub", strings.Repeat("cd", 32)); !errors.Is(err, ErrConnection) {
			t.Errorf("conflicting key accepted: %v", err)
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(before) {
			t.Error("idempotence or conflict rewrote existing pin bytes")
		}
	}
}

func TestHTTPNoiseAcceptsUppercaseSavedPinWithoutRewriting(t *testing.T) {
	fixture := newHTTPNoiseFixture(t)
	transport := fixture.transport(t)
	raw, err := json.Marshal(map[string]string{"test-hub": strings.ToUpper(hex.EncodeToString(fixture.responder.key.Public))})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(transport.NoiseStateDir, NoisePinsFilename)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !transport.IsHandshakeComplete() {
		t.Fatal("valid uppercase pin did not authenticate")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("authenticated handshake rewrote equivalent pin")
	}
	if err := transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
}
