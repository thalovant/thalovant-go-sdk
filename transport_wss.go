package thalovant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type WSSTransport struct {
	Identity       Identity
	UserAgent      string
	BusEvents      chan Event
	HiveEvents     chan HiveMessage
	conn           *websocket.Conn
	connected      bool
	handshake      bool
	lastError      error
	connection     connectionTelemetry
	handshakeReady chan struct{}
	writeMu        sync.Mutex
	mu             sync.RWMutex
}

func NewWSSTransport(identity Identity) *WSSTransport {
	return &WSSTransport{
		Identity:       identity,
		UserAgent:      DefaultUserAgent,
		BusEvents:      make(chan Event, 32),
		HiveEvents:     make(chan HiveMessage, 32),
		handshakeReady: make(chan struct{}),
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
	url, err := authorizedWSSURL(endpoint, t.Authorization())
	if err != nil {
		t.failConnection(err)
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		wrapped := fmt.Errorf("%w: %v", ErrConnection, err)
		t.failConnection(wrapped)
		return wrapped
	}
	t.conn = conn
	t.mu.Lock()
	t.connected = true
	t.connection.markOpen(time.Now(), true)
	ready := t.handshakeReady
	t.mu.Unlock()
	go t.readLoop(context.Background(), conn)

	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		t.completeConnection()
		return nil
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
	if t.conn != nil {
		_ = t.conn.Close()
	}
	t.mu.Lock()
	t.connected = false
	t.handshake = false
	t.conn = nil
	t.connection.close()
	t.mu.Unlock()
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

func (t *WSSTransport) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.mu.Lock()
			if t.connected {
				t.lastError = err
				t.connected = false
				t.connection.fail(time.Now(), err)
			}
			t.mu.Unlock()
			return
		}
		if err := t.handleRawMessage(ctx, payload); err != nil {
			t.mu.Lock()
			if t.connected {
				t.lastError = err
				t.connected = false
				t.connection.fail(time.Now(), err)
			}
			t.mu.Unlock()
			return
		}
	}
}

func (t *WSSTransport) handleRawMessage(ctx context.Context, raw []byte) error {
	rawBytes := raw
	var encrypted map[string]any
	if err := json.Unmarshal(raw, &encrypted); err == nil {
		if _, ok := encrypted["ciphertext"]; ok && t.Identity.CryptoKey != "" {
			decrypted, err := DecryptFromJSON(t.Identity.CryptoKey, string(raw))
			if err != nil {
				return err
			}
			rawBytes = []byte(decrypted)
		}
	}
	var message HiveMessage
	if err := json.Unmarshal(rawBytes, &message); err != nil {
		return err
	}
	switch message.MsgType {
	case "handshake", "shake":
		return t.handleHandshake(ctx, message.Payload)
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

func (t *WSSTransport) handleHandshake(ctx context.Context, payload map[string]any) error {
	if truthy(payload["preshared_key"]) && !truthy(payload["handshake"]) && payload["envelope"] == nil {
		if RuntimeCryptoKey(t.Identity.CryptoKey) == nil {
			return fmt.Errorf("%w: HiveMind requested preshared key but identity crypto_key is missing", ErrConnection)
		}
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
	return fmt.Errorf("%w: only preshared-key HiveMind WSS handshakes are supported", ErrConnection)
}

func (t *WSSTransport) sendHiveMessage(_ context.Context, message HiveMessage, encrypt bool) error {
	if t.conn == nil {
		return fmt.Errorf("%w: HiveMind WSS transport is not connected", ErrConnection)
	}
	payload, err := serializeHiveMessage(t.Identity, t.IsHandshakeComplete(), message, encrypt)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.conn.WriteMessage(websocket.TextMessage, []byte(payload))
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

func serializeHiveMessage(identity Identity, handshakeComplete bool, message HiveMessage, encrypt bool) (string, error) {
	raw, err := json.Marshal(message)
	if err != nil {
		return "", err
	}
	payload := string(raw)
	if encrypt && handshakeComplete && strings.TrimSpace(identity.CryptoKey) != "" {
		payload, err = EncryptAsJSON(identity.CryptoKey, payload)
		if err != nil {
			return "", err
		}
	}
	return payload, nil
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
	t.lastError = nil
	t.connected = false
	t.handshake = false
	t.handshakeReady = make(chan struct{})
	t.connection.begin(time.Now())
	t.mu.Unlock()
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
