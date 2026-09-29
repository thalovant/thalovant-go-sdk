package thalovant

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// noiseChannel implements the transport-independent v3 exchange. Its lock spans
// cipher advancement and delivery of every chunk, including the encrypted HELLO.
// A failed write poisons the channel: counters must never be reused after an
// uncertain delivery. A reconnect creates a fresh channel with the same key store.
type noiseChannel struct {
	mu        noiseLock
	identity  Identity
	stateDir  string
	hello     map[string]any
	nodeID    string
	handshake *noiseHandshake
	session   *noiseSession
	failed    bool
	write     func(context.Context, []byte, bool) error
	// forceXX makes this channel choose XX whatever is pinned: the KK attempt
	// before it failed, and only XX tells a changed password from a changed
	// hub key. pattern is what the channel chose.
	forceXX bool
	pattern string
	// finalSent is whether this channel sent the last message of an XX
	// handshake, the one that shows the hub this client's static key; heard
	// is whether a frame from the hub has decrypted under the session's keys.
	finalSent bool
	heard     bool
}

// verdict reads the failure err that ended this channel's session: whether
// it was the hub refusing the credentials, and whether it was the hub
// refusing this client's own key. A refusal counts only while the hub has
// sent nothing that decrypted, since a hub that has spoken accepted the
// credentials; it is the client's key it refused when it came once this
// client had sent the last message of an XX handshake.
func (n *noiseChannel) verdict(err error) (refused, keyRejected bool) {
	if n == nil || err == nil || !errors.Is(err, ErrHubRefused) {
		return false, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.heard {
		return false, false
	}
	if errors.Is(err, ErrClientKeyRejected) {
		return true, true
	}
	return true, n.pattern == noisePatternXX && n.finalSent
}

// triedKK reports whether this channel's handshake was a KK attempt.
func (n *noiseChannel) triedKK() bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pattern == noisePatternKK
}

// retryKKWithXX reports whether a connect that failed with err should make
// its one XX attempt at once: the channel tried KK and the hub refused it, or
// its answer did not authenticate. It is no downgrade -- the pinned key is
// still checked when XX completes, so a hub that is not the pinned one still
// fails, as ErrHubKeyChanged.
func retryKKWithXX(ctx context.Context, channel *noiseChannel, err error) bool {
	return err != nil && ctx.Err() == nil && errors.Is(err, ErrHubRefused) && channel.triedKK()
}

func (n *noiseChannel) remoteKey() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.session == nil || n.failed {
		return ""
	}
	return n.session.remoteStaticKey
}

func (n *noiseChannel) ready() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.session != nil && !n.failed
}

// send encrypts and writes message. Only the wait for the channel is
// withdrawn when ctx ends: a send still queued behind another frame then
// returns errNothingSent, and the session is as it was. Once the channel is
// held the frame is written to the end, bounded by noiseSendTimeout alone,
// since half of one would break the Noise stream.
func (n *noiseChannel) send(ctx context.Context, message HiveMessage) error {
	return n.sendRetiring(ctx, message, nil)
}

// sendRetiring is send, with retire called when a frame that began to be
// written fails, after the channel's lock is released: whether or not its
// caller still waits, so a carrier retires its session for a write that
// failed after the caller left (errFinishing) as it does for one that failed
// in front of it.
func (n *noiseChannel) sendRetiring(ctx context.Context, message HiveMessage, retire func(error)) (err error) {
	if err := n.mu.LockContext(ctx); err != nil {
		return fmt.Errorf("%w: %w: %w", ErrTimeout, errNothingSent, err)
	}
	if n.failed || n.session == nil {
		n.mu.Unlock()
		return fmt.Errorf("%w: refusing to send before the v3 Noise session is established", ErrConnection)
	}
	return finishWithout(ctx, func() error {
		write, cancel := context.WithTimeout(context.WithoutCancel(ctx), noiseSendTimeout)
		defer cancel()
		return n.sendLocked(write, message)
	}, func(err error) {
		if err != nil {
			n.failed = true
		}
		n.mu.Unlock()
		if err != nil && retire != nil {
			retire(err)
		}
	})
}

// noiseSendTimeout bounds the physical write of one message once it has
// started.
const noiseSendTimeout = 20 * time.Second

func (n *noiseChannel) sendLocked(ctx context.Context, message HiveMessage) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return n.session.sendMessage(raw, true, func(frame []byte) error { return n.write(ctx, frame, true) })
}

func (n *noiseChannel) clear(ctx context.Context, payload map[string]any) error {
	raw, err := json.Marshal(HiveMessage{MsgType: "shake", Payload: map[string]any{"noise": payload}, Metadata: map[string]any{}, Route: []any{}})
	if err != nil {
		return err
	}
	return n.write(ctx, raw, false)
}

// receive accepts plaintext only during negotiation. MQTT lacks a binary flag;
// once the session exists every MQTT payload is necessarily a Noise frame.
func (n *noiseChannel) receive(ctx context.Context, raw []byte, binary bool) (message *HiveMessage, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failed {
		return nil, fmt.Errorf("%w: the Noise session has failed; reconnect required", ErrConnection)
	}
	defer func() {
		if err != nil {
			n.failed = true
		}
	}()
	if n.session != nil {
		if !binary {
			return nil, fmt.Errorf("%w: plaintext received after Noise negotiation", ErrConnection)
		}
		payload, isJSON, complete, decryptErr := n.session.decryptFrame(raw)
		if decryptErr != nil {
			return nil, decryptErr
		}
		// Any frame that decrypts counts, a chunk and the hub's encrypted
		// HELLO included.
		n.heard = true
		if !complete {
			return nil, nil
		}
		var decoded HiveMessage
		if isJSON {
			err = json.Unmarshal(payload, &decoded)
		} else {
			decoded, err = DecodeHiveBinaryFrame(payload)
		}
		if err != nil {
			return nil, err
		}
		return &decoded, nil
	}
	if binary {
		return nil, fmt.Errorf("%w: ciphertext received before Noise negotiation", ErrConnection)
	}
	decoded, err := decodeJSONNumbers(raw)
	if err != nil {
		return nil, err
	}
	payload := mapValue(decoded["payload"])
	switch decoded["msg_type"] {
	case "hello":
		if n.hello != nil || n.handshake != nil {
			return nil, fmt.Errorf("%w: duplicate Noise HELLO", ErrConnection)
		}
		n.nodeID, _ = payload["node_id"].(string)
		if n.nodeID == "" {
			return nil, fmt.Errorf("%w: Noise HELLO lacks node_id", ErrConnection)
		}
		n.hello = payload
		return nil, nil
	case "shake", "handshake":
		params, ok := payload["noise"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: the hub did not offer v3 Noise", ErrConnection)
		}
		if _, continuation := params["msg"]; !continuation {
			return nil, n.start(ctx, payload, params)
		}
		return nil, n.continueHandshake(ctx, params)
	default:
		return nil, fmt.Errorf("%w: application traffic received before Noise negotiation", ErrConnection)
	}
}

func (n *noiseChannel) start(ctx context.Context, offer, params map[string]any) error {
	if n.nodeID == "" || n.handshake != nil {
		return fmt.Errorf("%w: unexpected Noise capability offer", ErrConnection)
	}
	if n.identity.Password == "" {
		return fmt.Errorf("%w: v3 Noise requires the identity password", ErrIdentity)
	}
	// The first use of the identity file's directory takes the key this
	// identity had in the old default, once the hub it meets is known.
	n.stateDir = resolveNoiseStateDir(n.stateDir, n.identity, n.nodeID)
	pinned, err := LoadNoisePin(n.stateDir, n.nodeID)
	if err != nil {
		return err
	}
	choosing := pinned
	if n.forceXX {
		// The pin still decides the outcome: it is checked when XX completes.
		choosing = ""
	}
	pattern, suite, ok := selectNoiseOptions(stringSlice(params["patterns"]), stringSlice(params["suites"]), choosing)
	if !ok {
		return fmt.Errorf("%w: no supported Noise pattern and suite", ErrConnection)
	}
	n.pattern = pattern
	prologue, err := buildPrologue(n.hello, offer, noiseProtocolName(pattern, suite))
	if err != nil {
		return err
	}
	key, err := LoadOrCreateNoiseKey(n.stateDir)
	if err != nil {
		return err
	}
	// Derive from the current password, so a rotated credential cannot reuse a
	// persisted PSK belonging to an earlier password.
	n.handshake, err = newNoiseHandshake(pattern, suite, derivePSK(n.identity.Password, n.nodeID), prologue, key, pinned)
	if err != nil {
		return err
	}
	preferences, err := canonicalJSON(map[string]any{"binarize": false, "encodings": []any{}})
	if err != nil {
		return err
	}
	msg, err := n.handshake.writeMessage(preferences)
	if err != nil {
		return err
	}
	return n.clear(ctx, map[string]any{"pattern": pattern, "suite": suite, "msg": hex.EncodeToString(msg)})
}

func (n *noiseChannel) continueHandshake(ctx context.Context, params map[string]any) error {
	if n.handshake == nil {
		return fmt.Errorf("%w: Noise response arrived before negotiation", ErrConnection)
	}
	encoded, ok := params["msg"].(string)
	if !ok || encoded == "" {
		return fmt.Errorf("%w: malformed Noise envelope", ErrConnection)
	}
	msg, err := hex.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("%w: malformed Noise envelope", ErrConnection)
	}
	if _, err = n.handshake.readMessage(msg); err != nil {
		// A message that does not authenticate under the key this password
		// derives is the hub refusing the credentials: a wrong password.
		return fmt.Errorf("%w: %w: the hub's handshake answer did not authenticate under this connection's password: %v", ErrConnection, ErrHubRefused, err)
	}
	if !n.handshake.complete {
		final, err := n.handshake.writeMessage(nil)
		if err != nil {
			return err
		}
		// Marked before it goes out: the hub may read it and refuse the
		// request carrying it.
		n.finalSent = true
		if err := n.clear(ctx, map[string]any{"msg": hex.EncodeToString(final)}); err != nil {
			return err
		}
	}
	session, err := newNoiseSession(n.handshake)
	if err != nil {
		return err
	}
	if err := pinNoisePeer(n.stateDir, n.nodeID, session.remoteStaticKey); err != nil {
		return err
	}
	n.session, n.handshake = session, nil
	return n.sendLocked(ctx, helloHiveMessage(n.identity, "thalovant-go-"))
}
