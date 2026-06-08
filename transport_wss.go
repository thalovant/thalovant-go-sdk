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
	Identity  Identity
	UserAgent string
	BusEvents chan Event
	conn      *websocket.Conn
	connected bool
	handshake bool
	lastError error
	writeMu   sync.Mutex
	mu        sync.RWMutex
}

func NewWSSTransport(identity Identity) *WSSTransport {
	return &WSSTransport{
		Identity:  identity,
		UserAgent: DefaultUserAgent,
		BusEvents: make(chan Event, 32),
	}
}

func (t *WSSTransport) Connect(ctx context.Context) error {
	endpoint := t.Identity.EndpointFor(ProtocolWSS)
	if endpoint == "" {
		return fmt.Errorf("%w: identity does not include a WSS endpoint", ErrProtocol)
	}
	url, err := authorizedWSSURL(endpoint, t.Authorization())
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	t.conn = conn
	t.mu.Lock()
	t.connected = true
	t.mu.Unlock()
	go t.readLoop(context.Background())

	deadline := time.Now().Add(6 * time.Second)
	for !t.IsHandshakeComplete() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !t.IsHandshakeComplete() {
		_ = t.Disconnect(ctx)
		return fmt.Errorf("%w: HiveMind WSS handshake timed out", ErrTimeout)
	}
	return nil
}

func (t *WSSTransport) Disconnect(_ context.Context) error {
	if t.conn != nil {
		_ = t.conn.Close()
	}
	t.mu.Lock()
	t.connected = false
	t.handshake = false
	t.mu.Unlock()
	return nil
}

func (t *WSSTransport) Healthcheck() TransportHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()
	health := TransportHealth{Connected: t.connected, HandshakeComplete: t.handshake, TransportAlive: t.connected && t.conn != nil}
	if t.lastError != nil {
		health.LastError = t.lastError.Error()
	}
	return health
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

func (t *WSSTransport) Authorization() string {
	return base64.StdEncoding.EncodeToString([]byte(t.UserAgent + ":" + t.Identity.AccessKey))
}

func (t *WSSTransport) IsHandshakeComplete() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.handshake
}

func (t *WSSTransport) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, payload, err := t.conn.ReadMessage()
		if err != nil {
			t.mu.Lock()
			t.lastError = err
			t.connected = false
			t.mu.Unlock()
			return
		}
		if err := t.handleRawMessage(ctx, payload); err != nil {
			t.mu.Lock()
			t.lastError = err
			t.connected = false
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
	case "handshake":
		return t.handleHandshake(ctx, message.Payload)
	case "bus":
		t.BusEvents <- Event{
			Name:    fmt.Sprint(message.Payload["type"]),
			Data:    mapValue(message.Payload["data"]),
			Context: mapValue(message.Payload["context"]),
			Raw:     message,
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
		t.handshake = true
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
