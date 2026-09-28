package thalovant

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeIdentityFile writes identity where IdentityFromFile will read it, and
// reads it back, so the result carries its SourcePath.
func writeIdentityFile(t *testing.T, dir string, identity Identity) Identity {
	t.Helper()
	path := filepath.Join(dir, "identity.json")
	if identity.DefaultMaster == "" {
		identity.DefaultMaster = "https://hub.example"
	}
	if identity.SiteID == "" {
		identity.SiteID = "site"
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := IdentityFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return read
}

func TestAnIdentityReadFromAFileKnowsItsPath(t *testing.T) {
	dir := t.TempDir()
	identity := writeIdentityFile(t, dir, Identity{AccessKey: "test-access", Password: "test-password", SiteID: "site"})
	if want := filepath.Join(dir, "identity.json"); identity.SourcePath() != want {
		t.Fatalf("SourcePath() = %q, want %q", identity.SourcePath(), want)
	}
	raw, err := json.Marshal(identity)
	if err != nil || strings.Contains(string(raw), dir) {
		t.Fatalf("the path is not identity material, yet it was serialized: %s", raw)
	}
	if (Identity{}).SourcePath() != "" {
		t.Fatal("an identity made in code has no file")
	}
}

func TestTheKeyFolderFollowsTheIdentityFile(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	legacy := filepath.Join(config, "thalovant")
	elsewhere := t.TempDir()
	identity := writeIdentityFile(t, elsewhere, Identity{AccessKey: "test-access", Password: "test-password"})

	if dir, _ := noiseStateDirFor("/named/by/the/caller", identity); dir != "/named/by/the/caller" {
		t.Fatalf("a named folder was not used: %s", dir)
	}
	if dir, adopt := noiseStateDirFor("", identity); dir != elsewhere || !adopt {
		t.Fatalf("an identity file elsewhere keeps its key beside it: %s %v", dir, adopt)
	}
	if dir, adopt := noiseStateDirFor("", Identity{AccessKey: "test-access"}); dir != legacy || adopt {
		t.Fatalf("an identity from no file keeps the old default: %s %v", dir, adopt)
	}
	// The usual identity.json already sits in the old default: nothing moves.
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	usual := writeIdentityFile(t, legacy, Identity{AccessKey: "test-access", Password: "test-password"})
	if dir, adopt := noiseStateDirFor("", usual); dir != legacy || adopt {
		t.Fatalf("the usual identity file keeps the old default: %s %v", dir, adopt)
	}
	used, other := keyFolders("", identity)
	if used != elsewhere || other != legacy {
		t.Fatalf("keyFolders = %q, %q", used, other)
	}
	rejected := clientKeyRejected("", identity)
	message := rejected.Error()
	if !strings.Contains(message, elsewhere) || !strings.Contains(message, legacy) || !strings.Contains(message, "Re-pair, or share the key folder") {
		t.Fatalf("the error does not name both folders and the way out: %s", message)
	}
	if !errors.Is(rejected, ErrClientKeyRejected) || !errors.Is(rejected, ErrHubRefused) || !errors.Is(rejected, ErrConnection) {
		t.Fatal("a rejected client key must match the refusal sentinels")
	}
}

// seedKeyFolder gives dir a client key and a pin for nodeID, as a client that
// met that hub leaves them.
func seedKeyFolder(t *testing.T, dir, nodeID string) string {
	t.Helper()
	key, err := LoadOrCreateNoiseKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveNoisePin(dir, nodeID, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(key.Private)
}

func readKey(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, NoiseKeyFilename))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func TestTheOldKeyIsCopiedOnceAndOnlyWhenItMetThisHub(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	legacy := filepath.Join(config, "thalovant")
	old := seedKeyFolder(t, legacy, "test-hub")

	// It met this hub: copied, pins and all, and the old folder is left as it was.
	target := t.TempDir()
	identity := writeIdentityFile(t, target, Identity{AccessKey: "test-access", Password: "test-password"})
	if dir := resolveNoiseStateDir("", identity, "test-hub"); dir != target {
		t.Fatalf("resolved %s", dir)
	}
	if readKey(t, target) != old {
		t.Fatal("the key that met this hub was not copied")
	}
	if pin, err := LoadNoisePin(target, "test-hub"); err != nil || pin == "" {
		t.Fatalf("the hub pin was not copied: %q %v", pin, err)
	}
	if readKey(t, legacy) != old {
		t.Fatal("the old folder was changed: a copy, never a move")
	}
	info, err := os.Stat(filepath.Join(target, NoiseKeyFilename))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		t.Fatalf("the copied key is not private: %v %v", info, err)
	}
	// Once: a folder that has a key keeps it.
	if copied, err := adoptLegacyNoiseKey(target, legacy, "test-hub"); copied || err != nil {
		t.Fatalf("copied again: %v %v", copied, err)
	}

	// It never met this hub: nothing to keep, and the folder starts afresh.
	fresh := t.TempDir()
	other := writeIdentityFile(t, fresh, Identity{AccessKey: "test-access", Password: "test-password"})
	resolveNoiseStateDir("", other, "another-hub")
	if readKey(t, fresh) != "" {
		t.Fatal("a key that never met this hub was copied")
	}
}

// End to end: a hub pinned the key this identity had in the old default. The
// same identity read from a file elsewhere, with no folder named, takes that
// key the first time and connects over KK; without the copy the hub would
// refuse the new key.
func TestAnIdentityFileElsewhereKeepsTheKeyTheHubPinned(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	legacy := filepath.Join(config, "thalovant")
	hub := newKeepingHub(t)
	if err := hub.connectOnce(hub.identity, legacy); err != nil {
		t.Fatalf("first contact from the old default: %v", err)
	}
	hub.pinnedBack(t)

	identity := writeIdentityFile(t, t.TempDir(), hub.identity)
	before := len(hub.seen())
	if err := hub.connectOnce(identity, ""); err != nil {
		t.Fatalf("the identity file elsewhere lost the key the hub pinned: %v", err)
	}
	if seen := hub.seen()[before:]; len(seen) != 1 || seen[0] != noisePatternKK {
		t.Fatalf("patterns %v, want KK with the copied key and pins", seen)
	}

	// Another program with a key of its own is refused, naming both folders.
	stranger := t.TempDir()
	err := hub.connectOnce(hub.identity, stranger)
	var rejected *ClientKeyRejectedError
	if !errors.As(err, &rejected) || rejected.KeyFolder != stranger {
		t.Fatalf("a key the hub did not pin = %v", err)
	}
}
