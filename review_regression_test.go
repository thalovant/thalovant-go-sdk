package thalovant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockedInfoTransport struct {
	*blockedClientTransport
	infoStarted  chan struct{}
	infoRelease  chan struct{}
	connectError error
}

func (t *blockedInfoTransport) ConnectionInfo() TransportConnectionInfo {
	t.infoStarted <- struct{}{}
	<-t.infoRelease
	return TransportConnectionInfo{Phase: ConnectionReady}
}
func (t *blockedInfoTransport) Connect(ctx context.Context) error {
	if t.connectError != nil {
		return t.connectError
	}
	return t.blockedClientTransport.Connect(ctx)
}
func TestConnectWithInfoBoundsBlockedDiagnosticsAndRetainsOwnership(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint("failed-connect-", failed), func(t *testing.T) {
			transport := &blockedInfoTransport{blockedClientTransport: newBlockedClientTransport(), infoStarted: make(chan struct{}, 2), infoRelease: make(chan struct{})}
			release := sync.OnceFunc(func() { close(transport.infoRelease) })
			defer release()
			if failed {
				transport.connectError = errors.New("synthetic connect refusal")
			}
			client := &Client{Transport: transport, ConnectTimeout: 30 * time.Millisecond}
			done := make(chan error, 1)
			go func() { _, err := client.ConnectWithInfo(context.Background()); done <- err }()
			awaitSignal(t, transport.infoStarted)
			select {
			case err := <-done:
				if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				if failed && !errors.Is(err, transport.connectError) {
					t.Fatal("original connection error lost", err)
				}
			case <-time.After(time.Second):
				t.Fatal("diagnostics defeated the deadline")
			}
			before := transport.disconnects.Load()
			if err := client.Close(context.Background()); !errors.Is(err, ErrTimeout) {
				t.Fatal("getter lost ownership", err)
			}
			if transport.disconnects.Load() != before {
				t.Fatal("close raced pending diagnostic getter")
			}
			release()
			if err := client.Close(context.Background()); err != nil {
				t.Fatal("getter did not release ownership", err)
			}
		})
	}
}
func TestConnectWithInfoDoesNotStartDiagnosticsAfterConnectionDeadline(t *testing.T) {
	transport := &blockedInfoTransport{blockedClientTransport: newBlockedClientTransport(), infoStarted: make(chan struct{}, 1), infoRelease: make(chan struct{})}
	transport.connectStarted = make(chan struct{}, 1)
	transport.connectRelease = make(chan struct{})
	defer close(transport.infoRelease)
	release := sync.OnceFunc(func() { close(transport.connectRelease) })
	defer release()
	client := &Client{Transport: transport, ConnectTimeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := client.ConnectWithInfo(ctx)
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if len(transport.infoStarted) != 0 {
		t.Fatal("diagnostics started after connection deadline")
	}
	release()
	awaitCleanup(t, transport.blockedClientTransport)
}

func TestOwnedAndDirectTransportQueuesRetainTimeoutCategory(t *testing.T) {
	httpTransport := NewHTTPTransport(Identity{})
	mqttTransport := &MQTTTransport{}
	client := &Client{Transport: newBlockedClientTransport()}
	cases := []struct {
		name string
		gate *contextMutex
		call func(context.Context) error
	}{
		{"client-send", &client.connectionGate, func(ctx context.Context) error { return client.Emit(ctx, "test", nil, nil) }},
		{"http-connect", &httpTransport.lifecycleMu, httpTransport.Connect},
		{"http-close", &httpTransport.lifecycleMu, httpTransport.Disconnect},
		{"http-send", &httpTransport.lifecycleMu, func(ctx context.Context) error { return httpTransport.EmitBus(ctx, "test", nil, nil) }},
		{"http-poll", &httpTransport.pollMu, httpTransport.PollOnce},
		{"http-connect-poll", &httpTransport.pollMu, httpTransport.Connect},
		{"mqtt-connect", &mqttTransport.lifecycleMu, mqttTransport.Connect},
		{"mqtt-close", &mqttTransport.lifecycleMu, mqttTransport.Disconnect},
		{"mqtt-send", &mqttTransport.lifecycleMu, func(ctx context.Context) error { return mqttTransport.EmitBus(ctx, "test", nil, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.gate.Lock(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer tc.gate.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel()
			err := tc.call(ctx)
			if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

type queuedSettlementTransport struct {
	*blockedClientTransport
	deny bool
}

func (t *queuedSettlementTransport) SubscribeEvents(capacity int) *Subscription[Event] {
	// Install a complete backlog before the collector receives the channel.
	// Emit and collection now run concurrently, so synchronous Emit alone no
	// longer guarantees that the later frames are queued at zero settlement.
	sub := t.blockedClientTransport.SubscribeEvents(capacity)
	ctx := Context{"request_id": "queued"}
	t.streams.bus.publish(Event{Name: EventUtteranceHandled, Context: ctx})
	for _, text := range []string{"one", "two", "three"} {
		t.streams.bus.publish(Event{Name: EventSpeak, Data: Data{"utterance": text}, Context: ctx})
	}
	t.streams.bus.publish(Event{Name: EventSpeak, Data: Data{"utterance": "foreign"}, Context: Context{"request_id": "foreign"}})
	if t.deny {
		t.streams.bus.publish(Event{Name: EventPolicyDenied, Context: ctx})
	}
	return sub
}
func TestAskSettlementIncludesQueuedSpeechAndHardFailure(t *testing.T) {
	for _, deny := range []bool{false, true} {
		for i := 0; i < 20; i++ {
			transport := &queuedSettlementTransport{blockedClientTransport: newBlockedClientTransport(), deny: deny}
			transport.ready.Store(true)
			reply, err := (&Client{Transport: transport}).AskWithOptions(context.Background(), "test", AskOptions{RequestOptions: RequestOptions{Timeout: time.Second, RequestID: "queued"}, EmptyReplyWait: -1, ReplySettle: -1})
			if err != nil || reply.Text != "one two three" || reply.OK == deny || len(reply.Utterances) != 3 {
				t.Fatalf("queued reply lost: %+v %v", reply, err)
			}
			if (reply.FailureEvent != nil) != deny {
				t.Fatal("queued hard failure lost")
			}
		}
	}
}

type capturedFallbackHub struct {
	*intentHub
	remaining chan time.Duration
}

func (h *capturedFallbackHub) EmitBus(ctx context.Context, name string, data Data, eventContext Context) error {
	if name == EventFallbackList {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("missing fallback deadline")
		}
		h.remaining <- time.Until(deadline)
		h.deliver(EventFallbackListResponse, Data{"fallbacks": []any{}}, eventContext)
		return nil
	}
	return h.intentHub.EmitBus(ctx, name, data, eventContext)
}
func TestDefaultCapabilityProbeAlreadyResolvesAndCapsTimeout(t *testing.T) {
	hub := &capturedFallbackHub{intentHub: newIntentHub(), remaining: make(chan time.Duration, 1)}
	_, err := (&Client{Transport: hub}).IntentsWithCapabilities(context.Background(), nil, IntentOptions{Describe: boolPointer(false)})
	if err != nil {
		t.Fatal(err)
	}
	remaining := <-hub.remaining
	if remaining <= 0 || remaining > 1500*time.Millisecond {
		t.Fatal("uncapped default probe", remaining)
	}
}
func TestFallbackPriorityDefaultsPreserveSharedContract(t *testing.T) {
	hub := &fallbackHub{intentHub: newIntentHub(), data: Data{"fallbacks": []any{
		map[string]any{"skill_id": "missing"}, map[string]any{"skill_id": "null", "priority": nil},
		map[string]any{"skill_id": "text", "priority": "invalid"}, map[string]any{"skill_id": "bool", "priority": true},
	}}}
	rows, err := (&Client{Transport: hub}).ListFallbacks(context.Background(), time.Second)
	if err != nil || len(rows) != 4 {
		t.Fatalf("shared defaults lost: %+v %v", rows, err)
	}
	for _, row := range rows {
		want := int64(0)
		if row.SkillID == "bool" {
			want = 1
		}
		if row.Priority != want {
			t.Fatal(row)
		}
	}
}

type ignorePollCancellation struct {
	base            http.RoundTripper
	paused          atomic.Bool
	entered, resume chan struct{}
}

func (h *ignorePollCancellation) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/get_messages" && h.paused.Swap(false) {
		close(h.entered)
		<-h.resume
		return nil, errors.New("retired poll failure")
	}
	return h.base.RoundTrip(req)
}
func TestHTTPDisconnectRetainsCleanupAfterCallerDeadline(t *testing.T) {
	fixture := newHTTPNoiseFixture(t)
	transport := fixture.transport(t)
	transport.PollInterval = 10 * time.Millisecond
	held := &ignorePollCancellation{base: transport.HTTPClient.Transport, entered: make(chan struct{}), resume: make(chan struct{})}
	transport.HTTPClient.Transport = held
	release := sync.OnceFunc(func() { close(held.resume) })
	defer release()
	if err := transport.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	held.paused.Store(true)
	awaitSignal(t, held.entered)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := transport.Disconnect(ctx); !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel2()
	if err := transport.Connect(ctx2); !errors.Is(err, ErrTimeout) {
		t.Fatal("replacement passed pending cleanup", err)
	}
	release()
	joined, joinCancel := context.WithTimeout(context.Background(), time.Second)
	defer joinCancel()
	if err := transport.lifecycleMu.Lock(joined); err != nil {
		t.Fatal("cleanup worker never retired", err)
	}
	transport.mu.RLock()
	stale := transport.noise != nil || transport.admitted || transport.connected || transport.connection.snapshot().Phase != ConnectionClosed
	transport.mu.RUnlock()
	transport.lifecycleMu.Unlock()
	if stale {
		t.Fatal("late poll left stale local or remote admission state")
	}
	if err := transport.Connect(context.Background()); err != nil {
		t.Fatal("fresh authenticated reconnect failed", err)
	}
	if err := transport.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// roundTripRecorder intentionally never performs network I/O.
type roundTripRecorder struct{ calls atomic.Int32 }

func (r *roundTripRecorder) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}
func TestControlCredentialEndpointRejectsRemoteHTTPBeforeIO(t *testing.T) {
	for _, endpoint := range []string{"http://example.invalid", "http://localhost.example.invalid", "http://127.0.0.2", "http://127.1", "http://[::ffff:127.0.0.1]", "ftp://localhost", "https://synthetic:synthetic-url-secret@localhost", "http://synthetic:synthetic-url-secret@127.0.0.1"} {
		t.Run(endpoint, func(t *testing.T) {
			recorder := &roundTripRecorder{}
			control := NewControlPlane(endpoint, "synthetic-bearer")
			control.HTTPClient = &http.Client{Transport: recorder}
			if _, err := control.Login(context.Background(), "synthetic@example.invalid", "synthetic-password", ""); !errors.Is(err, ErrAPI) {
				t.Fatal(err)
			}
			if _, err := control.GetHub(context.Background(), "synthetic"); !errors.Is(err, ErrAPI) {
				t.Fatal(err)
			}
			if recorder.calls.Load() != 0 {
				t.Fatal("credentials reached an insecure endpoint")
			}
		})
	}
	for _, endpoint := range []string{"http://localhost", "http://127.0.0.1", "http://[::1]", "https://example.invalid"} {
		recorder := &roundTripRecorder{}
		control := NewControlPlane(endpoint, "synthetic-bearer")
		control.HTTPClient = &http.Client{Transport: recorder}
		if _, err := control.GetHub(context.Background(), "synthetic"); err != nil {
			t.Fatal(endpoint, err)
		}
		if recorder.calls.Load() != 1 {
			t.Fatal("supported endpoint refused", endpoint)
		}
	}
}
func TestControlNeverForwardsCredentialsAcrossRedirects(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, login := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-login-%t", status, login), func(t *testing.T) {
				var destinationCalls, customRedirects atomic.Int32
				destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					destinationCalls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = w.Write([]byte(`{}`))
				}))
				defer destination.Close()
				source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if login {
						raw, _ := io.ReadAll(r.Body)
						if !strings.Contains(string(raw), "synthetic-password") {
							t.Error("login password not sent to original endpoint")
						}
					} else if r.Header.Get("authorization") != "Bearer synthetic-bearer" {
						t.Error("original bearer missing")
					}
					w.Header().Set("Location", destination.URL)
					w.WriteHeader(status)
				}))
				defer source.Close()
				client := source.Client()
				client.CheckRedirect = func(*http.Request, []*http.Request) error { customRedirects.Add(1); return nil }
				control := NewControlPlane(source.URL, "synthetic-bearer")
				control.HTTPClient = client
				var err error
				if login {
					_, err = control.Login(context.Background(), "synthetic@example.invalid", "synthetic-password", "")
				} else {
					_, err = control.GetHub(context.Background(), "synthetic")
				}
				if !errors.Is(err, ErrAPI) || destinationCalls.Load() != 0 || customRedirects.Load() != 0 {
					t.Fatalf("redirect followed: %v %d %d", err, destinationCalls.Load(), customRedirects.Load())
				}
				if client.CheckRedirect == nil {
					t.Fatal("injected client was mutated")
				}
				_ = client.CheckRedirect(nil, nil)
				if customRedirects.Load() != 1 {
					t.Fatal("injected redirect policy was replaced")
				}
			})
		}
	}
}
