package thalovant

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Where a client's Noise key lives (0.9.1).
//
// A hub pins one client key per connection, so every program that uses one
// identity must present the same key. Before 0.9.1 every transport with no
// NoiseStateDir used NoiseStateDir() -- the SDK config directory -- whatever
// file its identity came from. Now an identity read from a file keeps its key
// and hub pins in that file's directory: for the usual
// ~/.config/thalovant/identity.json that is the same directory, and nothing
// moves. For an identity file elsewhere, the first time its directory is used,
// the key it had in the old default is copied there (never moved), but only
// when that key has already met this identity's hub, so no device gets a new
// key and is locked out.

// identityNoiseFolder is the directory of the file identity was read from, or
// "" when it was not read from one.
func identityNoiseFolder(identity Identity) string {
	if identity.sourcePath == "" {
		return ""
	}
	return filepath.Dir(identity.sourcePath)
}

// noiseStateDirFor is the directory a transport keeps identity's Noise state
// in: explicit when named, else the identity file's directory when it has one
// that holds a key already or can be written, else the old default. An
// identity in /etc/thalovant read by a user who cannot write there keeps
// using the user's own directory. adopt reports whether the directory is the
// identity file's own, other than the old default, so the old key may be
// copied into it.
func noiseStateDirFor(explicit string, identity Identity) (dir string, adopt bool) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, false
	}
	legacy, _ := NoiseStateDir()
	beside := identityNoiseFolder(identity)
	if beside == "" || sameDirectory(beside, legacy) {
		return legacy, false
	}
	if _, err := os.Stat(filepath.Join(beside, NoiseKeyFilename)); err == nil {
		return beside, true
	}
	if !writableDirectory(beside) {
		return legacy, false
	}
	return beside, true
}

// keyFolders names the directory a transport's key is in, and the other one a
// program reading the same identity would likely use: the identity file's
// directory or the old default, whichever this one is not.
func keyFolders(explicit string, identity Identity) (used, other string) {
	used, _ = noiseStateDirFor(explicit, identity)
	if used == "" {
		used, _ = NoiseStateDir()
	}
	legacy, _ := NoiseStateDir()
	for _, candidate := range []string{identityNoiseFolder(identity), legacy} {
		if candidate != "" && !sameDirectory(candidate, used) {
			return used, candidate
		}
	}
	return used, ""
}

// clientKeyRejected is the error for a hub that refused the key kept in
// explicit (or identity's default directory).
func clientKeyRejected(explicit string, identity Identity) *ClientKeyRejectedError {
	used, other := keyFolders(explicit, identity)
	return &ClientKeyRejectedError{KeyFolder: used, OtherKeyFolder: other}
}

func sameDirectory(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	left, errLeft := filepath.Abs(a)
	right, errRight := filepath.Abs(b)
	if errLeft != nil || errRight != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	if left == right {
		return true
	}
	leftInfo, errLeft := os.Stat(left)
	rightInfo, errRight := os.Stat(right)
	return errLeft == nil && errRight == nil && os.SameFile(leftInfo, rightInfo)
}

func writableDirectory(dir string) bool {
	probe, err := os.CreateTemp(dir, ".thalovant-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}

// resolveNoiseStateDir is noiseStateDirFor, and on the way the one-time copy
// of this identity's old key into the identity file's directory once the hub
// it is about to meet (nodeID) is known. A failed copy is never a reason to
// fail the connection: the transport then makes a key of its own, as it did
// before 0.9.1 for a new directory.
func resolveNoiseStateDir(explicit string, identity Identity, nodeID string) string {
	dir, adopt := noiseStateDirFor(explicit, identity)
	if adopt && nodeID != "" {
		if legacy, err := NoiseStateDir(); err == nil {
			_, _ = adoptLegacyNoiseKey(dir, legacy, nodeID)
		}
	}
	return dir
}

// adoptLegacyNoiseKey copies the key and hub pins from legacy into target,
// once: only when target holds no key yet and legacy holds a key that has met
// the hub nodeID (a pin filed under it). The pins are copied whole; the PSK
// cache is not, since it derives again. Returns whether it copied.
func adoptLegacyNoiseKey(target, legacy, nodeID string) (bool, error) {
	if target == "" || legacy == "" || sameDirectory(target, legacy) || strings.TrimSpace(nodeID) == "" {
		return false, nil
	}
	targetKey := filepath.Join(target, NoiseKeyFilename)
	if _, err := os.Lstat(targetKey); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if info, err := os.Lstat(legacy); err != nil || !info.IsDir() {
		return false, nil
	}
	pins, _, err := readNoisePins(legacy)
	if err != nil || pins[nodeID] == "" {
		// The old key never met this hub: it is not the key the hub pinned
		// for this identity, so there is nothing to keep.
		return false, nil
	}
	legacyKey := filepath.Join(legacy, NoiseKeyFilename)
	if err := validateNoiseFile(legacyKey, "Noise key file"); err != nil {
		return false, nil
	}
	if err := assertSecureSecretFile(legacyKey, "Noise key file"); err != nil {
		return false, nil
	}
	raw, err := os.ReadFile(legacyKey)
	if err != nil {
		return false, nil
	}
	private, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(private) != 32 {
		return false, nil
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return false, fmt.Errorf("%w: unable to create %s: %v", ErrIdentity, target, err)
	}
	unlock, err := lockNoiseStore(target)
	if err != nil {
		return false, err
	}
	defer unlock()
	// Another process may have adopted, or started afresh, meanwhile.
	if _, err := os.Lstat(targetKey); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	existing, pinsPath, err := readNoisePinsLocked(target)
	if err != nil {
		return false, err
	}
	for id, key := range pins {
		if existing[id] == "" {
			existing[id] = key
		}
	}
	if err := writeNoisePinsLocked(pinsPath, existing); err != nil {
		return false, err
	}
	if err := publishNoiseFile(targetKey, []byte(hex.EncodeToString(private)), false); err != nil {
		return false, fmt.Errorf("%w: unable to write Noise key file %s: %v", ErrIdentity, targetKey, err)
	}
	return true, nil
}
