package thalovant

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type TransportHealth struct {
	Connected         bool
	HandshakeComplete bool
	TransportAlive    bool
	LastError         string
	Connection        TransportConnectionInfo
}

type TransportConnectionPhase string

const (
	ConnectionIdle       TransportConnectionPhase = "idle"
	ConnectionConnecting TransportConnectionPhase = "connecting"
	ConnectionHandshake  TransportConnectionPhase = "handshake"
	ConnectionReady      TransportConnectionPhase = "ready"
	ConnectionClosed     TransportConnectionPhase = "closed"
	ConnectionError      TransportConnectionPhase = "error"
)

type TransportConnectionInfo struct {
	Phase           TransportConnectionPhase `json:"phase"`
	StartedAt       time.Time                `json:"started_at,omitempty"`
	ConnectedAt     time.Time                `json:"connected_at,omitempty"`
	TransportOpenMS float64                  `json:"transport_open_ms,omitempty"`
	SocketOpenMS    float64                  `json:"socket_open_ms,omitempty"`
	HandshakeMS     float64                  `json:"handshake_ms,omitempty"`
	ConnectMS       float64                  `json:"connect_ms,omitempty"`
	LastError       string                   `json:"last_error,omitempty"`
}

type HiveMessage struct {
	MsgType      string         `json:"msg_type"`
	Payload      map[string]any `json:"payload"`
	Metadata     map[string]any `json:"metadata"`
	Route        []any          `json:"route"`
	Node         any            `json:"node"`
	TargetSiteID any            `json:"target_site_id"`
	TargetPubKey any            `json:"target_pubkey"`
	SourcePeer   any            `json:"source_peer"`
}

type RuntimeTransport interface {
	Connect(ctx context.Context) error
	Disconnect(ctx context.Context) error
	Healthcheck() TransportHealth
	ConnectionInfo() TransportConnectionInfo
	EmitBus(ctx context.Context, eventType string, data Data, eventContext Context) error
	Events() <-chan Event
}

type HTTPTransport struct {
	Identity      Identity
	UserAgent     string
	PollInterval  time.Duration
	HTTPClient    *http.Client
	BusEvents     chan Event
	HiveEvents    chan HiveMessage
	connected     bool
	handshake     bool
	lastError     error
	connection    connectionTelemetry
	cancelPolling context.CancelFunc
	mu            sync.RWMutex
}

func NewHTTPTransport(identity Identity) *HTTPTransport {
	return &HTTPTransport{
		Identity:     identity,
		UserAgent:    DefaultUserAgent,
		PollInterval: time.Second,
		HTTPClient:   http.DefaultClient,
		BusEvents:    make(chan Event, 32),
		HiveEvents:   make(chan HiveMessage, 32),
	}
}

func (t *HTTPTransport) BaseURL() string {
	return t.Identity.EndpointBase()
}

func (t *HTTPTransport) Authorization() string {
	return base64.StdEncoding.EncodeToString([]byte(t.UserAgent + ":" + t.Identity.AccessKey))
}

func (t *HTTPTransport) Connect(ctx context.Context) error {
	t.beginConnection()
	endpoint := t.BaseURL() + "/connect?authorization=" + url.QueryEscape(t.Authorization())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		t.failConnection(err)
		return err
	}
	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		wrapped := fmt.Errorf("%w: %v", ErrConnection, err)
		t.failConnection(wrapped)
		return wrapped
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := fmt.Errorf("%w: connect status %d", ErrConnection, resp.StatusCode)
		t.failConnection(err)
		return err
	}
	t.mu.Lock()
	t.connected = true
	t.connection.markOpen(time.Now(), false)
	t.mu.Unlock()

	deadline := time.Now().Add(6 * time.Second)
	for !t.IsHandshakeComplete() && time.Now().Before(deadline) {
		if err := t.PollOnce(ctx); err != nil {
			t.failConnection(err)
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !t.IsHandshakeComplete() {
		err := fmt.Errorf("%w: HiveMind HTTP handshake timed out", ErrTimeout)
		t.failConnection(err)
		return err
	}
	t.completeConnection()
	pollCtx, cancel := context.WithCancel(context.Background())
	t.cancelPolling = cancel
	go t.pollLoop(pollCtx)
	return nil
}

func (t *HTTPTransport) Disconnect(ctx context.Context) error {
	if t.cancelPolling != nil {
		t.cancelPolling()
	}
	endpoint := t.BaseURL() + "/disconnect?authorization=" + url.QueryEscape(t.Authorization())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err == nil {
		if resp, err := t.HTTPClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	t.mu.Lock()
	t.connected = false
	t.handshake = false
	t.connection.close()
	t.mu.Unlock()
	return nil
}

func (t *HTTPTransport) Healthcheck() TransportHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()
	health := TransportHealth{Connected: t.connected, HandshakeComplete: t.handshake, TransportAlive: t.connected, Connection: t.connection.snapshot()}
	if t.lastError != nil {
		health.LastError = t.lastError.Error()
	}
	return health
}

func (t *HTTPTransport) ConnectionInfo() TransportConnectionInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.connection.snapshot()
}

func (t *HTTPTransport) EmitBus(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return t.sendHiveMessage(ctx, HiveMessage{
		MsgType:  "bus",
		Payload:  map[string]any{"type": eventType, "data": data, "context": eventContext},
		Metadata: map[string]any{},
		Route:    []any{},
	}, true)
}

func (t *HTTPTransport) Events() <-chan Event {
	return t.BusEvents
}

func (t *HTTPTransport) HiveMessages() <-chan HiveMessage {
	return t.HiveEvents
}

func (t *HTTPTransport) SendHiveMessage(ctx context.Context, message HiveMessage, encrypt bool) error {
	return t.sendHiveMessage(ctx, message, encrypt)
}

func (t *HTTPTransport) IsHandshakeComplete() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.handshake
}

func (t *HTTPTransport) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(t.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := t.PollOnce(ctx); err != nil {
				t.mu.Lock()
				t.lastError = err
				t.connected = false
				t.connection.fail(time.Now(), err)
				t.mu.Unlock()
				return
			}
		}
	}
}

func (t *HTTPTransport) PollOnce(ctx context.Context) error {
	endpoint := t.BaseURL() + "/get_messages?authorization=" + url.QueryEscape(t.Authorization())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	defer resp.Body.Close()
	var body struct {
		Error    string `json:"error"`
		Messages []any  `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	if body.Error != "" {
		return fmt.Errorf("%w: %s", ErrRuntime, body.Error)
	}
	for _, raw := range body.Messages {
		if err := t.handleRawMessage(ctx, raw); err != nil {
			return err
		}
	}
	return nil
}

func (t *HTTPTransport) handleRawMessage(ctx context.Context, raw any) error {
	var rawBytes []byte
	switch value := raw.(type) {
	case string:
		rawBytes = []byte(value)
	case map[string]any:
		if _, ok := value["ciphertext"]; ok && t.Identity.CryptoKey != "" {
			encoded, _ := json.Marshal(value)
			decrypted, err := DecryptFromJSON(t.Identity.CryptoKey, string(encoded))
			if err != nil {
				return err
			}
			rawBytes = []byte(decrypted)
		} else {
			rawBytes, _ = json.Marshal(value)
		}
	default:
		rawBytes, _ = json.Marshal(value)
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

func (t *HTTPTransport) handleHandshake(ctx context.Context, payload map[string]any) error {
	if truthy(payload["preshared_key"]) && !truthy(payload["handshake"]) && payload["envelope"] == nil {
		if RuntimeCryptoKey(t.Identity.CryptoKey) == nil {
			return fmt.Errorf("%w: HiveMind requested preshared key but identity crypto_key is missing", ErrConnection)
		}
		if err := t.sendHiveMessage(ctx, HiveMessage{
			MsgType: "hello",
			Payload: map[string]any{
				"pubkey":  t.Identity.PublicKey,
				"session": map[string]any{"session_id": "thalovant-go-" + NewSessionID()},
				"site_id": t.Identity.SiteID,
			},
			Metadata: map[string]any{},
			Route:    []any{},
		}, false); err != nil {
			return err
		}
		t.mu.Lock()
		t.handshake = true
		t.mu.Unlock()
		return nil
	}
	return fmt.Errorf("%w: only preshared-key HiveMind HTTP handshakes are supported in this alpha", ErrConnection)
}

func (t *HTTPTransport) beginConnection() {
	t.mu.Lock()
	t.lastError = nil
	t.connection.begin(time.Now())
	t.mu.Unlock()
}

func (t *HTTPTransport) completeConnection() {
	t.mu.Lock()
	t.connection.complete(time.Now())
	t.mu.Unlock()
}

func (t *HTTPTransport) failConnection(err error) {
	t.mu.Lock()
	t.lastError = err
	t.connection.fail(time.Now(), err)
	t.mu.Unlock()
}

type connectionTelemetry struct {
	started time.Time
	opened  time.Time
	info    TransportConnectionInfo
}

func (c connectionTelemetry) snapshot() TransportConnectionInfo {
	if c.info.Phase == "" {
		return TransportConnectionInfo{Phase: ConnectionIdle}
	}
	return c.info
}

func (c *connectionTelemetry) begin(now time.Time) {
	c.started = now
	c.opened = time.Time{}
	c.info = TransportConnectionInfo{
		Phase:     ConnectionConnecting,
		StartedAt: now,
	}
}

func (c *connectionTelemetry) markOpen(now time.Time, socket bool) {
	if c.started.IsZero() {
		c.begin(now)
	}
	if !c.opened.IsZero() {
		return
	}
	c.opened = now
	openMS := elapsedMS(c.started, now)
	c.info.Phase = ConnectionHandshake
	c.info.TransportOpenMS = openMS
	if socket {
		c.info.SocketOpenMS = openMS
	}
}

func (c *connectionTelemetry) complete(now time.Time) {
	opened := c.opened
	if opened.IsZero() {
		opened = c.started
	}
	if opened.IsZero() {
		opened = now
	}
	started := c.started
	if started.IsZero() {
		started = opened
	}
	c.info.Phase = ConnectionReady
	c.info.ConnectedAt = now
	c.info.HandshakeMS = elapsedMS(opened, now)
	c.info.ConnectMS = elapsedMS(started, now)
	c.info.LastError = ""
}

func (c *connectionTelemetry) fail(now time.Time, err error) {
	started := c.started
	if started.IsZero() {
		started = now
	}
	c.info.Phase = ConnectionError
	c.info.ConnectMS = elapsedMS(started, now)
	if err != nil {
		c.info.LastError = err.Error()
	}
}

func (c *connectionTelemetry) close() {
	if c.info.Phase == "" {
		c.info.Phase = ConnectionClosed
		return
	}
	c.info.Phase = ConnectionClosed
}

func elapsedMS(start time.Time, end time.Time) float64 {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return float64(end.Sub(start).Microseconds()) / 1000
}

func (t *HTTPTransport) sendHiveMessage(ctx context.Context, message HiveMessage, encrypt bool) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	payload := string(raw)
	if encrypt && t.IsHandshakeComplete() && t.Identity.CryptoKey != "" {
		payload, err = EncryptAsJSON(t.Identity.CryptoKey, payload)
		if err != nil {
			return err
		}
	}
	form := url.Values{"message": []string{payload}}
	endpoint := t.BaseURL() + "/send_message?authorization=" + url.QueryEscape(t.Authorization())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: send status %d", ErrConnection, resp.StatusCode)
	}
	return nil
}

func mapValue(raw any) map[string]any {
	if value, ok := raw.(map[string]any); ok {
		return value
	}
	return map[string]any{}
}

func truthy(raw any) bool {
	value, ok := raw.(bool)
	return ok && value
}
