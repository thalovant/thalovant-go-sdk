package thalovant

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
)

// noiseChannel implements the transport-independent v3 exchange. Its lock spans
// cipher advancement and delivery of every chunk, including the encrypted HELLO.
// A failed write poisons the channel: counters must never be reused after an
// uncertain delivery. A reconnect creates a fresh channel with the same key store.
type noiseChannel struct {
	mu        sync.Mutex
	identity  Identity
	stateDir  string
	hello     map[string]any
	nodeID    string
	handshake *noiseHandshake
	session   *noiseSession
	failed    bool
	write     func(context.Context, []byte, bool) error
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

func (n *noiseChannel) send(ctx context.Context, message HiveMessage) (err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failed || n.session == nil {
		return fmt.Errorf("%w: refusing to send before the v3 Noise session is established", ErrConnection)
	}
	defer func() {
		if err != nil {
			n.failed = true
		}
	}()
	return n.sendLocked(ctx, message)
}

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
	pinned, err := LoadNoisePin(n.stateDir, n.nodeID)
	if err != nil {
		return err
	}
	pattern, suite, ok := selectNoiseOptions(stringSlice(params["patterns"]), stringSlice(params["suites"]), pinned)
	if !ok {
		return fmt.Errorf("%w: no supported Noise pattern and suite", ErrConnection)
	}
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
		return err
	}
	if !n.handshake.complete {
		final, err := n.handshake.writeMessage(nil)
		if err != nil {
			return err
		}
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
