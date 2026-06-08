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
	EmitBus(ctx context.Context, eventType string, data Data, eventContext Context) error
	Events() <-chan Event
}

type HTTPTransport struct {
	Identity      Identity
	UserAgent     string
	PollInterval  time.Duration
	HTTPClient    *http.Client
	BusEvents     chan Event
	connected     bool
	handshake     bool
	lastError     error
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
	}
}

func (t *HTTPTransport) BaseURL() string {
	return t.Identity.EndpointBase()
}

func (t *HTTPTransport) Authorization() string {
	return base64.StdEncoding.EncodeToString([]byte(t.UserAgent + ":" + t.Identity.AccessKey))
}

func (t *HTTPTransport) Connect(ctx context.Context) error {
	endpoint := t.BaseURL() + "/connect?authorization=" + url.QueryEscape(t.Authorization())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: connect status %d", ErrConnection, resp.StatusCode)
	}
	t.mu.Lock()
	t.connected = true
	t.mu.Unlock()

	deadline := time.Now().Add(6 * time.Second)
	for !t.IsHandshakeComplete() && time.Now().Before(deadline) {
		if err := t.PollOnce(ctx); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !t.IsHandshakeComplete() {
		return fmt.Errorf("%w: HiveMind HTTP handshake timed out", ErrTimeout)
	}
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
	t.mu.Unlock()
	return nil
}

func (t *HTTPTransport) Healthcheck() TransportHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()
	health := TransportHealth{Connected: t.connected, HandshakeComplete: t.handshake, TransportAlive: t.connected}
	if t.lastError != nil {
		health.LastError = t.lastError.Error()
	}
	return health
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
