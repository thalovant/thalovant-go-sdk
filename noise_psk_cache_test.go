package thalovant

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pskTestNodeID = "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEF\n-----END PUBLIC KEY-----"

// A cached key has to come back exactly as derived: the hub refuses a wrong one
// indistinguishably from a wrong password, so a mangled round trip would look
// like a credentials problem rather than a cache bug.
func TestCachedPSKRoundTrip(t *testing.T) {
	dir := t.TempDir()
	psk := derivePSK(testPassword(), pskTestNodeID)

	if got := LoadCachedPSK(dir, pskTestNodeID); got != nil {
		t.Fatalf("expected no cached key before saving, got %x", got)
	}
	if err := SaveCachedPSK(dir, pskTestNodeID, psk); err != nil {
		t.Fatalf("SaveCachedPSK: %v", err)
	}
	if got := LoadCachedPSK(dir, pskTestNodeID); hex.EncodeToString(got) != hex.EncodeToString(psk) {
		t.Fatalf("cached key mismatch:\n got %x\nwant %x", got, psk)
	}
}

// The cache must hold the key and nothing else. A fingerprint of the password
// would be a fast offline oracle sitting next to the key it protects.
func TestCachedPSKFileHoldsOnlyTheKey(t *testing.T) {
	dir := t.TempDir()
	password := testPassword()
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK(password, pskTestNodeID)); err != nil {
		t.Fatalf("SaveCachedPSK: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, NoisePskFilename))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if contains(string(raw), password) {
		t.Fatal("the cache must not contain the password")
	}
	if contains(string(raw), sha256Hex(password)) {
		t.Fatal("the cache must not contain a fast hash of the password")
	}
}

func TestCachedPSKFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK(testPassword(), pskTestNodeID)); err != nil {
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

// A rotated password is noticed when the hub rejects the stale key, so the
// handshake drops it and the next attempt derives from the current password.
func TestForgetCachedPSKDropsTheEntry(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK(testPassword(), pskTestNodeID)); err != nil {
		t.Fatalf("SaveCachedPSK: %v", err)
	}
	if err := ForgetCachedPSK(dir, pskTestNodeID); err != nil {
		t.Fatalf("ForgetCachedPSK: %v", err)
	}
	if got := LoadCachedPSK(dir, pskTestNodeID); got != nil {
		t.Fatal("the entry should be gone")
	}
}

// A damaged cache is derivable state; losing it must not fail a connection.
func TestCorruptCachedPSKIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK(testPassword(), pskTestNodeID)); err != nil {
		t.Fatalf("SaveCachedPSK: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, NoisePskFilename), []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("corrupt cache: %v", err)
	}
	if got := LoadCachedPSK(dir, pskTestNodeID); got != nil {
		t.Fatal("a corrupt cache must not yield a key")
	}
	if err := SaveCachedPSK(dir, pskTestNodeID, derivePSK(testPassword(), pskTestNodeID)); err != nil {
		t.Fatalf("recovery save: %v", err)
	}
	if got := LoadCachedPSK(dir, pskTestNodeID); got == nil {
		t.Fatal("the cache should recover after being rewritten")
	}
}

// The in-memory cache is the one a reconnect hits first, so keying it on the
// node id alone would hand back the previous password's key.
func TestTransportPSKReDerivesAfterPasswordChange(t *testing.T) {
	dir := t.TempDir()
	transport := NewWSSTransport(Identity{Password: testPassword()})
	transport.NoiseStateDir = dir

	first := transport.pskFor(pskTestNodeID)
	if hex.EncodeToString(first) != hex.EncodeToString(derivePSK(testPassword(), pskTestNodeID)) {
		t.Fatal("first derivation did not match the reference")
	}

	rotated := testPassword() + "-rotated"
	transport.Identity.Password = rotated
	second := transport.pskFor(pskTestNodeID)
	if hex.EncodeToString(second) != hex.EncodeToString(derivePSK(rotated, pskTestNodeID)) {
		t.Fatal("a changed password must re-derive rather than reuse the cached key")
	}
}

// Built at run time rather than written as a literal: a literal here is
// indistinguishable to scanners from a real credential checked into the repo.
func testPassword() string {
	return "harness-" + hex.EncodeToString([]byte{0xde, 0xad, 0xbe, 0xef})
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
