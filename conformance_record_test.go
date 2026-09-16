package thalovant

// Record what this SDK produced for each conformance case.
//
// The parity gate can check that a test *names* a vector file. It cannot check
// that the test ran it: a name reaching a loader call is evidence of intent,
// not of execution. So the gate stopped asking about the test and started
// asking about its output -- this writes what we computed, and the checker
// compares it against what the Python reference computed for the same case.
//
// The digest has to agree across languages, so it is deliberately the same
// recipe as the reference's tests/conformance_record.py: JSON with keys sorted
// at every depth, no insignificant whitespace, non-ASCII left as itself,
// SHA-256 of the UTF-8 bytes, and a whole number spelled without a fractional
// part. encoding/json already sorts map keys and already writes a float64 of 1
// as "1"; what it does not do by default is leave "<", ">" and "&" alone, and
// that escaping would be this language's spelling of a value rather than the
// value.
//
// Set THALOVANT_CONFORMANCE_OUT to a path and run the suite; the results are
// written when the package's tests finish.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

var (
	conformanceMutex   sync.Mutex
	conformanceResults = map[string]map[string]string{}
)

// canonicalDigest is the cross-language digest of a produced value.
func canonicalDigest(value any) string {
	if raw, ok := value.([]byte); ok {
		return "bytes:" + hex.EncodeToString(sha256Sum(raw))
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// Without this, "a & b" is written as "a & b" and no other language
	// agrees with us about what we produced.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		panic("conformance: " + err.Error())
	}
	// Encode appends a newline; the reference hashes the document alone.
	return hex.EncodeToString(sha256Sum(bytes.TrimRight(buffer.Bytes(), "\n")))
}

// absentIfEmpty spells "no value" the way the vectors spell it.
//
// Go has no optional string, so this SDK carries an absent utterance, language
// or file name as "". Every other SDK carries it as null, and that is what the
// vectors expect -- "an empty name is no name" is a case in binary-vectors.json
// precisely because the empty string is not a name. No case in those vectors
// ever expects a present-but-empty string, so the two spellings cannot be
// confused here.
func absentIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func sha256Sum(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}

// recordConformance records what this SDK produced for one case.
func recordConformance(t *testing.T, vectorFile, name string, produced any) {
	t.Helper()
	if os.Getenv("THALOVANT_CONFORMANCE_OUT") == "" {
		return
	}
	digest := canonicalDigest(produced)
	conformanceMutex.Lock()
	defer conformanceMutex.Unlock()
	cases, ok := conformanceResults[vectorFile]
	if !ok {
		cases = map[string]string{}
		conformanceResults[vectorFile] = cases
	}
	if previous, seen := cases[name]; seen && previous != digest {
		t.Fatalf("%s/%s: recorded twice with different outputs", vectorFile, name)
	}
	cases[name] = digest
}

// writeConformance is called once the package's tests have run. Every test
// file here is one package and so one process, which is why this needs no
// merging across shards the way the Node and Rust recorders do.
func writeConformance() {
	target := os.Getenv("THALOVANT_CONFORMANCE_OUT")
	if target == "" {
		return
	}
	// Written even when nothing ran. Leaving the old file alone would let a
	// suite that executed no case at all present last week's artifact as this
	// run's output, which is the hole this mechanism closes.
	results := map[string]any{}
	for vectorFile, cases := range conformanceResults {
		raw, err := os.ReadFile("testdata/" + vectorFile)
		if err != nil {
			panic("conformance: " + err.Error())
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			panic("conformance: " + err.Error())
		}
		names := make([]string, 0, len(cases))
		for name := range cases {
			names = append(names, name)
		}
		sort.Strings(names)
		recorded := map[string]string{}
		for _, name := range names {
			recorded[name] = cases[name]
		}
		// The parsed JSON, not the bytes: a vendored copy is allowed to differ
		// in indentation and line endings, and the checker accepts it on the
		// same terms.
		results[vectorFile] = map[string]any{
			"digest": canonicalDigest(parsed),
			"cases":  recorded,
		}
	}
	document := map[string]any{"schema_version": 1, "results": results}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		panic("conformance: " + err.Error())
	}
	if dir := filepath.Dir(target); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			panic("conformance: " + err.Error())
		}
	}
	if err := os.WriteFile(target, buffer.Bytes(), 0o644); err != nil {
		panic("conformance: " + err.Error())
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	writeConformance()
	os.Exit(code)
}
