package thalovant

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flynn/noise"
)

// The vectors below were produced by the reference implementation
// (poorman-handshake 2.0.0a3 / hivemind-bus-client 1.1.1a1) and pinned here.
// They are the interop contract: a peer whose PSK, canonical JSON or prologue
// bytes differ by one byte fails the handshake in exactly the same way as a
// wrong password, so a same-language round-trip test would prove nothing.

func TestDerivePSKMatchesReferenceVectors(t *testing.T) {
	cases := []struct {
		name     string
		password string
		nodeID   string
		want     string
	}{
		{"typical", "Tr0ub4dor-Horse-Battery-91x", "node-alpha", "ce6825b343771aed1833233c8d1af4ce4e470cee89b625065402def527f900ce"},
		{"empty inputs", "", "", "38bedc40c2ce3b79cd5ccf53745e029363f5ba1948cb21f018bcce6eb0869876"},
		{"non-ascii", "passé-wörd", "hub-ümläut", "988418601dbad183fbd6116e7981e9ab8ffe93be3f3f45c27eb0b70c325f9cd8"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := hex.EncodeToString(derivePSK(testCase.password, testCase.nodeID))
			if got != testCase.want {
				t.Fatalf("PSK for (%q, %q) = %s, reference says %s", testCase.password, testCase.nodeID, got, testCase.want)
			}
		})
	}
}

func TestDerivePSKIsSaltedByNodeID(t *testing.T) {
	first := derivePSK("same-password", "hub-one")
	second := derivePSK("same-password", "hub-two")
	if bytes.Equal(first, second) {
		t.Fatal("the same password produced the same PSK for two different hubs; the node id salt is not being applied")
	}
}

func TestCanonicalJSONMatchesReferenceVectors(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"keys are sorted and whitespace stripped",
			`{"b": 1, "a": [1, 2], "c": {"z": true, "y": null}}`,
			`{"a":[1,2],"b":1,"c":{"y":null,"z":true}}`,
		},
		{
			"the message 1 payload",
			`{"binarize": false, "encodings": []}`,
			`{"binarize":false,"encodings":[]}`,
		},
		{
			"quotes, backslashes and control characters escape",
			`{"s": "quote\" back\\slash\nnewline\ttab", "ctrl": "\u0001\u001f"}`,
			`{"ctrl":"\u0001\u001f","s":"quote\" back\\slash\nnewline\ttab"}`,
		},
		{
			// encoding/json would escape < > & here and produce different
			// prologue bytes than the reference implementation.
			"non-ascii and HTML characters stay literal",
			`{"unicode": "café über 日本", "amp": "a<b>c&d"}`,
			`{"amp":"a<b>c&d","unicode":"café über 日本"}`,
		},
		{
			"nesting and integers round-trip",
			`{"nested": {"deep": [{"k": "v"}, 2, null]}, "num": 3}`,
			`{"nested":{"deep":[{"k":"v"},2,null]},"num":3}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decoded, err := decodeJSONNumbers([]byte(testCase.input))
			if err != nil {
				t.Fatal(err)
			}
			got, err := canonicalJSON(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != testCase.want {
				t.Fatalf("canonicalJSON = %s, reference says %s", got, testCase.want)
			}
		})
	}
}

func TestCanonicalJSONKeepsIntegersIntegral(t *testing.T) {
	// Decoding without UseNumber turns 3 into float64(3), and Go renders that
	// as "3" while Python renders the float as "3.0" -- so the decode path has
	// to keep the original literal.
	decoded, err := decodeJSONNumbers([]byte(`{"max_protocol_version": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["max_protocol_version"].(json.Number); !ok {
		t.Fatalf("expected the number to survive as json.Number, got %T", decoded["max_protocol_version"])
	}
}

func TestBuildPrologueMatchesReferenceVector(t *testing.T) {
	const want = "7b226e6f64655f6964223a226e6f64652d616c706861222c227075626b6579223a222d2d2d2d2d424547494e205055424c4943204b45592d2d2d2d2d5c6e6162635c6e2d2d2d2d2d454e44205055424c4943204b45592d2d2d2d2d227d7b226d61785f70726f746f636f6c5f76657273696f6e223a332c226e6f697365223a7b227061747465726e73223a5b22585870736b32225d2c22737569746573223a5b2232353531395f436861436861506f6c795f534841323536225d7d7d4e6f6973655f585870736b325f32353531395f436861436861506f6c795f534841323536"

	hello, err := decodeJSONNumbers([]byte(`{"node_id": "node-alpha", "pubkey": "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----"}`))
	if err != nil {
		t.Fatal(err)
	}
	handshake, err := decodeJSONNumbers([]byte(`{"max_protocol_version": 3, "noise": {"patterns": ["XXpsk2"], "suites": ["25519_ChaChaPoly_SHA256"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	prologue, err := buildPrologue(hello, handshake, noiseProtocolName(noisePatternXX, "25519_ChaChaPoly_SHA256"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(prologue); got != want {
		t.Fatalf("prologue = %s\nreference  = %s", got, want)
	}
}

func TestNoiseProtocolName(t *testing.T) {
	if got := noiseProtocolName(noisePatternXX, "25519_ChaChaPoly_SHA256"); got != "Noise_XXpsk2_25519_ChaChaPoly_SHA256" {
		t.Fatalf("unexpected protocol name %q", got)
	}
}

func TestSelectNoiseOptions(t *testing.T) {
	both := []string{noisePatternKK, noisePatternXX}
	cases := []struct {
		name        string
		patterns    []string
		suites      []string
		pinned      string
		wantPattern string
		wantSuite   string
		wantOK      bool
	}{
		{
			name:        "no pin means first contact",
			patterns:    both,
			suites:      []string{"25519_ChaChaPoly_SHA256"},
			wantPattern: noisePatternXX,
			wantSuite:   "25519_ChaChaPoly_SHA256",
			wantOK:      true,
		},
		{
			name:        "a pinned key upgrades to KK when the hub offers it",
			patterns:    both,
			suites:      []string{"25519_ChaChaPoly_SHA256"},
			pinned:      strings.Repeat("ab", 32),
			wantPattern: noisePatternKK,
			wantSuite:   "25519_ChaChaPoly_SHA256",
			wantOK:      true,
		},
		{
			name:        "a pinned key still falls back when KK is not offered",
			patterns:    []string{noisePatternXX},
			suites:      []string{"25519_ChaChaPoly_SHA256"},
			pinned:      strings.Repeat("ab", 32),
			wantPattern: noisePatternXX,
			wantSuite:   "25519_ChaChaPoly_SHA256",
			wantOK:      true,
		},
		{
			// The walk is over our list, not the hub's, so ChaChaPoly wins
			// whenever both sides have it however the hub ordered them.
			name:        "our suite preference wins over the hub's ordering",
			patterns:    []string{noisePatternXX},
			suites:      []string{"25519_AESGCM_SHA256", "25519_ChaChaPoly_SHA256"},
			wantPattern: noisePatternXX,
			wantSuite:   "25519_ChaChaPoly_SHA256",
			wantOK:      true,
		},
		{
			name:        "AESGCM is taken when it is all the hub has",
			patterns:    []string{noisePatternXX},
			suites:      []string{"25519_AESGCM_SHA256"},
			wantPattern: noisePatternXX,
			wantSuite:   "25519_AESGCM_SHA256",
			wantOK:      true,
		},
		{
			name:     "no mutual suite",
			patterns: []string{noisePatternXX},
			suites:   []string{"448_ChaChaPoly_BLAKE2b"},
		},
		{
			name:     "no mutual pattern",
			patterns: []string{"NNpsk0"},
			suites:   []string{"25519_ChaChaPoly_SHA256"},
		},
		{
			name:     "hub advertised nothing",
			patterns: nil,
			suites:   nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pattern, suite, ok := selectNoiseOptions(testCase.patterns, testCase.suites, testCase.pinned)
			if ok != testCase.wantOK || pattern != testCase.wantPattern || suite != testCase.wantSuite {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", pattern, suite, ok, testCase.wantPattern, testCase.wantSuite, testCase.wantOK)
			}
		})
	}
}

// newTestHandshakePair completes a handshake between the SDK's initiator and a
// responder built straight from flynn/noise, so the tests exercise the real
// client side against an independent peer rather than against itself.
func newTestHandshakePair(t *testing.T, pattern, suite string, psk, prologue []byte) (*noiseHandshake, *noiseHandshake) {
	t.Helper()

	cipherSuite, err := noiseCipherSuite(suite)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	initiator, err := newNoiseHandshake(pattern, suite, psk, prologue, clientKey, "")
	if err != nil {
		t.Fatal(err)
	}
	responderState, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           cipherSuite,
		Random:                rand.Reader,
		Pattern:               noise.HandshakeXX,
		Initiator:             false,
		Prologue:              prologue,
		PresharedKey:          psk,
		PresharedKeyPlacement: 2,
		StaticKeypair:         serverKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	responder := &noiseHandshake{state: responderState, pattern: pattern, suite: suite}

	message1, err := initiator.writeMessage([]byte("payload-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := responder.readMessage(message1); err != nil {
		t.Fatal(err)
	}
	message2, err := responder.writeMessage(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initiator.readMessage(message2); err != nil {
		t.Fatal(err)
	}
	message3, err := initiator.writeMessage(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := responder.readMessage(message3); err != nil {
		t.Fatal(err)
	}
	if !initiator.complete || !responder.complete {
		t.Fatal("the handshake did not finish after three messages")
	}
	if initiator.remoteStaticKey() == "" {
		t.Fatal("XXpsk2 finished without learning the peer static key; there would be nothing to pin")
	}
	return initiator, responder
}

// newTestSessionPair completes a real XXpsk2 handshake between two in-process
// peers and returns the two resulting sessions, so the framing tests run over
// genuine cipher states rather than a stub.
func newTestSessionPair(t *testing.T) (*noiseSession, *noiseSession) {
	t.Helper()
	initiator, responder := newTestHandshakePair(t, noisePatternXX, "25519_ChaChaPoly_SHA256", derivePSK("shared", "hub"), []byte("prologue"))

	clientSession, err := newNoiseSession(initiator)
	if err != nil {
		t.Fatal(err)
	}
	serverSession, err := newNoiseSession(responder)
	if err != nil {
		t.Fatal(err)
	}
	// The responder's send state is the initiator's receive state, so swap them
	// to make the server session speak toward the client.
	serverSession.send, serverSession.recv = serverSession.recv, serverSession.send
	return clientSession, serverSession
}

func TestNoiseSessionRoundTripsASingleFrame(t *testing.T) {
	client, server := newTestSessionPair(t)

	var frames [][]byte
	if err := client.sendMessage([]byte(`{"msg_type":"bus"}`), true, func(frame []byte) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("a small message produced %d frames, want 1", len(frames))
	}

	payload, isJSON, complete, err := server.decryptFrame(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if !complete || !isJSON || string(payload) != `{"msg_type":"bus"}` {
		t.Fatalf("got (%q, json=%v, complete=%v)", payload, isJSON, complete)
	}
}

func TestNoiseSessionChunksAndReassemblesAnOversizeMessage(t *testing.T) {
	client, server := newTestSessionPair(t)

	// Two and a half chunks, so the sequence is FIRST, MORE, LAST.
	original := bytes.Repeat([]byte("x"), noiseChunkSize*2+1024)

	var frames [][]byte
	if err := client.sendMessage(original, true, func(frame []byte) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames for a two-and-a-bit chunk message, want 3", len(frames))
	}

	var assembled []byte
	for index, frame := range frames {
		payload, isJSON, complete, err := server.decryptFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		if index < len(frames)-1 {
			if complete {
				t.Fatalf("frame %d reported a complete message mid-sequence", index)
			}
			continue
		}
		if !complete || !isJSON {
			t.Fatalf("the final frame reported complete=%v json=%v", complete, isJSON)
		}
		assembled = payload
	}
	if !bytes.Equal(assembled, original) {
		t.Fatalf("reassembled %d bytes, sent %d", len(assembled), len(original))
	}
}

func TestNoiseSessionMarksBinaryFrames(t *testing.T) {
	client, server := newTestSessionPair(t)

	var frame []byte
	if err := client.sendMessage([]byte{0x0c, 0xff, 0x00}, false, func(f []byte) error {
		frame = f
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	payload, isJSON, complete, err := server.decryptFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if isJSON || !complete || !bytes.Equal(payload, []byte{0x0c, 0xff, 0x00}) {
		t.Fatalf("got (% x, json=%v, complete=%v)", payload, isJSON, complete)
	}
}

func TestNoiseSessionRejectsATamperedFrame(t *testing.T) {
	client, server := newTestSessionPair(t)

	var frame []byte
	if err := client.sendMessage([]byte(`{"a":1}`), true, func(f []byte) error {
		frame = f
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	frame[len(frame)-1] ^= 0xff

	if _, _, _, err := server.decryptFrame(frame); err == nil {
		t.Fatal("a tampered frame decrypted; the AEAD tag is not being enforced")
	} else if !errors.Is(err, ErrConnection) {
		t.Fatalf("tampering reported as %v, want an ErrConnection", err)
	}
}

func TestNoiseSessionRejectsAReplayedFrame(t *testing.T) {
	client, server := newTestSessionPair(t)

	var frames [][]byte
	send := func(f []byte) error {
		frames = append(frames, f)
		return nil
	}
	if err := client.sendMessage([]byte(`{"n":1}`), true, send); err != nil {
		t.Fatal(err)
	}
	if err := client.sendMessage([]byte(`{"n":2}`), true, send); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := server.decryptFrame(frames[0]); err != nil {
		t.Fatal(err)
	}
	// The nonce counter has advanced, so the first frame must not decrypt a
	// second time.
	if _, _, _, err := server.decryptFrame(frames[0]); err == nil {
		t.Fatal("a replayed frame decrypted; the nonce counter is not advancing")
	}
}

func TestNoiseSessionRejectsAMalformedChunkSequence(t *testing.T) {
	client, server := newTestSessionPair(t)

	// A middle chunk with no message open.
	orphan, err := client.send.Encrypt(nil, nil, append([]byte{frameMore}, []byte("body")...))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := server.decryptFrame(orphan); err == nil {
		t.Fatal("a middle chunk was accepted with no chunked message open")
	}
}

func TestNoiseSessionRejectsAnUnknownMarker(t *testing.T) {
	client, server := newTestSessionPair(t)

	frame, err := client.send.Encrypt(nil, nil, []byte{0x7f, 'x'})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = server.decryptFrame(frame)
	if err == nil || !strings.Contains(err.Error(), "unknown v3 frame marker") {
		t.Fatalf("unknown marker reported as %v", err)
	}
}

func TestNoiseSessionCapsReassembly(t *testing.T) {
	client, server := newTestSessionPair(t)
	server.reassembly = make([]byte, noiseMaxReassembly)

	frame, err := client.send.Encrypt(nil, nil, append([]byte{frameMore}, bytes.Repeat([]byte("y"), 64)...))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := server.decryptFrame(frame); err == nil {
		t.Fatal("reassembly grew past the cap")
	}
	if server.reassembly != nil {
		t.Fatal("the buffer was kept after the cap was hit; the memory is not released")
	}
}

func TestNoiseKeyPersistsAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateNoiseKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateNoiseKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Private, second.Private) || !bytes.Equal(first.Public, second.Public) {
		t.Fatal("a second call generated a new static key; every connection would look like a new peer")
	}

	info, err := os.Stat(filepath.Join(dir, NoiseKeyFilename))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("the static key file is %04o; it must not be group or world accessible", perm)
	}
}

func TestNoiseKeyRejectsAPermissiveFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateNoiseKey(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, NoiseKeyFilename)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateNoiseKey(dir); err == nil {
		t.Fatal("a world-readable static key file was accepted")
	}
}

func TestNoisePinsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	const nodeID = "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----"
	key := strings.Repeat("ab", 32)

	pinned, err := LoadNoisePin(dir, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if pinned != "" {
		t.Fatalf("an unseen hub reported a pin %q", pinned)
	}
	if err := SaveNoisePin(dir, nodeID, key); err != nil {
		t.Fatal(err)
	}
	if pinned, err = LoadNoisePin(dir, nodeID); err != nil || pinned != key {
		t.Fatalf("LoadNoisePin = (%q, %v), want the saved key", pinned, err)
	}
	if err := ForgetNoisePin(dir, nodeID); err != nil {
		t.Fatal(err)
	}
	if pinned, err = LoadNoisePin(dir, nodeID); err != nil || pinned != "" {
		t.Fatalf("the pin survived ForgetNoisePin: (%q, %v)", pinned, err)
	}
}

func TestNoisePinsKeepOtherHubs(t *testing.T) {
	dir := t.TempDir()
	if err := SaveNoisePin(dir, "hub-one", strings.Repeat("11", 32)); err != nil {
		t.Fatal(err)
	}
	if err := SaveNoisePin(dir, "hub-two", strings.Repeat("22", 32)); err != nil {
		t.Fatal(err)
	}
	if err := ForgetNoisePin(dir, "hub-one"); err != nil {
		t.Fatal(err)
	}
	remaining, err := LoadNoisePin(dir, "hub-two")
	if err != nil {
		t.Fatal(err)
	}
	if remaining != strings.Repeat("22", 32) {
		t.Fatalf("forgetting one hub lost another's pin: %q", remaining)
	}
}

func TestPinServerKeyRefusesAChangedKey(t *testing.T) {
	dir := t.TempDir()
	transport := NewWSSTransport(Identity{})
	transport.NoiseStateDir = dir

	if err := transport.pinServerKey("hub", strings.Repeat("aa", 32)); err != nil {
		t.Fatal(err)
	}
	// Same key again: a normal reconnect.
	if err := transport.pinServerKey("hub", strings.Repeat("aa", 32)); err != nil {
		t.Fatalf("reconnecting to the pinned key failed: %v", err)
	}
	// A different key is refused rather than silently re-pinned.
	err := transport.pinServerKey("hub", strings.Repeat("bb", 32))
	if err == nil {
		t.Fatal("a changed server key was accepted; pinning gives no protection")
	}
	if !errors.Is(err, ErrConnection) || !strings.Contains(err.Error(), "ForgetNoisePin") {
		t.Fatalf("the refusal does not tell the operator how to proceed: %v", err)
	}

	stored, err := LoadNoisePin(dir, "hub")
	if err != nil {
		t.Fatal(err)
	}
	if stored != strings.Repeat("aa", 32) {
		t.Fatalf("the stored pin was overwritten by the rejected key: %q", stored)
	}
}
