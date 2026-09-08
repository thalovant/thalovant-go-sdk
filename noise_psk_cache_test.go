package thalovant

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pskTestNodeID = "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEF\n-----END PUBLIC KEY-----"

// A cached PSK has to come back exactly as derived: the hub refuses a wrong one
// indistinguishably from a wrong password, so a mangled round trip would look
// like a credentials problem rather than a cache bug.
func TestCachedPSKRoundTrip(t *testing.T) {
	dir := t.TempDir()
	psk := derivePSK("hunter2", pskTestNodeID)
	verifier := PskPasswordVerifier("hunter2")

	if got := LoadCachedPSK(dir, pskTestNodeID, verifier); got != nil {
		t.Fatalf("expected no cached PSK before saving, got %x", got)
	}
	if err := SaveCachedPSK(dir, pskTestNodeID, psk, verifier); err != nil {
		t.Fatalf("SaveCachedPSK: %v", err)
	}
	got := LoadCachedPSK(dir, pskTestNodeID, verifier)
	if hex.EncodeToString(got) != hex.EncodeToString(psk) {
		t.Fatalf("cached PSK mismatch:\n got %x\nwant %x", got, psk)
	}
}

func TestCachedPSKIgnoredAfterPasswordRotation(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK("old", pskTestNodeID), PskPasswordVerifier("old")); err != nil {
		t.Fatalf("SaveCachedPSK: %v", err)
	}
	if got := LoadCachedPSK(dir, pskTestNodeID, PskPasswordVerifier("new")); got != nil {
		t.Fatal("a rotated password must not reuse the previous PSK")
	}
	if got := LoadCachedPSK(dir, pskTestNodeID, PskPasswordVerifier("old")); got == nil {
		t.Fatal("the original password should still hit the cache")
	}
}

func TestCachedPSKFileNeverHoldsThePassword(t *testing.T) {
	dir := t.TempDir()
	password := "a-very-distinctive-password-9931"
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK(password, pskTestNodeID), PskPasswordVerifier(password)); err != nil {
		t.Fatalf("SaveCachedPSK: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, NoisePskFilename))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Fatal("the verifier must not be reversible to the password")
	}
}

func TestCachedPSKFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK("hunter2", pskTestNodeID), PskPasswordVerifier("hunter2")); err != nil {
		t.Fatalf("SaveCachedPSK: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, NoisePskFilename))
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("PSK cache is group/world accessible: %o", mode)
	}
}

// A damaged cache is derivable state; losing it must not fail a connection.
func TestCorruptCachedPSKIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	verifier := PskPasswordVerifier("hunter2")
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK("hunter2", pskTestNodeID), verifier); err != nil {
		t.Fatalf("SaveCachedPSK: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, NoisePskFilename), []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("corrupt cache: %v", err)
	}
	if got := LoadCachedPSK(dir, pskTestNodeID, verifier); got != nil {
		t.Fatal("a corrupt cache must not yield a PSK")
	}
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK("hunter2", pskTestNodeID), verifier); err != nil {
		t.Fatalf("recovery save: %v", err)
	}
	if got := LoadCachedPSK(dir, pskTestNodeID, verifier); got == nil {
		t.Fatal("the cache should recover after being rewritten")
	}
}

// The transport's in-memory cache is the one a reconnect hits first, so keying
// it on the node id alone would hand back the previous password's PSK.
func TestTransportPSKReDerivesAfterPasswordChange(t *testing.T) {
	dir := t.TempDir()
	transport := NewWSSTransport(Identity{Password: "first-password"})
	transport.NoiseStateDir = dir

	first := transport.pskFor(pskTestNodeID)
	if hex.EncodeToString(first) != hex.EncodeToString(derivePSK("first-password", pskTestNodeID)) {
		t.Fatal("first derivation did not match the reference")
	}

	transport.Identity.Password = "second-password"
	second := transport.pskFor(pskTestNodeID)
	if hex.EncodeToString(second) != hex.EncodeToString(derivePSK("second-password", pskTestNodeID)) {
		t.Fatal("a changed password must re-derive rather than reuse the cached PSK")
	}
}
