package thalovant

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNoiseStoreChild(t *testing.T) {
	dir := os.Getenv("THALOVANT_TEST_NOISE_DIR")
	if dir == "" {
		return
	}
	mode := os.Getenv("THALOVANT_TEST_NOISE_MODE")
	if mode == "crash" {
		unlock, err := lockNoiseStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		stage, err := os.CreateTemp(dir, ".noise-stage-*")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stage.WriteString("interrupted"); err != nil {
			t.Fatal(err)
		}
		if err := stage.Sync(); err != nil {
			t.Fatal(err)
		}
		fmt.Println("staged")
		select {} // Parent kills the writer while its transaction lock is held.
	}
	key, err := LoadOrCreateNoiseKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveNoisePin(dir, mode, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	if err := SaveCachedPSK(dir, mode, key.Private); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, mode+".result"), []byte(hex.EncodeToString(key.Public)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func noiseChild(t *testing.T, dir, mode string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestNoiseStoreChild$")
	cmd.Env = append(os.Environ(), "THALOVANT_TEST_NOISE_DIR="+dir, "THALOVANT_TEST_NOISE_MODE="+mode)
	return cmd
}

func TestNoiseStoreIndependentProcesses(t *testing.T) {
	dir := t.TempDir()
	var children []*exec.Cmd
	for i := 0; i < 8; i++ {
		cmd := noiseChild(t, dir, fmt.Sprintf("hub-%d", i))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, cmd)
	}
	for _, cmd := range children {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	var first string
	for i := range children {
		node := fmt.Sprintf("hub-%d", i)
		key, err := os.ReadFile(filepath.Join(dir, node+".result"))
		if err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = string(key)
		}
		if string(key) != first {
			t.Fatal("concurrent processes published different identities")
		}
		pin, err := LoadNoisePin(dir, node)
		if err != nil || pin != strings.Repeat("ab", 32) {
			t.Fatalf("lost pin for %s: %v", node, err)
		}
		if len(LoadCachedPSK(dir, node)) != 32 {
			t.Fatalf("lost derived cache for %s", node)
		}
	}
	if err := SaveNoisePin(dir, "hub-0", strings.Repeat("cd", 32)); err == nil {
		t.Fatal("conflicting pin replaced established trust")
	}
	pin, err := LoadNoisePin(dir, "hub-0")
	if err != nil || pin != strings.Repeat("ab", 32) {
		t.Fatal("conflict changed established pin")
	}
}

func TestNoiseStoreKilledWriterPreservesTrust(t *testing.T) {
	dir := t.TempDir()
	key, err := LoadOrCreateNoiseKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveNoisePin(dir, "existing", strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	cmd := noiseChild(t, dir, "crash")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan bool, 1)
	go func() { scanner := bufio.NewScanner(stdout); ready <- scanner.Scan() && scanner.Text() == "staged" }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child did not stage")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child staging timed out")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	after, err := LoadOrCreateNoiseKey(dir)
	if err != nil || hex.EncodeToString(after.Public) != hex.EncodeToString(key.Public) {
		t.Fatal("crash replaced identity", err)
	}
	pin, err := LoadNoisePin(dir, "existing")
	if err != nil || pin != strings.Repeat("ab", 32) {
		t.Fatal("crash changed pin", err)
	}
}

func TestNoiseStoreRejectsNullPinsAndDoesNotPublishOverKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, NoiseKeyFilename)
	if err := publishNoiseFile(path, []byte("existing"), false); err != nil {
		t.Fatal(err)
	}
	if err := publishNoiseFile(path, []byte("replacement"), false); !os.IsExist(err) {
		t.Fatal("static key overwrite was allowed", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "existing" {
		t.Fatal("original key changed", err)
	}
	if err := os.WriteFile(filepath.Join(dir, NoisePinsFilename), []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveNoisePin(dir, "hub", "key"); err == nil {
		t.Fatal("null trust store silently replaced")
	}
}

func TestNoiseStateRejectsSymlinkTrustAndLockFiles(t *testing.T) {
	for _, name := range []string{NoiseKeyFilename, NoisePinsFilename, ".noise.lock"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Skipf("symbolic links unavailable: %v", err)
			}
			var err error
			if name == NoisePinsFilename {
				_, err = LoadNoisePin(dir, "node")
			} else {
				_, err = LoadOrCreateNoiseKey(dir)
			}
			if err == nil {
				t.Fatal("unexpected trust path accepted")
			}
			raw, err := os.ReadFile(target)
			if err != nil || string(raw) != "{}" {
				t.Fatal("link target was changed")
			}
		})
	}
}
