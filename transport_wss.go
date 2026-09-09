package thalovant

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type WSSTransport struct {
	streams   runtimeStreams
	Identity  Identity
	UserAgent string

	// NoiseStateDir overrides where the persistent static key and the server
	// pin file live. Empty uses the directory holding the SDK config file.
	NoiseStateDir string

	BusEvents      chan Event
	HiveEvents     chan HiveMessage
	conn           *websocket.Conn
	connected      bool
	handshake      bool
	lastError      error
	connection     connectionTelemetry
	handshakeReady chan struct{}
	readDone       chan struct{}
	writeMu        contextMutex
	receiveMu      sync.Mutex
	attempt        *wssConnectionAttempt
	mu             sync.RWMutex

	// generation identifies one connection attempt. Disconnect does not wait
	// for readLoop to exit, so a loop from a previous attempt can still be
	// running when a reconnect begins; everything it writes back is gated on
	// its generation still being current.
	generation uint64

	// v3 Noise state, all guarded by mu
	serverHello    map[string]any
	noiseHandshake *noiseHandshake
	session        *noiseSession
	nodeID         string

	// derivePSK costs 64 MiB and roughly 200ms and its result is fixed for a
	// (password, node id) pair, so a reconnect to the same hub reuses it
	// instead of paying for it again. The password is part of the key: a caller
	// that swaps Identity.Password and reconnects on this same transport would
	// otherwise be handed the previous password's key, which the hub refuses
	// exactly as it refuses a wrong password. It is held in memory only --
	// Identity already carries it there -- and never written beside the key.
	cachedPSK         []byte
	cachedPSKNodeID   string
	cachedPSKPassword string
}

func NewWSSTransport(identity Identity) *WSSTransport {
	return &WSSTransport{
		Identity:       identity,
		UserAgent:      DefaultUserAgent,
		BusEvents:      make(chan Event, 32),
		HiveEvents:     make(chan HiveMessage, 32),
		handshakeReady: make(chan struct{}),
		readDone:       make(chan struct{}),
	}
}

type wssConnectionAttempt struct {
	done   chan struct{}
	cancel context.CancelFunc
	err    error
}

type wssGenerationKey struct{}

func (t *WSSTransport) Connect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	t.mu.Lock()
	if t.connected && t.handshake && t.conn != nil {
		t.mu.Unlock()
		return nil
	}
	if pending := t.attempt; pending != nil {
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
		case <-pending.done:
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
			}
			return pending.err
		}
	}
	owned, cancel := context.WithTimeout(ctx, 20*time.Second)
	pending := &wssConnectionAttempt{done: make(chan struct{}), cancel: cancel}
	t.attempt = pending
	old := t.beginConnectionLocked()
	generation := t.generation
	t.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	err := t.connectGeneration(owned, generation)
	cancel()
	t.mu.Lock()
	pending.err = err
	if t.attempt == pending {
		t.attempt = nil
	}
	close(pending.done)
	t.mu.Unlock()
	return err
}

func (t *WSSTransport) connectGeneration(ctx context.Context, generation uint64) (result error) {
	var conn *websocket.Conn
	defer func() {
		if result != nil {
			t.poisonGeneration(generation, conn, result)
		}
	}()
	endpoint := t.Identity.EndpointFor(ProtocolWSS)
	if endpoint == "" {
		return fmt.Errorf("%w: identity does not include a WSS endpoint", ErrProtocol)
	}
	if t.Identity.Password == "" {
		return fmt.Errorf("%w: the v3 Noise handshake needs the identity password", ErrIdentity)
	}
	endpointURL, err := authorizedWSSURL(endpoint, t.Authorization())
	if err != nil {
		return err
	}
	conn, _, err = websocket.DefaultDialer.DialContext(ctx, endpointURL, nil)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
		}
		return fmt.Errorf("%w: %v", ErrConnection, scrubTransportError(err))
	}
	t.mu.Lock()
	if t.generation != generation || ctx.Err() != nil {
		t.mu.Unlock()
		_ = conn.Close()
		return fmt.Errorf("%w: connection retired during dial", ErrConnection)
	}
	t.conn, t.connected = conn, true
	t.connection.markOpen(time.Now(), true)
	ready, closed := t.handshakeReady, t.readDone
	t.mu.Unlock()
	go t.readLoop(context.Background(), conn, generation)
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
	case <-closed:
		t.mu.RLock()
		cause := t.lastError
		t.mu.RUnlock()
		return fmt.Errorf("%w: v3 Noise handshake did not complete: %v", ErrConnection, cause)
	case <-ready:
		t.mu.Lock()
		defer t.mu.Unlock()
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
		}
		if t.generation != generation || t.conn != conn || !t.connected || !t.handshake {
			return fmt.Errorf("%w: connection retired before authenticated readiness", ErrConnection)
		}
		t.connection.complete(time.Now())
		return nil
	}
}

func (t *WSSTransport) Disconnect(_ context.Context) error {
	t.mu.Lock()
	conn := t.conn
	pending := t.attempt
	t.generation++
	t.conn, t.connected, t.handshake = nil, false, false
	t.session, t.noiseHandshake, t.serverHello, t.nodeID = nil, nil, nil, ""
	t.connection.close()
	t.mu.Unlock()
	if pending != nil {
		pending.cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	return nil
}

// Retire only the captured socket and generation, never a replacement session.
func (t *WSSTransport) poisonGeneration(generation uint64, conn *websocket.Conn, err error) {
	t.mu.Lock()
	if t.generation == generation && (conn == nil || t.conn == conn) {
		t.connected, t.handshake, t.conn = false, false, nil
		t.session, t.noiseHandshake, t.serverHello, t.nodeID = nil, nil, nil, ""
		t.lastError = err
		t.connection.fail(time.Now(), err)
	}
	t.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (t *WSSTransport) generationCurrentLocked(ctx context.Context) bool {
	generation, scoped := ctx.Value(wssGenerationKey{}).(uint64)
	return !scoped || generation == t.generation && t.connected && t.conn != nil
}

func staleWSSGeneration() error {
	return fmt.Errorf("%w: stale WebSocket connection generation", ErrConnection)
}

func (t *WSSTransport) Healthcheck() TransportHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()
	health := TransportHealth{Connected: t.connected, HandshakeComplete: t.handshake, TransportAlive: t.connected && t.conn != nil, Connection: t.connection.snapshot()}
	if t.lastError != nil {
		health.LastError = t.lastError.Error()
	}
	return health
}

func (t *WSSTransport) ConnectionInfo() TransportConnectionInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.connection.snapshot()
}

// RemoteStaticKey is the server's Noise static public key for the current
// session, hex encoded. Empty before the handshake completes.
func (t *WSSTransport) RemoteStaticKey() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.session == nil {
		return ""
	}
	return t.session.remoteStaticKey
}

func (t *WSSTransport) EmitBus(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return t.sendHiveMessage(ctx, HiveMessage{
		MsgType:  "bus",
		Payload:  map[string]any{"type": eventType, "data": data, "context": eventContext},
		Metadata: map[string]any{},
		Route:    []any{},
	}, true)
}

func (t *WSSTransport) Events() <-chan Event {
	return t.BusEvents
}

func (t *WSSTransport) HiveMessages() <-chan HiveMessage {
	return t.HiveEvents
}

func (t *WSSTransport) SendHiveMessage(ctx context.Context, message HiveMessage, encrypt bool) error {
	return t.sendHiveMessage(ctx, message, encrypt)
}

func (t *WSSTransport) Authorization() string {
	return base64.StdEncoding.EncodeToString([]byte(t.UserAgent + ":" + t.Identity.AccessKey))
}

func (t *WSSTransport) IsHandshakeComplete() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.handshake
}

func (t *WSSTransport) readLoop(ctx context.Context, conn *websocket.Conn, generation uint64) {
	ctx = context.WithValue(ctx, wssGenerationKey{}, generation)
	defer t.signalReadDone(generation)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.recordReadFailure(generation, err)
			return
		}
		if err := t.handleRawMessage(ctx, payload); err != nil {
			t.recordReadFailure(generation, err)
			// Every v3 transport failure is fatal for the session: a frame
			// that does not decrypt at the current counter means tampering,
			// replay or reordering, so the socket goes rather than the frame.
			_ = conn.Close()
			return
		}
	}
}

func (t *WSSTransport) handleRawMessage(ctx context.Context, raw []byte) error {
	t.receiveMu.Lock()
	defer t.receiveMu.Unlock()
	t.mu.RLock()
	if !t.generationCurrentLocked(ctx) {
		t.mu.RUnlock()
		return staleWSSGeneration()
	}
	session := t.session
	t.mu.RUnlock()

	if session != nil {
		payload, isJSON, complete, err := session.decryptFrame(raw)
		if err != nil {
			return err
		}
		if !complete {
			return nil
		}
		if !isJSON {
			decoded, err := DecodeHiveBinaryFrame(payload)
			if err != nil {
				return err
			}
			payload, err = json.Marshal(decoded)
			if err != nil {
				return err
			}
		}
		raw = payload
	}

	decoded, err := decodeJSONNumbers(raw)
	if err != nil {
		return err
	}
	msgType, _ := decoded["msg_type"].(string)
	payload, _ := decoded["payload"].(map[string]any)

	switch msgType {
	case "hello":
		return t.handleHelloGeneration(ctx, payload)
	case "handshake", "shake":
		return t.handleHandshake(ctx, payload)
	}

	if session == nil {
		return fmt.Errorf("%w: application traffic received before Noise negotiation", ErrConnection)
	}
	var message HiveMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.generationCurrentLocked(ctx) {
		return staleWSSGeneration()
	}
	dispatchNoiseMessage(t.BusEvents, t.HiveEvents, message, &t.streams)
	return nil
}

// handleHelloGeneration records the server's cleartext HELLO. Both its payload
// and the parameter HANDSHAKE payload are bound into the Noise prologue, so it
// has to be kept verbatim rather than read for the node id alone.
func (t *WSSTransport) handleHelloGeneration(ctx context.Context, payload map[string]any) error {
	nodeID, _ := payload["node_id"].(string)
	t.mu.Lock()
	if !t.generationCurrentLocked(ctx) {
		t.mu.Unlock()
		return staleWSSGeneration()
	}
	if t.session == nil && t.serverHello == nil {
		t.serverHello = payload
		t.nodeID = nodeID
	}
	t.mu.Unlock()
	return nil
}

func (t *WSSTransport) handleHandshake(ctx context.Context, payload map[string]any) error {
	noiseParams, ok := payload["noise"].(map[string]any)
	if !ok {
		return fmt.Errorf("%w: this hub did not offer the v3 Noise handshake; the SDK requires a hub running HiveMind-core 5.x or newer", ErrConnection)
	}
	if _, carriesMessage := noiseParams["msg"]; carriesMessage {
		return t.continueNoiseHandshake(ctx, noiseParams)
	}
	return t.startNoiseHandshake(ctx, payload, noiseParams)
}

// startNoiseHandshake selects a pattern and suite, binds the negotiation into
// the prologue, and sends Noise message 1.
func (t *WSSTransport) startNoiseHandshake(ctx context.Context, handshakePayload, noiseParams map[string]any) error {
	t.mu.RLock()
	if !t.generationCurrentLocked(ctx) {
		t.mu.RUnlock()
		return staleWSSGeneration()
	}
	if t.session != nil || t.noiseHandshake != nil {
		t.mu.RUnlock()
		return fmt.Errorf("%w: duplicate Noise negotiation", ErrConnection)
	}
	serverHello, nodeID := t.serverHello, t.nodeID
	t.mu.RUnlock()

	if nodeID == "" {
		return fmt.Errorf("%w: the hub sent its HANDSHAKE parameters before a HELLO carrying node_id", ErrConnection)
	}

	stateDir := t.NoiseStateDir
	pinned, err := LoadNoisePin(stateDir, nodeID)
	if err != nil {
		return err
	}
	pattern, suite, ok := selectNoiseOptions(
		stringSlice(noiseParams["patterns"]),
		stringSlice(noiseParams["suites"]),
		pinned,
	)
	if !ok {
		return fmt.Errorf("%w: no Noise pattern and suite this SDK supports are on offer from the hub", ErrConnection)
	}

	protocolName := noiseProtocolName(pattern, suite)
	prologue, err := buildPrologue(serverHello, handshakePayload, protocolName)
	if err != nil {
		return err
	}
	staticKey, err := LoadOrCreateNoiseKey(stateDir)
	if err != nil {
		return err
	}
	psk, err := t.pskForGeneration(ctx, nodeID)
	if err != nil {
		return err
	}
	handshake, err := newNoiseHandshake(pattern, suite, psk, prologue, staticKey, pinned)
	if err != nil {
		return err
	}

	// Message 1 carries this node's binarize capability and preference-ordered
	// encodings, canonicalized so both peers hash identical bytes.
	noisePayload, err := canonicalJSON(map[string]any{
		"binarize":  false,
		"encodings": []any{},
	})
	if err != nil {
		return err
	}
	message, err := handshake.writeMessage(noisePayload)
	if err != nil {
		return err
	}

	t.mu.Lock()
	if !t.generationCurrentLocked(ctx) {
		t.mu.Unlock()
		return staleWSSGeneration()
	}
	t.noiseHandshake = handshake
	t.mu.Unlock()

	return t.sendCleartext(ctx, HiveMessage{
		MsgType: "shake",
		Payload: map[string]any{"noise": map[string]any{
			"pattern": pattern,
			"suite":   suite,
			"msg":     hex.EncodeToString(message),
		}},
		Metadata: map[string]any{},
		Route:    []any{},
	})
}

// continueNoiseHandshake consumes the server's Noise message, sends the final
// message when the pattern needs one, and brings the transport up.
func (t *WSSTransport) continueNoiseHandshake(ctx context.Context, noiseParams map[string]any) error {
	t.mu.RLock()
	if !t.generationCurrentLocked(ctx) {
		t.mu.RUnlock()
		return staleWSSGeneration()
	}
	handshake, nodeID := t.noiseHandshake, t.nodeID
	t.mu.RUnlock()
	if handshake == nil {
		return fmt.Errorf("%w: the hub sent a Noise handshake message before its parameters", ErrConnection)
	}

	encoded, _ := noiseParams["msg"].(string)
	message, err := hex.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("%w: malformed Noise handshake envelope", ErrConnection)
	}

	if _, err := handshake.readMessage(message); err != nil {
		// Authentication failure must not erase trust. Only an explicit
		// ForgetNoisePin after verifying a key rotation may permit a new key.
		// The PSK is the other thing this message authenticates, so a rejection
		// may mean the stored key was derived from a password that has since
		// been rotated. Drop it; the next attempt derives from the current one.
		t.mu.RLock()
		current := t.generationCurrentLocked(ctx)
		t.mu.RUnlock()
		if current {
			t.forgetPSK(nodeID)
		}
		return err
	}
	if !handshake.complete {
		// XXpsk2 message 3: our encrypted static key and the final DH mix.
		// pattern and suite are named only on message 1.
		final, err := handshake.writeMessage(nil)
		if err != nil {
			return err
		}
		if err := t.sendCleartext(ctx, HiveMessage{
			MsgType:  "shake",
			Payload:  map[string]any{"noise": map[string]any{"msg": hex.EncodeToString(final)}},
			Metadata: map[string]any{},
			Route:    []any{},
		}); err != nil {
			return err
		}
	}

	session, err := newNoiseSession(handshake)
	if err != nil {
		return err
	}

	t.mu.RLock()
	current := t.generationCurrentLocked(ctx)
	t.mu.RUnlock()
	if !current {
		return staleWSSGeneration()
	}
	if err := t.pinServerKey(nodeID, session.remoteStaticKey); err != nil {
		return err
	}

	t.mu.Lock()
	if !t.generationCurrentLocked(ctx) {
		t.mu.Unlock()
		return staleWSSGeneration()
	}
	t.session = session
	t.noiseHandshake = nil
	t.mu.Unlock()

	// The first Noise transport message is the encrypted HELLO.
	if err := t.sendHiveMessage(ctx, helloHiveMessage(t.Identity, "thalovant-go-wss-"), false); err != nil {
		return err
	}

	t.mu.Lock()
	if !t.generationCurrentLocked(ctx) {
		t.mu.Unlock()
		return staleWSSGeneration()
	}
	if !t.handshake {
		t.handshake = true
		close(t.handshakeReady)
	}
	t.mu.Unlock()
	return nil
}

// pinServerKey enforces trust on first use: the first key seen for a node id is
// recorded, and a later key that does not match it is refused.
//
// A changed key means either the hub was reinstalled or another machine is
// answering at this address. The SDK cannot tell those apart, so it refuses and
// leaves clearing the pin (ForgetNoisePin) as a deliberate act.
func (t *WSSTransport) pinServerKey(nodeID, remoteStaticKey string) error {
	return pinNoisePeer(t.NoiseStateDir, nodeID, remoteStaticKey)
}

// pskFor derives (or reuses) the pre-shared key for a hub.
func (t *WSSTransport) pskFor(nodeID string) []byte {
	psk, _ := t.pskForGeneration(context.Background(), nodeID)
	return psk
}

func (t *WSSTransport) pskForGeneration(ctx context.Context, nodeID string) ([]byte, error) {
	password := t.Identity.Password

	t.mu.Lock()
	if !t.generationCurrentLocked(ctx) {
		t.mu.Unlock()
		return nil, staleWSSGeneration()
	}
	cached, sameHub := t.cachedPSK, t.cachedPSKNodeID == nodeID
	samePassword := t.cachedPSKPassword == password
	stateDir := t.NoiseStateDir
	t.mu.Unlock()

	if cached != nil && sameHub && samePassword {
		return cached, nil
	}

	// Having already derived for this hub under a different password means the
	// stored key belongs to that one, so the disk read would only return
	// something known to be stale.
	psk := []byte(nil)
	if !(cached != nil && sameHub) {
		// On disk before deriving: the answer never changes for a password and
		// hub, so a restart should not pay argon2id again. A key left from a
		// password rotated elsewhere is caught by the handshake, which
		// forgets it.
		psk = LoadCachedPSK(stateDir, nodeID)
	}
	if psk == nil {
		psk = derivePSK(password, nodeID)
		t.mu.RLock()
		current := t.generationCurrentLocked(ctx)
		t.mu.RUnlock()
		if !current {
			return nil, staleWSSGeneration()
		}
		// Persisting is an optimisation, never a reason to fail the connection.
		_ = SaveCachedPSK(stateDir, nodeID, psk)
	}

	t.mu.Lock()
	if !t.generationCurrentLocked(ctx) {
		t.mu.Unlock()
		return nil, staleWSSGeneration()
	}
	t.cachedPSK, t.cachedPSKNodeID, t.cachedPSKPassword = psk, nodeID, password
	t.mu.Unlock()
	return psk, nil
}

// forgetPSK drops the cached key for a hub, in memory and on disk. The
// handshake calls it when the hub rejects the key we offered.
func (t *WSSTransport) forgetPSK(nodeID string) {
	t.mu.Lock()
	if t.cachedPSKNodeID == nodeID {
		t.cachedPSK, t.cachedPSKNodeID, t.cachedPSKPassword = nil, "", ""
	}
	stateDir := t.NoiseStateDir
	t.mu.Unlock()
	_ = ForgetCachedPSK(stateDir, nodeID)
}

// sendCleartext writes a handshake message as a JSON text frame. Only the
// handshake exchange itself travels this way; everything after Split() goes
// through the Noise session.
func (t *WSSTransport) sendCleartext(ctx context.Context, message HiveMessage) error {
	t.mu.RLock()
	generation, conn := t.generation, t.conn
	current := t.generationCurrentLocked(ctx)
	t.mu.RUnlock()
	if !current {
		return staleWSSGeneration()
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return t.writeGeneration(ctx, generation, conn, func() error { return conn.WriteMessage(websocket.TextMessage, raw) })
}

func (t *WSSTransport) sendHiveMessage(ctx context.Context, message HiveMessage, _ bool) error {
	t.mu.RLock()
	generation, session, conn := t.generation, t.session, t.conn
	current := t.generationCurrentLocked(ctx)
	_, scoped := ctx.Value(wssGenerationKey{}).(uint64)
	authenticated := t.handshake || scoped && message.MsgType == "hello"
	t.mu.RUnlock()
	if !current {
		return staleWSSGeneration()
	}
	if session == nil || !authenticated {
		return fmt.Errorf("%w: refusing to send before the v3 Noise session is established", ErrConnection)
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return t.writeGeneration(ctx, generation, conn, func() error {
		return session.sendMessage(raw, true, func(frame []byte) error { return conn.WriteMessage(websocket.BinaryMessage, frame) })
	})
}

// Cancellation while queued never advances a cipher. After admission, failed or
// cancelled writes poison only this captured generation, because delivery is uncertain.
func (t *WSSTransport) writeGeneration(ctx context.Context, generation uint64, conn *websocket.Conn, write func() error) error {
	if conn == nil {
		return fmt.Errorf("%w: HiveMind WSS transport is not connected", ErrConnection)
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := t.writeMu.Lock(bounded); err != nil {
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	defer t.writeMu.Unlock()
	t.mu.RLock()
	current := t.generation == generation && t.conn == conn && t.connected
	t.mu.RUnlock()
	if !current {
		return staleWSSGeneration()
	}
	deadline, _ := bounded.Deadline()
	_ = conn.SetWriteDeadline(deadline)
	finished := make(chan struct{})
	stop := context.AfterFunc(bounded, func() { _ = conn.Close(); close(finished) })
	err := write()
	if !stop() {
		<-finished
	}
	_ = conn.SetWriteDeadline(time.Time{})
	if bounded.Err() != nil {
		err = fmt.Errorf("%w: %w", ErrTimeout, bounded.Err())
	} else if timeoutErr, ok := err.(net.Error); ok && timeoutErr.Timeout() {
		err = fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	if err != nil {
		t.poisonGeneration(generation, conn, err)
	}
	return err
}

// decodeJSONNumbers decodes a JSON object keeping numbers as their original
// literal text, so a payload re-serialized for the prologue matches the bytes
// the server hashed.
func decodeJSONNumbers(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoded := map[string]any{}
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func stringSlice(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func authorizedWSSURL(endpoint string, authorization string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", fmt.Errorf("%w: WSS endpoint must start with ws:// or wss://", ErrConnection)
	}
	query := parsed.Query()
	query.Set("authorization", authorization)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func helloHiveMessage(identity Identity, prefix string) HiveMessage {
	return HiveMessage{
		MsgType: "hello",
		Payload: map[string]any{
			"pubkey":  identity.PublicKey,
			"session": map[string]any{"session_id": prefix + NewSessionID()},
			"site_id": identity.SiteID,
		},
		Metadata: map[string]any{},
		Route:    []any{},
	}
}

func (t *WSSTransport) beginConnection() {
	t.mu.Lock()
	old := t.beginConnectionLocked()
	t.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (t *WSSTransport) beginConnectionLocked() *websocket.Conn {
	old := t.conn
	t.conn = nil
	t.generation++
	t.lastError = nil
	t.connected = false
	t.handshake = false
	t.session = nil
	t.noiseHandshake = nil
	t.serverHello = nil
	t.nodeID = ""
	t.handshakeReady = make(chan struct{})
	t.readDone = make(chan struct{})
	t.connection.begin(time.Now())
	return old
}

// recordReadFailure stores why the read loop stopped, but only while its
// connection is still the current one. A loop left over from a previous attempt
// would otherwise overwrite lastError and mark a fresh connection failed.
func (t *WSSTransport) recordReadFailure(generation uint64, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.generation != generation || !t.connected {
		return
	}
	t.lastError = err
	t.connected = false
	t.handshake = false
	t.session = nil
	t.noiseHandshake = nil
	t.connection.fail(time.Now(), err)
}

// signalReadDone unblocks a Connect still waiting on the handshake once the
// read loop has stopped, so a refused connection reports its cause instead of
// running out the clock.
//
// Disconnect does not wait for readLoop to exit, so a loop from a previous
// attempt can outlive it. Closing the current readDone from that loop would
// abort the new handshake, so it only fires for its own generation.
func (t *WSSTransport) signalReadDone(generation uint64) {
	t.mu.Lock()
	if t.generation != generation {
		t.mu.Unlock()
		return
	}
	done := t.readDone
	t.readDone = nil
	t.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func (t *WSSTransport) completeConnection() {
	t.mu.Lock()
	t.connection.complete(time.Now())
	t.mu.Unlock()
}

func (t *WSSTransport) failConnection(err error) {
	t.mu.Lock()
	t.lastError = err
	t.connection.fail(time.Now(), err)
	t.mu.Unlock()
}
