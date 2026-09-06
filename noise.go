package thalovant

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/flynn/noise"
	"golang.org/x/crypto/argon2"
)

// HiveMind protocol version that switches the handshake to Noise.
const protocolV3 = 3

// Registered handshake patterns, preference ordered.
const (
	noisePatternKK = "KKpsk0" // both static keys known in advance
	noisePatternXX = "XXpsk2" // first contact, trust on first use
)

// Registered cipher suites, in our preference order. The selection walks this
// list rather than the server's, so ChaChaPoly wins whenever both peers have
// it regardless of how the server ordered its advertisement.
var noiseSuites = []string{
	"25519_ChaChaPoly_SHA256",
	"25519_AESGCM_SHA256",
}

// argon2id parameters for the password -> PSK derivation. These are part of
// the wire contract: a peer that derives with different parameters produces a
// different PSK, and the handshake fails exactly as it would on a wrong
// password.
const (
	pskTimeCost   = 3
	pskMemoryKiB  = 64 * 1024 // 64 MiB
	pskLanes      = 1
	pskLengthByte = 32
)

// Transport frame markers. The first plaintext byte of every Noise transport
// message tags the inner framing so the receiver knows how to parse the rest.
const (
	frameJSON        byte = 0x00 // a complete UTF-8 JSON message
	frameBinary      byte = 0x01 // a complete binary frame
	frameFirstJSON   byte = 0x02 // first chunk of a chunked JSON message
	frameFirstBinary byte = 0x03 // first chunk of a chunked binary message
	frameMore        byte = 0x04 // a middle chunk
	frameLast        byte = 0x05 // the final chunk
)

// A Noise transport message caps at 65535 bytes. Chunking well below that
// leaves room for the AEAD tag, the marker, and any implementation overhead.
const noiseChunkSize = 65000

// Bounded reassembly budget: a chunked message may not accumulate more than
// this before the whole buffer is dropped, so one peer cannot force us to
// allocate without limit.
const noiseMaxReassembly = 32 * 1024 * 1024

// derivePSK stretches the shared site password into the 32-byte Noise
// pre-shared key, salted with SHA-256 of the *server's* node id. Both peers
// must derive from identical inputs.
//
// This costs 64 MiB and roughly 200ms, and the result is constant for a given
// (password, nodeID) pair, so callers that reconnect should derive once and
// keep it rather than paying for it on every attempt.
func derivePSK(password, nodeID string) []byte {
	salt := sha256.Sum256([]byte(nodeID))
	return argon2.IDKey([]byte(password), salt[:], pskTimeCost, pskMemoryKiB, pskLanes, pskLengthByte)
}

// noiseProtocolName is the full Noise name for a pattern and suite selection.
func noiseProtocolName(pattern, suite string) string {
	return "Noise_" + pattern + "_" + suite
}

// selectNoiseOptions picks the handshake pattern and suite from the server's
// advertised lists. KKpsk0 is chosen only when we already hold a pinned static
// key for this peer and the server offers it; otherwise XXpsk2. Reports false
// when there is no mutual option.
func selectNoiseOptions(serverPatterns, serverSuites []string, pinnedRemoteKey string) (string, string, bool) {
	suite := ""
	for _, candidate := range noiseSuites {
		if containsString(serverSuites, candidate) {
			suite = candidate
			break
		}
	}
	if suite == "" {
		return "", "", false
	}
	if pinnedRemoteKey != "" && containsString(serverPatterns, noisePatternKK) {
		return noisePatternKK, suite, true
	}
	if containsString(serverPatterns, noisePatternXX) {
		return noisePatternXX, suite, true
	}
	return "", "", false
}

func containsString(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// canonicalJSON serializes a decoded JSON value the way the reference
// implementation does: sorted keys, no whitespace, and no ASCII escaping of
// non-ASCII runes.
//
// Both peers must produce identical prologue bytes, so this cannot go through
// encoding/json: that escapes <, >, & and U+2028/U+2029, which Python's
// json.dumps does not. Numbers are emitted from their original literal text
// (decode with UseNumber) so an integer stays an integer.
func canonicalJSON(value any) ([]byte, error) {
	var out strings.Builder
	if err := writeCanonical(&out, value); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

func writeCanonical(out *strings.Builder, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case json.Number:
		out.WriteString(typed.String())
	case float64:
		// Only reachable when a caller decoded without UseNumber. Go and
		// Python agree on the shortest round-trip form for values that are
		// not whole, and a whole float renders without the trailing ".0"
		// Python would emit -- so keep numbers as json.Number wherever the
		// bytes have to match.
		out.WriteString(strconv.FormatFloat(typed, 'g', -1, 64))
	case string:
		writeCanonicalString(out, typed)
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			writeCanonicalString(out, key)
			out.WriteByte(':')
			if err := writeCanonical(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("%w: cannot canonicalize %T for the Noise prologue", ErrProtocol, value)
	}
	return nil
}

// writeCanonicalString escapes exactly what json.dumps(ensure_ascii=False)
// escapes: the quote, the backslash, and control characters below 0x20.
// Everything else, including non-ASCII, is written literally.
func writeCanonicalString(out *strings.Builder, value string) {
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(out, `\u%04x`, r)
			} else if r == utf8.RuneError {
				// A lone surrogate or invalid byte; emit the replacement
				// character rather than raw invalid UTF-8.
				out.WriteRune(utf8.RuneError)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

// buildPrologue binds, in order, the server's cleartext HELLO payload, its
// cleartext parameter HANDSHAKE payload, and the selected protocol name. Both
// peers must supply identical bytes: this is what makes tampering with the
// negotiation abort the handshake instead of silently downgrading it.
func buildPrologue(helloPayload, handshakePayload map[string]any, protocolName string) ([]byte, error) {
	hello, err := canonicalJSON(helloPayload)
	if err != nil {
		return nil, err
	}
	handshake, err := canonicalJSON(handshakePayload)
	if err != nil {
		return nil, err
	}
	prologue := make([]byte, 0, len(hello)+len(handshake)+len(protocolName))
	prologue = append(prologue, hello...)
	prologue = append(prologue, handshake...)
	prologue = append(prologue, protocolName...)
	return prologue, nil
}

func noiseCipherSuite(suite string) (noise.CipherSuite, error) {
	switch suite {
	case "25519_ChaChaPoly_SHA256":
		return noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256), nil
	case "25519_AESGCM_SHA256":
		return noise.NewCipherSuite(noise.DH25519, noise.CipherAESGCM, noise.HashSHA256), nil
	default:
		return nil, fmt.Errorf("%w: unsupported Noise cipher suite %q", ErrProtocol, suite)
	}
}

// noiseHandshake drives the handshake for one connection.
type noiseHandshake struct {
	state    *noise.HandshakeState
	pattern  string
	suite    string
	send     *noise.CipherState
	recv     *noise.CipherState
	complete bool
}

// newNoiseHandshake initializes the initiator side of a v3 handshake.
//
// staticKey is this client's persistent X25519 keypair; regenerating it on
// every start would make each connection look like a new peer and defeat
// pinning in both directions. pinnedRemoteKey is the hex-encoded server static
// key, required for KKpsk0 and ignored otherwise.
func newNoiseHandshake(pattern, suite string, psk []byte, prologue []byte, staticKey noise.DHKey, pinnedRemoteKey string) (*noiseHandshake, error) {
	cipherSuite, err := noiseCipherSuite(suite)
	if err != nil {
		return nil, err
	}
	config := noise.Config{
		CipherSuite:   cipherSuite,
		Random:        rand.Reader,
		Initiator:     true,
		Prologue:      prologue,
		PresharedKey:  psk,
		StaticKeypair: staticKey,
	}
	switch pattern {
	case noisePatternXX:
		config.Pattern = noise.HandshakeXX
		config.PresharedKeyPlacement = 2
	case noisePatternKK:
		config.Pattern = noise.HandshakeKK
		config.PresharedKeyPlacement = 0
		peer, err := hex.DecodeString(pinnedRemoteKey)
		if err != nil || len(peer) != 32 {
			return nil, fmt.Errorf("%w: KKpsk0 needs a pinned 32-byte server static key", ErrProtocol)
		}
		config.PeerStatic = peer
	default:
		return nil, fmt.Errorf("%w: unsupported Noise pattern %q", ErrProtocol, pattern)
	}
	state, err := noise.NewHandshakeState(config)
	if err != nil {
		return nil, fmt.Errorf("%w: could not start %s: %v", ErrProtocol, noiseProtocolName(pattern, suite), err)
	}
	return &noiseHandshake{state: state, pattern: pattern, suite: suite}, nil
}

// writeMessage produces the next outgoing handshake message.
func (h *noiseHandshake) writeMessage(payload []byte) ([]byte, error) {
	message, cs0, cs1, err := h.state.WriteMessage(nil, payload)
	if err != nil {
		return nil, fmt.Errorf("%w: Noise handshake write failed: %v", ErrConnection, err)
	}
	h.adopt(cs0, cs1)
	return message, nil
}

// readMessage consumes an incoming handshake message and returns its payload.
//
// A failure here is authentication failing: a wrong password (PSK mismatch), a
// tampered negotiation (prologue mismatch), or a static key contradicting the
// pinned one. It is fatal; the connection must be rejected rather than retried
// on weaker terms.
func (h *noiseHandshake) readMessage(message []byte) ([]byte, error) {
	payload, cs0, cs1, err := h.state.ReadMessage(nil, message)
	if err != nil {
		return nil, fmt.Errorf("%w: Noise handshake authentication failed (wrong password or tampered negotiation): %v", ErrConnection, err)
	}
	h.adopt(cs0, cs1)
	return payload, nil
}

// adopt records the transport cipher states once Split() has happened. For the
// initiator the first state encrypts outbound traffic and the second decrypts
// inbound.
func (h *noiseHandshake) adopt(cs0, cs1 *noise.CipherState) {
	if cs0 == nil || cs1 == nil {
		return
	}
	h.send, h.recv, h.complete = cs0, cs1, true
}

// remoteStaticKey is the server's static public key, hex encoded. Under XXpsk2
// it is learned during the handshake and is what the client pins.
func (h *noiseHandshake) remoteStaticKey() string {
	peer := h.state.PeerStatic()
	if len(peer) == 0 {
		return ""
	}
	return hex.EncodeToString(peer)
}

// noiseSession is a completed v3 session: the two transport cipher states plus
// the HiveMind frame markers that distinguish JSON from binary after
// decryption.
type noiseSession struct {
	send            *noise.CipherState
	recv            *noise.CipherState
	remoteStaticKey string

	sendMu sync.Mutex
	recvMu sync.Mutex

	// open chunked reassembly; nil when no message is in progress
	reassembly []byte
	isJSON     bool
}

func newNoiseSession(handshake *noiseHandshake) (*noiseSession, error) {
	if !handshake.complete {
		return nil, fmt.Errorf("%w: Noise handshake is not finished", ErrConnection)
	}
	return &noiseSession{
		send:            handshake.send,
		recv:            handshake.recv,
		remoteStaticKey: handshake.remoteStaticKey(),
	}, nil
}

// sendMessage encrypts one message and hands each resulting Noise transport
// message to rawSend, chunking when the payload does not fit in one.
//
// The whole send holds the lock so every chunk of one message is encrypted and
// put on the wire contiguously: the cipher state nonce counter is strictly
// sequential, so interleaving two messages would break decryption at the
// receiver.
func (s *noiseSession) sendMessage(payload []byte, isJSON bool, rawSend func([]byte) error) error {
	single, first := frameBinary, frameFirstBinary
	if isJSON {
		single, first = frameJSON, frameFirstJSON
	}

	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	if len(payload) <= noiseChunkSize {
		frame, err := s.send.Encrypt(nil, nil, append([]byte{single}, payload...))
		if err != nil {
			return fmt.Errorf("%w: Noise transport encryption failed: %v", ErrConnection, err)
		}
		return rawSend(frame)
	}

	lastOffset := len(payload) - noiseChunkSize
	for offset := 0; offset < len(payload); offset += noiseChunkSize {
		end := offset + noiseChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		marker := frameMore
		switch {
		case offset == 0:
			marker = first
		case offset >= lastOffset:
			marker = frameLast
		}
		frame, err := s.send.Encrypt(nil, nil, append([]byte{marker}, payload[offset:end]...))
		if err != nil {
			return fmt.Errorf("%w: Noise transport encryption failed: %v", ErrConnection, err)
		}
		if err := rawSend(frame); err != nil {
			return err
		}
	}
	return nil
}

// decryptFrame decrypts one incoming Noise transport message.
//
// A complete frame is returned immediately. A chunked message is reassembled
// across calls: the first and middle chunks buffer and report complete=false,
// and the final chunk returns the whole message. Callers must not dispatch a
// frame reported incomplete.
//
// Every error here is fatal for the session. A message that fails to decrypt
// at the current counter means tampering, replay, or reordering, and so does a
// malformed chunk sequence; the connection must be dropped rather than the
// frame skipped.
func (s *noiseSession) decryptFrame(data []byte) (payload []byte, isJSON bool, complete bool, err error) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()

	plaintext, err := s.recv.Decrypt(nil, nil, data)
	if err != nil {
		return nil, false, false, fmt.Errorf("%w: Noise transport message rejected (tampered, replayed or out-of-order): %v", ErrConnection, err)
	}
	if len(plaintext) == 0 {
		return nil, false, false, fmt.Errorf("%w: empty Noise transport message", ErrConnection)
	}
	marker, body := plaintext[0], plaintext[1:]

	switch marker {
	case frameJSON, frameBinary:
		if s.reassembly != nil {
			buffered := len(s.reassembly)
			s.resetReassembly()
			return nil, false, false, fmt.Errorf("%w: a complete frame arrived while %d bytes of a chunked message were still buffered", ErrConnection, buffered)
		}
		return body, marker == frameJSON, true, nil

	case frameFirstJSON, frameFirstBinary:
		if s.reassembly != nil {
			buffered := len(s.reassembly)
			s.resetReassembly()
			return nil, false, false, fmt.Errorf("%w: a new chunked message started while %d bytes of a previous one were still buffered", ErrConnection, buffered)
		}
		s.reassembly = append([]byte(nil), body...)
		s.isJSON = marker == frameFirstJSON
		if err := s.guardReassemblyCap(); err != nil {
			return nil, false, false, err
		}
		return nil, false, false, nil

	case frameMore:
		if s.reassembly == nil {
			return nil, false, false, fmt.Errorf("%w: a middle chunk arrived with no chunked message open", ErrConnection)
		}
		s.reassembly = append(s.reassembly, body...)
		if err := s.guardReassemblyCap(); err != nil {
			return nil, false, false, err
		}
		return nil, false, false, nil

	case frameLast:
		if s.reassembly == nil {
			return nil, false, false, fmt.Errorf("%w: a final chunk arrived with no chunked message open", ErrConnection)
		}
		s.reassembly = append(s.reassembly, body...)
		if err := s.guardReassemblyCap(); err != nil {
			return nil, false, false, err
		}
		buffered, wasJSON := s.reassembly, s.isJSON
		s.resetReassembly()
		return buffered, wasJSON, true, nil

	default:
		return nil, false, false, fmt.Errorf("%w: unknown v3 frame marker 0x%02x", ErrConnection, marker)
	}
}

func (s *noiseSession) resetReassembly() {
	s.reassembly = nil
	s.isJSON = false
}

func (s *noiseSession) guardReassemblyCap() error {
	if len(s.reassembly) <= noiseMaxReassembly {
		return nil
	}
	buffered := len(s.reassembly)
	s.resetReassembly()
	return fmt.Errorf("%w: chunked reassembly exceeded the %d byte cap (%d buffered); dropping the message", ErrConnection, noiseMaxReassembly, buffered)
}
