package thalovant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type MQTTTransport struct {
	Identity  Identity
	UserAgent string
	Topics    MqttTopicSet
	BusEvents chan Event
	client    mqtt.Client
	connected bool
	handshake bool
	lastError error
	mu        sync.RWMutex
}

func NewMQTTTransport(identity Identity) (*MQTTTransport, error) {
	topics, err := MQTTTopicsForIdentity(identity)
	if err != nil {
		return nil, err
	}
	return &MQTTTransport{
		Identity:  identity,
		UserAgent: DefaultUserAgent,
		Topics:    topics,
		BusEvents: make(chan Event, 32),
	}, nil
}

func (t *MQTTTransport) Connect(ctx context.Context) error {
	if t.Identity.MQTT == nil {
		return fmt.Errorf("%w: identity does not include MQTT broker credentials", ErrProtocol)
	}
	brokerURL, err := pahoBrokerURL(t.Identity.MQTT.Endpoint)
	if err != nil {
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
	opts.SetWill(t.Topics.Status, "offline", 1, true)
	opts.SetDefaultPublishHandler(func(_ mqtt.Client, message mqtt.Message) {
		if err := t.handleRawMessage(context.Background(), message.Payload()); err != nil {
			t.mu.Lock()
			t.lastError = err
			t.connected = false
			t.mu.Unlock()
		}
	})
	client := mqtt.NewClient(opts)
	t.client = client
	if err := waitMQTTToken(ctx, client.Connect(), "MQTT connect"); err != nil {
		return err
	}
	if err := waitMQTTToken(ctx, client.Subscribe(t.Topics.S2C, t.Identity.MQTT.QOS, nil), "MQTT subscribe"); err != nil {
		return err
	}
	if err := waitMQTTToken(ctx, client.Publish(t.Topics.Status, 1, true, "online"), "MQTT status publish"); err != nil {
		return err
	}
	t.mu.Lock()
	t.connected = true
	t.mu.Unlock()
	if err := t.sendHiveMessage(ctx, helloHiveMessage(t.Identity, "thalovant-go-mqtt-"), false); err != nil {
		return err
	}
	deadline := time.Now().Add(6 * time.Second)
	for !t.IsHandshakeComplete() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !t.IsHandshakeComplete() {
		_ = t.Disconnect(ctx)
		return fmt.Errorf("%w: HiveMind MQTT handshake timed out", ErrTimeout)
	}
	return nil
}

func (t *MQTTTransport) Disconnect(ctx context.Context) error {
	if t.client != nil && t.client.IsConnected() {
		_ = waitMQTTToken(ctx, t.client.Publish(t.Topics.Status, 1, true, "offline"), "MQTT status publish")
		t.client.Disconnect(250)
	}
	t.mu.Lock()
	t.connected = false
	t.handshake = false
	t.mu.Unlock()
	return nil
}

func (t *MQTTTransport) Healthcheck() TransportHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()
	health := TransportHealth{Connected: t.connected, HandshakeComplete: t.handshake, TransportAlive: t.connected && t.client != nil && t.client.IsConnected()}
	if t.lastError != nil {
		health.LastError = t.lastError.Error()
	}
	return health
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

func (t *MQTTTransport) IsHandshakeComplete() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.handshake
}

func (t *MQTTTransport) handleRawMessage(ctx context.Context, raw []byte) error {
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

func (t *MQTTTransport) handleHandshake(ctx context.Context, payload map[string]any) error {
	if truthy(payload["preshared_key"]) && !truthy(payload["handshake"]) && payload["envelope"] == nil {
		if RuntimeCryptoKey(t.Identity.CryptoKey) == nil {
			return fmt.Errorf("%w: HiveMind requested preshared key but identity crypto_key is missing", ErrConnection)
		}
		if err := t.sendHiveMessage(ctx, helloHiveMessage(t.Identity, "thalovant-go-mqtt-"), false); err != nil {
			return err
		}
		t.mu.Lock()
		t.handshake = true
		t.mu.Unlock()
		return nil
	}
	return fmt.Errorf("%w: only preshared-key HiveMind MQTT handshakes are supported", ErrConnection)
}

func (t *MQTTTransport) sendHiveMessage(ctx context.Context, message HiveMessage, encrypt bool) error {
	if t.client == nil || !t.client.IsConnected() {
		return fmt.Errorf("%w: HiveMind MQTT transport is not connected", ErrConnection)
	}
	payload, err := serializeHiveMessage(t.Identity, t.IsHandshakeComplete(), message, encrypt)
	if err != nil {
		return err
	}
	qos := byte(1)
	if t.Identity.MQTT != nil {
		qos = t.Identity.MQTT.QOS
	}
	return waitMQTTToken(ctx, t.client.Publish(t.Topics.C2S, qos, false, payload), "MQTT publish")
}

func pahoBrokerURL(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "mqtt":
		parsed.Scheme = "tcp"
	case "mqtts":
		parsed.Scheme = "ssl"
	case "tcp", "ssl", "ws", "wss":
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
