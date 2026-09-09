package thalovant

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// One OS lock covers every read/modify/write transaction in this state
// directory. Closing a process releases the lock, including after a crash.
// Keep the lock inode: unlinking it would let old and new users lock different
// files. Different hubs still require distinct runtime identities.
func lockNoiseStore(dir string) (func(), error) {
	if strings.TrimSpace(dir) == "" {
		var err error
		dir, err = NoiseStateDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, ".noise.lock")
	if err := validateNoiseFile(lockPath, "Noise state lock"); err != nil {
		return nil, err
	}
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !locked {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: unable to acquire Noise state lock: %v", ErrIdentity, err)
	}
	return func() { _ = lock.Close() }, nil
}

// Only complete, flushed files become visible at trusted paths. Static keys
// are linked without overwrite, while mutable maps are atomically renamed
// under lock. A killed writer leaves an ignored staging file, never a partial
// key or a truncated pin map. No fallback ever replaces an existing key.
func publishNoiseFile(path string, payload []byte, replace bool) (err error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".noise-stage-*")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err = file.Write(payload); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if replace {
		err = os.Rename(file.Name(), path)
	} else {
		err = os.Link(file.Name(), path)
	}
	if err != nil {
		return err
	}
	// Windows does not expose directory fsync through os.File.Sync.
	if runtime.GOOS != "windows" {
		directory, openErr := os.Open(filepath.Dir(path))
		if openErr != nil {
			return openErr
		}
		defer directory.Close()
		return directory.Sync()
	}
	return nil
}

// Do not follow symbolic links or block on pipes at persisted trust paths.
func validateNoiseFile(path, label string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s must be a regular file", ErrIdentity, label)
	}
	return assertSecureSecretFile(path, label)
}
