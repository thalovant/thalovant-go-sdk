package thalovant

// link_carrier_vectors_test.go runs link-carrier-vectors.json: the handshake
// rules of link-keeping-vectors.json on the two carriers that are not a
// WebSocket. Each case is one connect as a kept link makes it -- the
// handshake, then the settle window -- through the real HTTPS polling and
// MQTT transports, against a hub that answers the way hivemind-core does:
// HELLO and the offer, the Noise responder (KK with the pinned client key, XX
// otherwise), the pin of the client's key on first contact, and an abort --
// nothing sent, the session dropped -- on a first message it cannot read, a
// final message that does not authenticate, or a client key that contradicts
// the pin. Over HTTPS every request of an aborted session is answered 401, as
// the hub's listener refuses a session it no longer holds; over MQTT an
// aborted session just stops answering.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
)

// carrierPeer is the hub's side of one client's negotiations, whatever
// carries them: its key and password, whether it offers KK, the client key it
// pinned, and every pattern a handshake chose.
type carrierPeer struct {
	mu       sync.Mutex
	key      noise.DHKey
	psk      []byte
	offerKK  bool
	tamperKK bool
	pinned   []byte
	patterns []string
	// session is the current session's responder; aborted says the hub dropped it.
	session *transportResponder
	aborted bool
	seen    int
	// abortAfter drops a session that long after the client's encrypted
	// HELLO arrived: the refusal then reaches the client on a poll right
	// after its connect returned, inside the settle window.
	abortAfter time.Duration
	helloAt    time.Time
}

func newCarrierPeer(t *testing.T) *carrierPeer {
	t.Helper()
	return &carrierPeer{key: newHubKey(t), psk: derivePSK("test-password", "test-hub"), offerKK: true}
}

// begin opens a session: a responder that offers KK to a client whose key it
// pinned, and HELLO and the offer queued. The caller holds mu.
func (p *carrierPeer) begin() *transportResponder {
	responder := &transportResponder{key: p.key, psk: p.psk, pinnedClient: append([]byte(nil), p.pinned...)}
	if p.offerKK {
		responder.peer = append([]byte(nil), p.pinned...)
	}
	if p.tamperKK {
		responder.corrupt = map[string]bool{noisePatternKK: true}
	}
	responder.reset()
	p.session, p.aborted, p.seen, p.helloAt = responder, false, 0, time.Time{}
	return responder
}

// receive hands one client frame to the session. A frame the responder cannot
// read aborts it; a handshake that completes pins the client's key. The
// caller holds mu.
func (p *carrierPeer) receive(raw []byte, binary bool) {
	responder := p.session
	if responder == nil || p.aborted {
		return
	}
	err := responder.receive(raw, binary)
	p.patterns = append(p.patterns, responder.patterns[p.seen:]...)
	p.seen = len(responder.patterns)
	if err != nil {
		p.aborted = true
		return
	}
	if responder.session != nil && len(responder.peer) > 0 {
		p.pinned = append([]byte(nil), responder.peer...)
	}
	if responder.helloCount > 0 && p.helloAt.IsZero() {
		p.helloAt = time.Now()
	}
}

// carrierHTTPSHub is hivemind-http-protocol's endpoints over TLS in front of
// one carrierPeer.
type carrierHTTPSHub struct {
	peer   *carrierPeer
	server *httptest.Server
}

func newCarrierHTTPSHub(t *testing.T) *carrierHTTPSHub {
	t.Helper()
	hub := &carrierHTTPSHub{peer: newCarrierPeer(t)}
	hub.server = httptest.NewTLSServer(http.HandlerFunc(hub.serve))
	t.Cleanup(hub.server.Close)
	return hub
}

func (h *carrierHTTPSHub) serve(w http.ResponseWriter, r *http.Request) {
	p := h.peer
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	reply := func(value any) { _, _ = fmt.Fprintf(w, "%s", mustJSON(value)) }
	switch {
	case r.URL.Path == "/connect":
		p.begin()
		http.SetCookie(w, &http.Cookie{Name: "hivemind_http_replica", Value: "one", Path: "/", Secure: true, HttpOnly: true})
		reply(map[string]any{"status": "Connected"})
		return
	case r.URL.Path == "/disconnect":
		reply(map[string]any{"status": "Disconnected"})
		return
	case p.abortAfter > 0 && !p.helloAt.IsZero() && time.Since(p.helloAt) >= p.abortAfter:
		p.aborted = true
		fallthrough
	case p.aborted || p.session == nil:
		w.WriteHeader(http.StatusUnauthorized)
		reply(map[string]any{"error": "Unauthorized"})
		return
	}
	responder := p.session
	switch r.URL.Path {
	case "/get_messages":
		messages := responder.plain
		responder.plain = []string{}
		reply(map[string]any{"messages": messages})
	case "/get_binary_messages":
		frames := responder.binary
		responder.binary = []string{}
		reply(map[string]any{"b64_messages": frames})
	case "/send_message":
		_ = r.ParseForm()
		raw := []byte(r.Form.Get("message"))
		binary := r.Form.Get("binary") == "1"
		if binary {
			decoded, err := base64.StdEncoding.DecodeString(string(raw))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			raw = decoded
		}
		p.receive(raw, binary)
		reply(map[string]any{"status": "message sent"})
	default:
		http.NotFound(w, r)
	}
}

func mustJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// carrierMQTTBroker is a TLS MQTT broker in front of one carrierPeer, one
// client connection at a time.
type carrierMQTTBroker struct {
	peer     *carrierPeer
	listener net.Listener
	roots    *x509.CertPool
}

func newCarrierMQTTBroker(t *testing.T) *carrierMQTTBroker {
	t.Helper()
	certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(certificateServer.Close)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certificateServer.TLS.Certificates, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(certificateServer.Certificate())
	broker := &carrierMQTTBroker{peer: newCarrierPeer(t), listener: listener, roots: roots}
	go broker.serve()
	return broker
}

func (b *carrierMQTTBroker) serve() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		p := b.peer
		p.mu.Lock()
		responder := p.begin()
		p.mu.Unlock()
		// serveNoiseMQTT drives the responder itself; an abort (a frame it
		// cannot read) ends the connection, and the hub answers nothing more.
		_ = serveNoiseMQTT(conn, responder, b.topics())
		_ = conn.Close()
		p.mu.Lock()
		p.patterns = append(p.patterns, responder.patterns[p.seen:]...)
		if responder.session != nil && len(responder.peer) > 0 {
			p.pinned = append([]byte(nil), responder.peer...)
		}
		p.session = nil
		p.mu.Unlock()
	}
}

func (b *carrierMQTTBroker) topics() MqttTopicSet {
	transport, err := NewMQTTTransport(b.identity())
	if err != nil {
		panic(err)
	}
	return transport.Topics
}

func (b *carrierMQTTBroker) identity() Identity {
	return Identity{AccessKey: "test-access", Password: "test-password", SiteID: "carrier", MQTT: &MqttBrokerCredentials{
		Endpoint: "mqtts://" + b.listener.Addr().String(), Username: "broker-user", Password: "broker-password",
		TLS: true, QOS: 1, TopicPrefix: "test",
	}}
}

// carrierConnect is one connect as a kept link makes it, over a transport
// made fresh for the case's key folder.
func carrierConnect(t *testing.T, build func(stateDir string) RuntimeTransport, identity Identity, stateDir string) error {
	t.Helper()
	session, err := NewHubSession(func(ctx context.Context) (HubSessionClient, error) {
		client := &Client{Identity: identity, Transport: build(stateDir), ConnectTimeout: 20 * time.Second}
		if err := client.Connect(ctx); err != nil {
			_ = client.Close(context.Background())
			return nil, err
		}
		return client, nil
	}, DefaultHubSessionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = session.Connect(ctx)
	_ = session.Close(context.Background())
	return err
}

func carrierOutcome(t *testing.T, err error) string {
	t.Helper()
	switch {
	case err == nil:
		return "connected"
	case errors.Is(err, ErrClientKeyRejected):
		var rejected *ClientKeyRejectedError
		if !errors.As(err, &rejected) || rejected.KeyFolder == "" || !errors.Is(err, ErrHubRefused) {
			t.Errorf("a rejected client key must be a refusal naming its folder: %v", err)
		}
		return "client_key_rejected"
	case errors.Is(err, ErrHubKeyChanged):
		if errors.Is(err, ErrHubRefused) {
			t.Errorf("a changed key must not read as a refusal: %v", err)
		}
		return "key_changed"
	case errors.Is(err, ErrHubRefused):
		return "refused"
	case errors.Is(err, ErrConnection), errors.Is(err, ErrTimeout):
		return "failed"
	}
	t.Fatalf("a connect failed outside ErrConnection: %T %v", err, err)
	return ""
}

func TestLinkCarrierVectors(t *testing.T) {
	for _, raw := range anySlice(loadHomeVectors(t, "link-carrier-vectors.json")["cases"]) {
		spec := mapValue(raw)
		name := fmt.Sprint(spec["name"])
		t.Run(name, func(t *testing.T) {
			var (
				peer     *carrierPeer
				identity Identity
				build    func(stateDir string) RuntimeTransport
			)
			switch spec["carrier"] {
			case "https":
				hub := newCarrierHTTPSHub(t)
				peer = hub.peer
				identity = Identity{AccessKey: "test-access", Password: "test-password", SiteID: "carrier", DefaultMaster: hub.server.URL, DataPlaneEndpoints: HubDataPlaneEndpoints{HTTPS: hub.server.URL}}
				build = func(stateDir string) RuntimeTransport {
					transport := NewHTTPTransport(identity)
					transport.HTTPClient = hub.server.Client()
					transport.NoiseStateDir = stateDir
					transport.PollInterval = 50 * time.Millisecond
					return transport
				}
			case "mqtt":
				broker := newCarrierMQTTBroker(t)
				peer = broker.peer
				identity = broker.identity()
				build = func(stateDir string) RuntimeTransport {
					transport, err := NewMQTTTransport(identity)
					if err != nil {
						t.Fatal(err)
					}
					transport.TLSConfig = &tls.Config{RootCAs: broker.roots, MinVersion: tls.VersionTLS12}
					transport.NoiseStateDir = stateDir
					return transport
				}
			default:
				t.Fatalf("unknown carrier %v", spec["carrier"])
			}
			stateDir := t.TempDir()
			situation := spec["situation"]
			switch situation {
			case "pinned", "password_changed_since_pinning", "hub_key_changed", "client_key_changed", "kk_answer_unauthenticated":
				// First contact pins both ways.
				if err := carrierConnect(t, build, identity, stateDir); err != nil {
					t.Fatalf("first contact: %v", err)
				}
			}
			settled := func() bool {
				peer.mu.Lock()
				defer peer.mu.Unlock()
				// The MQTT broker files a connection's patterns when it ends.
				return peer.session == nil || spec["carrier"] == "https"
			}
			waitFor(t, settled)
			peer.mu.Lock()
			switch situation {
			case "wrong_password":
				peer.psk = derivePSK("the-password-the-hub-holds", "test-hub")
			case "password_changed_since_pinning":
				peer.psk = derivePSK("the-password-now", "test-hub")
			case "hub_key_changed":
				peer.key = newHubKey(t)
				peer.offerKK, _ = spec["hub_offers_kk"].(bool)
			case "client_key_changed":
				stateDir = t.TempDir() // another program: its own folder, its own key
			case "kk_answer_unauthenticated":
				peer.tamperKK = true
			}
			before := len(peer.patterns)
			peer.mu.Unlock()

			outcome := carrierOutcome(t, carrierConnect(t, build, identity, stateDir))
			var patterns []any
			waitFor(t, settled)
			peer.mu.Lock()
			for _, pattern := range peer.patterns[before:] {
				patterns = append(patterns, pattern[:2])
			}
			peer.mu.Unlock()
			if patterns == nil {
				patterns = []any{}
			}
			produced := map[string]any{"outcome": outcome, "patterns": patterns}
			recordConformance(t, "link-carrier-vectors.json", name, produced)
			assertProduced(t, produced, spec["expect"])
		})
	}
}

// Over HTTPS, a 401 on a poll right after an XX connect returned -- before
// the hub sent anything -- is the same verdict as one during it: the settle
// window reads it as the hub refusing this client's own key. After KK it is a
// plain refusal.
func TestHTTPSARefusalRightAfterTheHandshakeIsReadByTheSettleWindow(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		hub := newCarrierHTTPSHub(t)
		identity := Identity{AccessKey: "test-access", Password: "test-password", SiteID: "carrier", DefaultMaster: hub.server.URL, DataPlaneEndpoints: HubDataPlaneEndpoints{HTTPS: hub.server.URL}}
		build := func(stateDir string) RuntimeTransport {
			transport := NewHTTPTransport(identity)
			transport.HTTPClient = hub.server.Client()
			transport.NoiseStateDir = stateDir
			transport.PollInterval = 50 * time.Millisecond
			return transport
		}
		stateDir := t.TempDir()
		if pinned {
			if err := carrierConnect(t, build, identity, stateDir); err != nil {
				t.Fatalf("first contact: %v", err)
			}
		}
		hub.peer.mu.Lock()
		hub.peer.abortAfter = 200 * time.Millisecond
		hub.peer.mu.Unlock()
		err := carrierConnect(t, build, identity, stateDir)
		want := "client_key_rejected"
		if pinned {
			want = "refused" // KK: the hub could only complete it with the key it pinned
		}
		if got := carrierOutcome(t, err); got != want {
			t.Fatalf("pinned=%v: %s (%v), want %s", pinned, got, err, want)
		}
	}
}
