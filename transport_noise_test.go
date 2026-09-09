package thalovant

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/eclipse/paho.mqtt.golang/packets"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
)

// The responder uses the r8 capability schema and real Noise cipher states.
// It independently consumes clear handshake messages, rejects plaintext bus
// traffic, and echoes only authenticated bus messages as encrypted responses.
type transportResponder struct {
	key        noise.DHKey
	peer       []byte
	psk        []byte
	hello      map[string]any
	offer      map[string]any
	handshake  *noiseHandshake
	session    *noiseSession
	plain      []string
	binary     []string
	helloCount int
	patterns   []string
}

func newTransportResponder(t *testing.T) *transportResponder {
	t.Helper()
	key, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &transportResponder{key: key, psk: derivePSK("test-password", "test-hub")}
}

func (s *transportResponder) reset() {
	s.hello = map[string]any{"node_id": "test-hub", "pubkey": "test-public", "peer": "test-peer"}
	patterns := []any{"XXpsk2"}
	if len(s.peer) > 0 {
		patterns = append(patterns, "KKpsk0")
	}
	s.offer = map[string]any{"max_protocol_version": json.Number("3"), "binarize": true, "encodings": []any{"JSON-HEX"}, "ciphers": []any{"AES-GCM"}, "noise": map[string]any{"patterns": patterns, "suites": []any{"25519_ChaChaPoly_SHA256", "25519_AESGCM_SHA256"}}}
	s.handshake, s.session = nil, nil
	s.plain = []string{marshalHive("hello", s.hello), marshalHive("shake", s.offer)}
	s.binary = []string{}
}

func marshalHive(kind string, payload map[string]any) string {
	raw, _ := json.Marshal(HiveMessage{MsgType: kind, Payload: payload, Metadata: map[string]any{}, Route: []any{}})
	return string(raw)
}

func (s *transportResponder) receive(raw []byte, binary bool) error {
	if binary {
		if s.session == nil {
			return fmt.Errorf("binary before split")
		}
		payload, isJSON, complete, err := s.session.decryptFrame(raw)
		if err != nil {
			return err
		}
		if !complete {
			return nil
		}
		if !isJSON {
			return fmt.Errorf("expected JSON marker")
		}
		var message HiveMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			return err
		}
		if message.MsgType == "hello" {
			s.helloCount++
			return nil
		}
		if message.MsgType != "bus" {
			return fmt.Errorf("unexpected application message")
		}
		return s.session.sendMessage(payload, true, func(frame []byte) error {
			s.binary = append(s.binary, base64.StdEncoding.EncodeToString(frame))
			return nil
		})
	}
	if s.session != nil {
		return fmt.Errorf("plaintext after split")
	}
	decoded, err := decodeJSONNumbers(raw)
	if err != nil {
		return err
	}
	if decoded["msg_type"] != "shake" {
		return fmt.Errorf("plaintext application traffic")
	}
	params := mapValue(mapValue(decoded["payload"])["noise"])
	encoded, _ := params["msg"].(string)
	msg, err := hex.DecodeString(encoded)
	if err != nil {
		return err
	}
	if s.handshake == nil {
		pattern, _ := params["pattern"].(string)
		suite, _ := params["suite"].(string)
		cipher, err := noiseCipherSuite(suite)
		if err != nil {
			return err
		}
		prologue, err := buildPrologue(s.hello, s.offer, noiseProtocolName(pattern, suite))
		if err != nil {
			return err
		}
		config := noise.Config{CipherSuite: cipher, Random: rand.Reader, Initiator: false, Prologue: prologue, PresharedKey: s.psk, StaticKeypair: s.key}
		switch pattern {
		case noisePatternXX:
			config.Pattern, config.PresharedKeyPlacement = noise.HandshakeXX, 2
		case noisePatternKK:
			config.Pattern, config.PresharedKeyPlacement, config.PeerStatic = noise.HandshakeKK, 0, s.peer
		default:
			return fmt.Errorf("unexpected pattern")
		}
		state, err := noise.NewHandshakeState(config)
		if err != nil {
			return err
		}
		s.handshake = &noiseHandshake{state: state, pattern: pattern, suite: suite}
		s.patterns = append(s.patterns, pattern)
		if _, err := s.handshake.readMessage(msg); err != nil {
			return err
		}
		reply, err := s.handshake.writeMessage(nil)
		if err != nil {
			return err
		}
		s.plain = append(s.plain, marshalHive("shake", map[string]any{"noise": map[string]any{"msg": hex.EncodeToString(reply)}}))
	} else {
		if _, err := s.handshake.readMessage(msg); err != nil {
			return err
		}
	}
	if s.handshake.complete {
		s.peer = append([]byte(nil), s.handshake.state.PeerStatic()...)
		s.session, err = newNoiseSession(s.handshake)
		if err != nil {
			return err
		}
		s.session.send, s.session.recv = s.session.recv, s.session.send
	}
	return nil
}

type httpNoiseFixture struct {
	mu          sync.Mutex
	responder   *transportResponder
	server      *httptest.Server
	reject      string
	tamper      bool
	plainBus    bool
	unsupported bool
	connected   bool
}

func newHTTPNoiseFixture(t *testing.T) *httpNoiseFixture {
	t.Helper()
	fixture := &httpNoiseFixture{responder: newTransportResponder(t)}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *httpNoiseFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	reply := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	if r.URL.Path == f.reject {
		reply(map[string]any{"error": "synthetic rejection"})
		return
	}
	if r.URL.Path != "/connect" {
		cookie, err := r.Cookie("hivemind_http_replica")
		if err != nil || cookie.Value != "test-replica" {
			reply(map[string]any{"error": "missing replica affinity"})
			return
		}
	}
	switch r.URL.Path {
	case "/connect":
		if f.connected {
			reply(map[string]any{"status": "Connected"})
			return
		}
		f.connected = true
		f.responder.reset()
		if f.unsupported {
			f.responder.offer["noise"] = map[string]any{"patterns": []any{"unsupported"}, "suites": []any{"unsupported"}}
			f.responder.plain[1] = marshalHive("shake", f.responder.offer)
		}
		http.SetCookie(w, &http.Cookie{Name: "hivemind_http_replica", Value: "test-replica", Path: "/", Secure: true})
		reply(map[string]any{"status": "Connected"})
	case "/get_messages":
		frames := f.responder.plain
		f.responder.plain = []string{}
		if f.plainBus && f.responder.session != nil {
			frames = append(frames, marshalHive("bus", map[string]any{"type": "untrusted"}))
			f.plainBus = false
		}
		reply(map[string]any{"messages": frames})
	case "/get_binary_messages":
		frames := f.responder.binary
		f.responder.binary = []string{}
		if f.tamper && len(frames) > 0 {
			frame, _ := base64.StdEncoding.DecodeString(frames[0])
			frame[0] ^= 1
			frames[0] = base64.StdEncoding.EncodeToString(frame)
		}
		reply(map[string]any{"b64_messages": frames})
	case "/send_message":
		_ = r.ParseForm()
		raw := []byte(r.Form.Get("message"))
		binary := r.Form.Get("binary") == "1"
		if binary {
			var err error
			raw, err = base64.StdEncoding.DecodeString(string(raw))
			if err != nil {
				reply(map[string]any{"error": "bad base64"})
				return
			}
		}
		if err := f.responder.receive(raw, binary); err != nil {
			reply(map[string]any{"error": "rejected frame"})
			return
		}
		reply(map[string]any{"status": "message sent"})
	case "/disconnect":
		f.connected = false
		reply(map[string]any{"status": "Disconnected"})
	default:
		http.NotFound(w, r)
	}
}

func (f *httpNoiseFixture) transport(t *testing.T) *HTTPTransport {
	t.Helper()
	transport := NewHTTPTransport(Identity{AccessKey: "test-access", Password: "test-password", DefaultMaster: f.server.URL, DataPlaneEndpoints: HubDataPlaneEndpoints{HTTPS: f.server.URL}})
	transport.HTTPClient = f.server.Client()
	transport.NoiseStateDir = t.TempDir()
	transport.PollInterval = time.Hour
	return transport
}

func TestHTTPNoiseRoundTripReconnectAndConcurrentChunks(t *testing.T) {
	fixture := newHTTPNoiseFixture(t)
	transport := fixture.transport(t)
	for attempt := 0; attempt < 2; attempt++ {
		if err := transport.Connect(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !transport.IsHandshakeComplete() || transport.ConnectionInfo().Phase != ConnectionReady || transport.RemoteStaticKey() != hex.EncodeToString(fixture.responder.key.Public) {
			t.Fatal("session not ready/pinned")
		}
		var workers sync.WaitGroup
		for i := 0; i < 4; i++ {
			workers.Add(1)
			go func(i int) {
				defer workers.Done()
				if err := transport.EmitBus(context.Background(), "echo", Data{"text": strings.Repeat("x", 140000)}, Context{"request_id": fmt.Sprint(i)}); err != nil {
					t.Error(err)
				}
			}(i)
		}
		workers.Wait()
		if err := transport.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for i := 0; i < 4; i++ {
			select {
			case event := <-transport.Events():
				ids[fmt.Sprint(event.Context["request_id"])] = true
				if len(fmt.Sprint(event.Data["text"])) != 140000 {
					t.Fatal("chunk loss")
				}
			default:
				t.Fatal("missing encrypted echo")
			}
		}
		if len(ids) != 4 {
			t.Fatal("correlation lost")
		}
		if err := transport.Disconnect(context.Background()); err != nil {
			t.Fatal(err)
		}
		if transport.IsHandshakeComplete() || transport.RemoteStaticKey() != "" {
			t.Fatal("stale session after disconnect")
		}
	}
	if fmt.Sprint(fixture.responder.patterns) != "[XXpsk2 KKpsk0]" || fixture.responder.helloCount != 2 {
		t.Fatalf("incorrect reconnect exchange: %v", fixture.responder.patterns)
	}
}

func TestHTTPNoiseFailsClosed(t *testing.T) {
	for _, failure := range []string{"wrong-password", "unsupported-offer", "tamper", "plaintext", "json-connect-error", "json-send-error"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newHTTPNoiseFixture(t)
			transport := fixture.transport(t)
			switch failure {
			case "wrong-password":
				transport.Identity.Password = "wrong"
			case "json-connect-error":
				fixture.reject = "/connect"
			case "unsupported-offer":
				fixture.unsupported = true
			}
			err := transport.Connect(context.Background())
			if failure == "wrong-password" || failure == "unsupported-offer" || failure == "json-connect-error" {
				if err == nil || transport.IsHandshakeComplete() || transport.ConnectionInfo().Phase == ConnectionReady {
					t.Fatal("invalid exchange became ready")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer transport.Disconnect(context.Background())
			fixture.mu.Lock()
			fixture.tamper = failure == "tamper"
			fixture.plainBus = failure == "plaintext"
			if failure == "json-send-error" {
				fixture.reject = "/send_message"
			}
			fixture.mu.Unlock()
			err = transport.EmitBus(context.Background(), "echo", Data{}, Context{})
			if failure == "json-send-error" {
				if err == nil || transport.IsHandshakeComplete() {
					t.Fatal("failed send did not invalidate readiness")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := transport.PollOnce(context.Background()); err == nil {
				t.Fatal("invalid frame accepted")
			}
			if transport.IsHandshakeComplete() {
				t.Fatal("invalid frame left transport ready")
			}
			select {
			case <-transport.Events():
				t.Fatal("untrusted event dispatched")
			default:
			}
		})
	}
}

func TestNoiseOfferAloneIsNotReadyAndRejectsPlaintextBypass(t *testing.T) {
	for _, mqttTransport := range []bool{false, true} {
		t.Run(fmt.Sprint(mqttTransport), func(t *testing.T) {
			responder := newTransportResponder(t)
			responder.reset()
			var written [][]byte
			channel := &noiseChannel{identity: Identity{Password: "test-password"}, stateDir: t.TempDir(), write: func(_ context.Context, raw []byte, _ bool) error { written = append(written, raw); return nil }}
			for _, raw := range responder.plain {
				if _, err := channel.receive(context.Background(), []byte(raw), false); err != nil {
					t.Fatal(err)
				}
			}
			if channel.ready() || len(written) != 1 {
				t.Fatal("offer marked ready without Noise response")
			}
			if err := channel.send(context.Background(), HiveMessage{MsgType: "bus"}); err == nil {
				t.Fatal("plaintext bypass")
			}
		})
	}
}

func TestMQTTNoiseRawFramesAndReconnect(t *testing.T) {
	responder := newTransportResponder(t)
	identity := Identity{AccessKey: "test-access", Password: "test-password", MQTT: &MqttBrokerCredentials{Endpoint: "mqtts://example.invalid", TLS: true, TopicPrefix: "test"}}
	transport, err := NewMQTTTransport(identity)
	if err != nil {
		t.Fatal(err)
	}
	transport.NoiseStateDir = t.TempDir()
	for attempt := 0; attempt < 2; attempt++ {
		responder.reset()
		transport.beginConnection()
		transport.connected = true
		transport.noise = &noiseChannel{identity: identity, stateDir: transport.NoiseStateDir, write: func(_ context.Context, raw []byte, binary bool) error { return responder.receive(raw, binary) }}
		for len(responder.plain) > 0 {
			raw := responder.plain[0]
			responder.plain = responder.plain[1:]
			if err := transport.handleRawMessage(context.Background(), []byte(raw)); err != nil {
				t.Fatal(err)
			}
		}
		if !transport.IsHandshakeComplete() {
			t.Fatal("not ready after handshake")
		}
		// Even encrypt=false cannot bypass the negotiated Noise session.
		if err := transport.SendHiveMessage(context.Background(), HiveMessage{MsgType: "bus", Payload: map[string]any{"type": "echo", "data": map[string]any{}, "context": map[string]any{"request_id": "mqtt"}}}, false); err != nil {
			t.Fatal(err)
		}
		frame, _ := base64.StdEncoding.DecodeString(responder.binary[0])
		if json.Valid(frame) {
			t.Fatal("MQTT payload leaked JSON")
		}
		if err := transport.handleRawMessage(context.Background(), frame); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-transport.Events():
			if event.Context["request_id"] != "mqtt" {
				t.Fatal("lost correlation")
			}
		default:
			t.Fatal("missing encrypted event")
		}
		if err := transport.handleRawMessage(context.Background(), frame); err == nil {
			t.Fatal("replay accepted")
		}
		transport.closeClient()
	}
	if fmt.Sprint(responder.patterns) != "[XXpsk2 KKpsk0]" {
		t.Fatalf("bad patterns %v", responder.patterns)
	}
}

// Exercise Connect/Subscribe/Publish/Disconnect through Paho and an actual TLS
// socket. The small broker fixture only routes packets; it cannot decrypt or
// fabricate an application response without the independent hub responder.
func TestMQTTNoiseOverTLSBrokerReconnect(t *testing.T) {
	for _, wrongPassword := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongPassword), func(t *testing.T) {
			certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
			defer certificateServer.Close()
			listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certificateServer.TLS.Certificates, MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			roots := x509.NewCertPool()
			roots.AddCert(certificateServer.Certificate())
			identity := Identity{AccessKey: "test-access", Password: "test-password", MQTT: &MqttBrokerCredentials{Endpoint: "mqtts://" + listener.Addr().String(), Username: "broker-user", Password: "broker-password", TLS: true, QOS: 1, TopicPrefix: "test"}}
			if wrongPassword {
				identity.Password = "wrong"
			}
			transport, err := NewMQTTTransport(identity)
			if err != nil {
				t.Fatal(err)
			}
			transport.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			transport.NoiseStateDir = t.TempDir()
			responder := newTransportResponder(t)
			attempts := 2
			if wrongPassword {
				attempts = 1
			}
			completed := make(chan error, 1)
			go func() {
				for attempt := 0; attempt < attempts; attempt++ {
					conn, err := listener.Accept()
					if err != nil {
						completed <- err
						return
					}
					err = serveNoiseMQTT(conn, responder, transport.Topics)
					_ = conn.Close()
					if err != nil && !wrongPassword && !errors.Is(err, io.EOF) {
						completed <- err
						return
					}
				}
				completed <- nil
			}()
			for attempt := 0; attempt < attempts; attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := transport.Connect(ctx)
				cancel()
				if wrongPassword {
					if err == nil || transport.IsHandshakeComplete() {
						t.Fatal("wrong password became ready")
					}
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := transport.EmitBus(context.Background(), "echo", Data{"hello": "broker"}, Context{"request_id": "broker-echo"}); err != nil {
					t.Fatal(err)
				}
				select {
				case event := <-transport.Events():
					if event.Context["request_id"] != "broker-echo" {
						t.Fatal("lost MQTT response correlation")
					}
				case <-time.After(time.Second):
					t.Fatal("missing MQTT encrypted echo")
				}
				if err := transport.Disconnect(context.Background()); err != nil {
					t.Fatal(err)
				}
				if transport.RemoteStaticKey() != "" || transport.IsHandshakeComplete() {
					t.Fatal("stale MQTT session")
				}
			}
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("broker did not finish")
			}
			if !wrongPassword && fmt.Sprint(responder.patterns) != "[XXpsk2 KKpsk0]" {
				t.Fatalf("reconnect did not preserve key: %v", responder.patterns)
			}
		})
	}
}

func serveNoiseMQTT(conn net.Conn, responder *transportResponder, topics MqttTopicSet) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	started := false
	publish := func(raw []byte) error {
		packet := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
		packet.TopicName = topics.Outbound
		packet.Payload = raw
		return packet.Write(conn)
	}
	for {
		packet, err := packets.ReadPacket(conn)
		if err != nil {
			return err
		}
		switch p := packet.(type) {
		case *packets.ConnectPacket:
			if strings.Contains(p.ClientIdentifier, "test-access") {
				return fmt.Errorf("access key leaked in broker client ID")
			}
			if p.Username != "broker-user" || string(p.Password) != "broker-password" {
				return fmt.Errorf("wrong broker credentials")
			}
			if err := packets.NewControlPacket(packets.Connack).Write(conn); err != nil {
				return err
			}
		case *packets.SubscribePacket:
			if len(p.Topics) != 1 || p.Topics[0] != topics.Outbound {
				return fmt.Errorf("wrong subscription")
			}
			ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
			ack.MessageID = p.MessageID
			ack.ReturnCodes = []byte{1}
			if err := ack.Write(conn); err != nil {
				return err
			}
		case *packets.PublishPacket:
			if p.Qos > 0 {
				ack := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
				ack.MessageID = p.MessageID
				if err := ack.Write(conn); err != nil {
					return err
				}
			}
			if p.TopicName != topics.Inbound {
				continue
			}
			if !started {
				var hello HiveMessage
				if err := json.Unmarshal(p.Payload, &hello); err != nil || hello.MsgType != "hello" {
					return fmt.Errorf("missing MQTT initial HELLO")
				}
				responder.reset()
				started = true
			} else if err := responder.receive(p.Payload, responder.session != nil); err != nil {
				return err
			}
			for len(responder.plain) > 0 {
				raw := responder.plain[0]
				responder.plain = responder.plain[1:]
				if err := publish([]byte(raw)); err != nil {
					return err
				}
			}
			for len(responder.binary) > 0 {
				raw, _ := base64.StdEncoding.DecodeString(responder.binary[0])
				responder.binary = responder.binary[1:]
				if err := publish(raw); err != nil {
					return err
				}
			}
		case *packets.DisconnectPacket:
			return nil
		case *packets.PingreqPacket:
			if err := packets.NewControlPacket(packets.Pingresp).Write(conn); err != nil {
				return err
			}
		}
	}
}

func TestHTTPNoiseRejectsChangedHubKeyWithoutRepinning(t *testing.T) {
	fixture := newHTTPNoiseFixture(t)
	transport := fixture.transport(t)
	if err := transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	pin := transport.RemoteStaticKey()
	if err := transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	replacement, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fixture.responder.key = replacement
	fixture.responder.peer = nil
	fixture.mu.Unlock()
	if err := transport.Connect(context.Background()); err == nil || transport.IsHandshakeComplete() {
		t.Fatal("changed hub key was trusted")
	}
	stored, err := LoadNoisePin(transport.NoiseStateDir, "test-hub")
	if err != nil || stored != pin {
		t.Fatal("authentication failure changed the trusted pin")
	}
}

func TestWSSNoiseKKFailurePreservesHubPin(t *testing.T) {
	transport := NewWSSTransport(Identity{Password: "test-password"})
	transport.NoiseStateDir = t.TempDir()
	transport.nodeID = "test-hub"
	key, err := LoadOrCreateNoiseKey(transport.NoiseStateDir)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pin := hex.EncodeToString(peer.Public)
	if err := SaveNoisePin(transport.NoiseStateDir, transport.nodeID, pin); err != nil {
		t.Fatal(err)
	}
	transport.noiseHandshake, err = newNoiseHandshake(noisePatternKK, noiseSuites[0], derivePSK("test-password", transport.nodeID), nil, key, pin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.noiseHandshake.writeMessage(nil); err != nil {
		t.Fatal(err)
	}
	if err := transport.continueNoiseHandshake(context.Background(), map[string]any{"msg": "00"}); err == nil {
		t.Fatal("malformed authenticated response accepted")
	}
	stored, err := LoadNoisePin(transport.NoiseStateDir, transport.nodeID)
	if err != nil || stored != pin {
		t.Fatal("failed KK silently erased the hub pin")
	}
}

func TestWSSNoiseRejectsPreHandshakeBusAndDecodesAuthenticatedBinary(t *testing.T) {
	transport := NewWSSTransport(Identity{})
	if err := transport.handleRawMessage(context.Background(), []byte(marshalHive("bus", map[string]any{"type": "untrusted"}))); err == nil {
		t.Fatal("unauthenticated bus accepted")
	}
	client, server := newTestSessionPair(t)
	transport.session = client
	raw, err := EncodeHiveBinaryFrame(HiveMessage{MsgType: "bus", Payload: map[string]any{"type": "echo", "data": map[string]any{}, "context": map[string]any{"request_id": "binary"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.sendMessage(raw, false, func(frame []byte) error { return transport.handleRawMessage(context.Background(), frame) }); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-transport.Events():
		if event.Context["request_id"] != "binary" {
			t.Fatal("lost binary event correlation")
		}
	default:
		t.Fatal("authenticated binary event discarded")
	}
}

func TestHTTPNoiseRefusesAdmissionRedirects(t *testing.T) {
	for _, downgrade := range []bool{false, true} {
		t.Run(fmt.Sprint(downgrade), func(t *testing.T) {
			var contacted bool
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				contacted = true
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "Connected"})
			})
			var target *httptest.Server
			if downgrade {
				target = httptest.NewServer(handler)
			} else {
				target = httptest.NewTLSServer(handler)
			}
			defer target.Close()
			source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			}))
			defer source.Close()
			transport := NewHTTPTransport(Identity{AccessKey: "test-access", Password: "test-password", DefaultMaster: source.URL, DataPlaneEndpoints: HubDataPlaneEndpoints{HTTPS: source.URL}})
			transport.HTTPClient = source.Client()
			if err := transport.Connect(context.Background()); err == nil {
				t.Fatal("redirect accepted")
			}
			if contacted {
				t.Fatal("redirect destination received an SDK request")
			}
			if transport.Healthcheck().HandshakeComplete {
				t.Fatal("redirect marked ready")
			}
		})
	}
}

func TestHTTPNoiseReconnectResetsPreviouslyAdmittedSession(t *testing.T) {
	fixture := newHTTPNoiseFixture(t)
	transport := fixture.transport(t)
	if err := transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.tamper = true
	fixture.mu.Unlock()
	if err := transport.EmitBus(context.Background(), "echo", Data{}, Context{}); err != nil {
		t.Fatal(err)
	}
	if err := transport.PollOnce(context.Background()); err == nil {
		t.Fatal("tampered reply accepted")
	}
	fixture.mu.Lock()
	fixture.tamper = false
	fixture.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := transport.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if !transport.IsHandshakeComplete() {
		t.Fatal("session did not recover")
	}
	fixture.mu.Lock()
	patterns := fmt.Sprint(fixture.responder.patterns)
	fixture.mu.Unlock()
	if patterns != "[XXpsk2 KKpsk0]" {
		t.Fatalf("recovery lost key continuity: %s", patterns)
	}
	if err := transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
}
