package thalovant

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type MQTTTransport struct {
	streams        runtimeStreams
	Identity       Identity
	UserAgent      string
	Topics         MqttTopicSet
	BusEvents      chan Event
	HiveEvents     chan HiveMessage
	client         mqtt.Client
	connected      bool
	handshake      bool
	lastError      error
	connection     connectionTelemetry
	handshakeReady chan struct{}
	// NoiseStateDir selects the persistent client key and hub pin directory.
	NoiseStateDir string
	// TLSConfig optionally supplies broker trust roots or a client certificate.
	TLSConfig    *tls.Config
	noise        *noiseChannel
	lifecycleMu  contextMutex
	failureReady chan struct{}
	generation   uint64
	mu           sync.RWMutex
}

func NewMQTTTransport(identity Identity) (*MQTTTransport, error) {
	topics, err := MQTTTopicsForIdentity(identity)
	if err != nil {
		return nil, err
	}
	return &MQTTTransport{
		Identity:       identity,
		UserAgent:      DefaultUserAgent,
		Topics:         topics,
		BusEvents:      make(chan Event, 32),
		HiveEvents:     make(chan HiveMessage, 32),
		handshakeReady: make(chan struct{}),
	}, nil
}

func (t *MQTTTransport) Connect(ctx context.Context) (err error) {
	ctx, cancelConnect := context.WithTimeout(ctx, 20*time.Second)
	defer cancelConnect()
	if err := t.lifecycleMu.Lock(ctx); err != nil {
		return err
	}
	defer t.lifecycleMu.Unlock()
	health := t.Healthcheck()
	if health.Connected && health.HandshakeComplete {
		return nil
	}
	t.closeClient()
	t.beginConnection()
	defer func() {
		if err != nil {
			t.closeClient()
			t.failConnection(err)
		}
	}()
	if t.Identity.MQTT == nil {
		err := fmt.Errorf("%w: identity does not include MQTT broker credentials", ErrProtocol)
		t.failConnection(err)
		return err
	}
	if t.Identity.Password == "" {
		return fmt.Errorf("%w: v3 Noise requires the identity password", ErrIdentity)
	}
	// TLS protects broker credentials; Noise protects the end-to-end hub session.
	if !t.Identity.MQTT.TLS {
		err := fmt.Errorf("%w: refusing to connect to an MQTT broker without TLS. Use an mqtts:// endpoint, or set tls: true on the identity's mqtt block", ErrConnection)
		t.failConnection(err)
		return err
	}
	brokerURL, err := pahoBrokerURL(t.Identity.MQTT.Endpoint, t.Identity.MQTT.TLS)
	if err != nil {
		t.failConnection(err)
		return err
	}
	opts := mqtt.NewClientOptions()
	opts.AddBroker(brokerURL)
	// Broker authentication and ACLs use the per-client credentials/topics.
	// Keep the broker's connection identifier independent of the access key.
	opts.SetClientID("thalovant-" + NewSessionID())
	opts.SetUsername(t.Identity.MQTT.Username)
	opts.SetPassword(t.Identity.MQTT.Password)
	opts.SetCleanSession(true)
	opts.SetKeepAlive(60 * time.Second)
	// A broker reconnect has lost its subscription and its Noise session. Do
	// not let Paho resume publishing with stale counters; callers reconnect the
	// transport explicitly, which resubscribes and performs a fresh exchange.
	opts.SetAutoReconnect(false)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.TLSConfig != nil {
		tlsConfig = t.TLSConfig.Clone()
		if tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	opts.SetTLSConfig(tlsConfig)
	opts.SetWill(t.Topics.Status, "offline", 1, true)
	t.mu.RLock()
	generation := t.generation
	t.mu.RUnlock()
	opts.SetConnectionLostHandler(func(_ mqtt.Client, _ error) {
		t.failGeneration(generation, fmt.Errorf("%w: MQTT broker disconnected; reconnect required", ErrConnection))
	})
	opts.SetDefaultPublishHandler(func(_ mqtt.Client, message mqtt.Message) {
		t.mu.RLock()
		current := t.generation == generation && t.connected
		channel := t.noise
		t.mu.RUnlock()
		if !current || channel == nil {
			return
		}
		messageCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := t.receive(messageCtx, channel, message.Payload()); err != nil {
			t.failGeneration(generation, err)
		}
	})
	client := mqtt.NewClient(opts)
	channel := &noiseChannel{identity: t.Identity, stateDir: t.NoiseStateDir, write: func(ctx context.Context, raw []byte, _ bool) error {
		return waitMQTTToken(ctx, client.Publish(t.Topics.Inbound, t.Identity.MQTT.QOS, false, raw), "MQTT publish")
	}}
	t.mu.Lock()
	t.client = client
	t.noise = channel
	t.mu.Unlock()
	if err := waitMQTTToken(ctx, client.Connect(), "MQTT connect"); err != nil {
		t.failConnection(err)
		return err
	}
	t.mu.Lock()
	t.connected = true
	t.connection.markOpen(time.Now(), false)
	ready := t.handshakeReady
	failed := t.failureReady
	t.mu.Unlock()
	if err := waitMQTTToken(ctx, client.Subscribe(t.Topics.Outbound, t.Identity.MQTT.QOS, nil), "MQTT subscribe"); err != nil {
		t.failConnection(err)
		return err
	}
	if err := waitMQTTToken(ctx, client.Publish(t.Topics.Status, 1, true, "online"), "MQTT status publish"); err != nil {
		t.failConnection(err)
		return err
	}
	// A cleartext HELLO creates the server-side MQTT peer and triggers its
	// HELLO/offer. Application messages remain blocked until Noise is complete.
	initial, err := json.Marshal(helloHiveMessage(t.Identity, "thalovant-go-mqtt-"))
	if err != nil {
		return err
	}
	if err := channel.write(ctx, initial, false); err != nil {
		t.failConnection(err)
		return err
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		t.completeConnection()
		return nil
	case <-failed:
		t.mu.RLock()
		err := t.lastError
		t.mu.RUnlock()
		return err
	case <-ctx.Done():
		err := fmt.Errorf("%w: %v", ErrTimeout, ctx.Err())
		t.failConnection(err)
		return err
	case <-timer.C:
		err := fmt.Errorf("%w: HiveMind MQTT handshake timed out", ErrTimeout)
		t.failConnection(err)
		return err
	}
}

func (t *MQTTTransport) Disconnect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := t.lifecycleMu.Lock(ctx); err != nil {
		return err
	}
	defer t.lifecycleMu.Unlock()
	t.mu.RLock()
	client := t.client
	t.mu.RUnlock()
	if client != nil && client.IsConnected() {
		_ = waitMQTTToken(ctx, client.Publish(t.Topics.Status, 1, true, "offline"), "MQTT status publish")
	}
	t.closeClient()
	t.mu.Lock()
	t.connection.close()
	t.mu.Unlock()
	return nil
}

func (t *MQTTTransport) closeClient() {
	t.mu.Lock()
	client, channel := t.client, t.noise
	t.generation++
	t.client, t.noise = nil, nil
	t.connected, t.handshake = false, false
	t.mu.Unlock()
	// Disconnect first to unblock any publish waiting for the broker.
	if client != nil {
		client.Disconnect(0)
	}
	if channel != nil {
		channel.mu.Lock()
		channel.failed = true
		channel.mu.Unlock()
	}
}

// RemoteStaticKey returns the authenticated hub key, empty outside a session.
func (t *MQTTTransport) RemoteStaticKey() string {
	t.mu.RLock()
	channel, connected := t.noise, t.connected
	t.mu.RUnlock()
	if channel == nil || !connected {
		return ""
	}
	return channel.remoteKey()
}

func (t *MQTTTransport) Healthcheck() TransportHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()
	health := TransportHealth{Connected: t.connected, HandshakeComplete: t.handshake, TransportAlive: t.connected && t.client != nil && t.client.IsConnected(), Connection: t.connection.snapshot()}
	if t.lastError != nil {
		health.LastError = t.lastError.Error()
	}
	return health
}

func (t *MQTTTransport) ConnectionInfo() TransportConnectionInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.connection.snapshot()
}

func (t *MQTTTransport) EmitBus(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return t.sendHiveMessage(ctx, HiveMessage{
		MsgType:  "bus",
		Payload:  map[string]any{"type": eventType, "data": data, "context": eventContext},
		Metadata: map[string]any{},
		Route:    []any{},
	}, true)
}

func (t *MQTTTransport) Events() <-chan Event {
	return t.BusEvents
}

func (t *MQTTTransport) HiveMessages() <-chan HiveMessage {
	return t.HiveEvents
}

func (t *MQTTTransport) SendHiveMessage(ctx context.Context, message HiveMessage, encrypt bool) error {
	return t.sendHiveMessage(ctx, message, encrypt)
}

func (t *MQTTTransport) IsHandshakeComplete() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.handshake
}

func (t *MQTTTransport) handleRawMessage(ctx context.Context, raw []byte) error {
	t.mu.RLock()
	channel := t.noise
	t.mu.RUnlock()
	if channel == nil {
		return fmt.Errorf("%w: MQTT transport is not connected", ErrConnection)
	}
	err := t.receive(ctx, channel, raw)
	if err != nil {
		t.mu.RLock()
		generation := t.generation
		t.mu.RUnlock()
		t.failGeneration(generation, err)
	}
	return err
}

func (t *MQTTTransport) receive(ctx context.Context, channel *noiseChannel, raw []byte) error {
	message, err := channel.receive(ctx, raw, channel.ready())
	if err != nil {
		return err
	}
	if message != nil {
		dispatchNoiseMessage(t.BusEvents, t.HiveEvents, *message, &t.streams)
	}
	if channel.ready() {
		t.mu.Lock()
		if t.noise == channel && t.connected && !t.handshake {
			t.handshake = true
			close(t.handshakeReady)
		}
		t.mu.Unlock()
	}
	return nil
}

func (t *MQTTTransport) sendHiveMessage(ctx context.Context, message HiveMessage, _ bool) error {
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ctx = sendCtx
	if err := t.lifecycleMu.Lock(ctx); err != nil {
		return err
	}
	defer t.lifecycleMu.Unlock()
	t.mu.RLock()
	channel, connected, generation := t.noise, t.connected, t.generation
	t.mu.RUnlock()
	if channel == nil || !connected {
		return fmt.Errorf("%w: MQTT transport is not connected", ErrConnection)
	}
	if err := channel.send(sendCtx, message); err != nil {
		t.failGeneration(generation, err)
		return err
	}
	return nil
}

func (t *MQTTTransport) failGeneration(generation uint64, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.generation != generation || !t.connected {
		return
	}
	t.connected, t.handshake = false, false
	t.lastError = err
	t.connection.fail(time.Now(), err)
	close(t.failureReady)
}

func decodeMQTTHiveMessage(_ Identity, raw []byte) (HiveMessage, error) {
	var message HiveMessage
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err == nil {
		if _, ok := parsed["msg_type"]; ok {
			if err := json.Unmarshal(raw, &message); err != nil {
				return HiveMessage{}, err
			}
			return message, nil
		}
	}
	return DecodeHiveBinaryFrame(raw)
}

func pahoBrokerURL(endpoint string, tlsEnabled bool) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "mqtt":
		if tlsEnabled {
			parsed.Scheme = "ssl"
		} else {
			parsed.Scheme = "tcp"
		}
	case "mqtts", "ssl":
		parsed.Scheme = "ssl"
	case "tcp":
		if tlsEnabled {
			parsed.Scheme = "ssl"
		}
	case "ws":
		if tlsEnabled {
			parsed.Scheme = "wss"
		}
	case "wss":
	default:
		return "", fmt.Errorf("%w: MQTT endpoint must start with mqtt://, mqtts://, tcp://, ssl://, ws://, or wss://", ErrConnection)
	}
	return parsed.String(), nil
}

func waitMQTTToken(ctx context.Context, token mqtt.Token, operation string) error {
	done := make(chan struct{})
	go func() {
		token.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w: %s timed out", ErrTimeout, operation)
	case <-done:
		if err := token.Error(); err != nil {
			return fmt.Errorf("%w: %s failed: %v", ErrConnection, operation, err)
		}
		return nil
	}
}

func (t *MQTTTransport) beginConnection() {
	t.mu.Lock()
	t.lastError = nil
	t.connected = false
	t.handshake = false
	t.handshakeReady = make(chan struct{})
	t.failureReady = make(chan struct{})
	t.connection.begin(time.Now())
	t.mu.Unlock()
}

func (t *MQTTTransport) completeConnection() {
	t.mu.Lock()
	t.connection.complete(time.Now())
	t.mu.Unlock()
}

func (t *MQTTTransport) failConnection(err error) {
	t.mu.Lock()
	t.connected, t.handshake = false, false
	t.lastError = err
	t.connection.fail(time.Now(), err)
	t.mu.Unlock()
}
