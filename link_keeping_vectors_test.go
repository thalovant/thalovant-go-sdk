package thalovant

// Keeping a hub link up, against link-keeping-vectors.json from the Python
// SDK, vendored here unchanged under testdata/ and pinned by the parity
// contract. "close" cases hold the close classifier to the vectors;
// "handshake" cases run a real Noise handshake over WSS against a loopback hub
// that fails the way the case says and records the patterns it saw;
// "supervise" cases drive the LinkSupervisor HubSession's Run asks after every
// attempt.

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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/gorilla/websocket"
)

func TestLinkKeepingPolicyIsTheSDKs(t *testing.T) {
	policy := mapValue(loadHomeVectors(t, "link-keeping-vectors.json")["policy"])
	defaults := DefaultHubSessionPolicy()
	for name, have := range map[string]time.Duration{
		"retry_ms":            defaults.Retry,
		"retry_ceiling_ms":    defaults.RetryCeiling,
		"probe_ms":            defaults.Probe,
		"probe_down_ms":       defaults.ProbeDown,
		"refusal_grace_ms":    DefaultHubRefusalGrace,
		"settle_ms":           DefaultHubSettle,
		"close_code_grace_ms": closeCodeGrace,
	} {
		if want := milliseconds(policy[name]); have != want {
			t.Errorf("%s: the SDK has %s, the contract %s", name, have, want)
		}
	}
	var codes []int
	for code := range refusalCloseCodes {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	if fmt.Sprint(codes) != fmt.Sprint(anySlice(policy["refusal_close_codes"])) {
		t.Errorf("refusal close codes %v, the contract %v", codes, policy["refusal_close_codes"])
	}
}

func linkKeepingCases(t *testing.T, kind string) []map[string]any {
	t.Helper()
	var cases []map[string]any
	for _, raw := range anySlice(loadHomeVectors(t, "link-keeping-vectors.json")["cases"]) {
		if spec := mapValue(raw); spec["kind"] == kind {
			cases = append(cases, spec)
		}
	}
	if len(cases) == 0 {
		t.Fatalf("no %s cases", kind)
	}
	return cases
}

func TestLinkKeepingCloseVectors(t *testing.T) {
	for _, spec := range linkKeepingCases(t, "close") {
		name := fmt.Sprint(spec["name"])
		t.Run(name, func(t *testing.T) {
			code, _ := spec["code"].(float64) // null: no close frame, which is 0 here
			spoken, _ := spec["after_authenticated_frame"].(bool)
			refused := closeRefuses(int(code), spec["when"] == "handshake", milliseconds(spec["after_ms"]), milliseconds(spec["code_late_ms"]), spoken)
			produced := map[string]any{"outcome": "dropped"}
			if refused {
				produced["outcome"] = "refused"
			}
			recordConformance(t, "link-keeping-vectors.json", name, produced)
			assertProduced(t, produced, spec["expect"])
		})
	}
}

func TestLinkKeepingSuperviseVectors(t *testing.T) {
	policy := mapValue(loadHomeVectors(t, "link-keeping-vectors.json")["policy"])
	for _, spec := range linkKeepingCases(t, "supervise") {
		name := fmt.Sprint(spec["name"])
		t.Run(name, func(t *testing.T) {
			supervisor := NewLinkSupervisor(HubSessionPolicy{
				Retry:        milliseconds(policy["retry_ms"]),
				RetryCeiling: milliseconds(policy["retry_ceiling_ms"]),
				Probe:        milliseconds(policy["probe_ms"]),
				ProbeDown:    milliseconds(policy["probe_down_ms"]),
			}, milliseconds(policy["refusal_grace_ms"]))
			origin := time.Now()
			produced := []any{}
			for _, raw := range anySlice(spec["events"]) {
				event := mapValue(raw)
				decision := supervisor.After(LinkOutcome(fmt.Sprint(event["outcome"])), origin.Add(milliseconds(event["at_ms"])))
				switch decision.Action {
				case LinkRetry:
					produced = append(produced, map[string]any{"action": "retry", "wait_ms": decision.Wait.Milliseconds()})
				case LinkGiveUp:
					produced = append(produced, map[string]any{"action": "give_up", "reason": string(decision.Reason)})
				default:
					produced = append(produced, map[string]any{"action": string(decision.Action)})
				}
			}
			recordConformance(t, "link-keeping-vectors.json", name, produced)
			assertProduced(t, produced, spec["expect"])
		})
	}
}

// keepingHub is a loopback hub over WSS with real Noise, whose key, password
// and offer a case can change between connects, and which records the pattern
// each handshake chose. A KK first message it cannot read ends the socket with
// a close frame and no status, as hivemind-core does after a Noise abort; so
// does a completed handshake that shows another client key than the one it
// pinned on first contact.
//
// speakThenClose makes it send one encrypted frame as a handshake completes,
// and close with no status at once after it.
type keepingHub struct {
	server   *httptest.Server
	identity Identity

	mu             sync.Mutex
	key            noise.DHKey
	psk            []byte
	peer           []byte
	offerKK        bool
	speakThenClose bool
	upgradeStatus  int
	patterns       []string
	sockets        sync.WaitGroup
	// connections counts the upgrades it accepted, and bus is every bus
	// message a client sent over them, in order.
	connections int
	bus         []HiveMessage
}

func newKeepingHub(t *testing.T) *keepingHub {
	t.Helper()
	hub := &keepingHub{offerKK: true}
	hub.key = newHubKey(t)
	hub.psk = derivePSK("test-password", "test-hub")
	hub.server = httptest.NewServer(http.HandlerFunc(hub.serve))
	hub.identity = Identity{AccessKey: "test-access", Password: "test-password", SiteID: "test-site", DefaultMaster: "ws" + strings.TrimPrefix(hub.server.URL, "http")}
	hub.identity.DataPlaneEndpoints = HubDataPlaneEndpoints{WSS: hub.identity.DefaultMaster}
	t.Cleanup(func() {
		hub.server.CloseClientConnections()
		hub.server.Close()
		hub.sockets.Wait()
	})
	return hub
}

func newHubKey(t *testing.T) noise.DHKey {
	t.Helper()
	key, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func (h *keepingHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	status := h.upgradeStatus
	responder := &transportResponder{key: h.key, psk: h.psk, pinnedClient: append([]byte(nil), h.peer...)}
	if h.offerKK {
		responder.peer = append([]byte(nil), h.peer...)
	}
	speak := h.speakThenClose
	h.mu.Unlock()
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.sockets.Add(1)
	defer h.sockets.Done()
	defer conn.Close()
	h.mu.Lock()
	h.connections++
	h.mu.Unlock()
	responder.reset()
	flush := func() error {
		for _, raw := range responder.plain {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
				return err
			}
		}
		responder.plain = nil
		for _, raw := range responder.binary {
			payload, _ := base64.StdEncoding.DecodeString(raw)
			if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
				return err
			}
		}
		responder.binary = nil
		return nil
	}
	if flush() != nil {
		return
	}
	seen := 0
	for {
		kind, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		wasUp := responder.session != nil
		failure := responder.receive(raw, kind == websocket.BinaryMessage)
		if len(responder.received) > 0 {
			h.mu.Lock()
			h.bus = append(h.bus, responder.received...)
			h.mu.Unlock()
			responder.received = nil
		}
		if len(responder.patterns) > seen {
			h.mu.Lock()
			h.patterns = append(h.patterns, responder.patterns[seen:]...)
			h.mu.Unlock()
			seen = len(responder.patterns)
		}
		if failure != nil {
			// A handshake message it cannot read: a close frame with no status.
			closeLikeAHub(conn)
			return
		}
		if responder.session != nil && len(responder.peer) > 0 {
			h.mu.Lock()
			h.peer = append([]byte(nil), responder.peer...)
			h.mu.Unlock()
		}
		if speak && !wasUp && responder.session != nil {
			// The hub speaks first -- one frame under the new keys -- then
			// closes with no status inside the settle window.
			raw, _ := json.Marshal(HiveMessage{MsgType: "bus", Payload: map[string]any{"type": "hub.ready", "data": map[string]any{}, "context": map[string]any{}}, Metadata: map[string]any{}, Route: []any{}})
			_ = responder.session.sendMessage(raw, true, func(frame []byte) error {
				responder.binary = append(responder.binary, base64.StdEncoding.EncodeToString(frame))
				return nil
			})
			if flush() != nil {
				return
			}
			closeLikeAHub(conn)
			return
		}
		if flush() != nil {
			return
		}
	}
}

// closeLikeAHub sends a close frame with no status and finishes the closing
// handshake before the socket goes, as a hub's WebSocket server does (RFC
// 6455 section 7.1.1): it reads, dropping whatever the client already had in
// flight, until the client's own close answers or a grace period passes.
//
// Dropping the socket straight after the close frame is not a hub a client
// can meet, and on Windows it loses the close. The client sends its encrypted
// HELLO right behind the last XX message, so that frame is still unread when
// the hub fails the handshake; closing a socket with unread data sends a TCP
// reset instead of a FIN, and Windows discards everything the client had
// received but not yet read when a reset arrives -- the close frame included.
// The client then sees a socket that ended with no close frame (1006), which
// is a drop by the vectors, not a refusal.
func closeLikeAHub(conn *websocket.Conn) {
	if conn.WriteMessage(websocket.CloseMessage, []byte{}) != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// pinnedBack waits until the hub has recorded the client's key from a
// completed handshake: the client is up once it has sent its last message,
// which the hub may not have read yet.
func (h *keepingHub) pinnedBack(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.peer) > 0
	})
}

func (h *keepingHub) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.patterns...)
}

// connectOnce is one connect as a kept link makes it -- the handshake, then
// the settle window -- with a fresh transport over the case's key store,
// closed afterwards. A close that lands just after the handshake would
// otherwise race the connect returning.
func (h *keepingHub) connectOnce(identity Identity, stateDir string) error {
	session, err := NewHubSession(func(ctx context.Context) (HubSessionClient, error) {
		transport := NewWSSTransport(identity)
		transport.NoiseStateDir = stateDir
		client := &Client{Identity: identity, Transport: transport, ConnectTimeout: 20 * time.Second}
		if err := client.Connect(ctx); err != nil {
			_ = client.Close(context.Background())
			return nil, err
		}
		return client, nil
	}, DefaultHubSessionPolicy())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = session.Connect(ctx)
	_ = session.Close(context.Background())
	return err
}

func handshakeOutcome(t *testing.T, err error) string {
	t.Helper()
	switch {
	case err == nil:
		return "connected"
	case errors.Is(err, ErrClientKeyRejected):
		var rejected *ClientKeyRejectedError
		if !errors.As(err, &rejected) || rejected.KeyFolder == "" || !errors.Is(err, ErrHubRefused) || !errors.Is(err, ErrConnection) {
			t.Errorf("a rejected client key must be a refusal naming its folder: %#v", err)
		}
		return "client_key_rejected"
	case errors.Is(err, ErrHubKeyChanged):
		if errors.Is(err, ErrHubRefused) {
			t.Errorf("a changed key must not read as a refusal: %v", err)
		}
		return "key_changed"
	case errors.Is(err, ErrHubRefused):
		return "refused"
	case errors.Is(err, ErrConnection):
		return "failed"
	}
	t.Fatalf("a connect failed outside ErrConnection: %T %v", err, err)
	return ""
}

func TestLinkKeepingHandshakeVectors(t *testing.T) {
	for _, spec := range linkKeepingCases(t, "handshake") {
		name := fmt.Sprint(spec["name"])
		t.Run(name, func(t *testing.T) {
			hub := newKeepingHub(t)
			stateDir := t.TempDir()
			identity := hub.identity
			situation := spec["situation"]
			switch situation {
			case "pinned", "password_changed_since_pinning", "hub_key_changed", "client_key_changed", "client_key_changed_pinned_here":
				// First contact pins both ways.
				if err := hub.connectOnce(identity, stateDir); err != nil {
					t.Fatalf("first contact: %v", err)
				}
				hub.pinnedBack(t)
			}
			hub.mu.Lock()
			switch situation {
			case "wrong_password":
				identity.Password = "a-wrong-password"
			case "password_changed_since_pinning":
				hub.psk = derivePSK("the-password-now", "test-hub") // the hub's side changed
			case "hub_key_changed":
				hub.key = newHubKey(t) // the hub was replaced
				hub.offerKK, _ = spec["hub_offers_kk"].(bool)
			case "upgrade_status":
				status, _ := spec["status"].(float64)
				hub.upgradeStatus = int(status)
			case "client_key_changed":
				stateDir = t.TempDir() // another program: its own folder, its own key
			case "client_key_changed_pinned_here":
				replaceClientKey(t, stateDir) // a new key, the same hub pins
			case "closed_after_first_frame":
				hub.speakThenClose = true
			}
			before := len(hub.patterns)
			hub.mu.Unlock()

			outcome := handshakeOutcome(t, hub.connectOnce(identity, stateDir))
			patterns := []any{}
			for _, pattern := range hub.seen()[before:] {
				patterns = append(patterns, pattern[:2])
			}
			produced := map[string]any{"outcome": outcome, "patterns": patterns}
			recordConformance(t, "link-keeping-vectors.json", name, produced)
			assertProduced(t, produced, spec["expect"])

			if situation == "hub_key_changed" {
				pinned, err := LoadNoisePin(stateDir, "test-hub")
				if err != nil || pinned == "" {
					t.Fatalf("the pin is gone after a changed key: %q %v", pinned, err)
				}
			}
		})
	}
}

// replaceClientKey gives the client in stateDir a new static key, keeping
// the hub pins it has.
func replaceClientKey(t *testing.T, stateDir string) {
	t.Helper()
	key := newHubKey(t)
	if err := os.WriteFile(filepath.Join(stateDir, NoiseKeyFilename), []byte(hex.EncodeToString(key.Private)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunStopsAtOnceWhenTheHubKeyChanged(t *testing.T) {
	hub := newKeepingHub(t)
	stateDir := t.TempDir()
	if err := hub.connectOnce(hub.identity, stateDir); err != nil {
		t.Fatal(err)
	}
	hub.pinnedBack(t)
	hub.mu.Lock()
	hub.key = newHubKey(t)
	hub.mu.Unlock()
	attempts := 0
	session, err := NewHubSession(func(ctx context.Context) (HubSessionClient, error) {
		attempts++
		transport := NewWSSTransport(hub.identity)
		transport.NoiseStateDir = stateDir
		client := &Client{Identity: hub.identity, Transport: transport, ConnectTimeout: 20 * time.Second}
		if err := client.Connect(ctx); err != nil {
			_ = client.Close(context.Background())
			return nil, err
		}
		return client, nil
	}, HubSessionPolicy{Retry: 10 * time.Millisecond, RetryCeiling: 40 * time.Millisecond, Probe: time.Second, ProbeDown: 10 * time.Millisecond}, WithSettle(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// KK against the old key fails, XX follows at once and meets the pin: Run
	// ends there rather than retrying for ever.
	if err := session.Run(ctx); !errors.Is(err, ErrHubKeyChanged) {
		t.Fatalf("Run = %v, want the changed key", err)
	}
	if seen := hub.seen(); attempts != 1 || fmt.Sprint(seen[len(seen)-2:]) != "[KKpsk0 XXpsk2]" {
		t.Fatalf("%d attempts, patterns %v", attempts, seen)
	}
}

// A KK answer that does not authenticate is followed at once by one XX
// attempt on every transport, not just WSS. Over HTTP polling:
func TestHTTPRetriesAFailedKKWithXXOnce(t *testing.T) {
	fixture := newHTTPNoiseFixture(t)
	transport := fixture.transport(t)
	if err := transport.Connect(context.Background()); err != nil { // first contact pins over XX
		t.Fatal(err)
	}
	if err := transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.responder.corrupt = map[string]bool{noisePatternKK: true}
	fixture.mu.Unlock()
	if err := transport.Connect(context.Background()); err != nil {
		t.Fatalf("a spoiled KK answer was not followed by XX: %v", err)
	}
	if err := transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.responder.corrupt = map[string]bool{noisePatternKK: true, noisePatternXX: true}
	fixture.mu.Unlock()
	if err := transport.Connect(context.Background()); !errors.Is(err, ErrHubRefused) {
		t.Fatalf("an XX answer that does not authenticate = %v, want the refusal", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if got := fmt.Sprint(fixture.responder.patterns); got != "[XXpsk2 KKpsk0 XXpsk2 KKpsk0 XXpsk2]" {
		t.Fatalf("patterns %s: want XX once after each failed KK, and never twice", got)
	}
}

// Over MQTT, against a loopback TLS broker:
func TestMQTTRetriesAFailedKKWithXXOnce(t *testing.T) {
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
	transport, err := NewMQTTTransport(identity)
	if err != nil {
		t.Fatal(err)
	}
	transport.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	transport.NoiseStateDir = t.TempDir()
	responder := newTransportResponder(t)
	var brokerMu sync.Mutex
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			brokerMu.Lock()
			_ = serveNoiseMQTT(conn, responder, transport.Topics)
			brokerMu.Unlock()
			_ = conn.Close()
		}
	}()
	connect := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return transport.Connect(ctx)
	}
	if err := connect(); err != nil { // first contact pins over XX
		t.Fatal(err)
	}
	if err := transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	brokerMu.Lock()
	responder.corrupt = map[string]bool{noisePatternKK: true}
	brokerMu.Unlock()
	if err := connect(); err != nil {
		t.Fatalf("a spoiled KK answer was not followed by XX: %v", err)
	}
	if err := transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		brokerMu.Lock()
		defer brokerMu.Unlock()
		return len(responder.patterns) == 3
	})
	brokerMu.Lock()
	defer brokerMu.Unlock()
	if got := fmt.Sprint(responder.patterns); got != "[XXpsk2 KKpsk0 XXpsk2]" {
		t.Fatalf("patterns %s", got)
	}
}

// Over HTTP polling, a request answered 401 or 403 is a refusal, as an
// upgrade answered so is over WSS; during a KK exchange it is followed by XX.
func TestHTTPAKKAnsweredUnauthorizedIsFollowedByXX(t *testing.T) {
	fixture := newHTTPNoiseFixture(t)
	transport := fixture.transport(t)
	if err := transport.Connect(context.Background()); err != nil { // first contact pins over XX
		t.Fatal(err)
	}
	if err := transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.refuseKK = true
	fixture.mu.Unlock()
	if err := transport.Connect(context.Background()); err != nil {
		t.Fatalf("a KK answered 401 was not followed by XX: %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.refusedKK != 1 || fmt.Sprint(fixture.responder.patterns) != "[XXpsk2 XXpsk2]" {
		t.Fatalf("%d KK refused, patterns %v", fixture.refusedKK, fixture.responder.patterns)
	}
}

func TestADeviceTokenAnswerThatIsNoObjectCarriesNoStatus(t *testing.T) {
	for _, body := range []string{`[1, 2]`, `"token"`, `not json`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		plane := NewControlPlane(server.URL, "")
		_, err := plane.PollDeviceLogin(context.Background(), &DeviceAuthorization{DeviceCode: "dc", Interval: time.Second})
		server.Close()
		var apiErr *APIError
		if !errors.Is(err, ErrAPI) || errors.As(err, &apiErr) {
			t.Errorf("a 2xx answering %s = %v, want an error with no status", body, err)
		}
	}
}
