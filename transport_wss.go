package thalovant

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type WSSTransport struct {
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
	writeMu        sync.Mutex
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

func (t *WSSTransport) Connect(ctx context.Context) error {
	t.beginConnection()
	endpoint := t.Identity.EndpointFor(ProtocolWSS)
	if endpoint == "" {
		err := fmt.Errorf("%w: identity does not include a WSS endpoint", ErrProtocol)
		t.failConnection(err)
		return err
	}
	if t.Identity.Password == "" {
		err := fmt.Errorf("%w: the v3 Noise handshake needs the identity password", ErrIdentity)
		t.failConnection(err)
		return err
	}
	url, err := authorizedWSSURL(endpoint, t.Authorization())
	if err != nil {
		t.failConnection(err)
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		// The dial URL carries the access key in its ?authorization= query.
		// gorilla does not put the URL in its dial errors today -- refused,
		// DNS, bad-scheme and TLS failures all come back as a plain net error
		// -- so this is defence rather than a fix: scrubTransportError is a
		// no-op unless the error is a *url.Error, and it costs nothing to keep
		// the guarantee independent of the library's error shape.
		wrapped := fmt.Errorf("%w: %v", ErrConnection, scrubTransportError(err))
		t.failConnection(wrapped)
		return wrapped
	}
	t.mu.Lock()
	t.conn = conn
	t.connected = true
	t.connection.markOpen(time.Now(), true)
	ready := t.handshakeReady
	closed := t.readDone
	generation := t.generation
	t.mu.Unlock()
	go t.readLoop(context.Background(), conn, generation)

	// The handshake runs argon2id at 64 MiB on first contact with a hub, which
	// takes a few hundred milliseconds on top of the round trips.
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		t.completeConnection()
		return nil
	case <-closed:
		// The socket went before the handshake finished. Report why -- a hub
		// that refuses the handshake closes with 1008, and reporting that as a
		// timeout would hide a wrong password behind a twenty second wait.
		_ = t.Disconnect(ctx)
		t.mu.RLock()
		cause := t.lastError
		t.mu.RUnlock()
		if cause == nil {
			cause = fmt.Errorf("the hub closed the connection during the v3 Noise handshake")
		}
		err := fmt.Errorf("%w: v3 Noise handshake did not complete: %v", ErrConnection, cause)
		t.failConnection(err)
		return err
	case <-ctx.Done():
		_ = t.Disconnect(ctx)
		err := fmt.Errorf("%w: %v", ErrTimeout, ctx.Err())
		t.failConnection(err)
		return err
	case <-timer.C:
		_ = t.Disconnect(ctx)
		err := fmt.Errorf("%w: HiveMind WSS handshake timed out", ErrTimeout)
		t.failConnection(err)
		return err
	}
}

func (t *WSSTransport) Disconnect(_ context.Context) error {
	// Capture and clear under one lock, then close outside it: reading t.conn
	// unlocked races a concurrent Connect or Disconnect.
	t.mu.Lock()
	conn := t.conn
	t.conn = nil
	t.handshake = false
	t.session = nil
	t.noiseHandshake = nil
	t.serverHello = nil
	t.nodeID = ""
	t.connection.close()
	t.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return nil
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
	t.mu.RLock()
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
			// A HIVEMIND-WIRE-1 binary frame. The Go SDK does not decode
			// binary bus payloads yet, so it is dropped rather than
			// mis-parsed as JSON.
			return nil
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
		return t.handleHello(payload)
	case "handshake", "shake":
		return t.handleHandshake(ctx, payload)
	}

	var message HiveMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return err
	}
	switch message.MsgType {
	case "bus":
		t.BusEvents <- Event{
			Name:    fmt.Sprint(message.Payload["type"]),
			Data:    mapValue(message.Payload["data"]),
			Context: mapValue(message.Payload["context"]),
			Raw:     message,
		}
	case "query", "cascade":
		select {
		case t.HiveEvents <- message:
		default:
		}
	}
	return nil
}

// handleHello records the server's cleartext HELLO. Both its payload and the
// parameter HANDSHAKE payload are bound into the Noise prologue, so it has to
// be kept verbatim rather than read for the node id alone.
func (t *WSSTransport) handleHello(payload map[string]any) error {
	nodeID, _ := payload["node_id"].(string)
	t.mu.Lock()
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
	handshake, err := newNoiseHandshake(pattern, suite, t.pskFor(nodeID), prologue, staticKey, pinned)
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
		// KKpsk0 needs each side to hold the other's static key, but the
		// client chose it knowing only that it had pinned the server's. The
		// failure is as likely to mean the server never had ours, so drop the
		// pin and let the next attempt fall back to XXpsk2.
		if handshake.pattern == noisePatternKK {
			_ = ForgetNoisePin(t.NoiseStateDir, nodeID)
		}
		// The PSK is the other thing this message authenticates, so a rejection
		// may mean the stored key was derived from a password that has since
		// been rotated. Drop it; the next attempt derives from the current one.
		t.forgetPSK(nodeID)
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

	if err := t.pinServerKey(nodeID, session.remoteStaticKey); err != nil {
		return err
	}

	t.mu.Lock()
	t.session = session
	t.noiseHandshake = nil
	t.mu.Unlock()

	// The first Noise transport message is the encrypted HELLO.
	if err := t.sendHiveMessage(ctx, helloHiveMessage(t.Identity, "thalovant-go-wss-"), false); err != nil {
		return err
	}

	t.mu.Lock()
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
	if remoteStaticKey == "" {
		return nil
	}
	pinned, err := LoadNoisePin(t.NoiseStateDir, nodeID)
	if err != nil {
		return err
	}
	if pinned == "" {
		return SaveNoisePin(t.NoiseStateDir, nodeID, remoteStaticKey)
	}
	if pinned != remoteStaticKey {
		return fmt.Errorf("%w: the hub's Noise static key changed. If the hub was not reinstalled or replaced, another machine may be answering at this address. If it was, drop the stale pin with ForgetNoisePin and reconnect to trust the new key", ErrConnection)
	}
	return nil
}

// pskFor derives (or reuses) the pre-shared key for a hub.
func (t *WSSTransport) pskFor(nodeID string) []byte {
	password := t.Identity.Password

	t.mu.Lock()
	cached, sameHub := t.cachedPSK, t.cachedPSKNodeID == nodeID
	samePassword := t.cachedPSKPassword == password
	stateDir := t.NoiseStateDir
	t.mu.Unlock()

	if cached != nil && sameHub && samePassword {
		return cached
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
		// Persisting is an optimisation, never a reason to fail the connection.
		_ = SaveCachedPSK(stateDir, nodeID, psk)
	}

	t.mu.Lock()
	t.cachedPSK, t.cachedPSKNodeID, t.cachedPSKPassword = psk, nodeID, password
	t.mu.Unlock()
	return psk
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
func (t *WSSTransport) sendCleartext(_ context.Context, message HiveMessage) error {
	// One snapshot under the lock: readLoop calls this during the handshake
	// while Connect's timeout branch can be running Disconnect, which sets
	// t.conn to nil.
	t.mu.RLock()
	conn := t.conn
	t.mu.RUnlock()
	if conn == nil {
		return fmt.Errorf("%w: HiveMind WSS transport is not connected", ErrConnection)
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return conn.WriteMessage(websocket.TextMessage, raw)
}

func (t *WSSTransport) sendHiveMessage(_ context.Context, message HiveMessage, _ bool) error {
	t.mu.RLock()
	session, conn := t.session, t.conn
	t.mu.RUnlock()
	if conn == nil {
		return fmt.Errorf("%w: HiveMind WSS transport is not connected", ErrConnection)
	}
	if session == nil {
		return fmt.Errorf("%w: refusing to send before the v3 Noise session is established", ErrConnection)
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	// sendMessage holds its own lock across every chunk of one message, and
	// writeMu keeps two senders from interleaving on the socket.
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return session.sendMessage(raw, true, func(frame []byte) error {
		return conn.WriteMessage(websocket.BinaryMessage, frame)
	})
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
	t.mu.Unlock()
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
