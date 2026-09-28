package thalovant

// The home link beyond the shared vectors: a request answered end to end over
// a real WSS Noise connection, the kept link (handlers that outlive a
// reconnect, the settle window, the refusal grace), and the error kinds the
// control plane hands a caller to branch on.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// homeHub is a loopback hub over WSS with real Noise. Once the client has
// said hello it either sends one bus message and collects what comes back, or
// closes the socket the way closeWith says: a close code, or -1 for a close
// frame with no status.
type homeHub struct {
	server    *httptest.Server
	identity  Identity
	request   map[string]any
	closeWith int
	received  chan HiveMessage
	accepted  atomic.Int32
}

func newHomeHub(t *testing.T, request map[string]any, closeWith int) *homeHub {
	t.Helper()
	hub := &homeHub{request: request, closeWith: closeWith, received: make(chan HiveMessage, 16)}
	shared := newTransportResponder(t)
	var sockets sync.WaitGroup
	hub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		sockets.Add(1)
		defer sockets.Done()
		defer conn.Close()
		hub.accepted.Add(1)
		responder := &transportResponder{key: shared.key, psk: shared.psk}
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
		for {
			kind, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage || responder.session == nil {
				if responder.receive(raw, kind == websocket.BinaryMessage) != nil || flush() != nil {
					return
				}
				continue
			}
			payload, _, complete, err := responder.session.decryptFrame(raw)
			if err != nil {
				return
			}
			if !complete {
				continue
			}
			var message HiveMessage
			if json.Unmarshal(payload, &message) != nil {
				return
			}
			if message.MsgType != "hello" {
				hub.received <- message
				continue
			}
			switch {
			case hub.closeWith == -1:
				_ = conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			case hub.closeWith > 0:
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(hub.closeWith, ""))
				return
			case hub.request != nil:
				frame := []byte(marshalHive("bus", hub.request))
				if responder.session.sendMessage(frame, true, func(chunk []byte) error {
					return conn.WriteMessage(websocket.BinaryMessage, chunk)
				}) != nil {
					return
				}
			}
		}
	}))
	hub.identity = Identity{AccessKey: "test-access", Password: "test-password", SiteID: "test-site", DefaultMaster: "ws" + strings.TrimPrefix(hub.server.URL, "http")}
	hub.identity.DataPlaneEndpoints = HubDataPlaneEndpoints{WSS: hub.identity.DefaultMaster}
	t.Cleanup(func() {
		hub.server.CloseClientConnections()
		hub.server.Close()
		sockets.Wait()
	})
	return hub
}

// connect is a HubSession factory dialling the hub with a fresh transport.
func (h *homeHub) connect(t *testing.T) func(context.Context) (HubSessionClient, error) {
	return func(ctx context.Context) (HubSessionClient, error) {
		transport := NewWSSTransport(h.identity)
		transport.NoiseStateDir = t.TempDir()
		client := &Client{Identity: h.identity, Transport: transport}
		if err := client.Connect(ctx); err != nil {
			_ = client.Close(context.Background())
			return nil, err
		}
		return client, nil
	}
}

func TestHomeRequestAnsweredOverWSSAlongTheWayItCame(t *testing.T) {
	hub := newHomeHub(t, map[string]any{
		"type": HomeRequestEvent,
		"data": map[string]any{"request_id": "r1", "utterance": "turn off the kitchen light", "lang": "en-US", "conversation_id": "conv-1"},
		"context": map[string]any{
			"source":      "thalovant-skill-home",
			"destination": []any{"ha-peer", "other"},
			"session":     map[string]any{"session_id": "kitchen"},
		},
	}, 0)
	session, err := NewHubSession(hub.connect(t), DefaultHubSessionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	asked := make(chan HomeRequest, 1)
	stop := AnswerHomeRequests(session, func(ctx context.Context, request HomeRequest) (HomeAnswer, error) {
		asked <- request
		return HomeAnswer{Speech: "<speak>Turned off the kitchen light.</speak>"}, nil
	}, HomeAnswerOptions{})
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan error, 1)
	go func() { ran <- session.Run(ctx) }()

	var request HomeRequest
	select {
	case request = <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the handler")
	}
	if request.RequestID != "r1" || request.Utterance != "turn off the kitchen light" || request.Lang != "en-US" {
		t.Fatalf("request read as %+v", request)
	}
	var reply HiveMessage
	select {
	case reply = <-hub.received:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub never got an answer")
	}
	if reply.MsgType != "bus" || reply.Payload["type"] != HomeResponseEvent {
		t.Fatalf("the answer is %s %v", reply.MsgType, reply.Payload["type"])
	}
	wantData := map[string]any{
		"request_id": "r1", "speech": "Turned off the kitchen light.", "response_type": "action_done",
		"continue_conversation": false, "conversation_id": "conv-1",
	}
	if !reflect.DeepEqual(reply.Payload["data"], wantData) {
		t.Errorf("answer data = %v", reply.Payload["data"])
	}
	replyContext := mapValue(reply.Payload["context"])
	if replyContext["source"] != "ha-peer" || replyContext["destination"] != "thalovant-skill-home" ||
		!reflect.DeepEqual(replyContext["session"], map[string]any{"session_id": "kitchen"}) {
		t.Errorf("the answer is not routed back: %v", replyContext)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("Run after Close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Close")
	}
}

func TestSettleWindowTellsARefusalFromADrop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		closeWith int
		refused   bool
	}{
		{"policy violation", websocket.ClosePolicyViolation, true},
		{"normal closure", websocket.CloseNormalClosure, true},
		{"no status", -1, true},
		{"internal error", websocket.CloseInternalServerErr, false},
		{"try again later", websocket.CloseTryAgainLater, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newHomeHub(t, nil, tc.closeWith)
			session, err := NewHubSession(hub.connect(t), DefaultHubSessionPolicy(), WithSettle(3*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close(context.Background())
			err = session.Connect(context.Background())
			if !errors.Is(err, ErrConnection) {
				t.Fatalf("Connect = %v, want a connection error", err)
			}
			if errors.Is(err, ErrHubRefused) != tc.refused {
				t.Fatalf("Connect = %v; refused = %v, want %v", err, errors.Is(err, ErrHubRefused), tc.refused)
			}
			if session.Held() || session.Connected() {
				t.Fatal("a link closed inside the settle window is still held")
			}
		})
	}
}

// linkFixture is a HubSessionClient whose liveness and refusal a test sets.
type linkFixture struct {
	managedFixture
	mu      sync.Mutex
	dead    bool
	refused bool
	emitted []Context
}

func (f *linkFixture) ConnectionInfo() TransportConnectionInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead {
		return TransportConnectionInfo{Phase: ConnectionError}
	}
	return TransportConnectionInfo{Phase: ConnectionReady}
}
func (f *linkFixture) ClosedRefused() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.refused }
func (f *linkFixture) kill()               { f.mu.Lock(); f.dead = true; f.mu.Unlock() }
func (f *linkFixture) Close(context.Context) error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}
func (f *linkFixture) Emit(_ context.Context, _ string, _ Data, eventContext Context) error {
	f.mu.Lock()
	f.emitted = append(f.emitted, eventContext)
	f.mu.Unlock()
	return nil
}

func quickPolicy() HubSessionPolicy {
	return HubSessionPolicy{Retry: 10 * time.Millisecond, RetryCeiling: 40 * time.Millisecond, Probe: time.Second, ProbeDown: 10 * time.Millisecond}
}

func TestHubSessionHandlersOutliveAReconnect(t *testing.T) {
	links := make(chan *linkFixture, 4)
	session, err := NewHubSession(func(context.Context) (HubSessionClient, error) {
		link := &linkFixture{}
		links <- link
		return link, nil
	}, quickPolicy(), WithSettle(0))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var heard []string
	var states []bool
	stopHearing := session.On("ping", func(event Event) {
		mu.Lock()
		heard = append(heard, fmt.Sprint(event.Data["n"]))
		mu.Unlock()
	})
	session.On("ping", func(Event) { panic("a handler that panics is skipped") })
	session.OnStateChange(func(up bool) { mu.Lock(); states = append(states, up); mu.Unlock() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan error, 1)
	go func() { ran <- session.Run(ctx) }()

	first := <-links
	waitFor(t, session.Connected)
	first.events.publish(Event{Name: "ping", Data: Data{"n": 1}})
	first.events.publish(Event{Name: "other", Data: Data{"n": 9}})
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(heard) == 1 })
	first.kill()
	second := <-links
	waitFor(t, session.Connected)
	second.events.publish(Event{Name: "ping", Data: Data{"n": 2}})
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(heard) == 2 })
	stopHearing()
	second.events.publish(Event{Name: "ping", Data: Data{"n": 3}})
	time.Sleep(50 * time.Millisecond)

	cancel()
	if err := <-ran; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the context's error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(heard, []string{"1", "2"}) {
		t.Errorf("heard %v", heard)
	}
	if !reflect.DeepEqual(states, []bool{true, false, true}) {
		t.Errorf("state changes %v", states)
	}
	first.mu.Lock()
	closed := first.closed
	first.mu.Unlock()
	if closed != 1 {
		t.Error("the dropped client was not closed")
	}
}

func TestHubSessionRunGivesUpOnARefusalAfterTheGrace(t *testing.T) {
	attempts := atomic.Int32{}
	session, err := NewHubSession(func(context.Context) (HubSessionClient, error) {
		attempts.Add(1)
		link := &linkFixture{dead: true, refused: true}
		return link, nil
	}, quickPolicy(), WithSettle(50*time.Millisecond), WithRefusalGrace(150*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	done := make(chan error, 1)
	go func() { done <- session.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrHubRefused) || !errors.Is(err, ErrConnection) {
			t.Fatalf("Run = %v, want the refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never gave up on a hub that keeps refusing")
	}
	if attempts.Load() < 2 {
		t.Errorf("a refusal was not retried inside the grace (%d attempts)", attempts.Load())
	}
}

func TestHubSessionRunRetriesADropOnTheLadder(t *testing.T) {
	attempts := atomic.Int32{}
	session, err := NewHubSession(func(context.Context) (HubSessionClient, error) {
		if attempts.Add(1) < 3 {
			return nil, fmt.Errorf("%w: the hub is restarting", ErrConnection)
		}
		return &linkFixture{}, nil
	}, quickPolicy(), WithSettle(0), WithRefusalGrace(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = session.Run(context.Background()) }()
	waitFor(t, session.Connected)
	if attempts.Load() != 3 {
		t.Errorf("connected after %d attempts, want 3", attempts.Load())
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := session.Run(context.Background()); err != nil {
		t.Errorf("Run on a closed session = %v", err)
	}
}

func TestHubSessionReplyTurnsTheRouteRound(t *testing.T) {
	link := &linkFixture{}
	session, err := NewHubSession(func(context.Context) (HubSessionClient, error) { return link, nil }, quickPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	event := Event{Name: HomeRequestEvent, Context: Context{
		"source": "skills", "destination": []any{"ha-peer"}, "session": map[string]any{"session_id": "s"},
	}}
	if err := session.Reply(context.Background(), event, HomeResponseEvent, Data{}, Context{"lang": "fr-FR"}); err != nil {
		t.Fatal(err)
	}
	want := Context{"source": "ha-peer", "destination": "skills", "session": map[string]any{"session_id": "s"}, "lang": "fr-FR"}
	if len(link.emitted) != 1 || !reflect.DeepEqual(link.emitted[0], want) {
		t.Fatalf("replied with %v", link.emitted)
	}
	link.emitted[0]["session"].(map[string]any)["session_id"] = "changed"
	if event.Context["session"].(map[string]any)["session_id"] != "s" {
		t.Error("the reply shares its context with the event")
	}
	if err := session.Reply(context.Background(), event, " ", nil, nil); err == nil {
		t.Error("a reply with no type was sent")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHomeHandlerOutcomes(t *testing.T) {
	event := Event{Name: HomeRequestEvent, Data: Data{"request_id": "r", "conversation_id": "c"}}
	for _, tc := range []struct {
		name    string
		handler HomeHandler
		want    Data
	}{
		{"a handler that honours its deadline is a timeout", func(ctx context.Context, _ HomeRequest) (HomeAnswer, error) {
			<-ctx.Done()
			return HomeAnswer{}, ctx.Err()
		}, Data{"request_id": "r", "speech": "", "response_type": "error", "error_code": "timeout", "continue_conversation": false, "conversation_id": "c"}},
		{"a panic is failed_to_handle", func(context.Context, HomeRequest) (HomeAnswer, error) {
			panic("boom")
		}, Data{"request_id": "r", "speech": "", "response_type": "error", "error_code": "failed_to_handle", "continue_conversation": false, "conversation_id": "c"}},
		{"no handler is failed_to_handle", nil,
			Data{"request_id": "r", "speech": "", "response_type": "error", "error_code": "failed_to_handle", "continue_conversation": false, "conversation_id": "c"}},
		{"an error without a code is unknown", func(context.Context, HomeRequest) (HomeAnswer, error) {
			return HomeAnswer{ResponseType: HomeError, Speech: "Nope &amp; no."}, nil
		}, Data{"request_id": "r", "speech": "Nope & no.", "response_type": "error", "error_code": "unknown", "continue_conversation": false, "conversation_id": "c"}},
		{"a code outside an error is dropped", func(context.Context, HomeRequest) (HomeAnswer, error) {
			return HomeAnswer{ResponseType: HomeQueryAnswer, ErrorCode: HomeErrorTimeout, ConversationID: "own"}, nil
		}, Data{"request_id": "r", "speech": "", "response_type": "query_answer", "continue_conversation": false, "conversation_id": "own"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replier := &capturingReplier{}
			payload, err := AnswerHomeRequest(context.Background(), replier, event, tc.handler, HomeAnswerOptions{Timeout: 50 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(payload, tc.want) || len(replier.sent) != 1 {
				t.Fatalf("answered %v (%d replies)", payload, len(replier.sent))
			}
		})
	}
}

func TestHomeAnswerNotSentWhenTheLinkIsGoingAway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	replier := &capturingReplier{}
	started := make(chan struct{})
	go func() { <-started; cancel() }()
	_, err := AnswerHomeRequest(ctx, replier, Event{Name: HomeRequestEvent}, func(handlerCtx context.Context, _ HomeRequest) (HomeAnswer, error) {
		close(started)
		<-handlerCtx.Done()
		return HomeAnswer{}, handlerCtx.Err()
	}, HomeAnswerOptions{Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) || len(replier.sent) != 0 {
		t.Fatalf("err = %v, %d replies", err, len(replier.sent))
	}
}

func TestPlainSpeech(t *testing.T) {
	for input, want := range map[string]string{
		"":                                    "",
		"  plain  ":                           "plain",
		"<b>bold</b>\u00a0&lt;tag&gt; &#233;": "bold <tag> é",
		// Unicode White_Space collapses; the information separators do not,
		// though the ends are trimmed of them as the reference's strip() does.
		"line\none\u2028two\x1cthree\x1c": "line one two\x1cthree",
		"5 < 6 and 7 > 3":                 "5 < 6 and 7 > 3",
	} {
		if got := PlainSpeech(input); got != want {
			t.Errorf("PlainSpeech(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAPIErrorRefusalKinds(t *testing.T) {
	for _, tc := range []struct {
		err  *APIError
		kind error
	}{
		{&APIError{StatusCode: 401}, ErrAuth},
		{&APIError{StatusCode: 423}, ErrAuth},
		{&APIError{StatusCode: 403, Problem: map[string]any{"detail": "Insufficient scopes"}}, ErrAuth},
		{&APIError{StatusCode: 402}, ErrPlan},
		{&APIError{StatusCode: 403, Code: "plan_limit"}, ErrPlan},
		{&APIError{StatusCode: 409, Problem: map[string]any{"detail": map[string]any{"code": "home_assistant_already_linked", "existing_client_id": "c1"}}}, ErrAlreadyLinked},
		{&APIError{StatusCode: 403, Problem: map[string]any{"detail": "Forbidden"}}, nil},
		{&APIError{StatusCode: 409, Problem: map[string]any{"detail": "Client name already exists"}}, nil},
	} {
		for _, kind := range []error{ErrAuth, ErrPlan, ErrAlreadyLinked} {
			var err error = fmt.Errorf("wrapped: %w", tc.err)
			if errors.Is(err, kind) != (kind == tc.kind) {
				t.Errorf("%d %v: errors.Is(%v) = %v", tc.err.StatusCode, tc.err.Problem, kind, !(kind == tc.kind))
			}
			if !errors.Is(err, ErrAPI) {
				t.Errorf("%v no longer matches ErrAPI", tc.err)
			}
		}
	}
	linked := &APIError{StatusCode: 409, Problem: map[string]any{"detail": map[string]any{"code": "home_assistant_already_linked", "existing_client_id": "c1"}}}
	if linked.LinkedClientID() != "c1" {
		t.Errorf("LinkedClientID() = %q", linked.LinkedClientID())
	}
	if (&APIError{StatusCode: 409}).LinkedClientID() != "" {
		t.Error("a bare 409 names a linked client")
	}
}

func TestReplyContextLeavesAbsentAndNullRoutingAlone(t *testing.T) {
	got := ReplyContext(Context{"source": nil, "destination": []any{}, "session": map[string]any{"session_id": "s"}})
	// A null source is no source, so the reply has no destination; an empty
	// destination list has no first entry, so the list itself becomes the
	// source, as in the reference.
	want := Context{"source": []any{}, "session": map[string]any{"session_id": "s"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReplyContext = %v, want %v", got, want)
	}
	if got := ReplyContext(nil); got == nil || len(got) != 0 {
		t.Errorf("ReplyContext(nil) = %v", got)
	}
}

func TestLoginWithBrowserKeepsTheTokenID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path == "/v1/auth/device/authorize" {
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"U","verification_uri":"https://thalovant.com/activate","interval":0}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"tok","token_id":"tid"}`))
	}))
	defer server.Close()
	plane := NewControlPlane(server.URL, "")
	open := false
	if _, err := plane.LoginWithBrowser(context.Background(), DeviceLoginOptions{OpenBrowser: &open, Prompt: func(map[string]any) {}}); err != nil {
		t.Fatal(err)
	}
	if plane.AccessToken != "tok" || plane.TokenID != "tid" {
		t.Fatalf("kept %q / %q", plane.AccessToken, plane.TokenID)
	}
}

func TestWaitForAdmissionEndsWithTheContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"op","status":"committed"}`))
	}))
	defer server.Close()
	plane := NewControlPlane(server.URL, "token")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := plane.WaitForAdmission(ctx, &OperationResource{ID: "op"}, AdmissionOptions{PollInterval: 10 * time.Millisecond})
	var timeout *AdmissionTimeoutError
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) {
		t.Fatalf("WaitForAdmission = %v", err)
	}
	link := "http://" + strings.TrimPrefix(server.URL, "http://") + "/v1/operations/op"
	if err := plane.WaitForAdmission(context.Background(), &OperationResource{Links: map[string]*string{"self": &link}}, AdmissionOptions{Timeout: 50 * time.Millisecond, PollInterval: 10 * time.Millisecond}); !errors.As(err, &timeout) {
		t.Fatalf("an operation named only by its same-origin link was not followed: %v", err)
	}
	other := "https://" + strings.TrimPrefix(server.URL, "http://") + "/v1/operations/op"
	if err := plane.WaitForAdmission(context.Background(), &OperationResource{ID: "op", Links: map[string]*string{"self": &other}}, AdmissionOptions{}); !errors.Is(err, ErrAPI) {
		t.Fatalf("a link with another scheme was followed: %v", err)
	}
}

func TestDeviceGrantNumbersNeverOverflow(t *testing.T) {
	grant, err := deviceAuthorizationFromGrant(map[string]any{
		"device_code": "dc", "user_code": "U", "verification_uri": "https://thalovant.com/activate",
		"interval": 1e300, "expires_in": 1e300,
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant.Interval != defaultDevicePollInterval || grant.ExpiresIn != 900*time.Second {
		t.Fatalf("out-of-range numbers read as %s / %s", grant.Interval, grant.ExpiresIn)
	}
}

func TestAConnectionTypeRefusalIsReadFromWhereTheAPISaysWhatIsWrong(t *testing.T) {
	for _, tc := range []struct {
		problem map[string]any
		refused bool
	}{
		{map[string]any{"detail": "Schema validation failed: 'home_assistant' is not one of [...] at spec.connection_type"}, true},
		{map[string]any{"detail": []any{map[string]any{"loc": []any{"body", "spec", "connection_type"}, "msg": "Input should be ..."}}}, true},
		// A 422 about another field that echoes the request, which always
		// names the connection type, is not about the connection type.
		{map[string]any{"detail": []any{map[string]any{"loc": []any{"body", "spec", "siteId"}, "msg": "too long",
			"input": map[string]any{"connection_type": "home_assistant", "siteId": "x"}}}}, false},
		{map[string]any{"detail": "Name too long", "spec": map[string]any{"connection_type": "home_assistant"}}, false},
	} {
		_, refused := refusesConnectionType(&APIError{StatusCode: http.StatusUnprocessableEntity, Problem: tc.problem})
		if refused != tc.refused {
			t.Errorf("%v: refused = %v, want %v", tc.problem, refused, tc.refused)
		}
	}
	if _, refused := refusesConnectionType(&APIError{StatusCode: http.StatusBadRequest, ProblemDetail: "connection_type"}); refused {
		t.Error("a 400 read as a connection-type refusal")
	}
}

func TestEverySignInSetsTheTokenIDFromItsOwnAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"session-token"}`))
	}))
	defer server.Close()
	plane := NewControlPlane(server.URL, "device-token")
	plane.TokenID = "device-token-id"
	if _, err := plane.Login(context.Background(), "me@example.com", "pw", ""); err != nil {
		t.Fatal(err)
	}
	if plane.AccessToken != "session-token" || plane.TokenID != "" {
		t.Fatalf("after a password sign-in: %q / %q", plane.AccessToken, plane.TokenID)
	}
	if err := plane.RevokeAPIToken(context.Background(), ""); !errors.Is(err, ErrAPI) {
		t.Fatalf("revoking with no device token = %v, want a local refusal", err)
	}
}

// stallingReplier holds every reply until its context ends.
type stallingReplier struct{ deadline chan time.Time }

func (r stallingReplier) Reply(ctx context.Context, _ Event, _ string, _ Data, _ Context) error {
	deadline, _ := ctx.Deadline()
	r.deadline <- deadline
	<-ctx.Done()
	return ctx.Err()
}

func TestAHomeReplyGetsWhatIsLeftOfTheHubsBound(t *testing.T) {
	replier := stallingReplier{deadline: make(chan time.Time, 1)}
	started := time.Now()
	sent, err := AnswerHomeRequest(context.Background(), replier, Event{Name: HomeRequestEvent}, func(context.Context, HomeRequest) (HomeAnswer, error) {
		return HomeAnswer{Speech: "done"}, nil
	}, HomeAnswerOptions{Timeout: time.Minute, HubTimeout: 200 * time.Millisecond})
	if err != nil || sent != nil {
		t.Fatalf("a reply stalled past the bound = %v, %v; want it withdrawn", sent, err)
	}
	// A handler timeout longer than the hub's bound does not stretch it.
	if deadline := <-replier.deadline; deadline.Sub(started) > 200*time.Millisecond+50*time.Millisecond {
		t.Fatalf("the reply could run until %s after the request", deadline.Sub(started))
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("answering took %s against a 200ms bound", elapsed)
	}
}

// deafReplier never returns, whatever its context says.
type deafReplier struct{}

func (deafReplier) Reply(context.Context, Event, string, Data, Context) error { select {} }

func TestAReplierThatIgnoresItsContextCannotHoldTheAnswerPastTheBound(t *testing.T) {
	started := time.Now()
	sent, err := AnswerHomeRequest(context.Background(), deafReplier{}, Event{Name: HomeRequestEvent}, func(context.Context, HomeRequest) (HomeAnswer, error) {
		return HomeAnswer{Speech: "done"}, nil
	}, HomeAnswerOptions{HubTimeout: 100 * time.Millisecond})
	if err != nil || sent != nil || time.Since(started) > time.Second {
		t.Fatalf("= %v, %v after %s", sent, err, time.Since(started))
	}
}

func TestAStalledOperationReadEndsTheAdmissionWaitOnTime(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	plane := NewControlPlane(server.URL, "token")
	started := time.Now()
	err := plane.WaitForAdmission(context.Background(), &OperationResource{ID: "op"}, AdmissionOptions{Timeout: 200 * time.Millisecond})
	var timeout *AdmissionTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("WaitForAdmission = %v, want the admission timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("a stalled read held the wait for %s", elapsed)
	}
}

func TestAnUnsubscribedHandlerIsNeverCalledAgain(t *testing.T) {
	session, err := NewHubSession(func(context.Context) (HubSessionClient, error) { return &linkFixture{}, nil }, quickPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	calls := atomic.Int32{}
	unsubscribe := session.On("ping", func(Event) { calls.Add(1) })
	session.mu.Lock()
	copied := append([]*sessionHandler(nil), session.handlers["ping"]...)
	session.mu.Unlock()
	// A delivery that copied the handler just before the unsubscribe reaches
	// it after: it must not call it.
	unsubscribe()
	for _, handler := range copied {
		deliver(handler, Event{Name: "ping"})
	}
	if calls.Load() != 0 {
		t.Fatalf("an unsubscribed handler was called %d times", calls.Load())
	}
}
