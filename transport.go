package thalovant

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
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
	Identity     Identity
	UserAgent    string
	PollInterval time.Duration
	HTTPClient   *http.Client
	// NoiseStateDir selects the persistent client key and hub pin directory.
	NoiseStateDir string
	noise         *noiseChannel
	pollMu        sync.Mutex
	lifecycleMu   sync.Mutex
	pollDone      chan struct{}
	BusEvents     chan Event
	HiveEvents    chan HiveMessage
	admitted      bool
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

// requireTLSEndpoint refuses a hub endpoint that is not https.
func requireTLSEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%w: the HTTP transport needs a valid https:// endpoint; got %q", ErrConnection, endpoint)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%w: refusing to use the HTTP transport over %s://. It needs an https:// endpoint: without TLS every message and the access key travel in the clear", ErrConnection, parsed.Scheme)
	}
	return nil
}

func (t *HTTPTransport) Authorization() string {
	return base64.StdEncoding.EncodeToString([]byte(t.UserAgent + ":" + t.Identity.AccessKey))
}

func (t *HTTPTransport) Connect(ctx context.Context) (err error) {
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	t.stopPolling()
	t.mu.Lock()
	admitted := t.admitted
	t.admitted = false
	t.mu.Unlock()
	if admitted {
		// The HTTP plugin only offers a fresh handshake for an unregistered peer.
		// Reset this object's own previous admission before renewing its session.
		cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, _ = t.request(cleanupCtx, http.MethodPost, "/disconnect", nil)
		cancel()
	}
	t.invalidateNoise()
	t.beginConnection()
	defer func() {
		if err != nil {
			t.failConnection(err)
		}
	}()
	if err = requireTLSEndpoint(t.BaseURL()); err != nil {
		return err
	}
	if t.Identity.Password == "" {
		return fmt.Errorf("%w: v3 Noise requires the identity password", ErrIdentity)
	}
	client := t.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	// Never mutate the caller's client or a process-global default. Keep the jar
	// across requests and reconnects for the HTTP plugin's replica affinity cookie.
	copyClient := *client
	// Identity authorization and encrypted form traffic are bound to this
	// endpoint; never let a redirect move credentials or downgrade TLS.
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if copyClient.Jar == nil {
		copyClient.Jar, err = cookiejar.New(nil)
		if err != nil {
			return err
		}
	}
	t.HTTPClient = &copyClient
	t.mu.Lock()
	t.noise = &noiseChannel{identity: t.Identity, stateDir: t.NoiseStateDir, write: t.writeFrame}
	t.mu.Unlock()
	handshakeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err = t.request(handshakeCtx, http.MethodPost, "/connect", nil); err != nil {
		return err
	}
	t.mu.Lock()
	t.connected = true
	t.admitted = true
	t.connection.markOpen(time.Now(), false)
	t.mu.Unlock()
	defer func() {
		if err != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cleanupCancel()
			_, _ = t.request(cleanupCtx, http.MethodPost, "/disconnect", nil)
			t.mu.Lock()
			t.admitted = false
			t.mu.Unlock()
		}
	}()
	for !t.IsHandshakeComplete() {
		if err = t.PollOnce(handshakeCtx); err != nil {
			return err
		}
		if t.IsHandshakeComplete() {
			break
		}
		select {
		case <-handshakeCtx.Done():
			return fmt.Errorf("%w: HiveMind HTTP Noise handshake timed out", ErrTimeout)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.completeConnection()
	pollCtx, pollCancel := context.WithCancel(context.Background())
	t.cancelPolling = pollCancel
	t.pollDone = make(chan struct{})
	go func(done chan struct{}) { defer close(done); t.pollLoop(pollCtx) }(t.pollDone)
	return nil
}

// stopPolling joins the old reader before replacing a channel, preventing a
// previous connection's delayed poll from consuming a new session's counters.
func (t *HTTPTransport) stopPolling() {
	if t.cancelPolling != nil {
		t.cancelPolling()
		t.cancelPolling = nil
	}
	if t.pollDone != nil {
		<-t.pollDone
		t.pollDone = nil
	}
}

func (t *HTTPTransport) Disconnect(ctx context.Context) error {
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	t.stopPolling()
	t.invalidateNoise()
	t.pollMu.Lock()
	defer t.pollMu.Unlock()
	t.mu.Lock()
	admitted := t.admitted
	t.admitted = false
	t.mu.Unlock()
	var err error
	if admitted {
		_, err = t.request(ctx, http.MethodPost, "/disconnect", nil)
	}
	t.mu.Lock()
	t.connected, t.handshake, t.admitted = false, false, false
	t.noise = nil
	t.connection.close()
	t.mu.Unlock()
	return err
}

func (t *HTTPTransport) invalidateNoise() {
	t.mu.Lock()
	channel := t.noise
	t.noise = nil
	t.connected, t.handshake = false, false
	t.mu.Unlock()
	if channel != nil {
		channel.mu.Lock()
		channel.failed = true
		channel.mu.Unlock()
	}
}

// RemoteStaticKey returns the authenticated peer key, empty outside a session.
func (t *HTTPTransport) RemoteStaticKey() string {
	t.mu.RLock()
	channel := t.noise
	t.mu.RUnlock()
	if channel == nil {
		return ""
	}
	return channel.remoteKey()
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
				t.handshake = false
				t.noise = nil
				t.connected = false
				t.connection.fail(time.Now(), err)
				t.mu.Unlock()
				return
			}
		}
	}
}

func (t *HTTPTransport) PollOnce(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			t.failConnection(err)
		}
	}()
	t.pollMu.Lock()
	defer t.pollMu.Unlock()
	body, err := t.request(ctx, http.MethodGet, "/get_messages", nil)
	if err != nil {
		return err
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		return fmt.Errorf("%w: malformed HTTP message queue", ErrProtocol)
	}
	for _, raw := range messages {
		if err := t.handleRawMessage(ctx, raw); err != nil {
			return err
		}
	}
	t.mu.RLock()
	channel := t.noise
	t.mu.RUnlock()
	if channel == nil || !channel.ready() {
		return nil
	}
	body, err = t.request(ctx, http.MethodGet, "/get_binary_messages", nil)
	if err != nil {
		return err
	}
	frames, ok := body["b64_messages"].([]any)
	if !ok {
		return fmt.Errorf("%w: malformed HTTP binary message queue", ErrProtocol)
	}
	for _, encoded := range frames {
		value, ok := encoded.(string)
		if !ok {
			return fmt.Errorf("%w: malformed HTTP binary frame", ErrProtocol)
		}
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return fmt.Errorf("%w: malformed HTTP binary frame", ErrProtocol)
		}
		message, err := channel.receive(ctx, raw, true)
		if err != nil {
			return err
		}
		t.dispatch(message)
	}
	return nil
}

func (t *HTTPTransport) handleRawMessage(ctx context.Context, raw any) error {
	var rawBytes []byte
	if value, ok := raw.(string); ok {
		rawBytes = []byte(value)
	} else {
		var err error
		rawBytes, err = json.Marshal(raw)
		if err != nil {
			return err
		}
	}
	t.mu.RLock()
	channel := t.noise
	t.mu.RUnlock()
	if channel == nil {
		return fmt.Errorf("%w: HTTP transport is not connected", ErrConnection)
	}
	message, err := channel.receive(ctx, rawBytes, false)
	if err != nil {
		return err
	}
	t.dispatch(message)
	if channel.ready() {
		t.mu.Lock()
		t.handshake = true
		t.mu.Unlock()
	}
	return nil
}

func (t *HTTPTransport) dispatch(message *HiveMessage) {
	if message == nil {
		return
	}
	dispatchNoiseMessage(t.BusEvents, t.HiveEvents, *message)
}

func dispatchNoiseMessage(bus chan Event, hive chan HiveMessage, message HiveMessage) {
	switch message.MsgType {
	case "bus":
		select {
		case bus <- Event{Name: fmt.Sprint(message.Payload["type"]), Data: mapValue(message.Payload["data"]), Context: mapValue(message.Payload["context"]), Raw: message}:
		default:
		}
	case "query", "cascade":
		select {
		case hive <- message:
		default:
		}
	}
}

func (t *HTTPTransport) beginConnection() {
	t.mu.Lock()
	t.connected, t.handshake = false, false
	t.noise = nil
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
	t.connected, t.handshake = false, false
	t.noise = nil
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

func (t *HTTPTransport) sendHiveMessage(ctx context.Context, message HiveMessage, _ bool) error {
	t.mu.RLock()
	channel := t.noise
	connected := t.connected
	t.mu.RUnlock()
	if channel == nil || !connected {
		return fmt.Errorf("%w: HTTP transport is not connected", ErrConnection)
	}
	if err := channel.send(ctx, message); err != nil {
		t.failConnection(err)
		return err
	}
	return nil
}

func (t *HTTPTransport) writeFrame(ctx context.Context, raw []byte, binary bool) error {
	form := url.Values{}
	if binary {
		form.Set("message", base64.StdEncoding.EncodeToString(raw))
		form.Set("binary", "1")
	} else {
		form.Set("message", string(raw))
	}
	_, err := t.request(ctx, http.MethodPost, "/send_message", form)
	return err
}

func (t *HTTPTransport) request(ctx context.Context, method, path string, form url.Values) (map[string]any, error) {
	endpoint := t.BaseURL() + path + "?authorization=" + url.QueryEscape(t.Authorization())
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, scrubTransportError(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnection, scrubTransportError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: HTTP %s status %d", ErrConnection, path, resp.StatusCode)
	}
	var body map[string]any
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil || body == nil {
		return nil, fmt.Errorf("%w: malformed HTTP %s response", ErrProtocol, path)
	}
	if _, failed := body["error"]; failed {
		return nil, fmt.Errorf("%w: HTTP %s rejected by the hub", ErrRuntime, path)
	}
	switch path {
	case "/connect":
		if body["status"] != "Connected" {
			return nil, fmt.Errorf("%w: HTTP connect was not acknowledged", ErrProtocol)
		}
	case "/send_message":
		if body["status"] != "message sent" && body["status"] != "buffered" {
			return nil, fmt.Errorf("%w: HTTP send was not acknowledged", ErrProtocol)
		}
	case "/disconnect":
		if body["status"] != "Disconnected" {
			return nil, fmt.Errorf("%w: HTTP disconnect was not acknowledged", ErrProtocol)
		}
	}
	return body, nil
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

// scrubTransportError removes the query string from a *url.Error's URL so a
// wrapped transport error never carries the ?authorization=<base64(user-agent:
// access key)> data-plane credential into LastError, which ConnectionInfo() and
// Healthcheck() serialize to JSON. Non-URL errors are returned unchanged.
func scrubTransportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		cleaned := *urlErr
		cleaned.URL = stripURLQuery(urlErr.URL)
		return &cleaned
	}
	return err
}

// stripURLQuery returns rawURL without its query string or fragment.
func stripURLQuery(rawURL string) string {
	if rawURL == "" {
		return rawURL
	}
	if parsed, err := url.Parse(rawURL); err == nil {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	if idx := strings.IndexAny(rawURL, "?#"); idx >= 0 {
		return rawURL[:idx]
	}
	return rawURL
}
