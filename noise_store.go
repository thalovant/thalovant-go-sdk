package thalovant

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/flynn/noise"
)

// NoiseKeyFilename is the static X25519 private key used for every v3
// handshake, hex encoded. It must persist: regenerating it on each start makes
// every connection look like a new peer and defeats pinning in both directions.
const NoiseKeyFilename = "noise_key"

// NoisePinsFilename records the server static keys this client has pinned, as
// a JSON object keyed by the server node id.
const NoisePinsFilename = "noise_pins.json"

// noiseStoreMu serializes the read-modify-write of the pin file so two
// connections pinning different hubs at once cannot lose one another's entry.
var noiseStoreMu sync.Mutex

// NoiseStateDir is the directory holding the static key and the pin file. It
// sits beside the SDK config file, so XDG_CONFIG_HOME and the Windows APPDATA
// location are honored the same way.
func NoiseStateDir() (string, error) {
	configPath, err := DefaultConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(configPath), nil
}

// LoadOrCreateNoiseKey returns this client's persistent static X25519 keypair,
// generating and storing one on first use.
//
// The key file is created 0600 and is rejected if it is group- or
// world-accessible, matching how the SDK treats every other on-disk secret.
func LoadOrCreateNoiseKey(dir string) (noise.DHKey, error) {
	if strings.TrimSpace(dir) == "" {
		resolved, err := NoiseStateDir()
		if err != nil {
			return noise.DHKey{}, err
		}
		dir = resolved
	}
	path := filepath.Join(dir, NoiseKeyFilename)

	noiseStoreMu.Lock()
	defer noiseStoreMu.Unlock()

	if raw, err := os.ReadFile(path); err == nil {
		if err := assertSecureSecretFile(path, "Noise key file"); err != nil {
			return noise.DHKey{}, err
		}
		private, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(private) != 32 {
			return noise.DHKey{}, fmt.Errorf("%w: Noise key file %s is not a 32-byte hex key", ErrIdentity, path)
		}
		public, err := noise.DH25519.DH(private, curve25519Basepoint())
		if err != nil {
			return noise.DHKey{}, fmt.Errorf("%w: Noise key file %s does not hold a usable key: %v", ErrIdentity, path, err)
		}
		return noise.DHKey{Private: private, Public: public}, nil
	} else if !os.IsNotExist(err) {
		return noise.DHKey{}, fmt.Errorf("%w: unable to read Noise key file %s: %v", ErrIdentity, path, err)
	}

	key, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return noise.DHKey{}, fmt.Errorf("%w: unable to generate a Noise static key: %v", ErrIdentity, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return noise.DHKey{}, fmt.Errorf("%w: unable to create %s: %v", ErrIdentity, dir, err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key.Private)), 0o600); err != nil {
		return noise.DHKey{}, fmt.Errorf("%w: unable to write Noise key file %s: %v", ErrIdentity, path, err)
	}
	return key, nil
}

// curve25519Basepoint is the standard X25519 base point, used to recover the
// public half of a stored private key.
func curve25519Basepoint() []byte {
	basepoint := make([]byte, 32)
	basepoint[0] = 9
	return basepoint
}

// LoadNoisePin returns the pinned server static key for a node id, or "" when
// this client has not seen that server before.
func LoadNoisePin(dir, nodeID string) (string, error) {
	pins, path, err := readNoisePins(dir)
	if err != nil {
		return "", err
	}
	_ = path
	return pins[nodeID], nil
}

// SaveNoisePin records the server static key for a node id on first contact.
func SaveNoisePin(dir, nodeID, publicKey string) error {
	if strings.TrimSpace(nodeID) == "" || strings.TrimSpace(publicKey) == "" {
		return nil
	}

	noiseStoreMu.Lock()
	defer noiseStoreMu.Unlock()

	pins, path, err := readNoisePinsLocked(dir)
	if err != nil {
		return err
	}
	if pins[nodeID] == publicKey {
		return nil
	}
	pins[nodeID] = publicKey
	return writeNoisePinsLocked(path, pins)
}

// ForgetNoisePin drops a pinned server key. Use it when a server was
// deliberately reinstalled or replaced; a pin that stops matching on its own is
// a failure to investigate, not one to clear.
func ForgetNoisePin(dir, nodeID string) error {
	noiseStoreMu.Lock()
	defer noiseStoreMu.Unlock()

	pins, path, err := readNoisePinsLocked(dir)
	if err != nil {
		return err
	}
	if _, found := pins[nodeID]; !found {
		return nil
	}
	delete(pins, nodeID)
	return writeNoisePinsLocked(path, pins)
}

func readNoisePins(dir string) (map[string]string, string, error) {
	noiseStoreMu.Lock()
	defer noiseStoreMu.Unlock()
	return readNoisePinsLocked(dir)
}

func readNoisePinsLocked(dir string) (map[string]string, string, error) {
	if strings.TrimSpace(dir) == "" {
		resolved, err := NoiseStateDir()
		if err != nil {
			return nil, "", err
		}
		dir = resolved
	}
	path := filepath.Join(dir, NoisePinsFilename)
	pins := map[string]string{}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return pins, path, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("%w: unable to read Noise pin file %s: %v", ErrIdentity, path, err)
	}
	if err := json.Unmarshal(raw, &pins); err != nil {
		return nil, "", fmt.Errorf("%w: Noise pin file %s is not a JSON object of node id to key: %v", ErrIdentity, path, err)
	}
	return pins, path, nil
}

func writeNoisePinsLocked(path string, pins map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: unable to create %s: %v", ErrIdentity, filepath.Dir(path), err)
	}
	encoded, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: unable to encode Noise pins: %v", ErrIdentity, err)
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

// NoisePskFilename caches password-to-PSK derivations, as a JSON object keyed
// by the server node id.
//
// The derivation is argon2id at 64 MiB and depends only on the password and the
// hub's node id, both constant for the life of the pairing, so it is the same
// answer every time. The in-memory cache on a transport only helps that one
// object; this survives reconnects, other transports in the same process, and
// restarts.
const NoisePskFilename = "noise_psks.json"

type cachedPskEntry struct {
	PSK      string `json:"psk"`
	Verifier string `json:"verifier"`
}

// PskPasswordVerifier fingerprints the password a cached PSK came from, so a
// rotated password is noticed and re-derived rather than offered to the hub --
// which would refuse it exactly as it refuses a wrong password.
//
// It is a hash, never the password, and it never leaves the machine. The
// identity file on the same host already holds the password itself, so this
// adds no exposure that was not already there.
func PskPasswordVerifier(password string) string {
	sum := sha256.Sum256([]byte("thalovant-psk-verifier:" + password))
	return hex.EncodeToString(sum[:])
}

func pskCachePathLocked(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		resolved, err := NoiseStateDir()
		if err != nil {
			return "", err
		}
		dir = resolved
	}
	return filepath.Join(dir, NoisePskFilename), nil
}

func readPskCacheLocked(path string) map[string]cachedPskEntry {
	cache := map[string]cachedPskEntry{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cache
	}
	if err := assertSecureSecretFile(path, "Noise PSK cache"); err != nil {
		return cache
	}
	// A corrupt cache is derivable state, not a reason to fail a connection.
	if err := json.Unmarshal(raw, &cache); err != nil {
		return map[string]cachedPskEntry{}
	}
	return cache
}

// LoadCachedPSK returns the stored PSK for a hub, or nil when there is none or
// it was derived from a different password.
func LoadCachedPSK(dir, nodeID, verifier string) []byte {
	if strings.TrimSpace(nodeID) == "" {
		return nil
	}

	noiseStoreMu.Lock()
	defer noiseStoreMu.Unlock()

	path, err := pskCachePathLocked(dir)
	if err != nil {
		return nil
	}
	entry, found := readPskCacheLocked(path)[nodeID]
	if !found || entry.Verifier != verifier {
		return nil
	}
	psk, err := hex.DecodeString(strings.TrimSpace(entry.PSK))
	if err != nil || len(psk) != pskLengthByte {
		return nil
	}
	return psk
}

// SaveCachedPSK records a derived PSK so the next connection to this hub skips
// argon2id. The cache is an optimisation, so callers treat a failure here as
// non-fatal.
func SaveCachedPSK(dir, nodeID string, psk []byte, verifier string) error {
	if strings.TrimSpace(nodeID) == "" || len(psk) != pskLengthByte {
		return nil
	}

	noiseStoreMu.Lock()
	defer noiseStoreMu.Unlock()

	path, err := pskCachePathLocked(dir)
	if err != nil {
		return err
	}
	cache := readPskCacheLocked(path)
	encoded := hex.EncodeToString(psk)
	if existing, found := cache[nodeID]; found && existing.PSK == encoded && existing.Verifier == verifier {
		return nil
	}
	cache[nodeID] = cachedPskEntry{PSK: encoded, Verifier: verifier}
	payload, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: unable to create %s: %v", ErrIdentity, filepath.Dir(path), err)
	}
	return os.WriteFile(path, append(payload, '\n'), 0o600)
}
