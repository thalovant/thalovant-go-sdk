package thalovant

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type wssLifecycleFixture struct {
	transport   *WSSTransport
	opened      chan int
	ready       chan struct{}
	resume      chan struct{}
	releaseOnce sync.Once
	connections atomic.Int32
}

func newWSSLifecycleFixture(t *testing.T, pauseFirst, stallFirst bool) *wssLifecycleFixture {
	t.Helper()
	fixture := &wssLifecycleFixture{opened: make(chan int, 8), ready: make(chan struct{}, 1), resume: make(chan struct{})}
	shared := newTransportResponder(t)
	var peers sync.Mutex
	var socketsMu sync.Mutex
	var sockets []*websocket.Conn
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		socketsMu.Lock()
		sockets = append(sockets, conn)
		socketsMu.Unlock()
		count := int(fixture.connections.Add(1))
		fixture.opened <- count
		if pauseFirst && count == 1 {
			<-fixture.resume
		}
		peers.Lock()
		responder := &transportResponder{key: shared.key, psk: shared.psk, peer: append([]byte(nil), shared.peer...)}
		peers.Unlock()
		responder.reset()
		flush := func() error {
			for _, raw := range responder.plain {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
					return err
				}
			}
			responder.plain = nil
			for _, raw := range responder.binary {
				payload, err := base64.StdEncoding.DecodeString(raw)
				if err != nil {
					return err
				}
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
		for {
			kind, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := responder.receive(raw, kind == websocket.BinaryMessage); err != nil {
				return
			}
			peers.Lock()
			shared.peer = append([]byte(nil), responder.peer...)
			peers.Unlock()
			if flush() != nil {
				return
			}
			if stallFirst && count == 1 && responder.helloCount == 1 {
				fixture.ready <- struct{}{}
				<-fixture.resume
				return
			}
		}
	}))
	identity := Identity{AccessKey: "test-access", Password: "test-password", SiteID: "test-site", DefaultMaster: "ws" + strings.TrimPrefix(server.URL, "http")}
	identity.DataPlaneEndpoints = HubDataPlaneEndpoints{WSS: identity.DefaultMaster}
	fixture.transport = NewWSSTransport(identity)
	fixture.transport.NoiseStateDir = t.TempDir()
	t.Cleanup(func() {
		fixture.release()
		_ = fixture.transport.Disconnect(context.Background())
		socketsMu.Lock()
		for _, conn := range sockets {
			_ = conn.Close()
		}
		socketsMu.Unlock()
		server.Close()
	})
	return fixture
}

func (f *wssLifecycleFixture) release() { f.releaseOnce.Do(func() { close(f.resume) }) }

func waitWSSResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("WSS operation did not settle")
		return nil
	}
}

func wssConnectResult(transport *WSSTransport, ctx context.Context) <-chan error {
	result := make(chan error, 1)
	go func() { result <- transport.Connect(ctx) }()
	return result
}

func waitWSSOpen(t *testing.T, f *wssLifecycleFixture) {
	t.Helper()
	select {
	case <-f.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("socket did not open")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !f.transport.Healthcheck().Connected {
		if time.Now().After(deadline) {
			t.Fatal("transport did not observe socket open")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWSSConcurrentConnectRequiresAuthenticationAndJoinCancellationIsLocal(t *testing.T) {
	f := newWSSLifecycleFixture(t, true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	initiating := wssConnectResult(f.transport, ctx)
	waitWSSOpen(t, f)
	if f.transport.Healthcheck().HandshakeComplete {
		t.Fatal("socket open was reported as authenticated")
	}
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := f.transport.Connect(short); !errors.Is(err, ErrTimeout) {
		t.Fatalf("join timeout: %v", err)
	}
	stop()
	select {
	case err := <-initiating:
		t.Fatalf("joiner ended initiator: %v", err)
	default:
	}
	joining := wssConnectResult(f.transport, ctx)
	f.release()
	if err := waitWSSResult(t, initiating); err != nil {
		t.Fatal(err)
	}
	if err := waitWSSResult(t, joining); err != nil {
		t.Fatal(err)
	}
	if err := f.transport.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if f.connections.Load() != 1 || !f.transport.Healthcheck().HandshakeComplete {
		t.Fatal("concurrent Connect did not coalesce")
	}
}

func TestWSSInitiatingCancellationAndExplicitCloseRetireOnlyTheirGeneration(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			f := newWSSLifecycleFixture(t, true, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pending := wssConnectResult(f.transport, ctx)
			waitWSSOpen(t, f)
			if explicit {
				_ = f.transport.Disconnect(context.Background())
			} else {
				cancel()
			}
			if err := waitWSSResult(t, pending); err == nil {
				t.Fatal("retired connection became ready")
			}
			if f.transport.Healthcheck().Connected {
				t.Fatal("cancelled connection remained usable")
			}
			f.release()
			fresh, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := f.transport.Connect(fresh); err != nil {
				t.Fatal(err)
			}
			if !f.transport.Healthcheck().HandshakeComplete {
				t.Fatal("new generation did not authenticate")
			}
		})
	}
}

func TestWSSQueuedSendCancellationDoesNotAdvanceCipher(t *testing.T) {
	f := newWSSLifecycleFixture(t, false, false)
	if err := f.transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.transport.writeMu.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := f.transport.EmitBus(ctx, "cancelled", Data{}, Context{})
	cancel()
	f.transport.writeMu.Unlock()
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("queued cancellation: %v", err)
	}
	if !f.transport.Healthcheck().HandshakeComplete {
		t.Fatal("a queued cancellation poisoned the session")
	}
	assertWSSEcho(t, f.transport)
}

func assertWSSEcho(t *testing.T, transport *WSSTransport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := transport.EmitBus(ctx, "echo", Data{}, Context{"request_id": "current"}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-transport.Events():
		if event.Context["request_id"] != "current" {
			t.Fatal("unexpected event correlation")
		}
	case <-ctx.Done():
		t.Fatal("current session did not decrypt the echo")
	}
}

func TestWSSBackpressuredSendCancellationPoisonsCapturedSessionAndReconnects(t *testing.T) {
	f := newWSSLifecycleFixture(t, false, true)
	if err := f.transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not stop reading")
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- f.transport.EmitBus(ctx, "large", Data{"payload": strings.Repeat("x", 16<<20)}, Context{})
	}()
	admitted := time.Now().Add(3 * time.Second)
	for len(f.transport.writeMu.token) == 0 {
		if time.Now().After(admitted) {
			cancel()
			t.Fatal("send never reached the socket writer")
		}
		time.Sleep(time.Millisecond)
	}
	// The peer is not reading; allow chunks to fill the socket before cancelling.
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := waitWSSResult(t, result); err == nil {
		t.Fatal("backpressured send completed after cancellation")
	}
	if f.transport.Healthcheck().HandshakeComplete {
		t.Fatal("uncertain delivery did not poison Noise")
	}
	f.release()
	if err := f.transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWSSEcho(t, f.transport)
}

func TestWSSStaleReaderAndSendFailureCannotMutateReconnectedSession(t *testing.T) {
	f := newWSSLifecycleFixture(t, false, false)
	if err := f.transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.transport.mu.RLock()
	oldGeneration, oldConn := f.transport.generation, f.transport.conn
	f.transport.mu.RUnlock()
	if err := f.transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	stale := context.WithValue(context.Background(), wssGenerationKey{}, oldGeneration)
	raw := []byte(marshalHive("hello", map[string]any{"node_id": "stale"}))
	if err := f.transport.handleRawMessage(stale, raw); err == nil {
		t.Fatal("stale input reached the current Noise state")
	}
	f.transport.recordReadFailure(oldGeneration, errors.New("stale EOF"))
	f.transport.signalReadDone(oldGeneration)
	f.transport.poisonGeneration(oldGeneration, oldConn, errors.New("stale failed write"))
	if !f.transport.Healthcheck().HandshakeComplete {
		t.Fatal("stale callbacks poisoned the new session")
	}
	assertWSSEcho(t, f.transport)
}

func TestWSSApplicationSendWaitsUntilEncryptedHelloCompletes(t *testing.T) {
	f := newWSSLifecycleFixture(t, false, false)
	if err := f.transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Model the interval after Noise split/pinning but before encrypted HELLO finishes.
	f.transport.mu.Lock()
	f.transport.handshake = false
	f.transport.mu.Unlock()
	if err := f.transport.EmitBus(context.Background(), "premature", Data{}, Context{}); !errors.Is(err, ErrConnection) {
		t.Fatalf("application send escaped readiness: %v", err)
	}
	f.transport.mu.Lock()
	f.transport.handshake = true
	f.transport.mu.Unlock()
	assertWSSEcho(t, f.transport)
}
