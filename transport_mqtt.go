package thalovant

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type MQTTTransport struct {
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
	mu             sync.RWMutex
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

func (t *MQTTTransport) Connect(ctx context.Context) error {
	t.beginConnection()
	if t.Identity.MQTT == nil {
		err := fmt.Errorf("%w: identity does not include MQTT broker credentials", ErrProtocol)
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
	opts.SetClientID("thalovant-" + safeMQTTClientID(t.Identity.AccessKey))
	opts.SetUsername(t.Identity.MQTT.Username)
	opts.SetPassword(t.Identity.MQTT.Password)
	opts.SetCleanSession(true)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetAutoReconnect(true)
	if t.Identity.MQTT.TLS {
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	opts.SetWill(t.Topics.Status, "offline", 1, true)
	opts.SetDefaultPublishHandler(func(_ mqtt.Client, message mqtt.Message) {
		if err := t.handleRawMessage(context.Background(), message.Payload()); err != nil {
			t.mu.Lock()
			t.lastError = err
			t.connected = false
			t.connection.fail(time.Now(), err)
			t.mu.Unlock()
		}
	})
	client := mqtt.NewClient(opts)
	t.client = client
	if err := waitMQTTToken(ctx, client.Connect(), "MQTT connect"); err != nil {
		t.failConnection(err)
		return err
	}
	t.mu.Lock()
	t.connected = true
	t.connection.markOpen(time.Now(), false)
	ready := t.handshakeReady
	t.mu.Unlock()
	if err := waitMQTTToken(ctx, client.Subscribe(t.Topics.S2C, t.Identity.MQTT.QOS, nil), "MQTT subscribe"); err != nil {
		t.failConnection(err)
		return err
	}
	if err := waitMQTTToken(ctx, client.Publish(t.Topics.Status, 1, true, "online"), "MQTT status publish"); err != nil {
		t.failConnection(err)
		return err
	}
	if err := t.sendHiveMessage(ctx, helloHiveMessage(t.Identity, "thalovant-go-mqtt-"), true); err != nil {
		t.failConnection(err)
		return err
	}
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
		err := fmt.Errorf("%w: HiveMind MQTT handshake timed out", ErrTimeout)
		t.failConnection(err)
		return err
	}
}

func (t *MQTTTransport) Disconnect(ctx context.Context) error {
	if t.client != nil && t.client.IsConnected() {
		_ = waitMQTTToken(ctx, t.client.Publish(t.Topics.Status, 1, true, "offline"), "MQTT status publish")
		t.client.Disconnect(250)
	}
	t.mu.Lock()
	t.connected = false
	t.handshake = false
	t.client = nil
	t.connection.close()
	t.mu.Unlock()
	return nil
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
	message, err := decodeMQTTHiveMessage(t.Identity, raw)
	if err != nil {
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

func (t *MQTTTransport) handleHandshake(ctx context.Context, payload map[string]any) error {
	if truthy(payload["preshared_key"]) && !truthy(payload["handshake"]) && payload["envelope"] == nil {
		if RuntimeCryptoKey(t.Identity.CryptoKey) == nil {
			return fmt.Errorf("%w: HiveMind requested preshared key but identity crypto_key is missing", ErrConnection)
		}
		t.mu.Lock()
		if !t.handshake {
			t.handshake = true
			close(t.handshakeReady)
		}
		t.mu.Unlock()
		return nil
	}
	return fmt.Errorf("%w: only preshared-key HiveMind MQTT handshakes are supported", ErrConnection)
}

func (t *MQTTTransport) sendHiveMessage(ctx context.Context, message HiveMessage, _ bool) error {
	if t.client == nil || !t.client.IsConnected() {
		return fmt.Errorf("%w: HiveMind MQTT transport is not connected", ErrConnection)
	}
	payload, err := EncodeHiveBinaryFrame(message)
	if err != nil {
		return err
	}
	if strings.TrimSpace(t.Identity.CryptoKey) != "" {
		payload, err = EncryptAsBinary(t.Identity.CryptoKey, payload)
		if err != nil {
			return err
		}
	}
	qos := byte(1)
	if t.Identity.MQTT != nil {
		qos = t.Identity.MQTT.QOS
	}
	return waitMQTTToken(ctx, t.client.Publish(t.Topics.C2S, qos, false, payload), "MQTT publish")
}

func decodeMQTTHiveMessage(identity Identity, raw []byte) (HiveMessage, error) {
	var message HiveMessage
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err == nil {
		if _, ok := parsed["ciphertext"]; ok && strings.TrimSpace(identity.CryptoKey) != "" {
			decrypted, err := DecryptFromJSON(identity.CryptoKey, string(raw))
			if err != nil {
				return HiveMessage{}, err
			}
			if err := json.Unmarshal([]byte(decrypted), &message); err != nil {
				return HiveMessage{}, err
			}
			return message, nil
		}
		if _, ok := parsed["msg_type"]; ok {
			if err := json.Unmarshal(raw, &message); err != nil {
				return HiveMessage{}, err
			}
			return message, nil
		}
	}
	if strings.TrimSpace(identity.CryptoKey) != "" {
		decrypted, err := DecryptBinary(identity.CryptoKey, raw)
		if err == nil {
			return DecodeHiveBinaryFrame(decrypted)
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
	t.lastError = err
	t.connection.fail(time.Now(), err)
	t.mu.Unlock()
}

func safeMQTTClientID(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			builder.WriteRune(char)
		} else {
			builder.WriteRune('-')
		}
		if builder.Len() >= 48 {
			break
		}
	}
	if builder.Len() == 0 {
		return NewSessionID()
	}
	return builder.String()
}
