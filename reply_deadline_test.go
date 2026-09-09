package thalovant

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAskFirstSpeechStartsFixedSettlement(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	done := make(chan Reply, 1)
	errs := make(chan error, 1)
	go func() {
		reply, err := (&Client{Transport: transport}).AskWithOptions(context.Background(), "hello", AskOptions{RequestOptions: RequestOptions{Timeout: time.Second}, ReplySettle: 100 * time.Millisecond})
		done <- reply
		errs <- err
	}()
	q := <-transport.emitted
	transport.streams.bus.publish(Event{Name: EventSpeak, Data: Data{"utterance": "first"}, Context: q.context})
	// Keep producing fragments past the first settlement window. A resetting
	// window or one waiting for handled would never finish while this runs.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	limit := time.NewTimer(500 * time.Millisecond)
	defer limit.Stop()
	for {
		select {
		case reply := <-done:
			if err := <-errs; err != nil || !reply.OK || reply.Text == "" {
				t.Fatalf("speech did not settle: %+v %v", reply, err)
			}
			return
		case <-ticker.C:
			transport.streams.bus.publish(Event{Name: EventSpeak, Data: Data{"utterance": "more"}, Context: q.context})
		case <-limit.C:
			t.Fatal("first speech did not start a fixed settlement window")
		}
	}
}

type retiringReplyTransport struct {
	*blockedClientTransport
	hard string
}

func (t *retiringReplyTransport) EmitBus(_ context.Context, _ string, _ Data, ctx Context) error {
	t.streams.bus.publish(Event{Name: EventSpeak, Data: Data{"utterance": "partial"}, Context: ctx})
	if t.hard != "" {
		t.streams.bus.publish(Event{Name: t.hard, Context: ctx})
		t.streams.bus.publish(Event{Name: EventSpeak, Data: Data{"utterance": "too late"}, Context: ctx})
	}
	t.sendStarted <- struct{}{}
	<-t.sendRelease
	return nil
}

func TestAskReturnsCollectedSpeechWhileSendRetires(t *testing.T) {
	for _, hard := range []string{"", EventPolicyDenied, EventQueryTimeout} {
		t.Run("terminal="+hard, func(t *testing.T) {
			transport := &retiringReplyTransport{blockedClientTransport: newBlockedClientTransport(), hard: hard}
			transport.ready.Store(true)
			transport.sendStarted = make(chan struct{}, 1)
			transport.sendRelease = make(chan struct{})
			defer close(transport.sendRelease)
			client := &Client{Transport: transport, ConnectTimeout: 20 * time.Millisecond}
			reply, err := client.AskWithOptions(context.Background(), "hello", AskOptions{RequestOptions: RequestOptions{Timeout: 100 * time.Millisecond}, ReplySettle: time.Second})
			if err != nil || reply.Text != "partial" || reply.OK != (hard == "") {
				t.Fatalf("collected reply lost or changed: %+v %v", reply, err)
			}
			if err := client.Connect(context.Background()); !errors.Is(err, ErrTimeout) {
				t.Fatal("retiring send lost ownership", err)
			}
		})
	}
}

func TestAskRejectsUncorrelatedEventsAndAcceptsServerSession(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	done := make(chan Reply, 1)
	errs := make(chan error, 1)
	go func() {
		reply, err := (&Client{Transport: transport}).AskWithOptions(context.Background(), "hello", AskOptions{RequestOptions: RequestOptions{Timeout: time.Second, SessionID: "client"}, ReplySettle: 20 * time.Millisecond})
		done <- reply
		errs <- err
	}()
	q := <-transport.emitted
	for _, ctx := range []Context{nil, {"request_id": "foreign"}} {
		transport.streams.bus.publish(Event{Name: EventPolicyDenied, Context: ctx})
	}
	ctx := ContextWithCorrelation(nil, "server", "", "", RequestIDFromContext(q.context))
	transport.streams.bus.publish(Event{Name: EventSpeak, Data: Data{"utterance": "answer"}, Context: ctx})
	reply := <-done
	if err := <-errs; err != nil || reply.Text != "answer" || reply.SessionID != "server" {
		t.Fatalf("incorrect correlation: %+v %v", reply, err)
	}
}

func TestQueryHardFailureFreezesPartialAndSoftMissAfterSpeechRecovers(t *testing.T) {
	for _, terminal := range []string{EventPolicyDenied, EventQueryTimeout, EventIntentUnmatched, EventIntentFailure} {
		t.Run(terminal, func(t *testing.T) {
			transport := &queryDispatchTransport{blockedClientTransport: newBlockedClientTransport(), sent: make(chan HiveMessage, 1)}
			transport.ready.Store(true)
			done := make(chan Reply, 1)
			errs := make(chan error, 1)
			go func() {
				reply, err := (&Client{Transport: transport}).Query(context.Background(), "hello", QueryOptions{Timeout: time.Second, QueryID: "owned"})
				done <- reply
				errs <- err
			}()
			<-transport.sent
			publish := func(name, text string) {
				transport.streams.hive.publish(HiveMessage{MsgType: "cascade", Metadata: map[string]any{"query_id": "owned"}, Payload: map[string]any{"type": name, "data": map[string]any{"utterance": text}, "context": map[string]any{}}})
			}
			publish(EventSpeak, "partial")
			publish(terminal, "")
			hard := terminal == EventPolicyDenied || terminal == EventQueryTimeout
			if hard {
				publish(EventSpeak, "too late")
			} else {
				publish("hive.query.complete", "")
			}
			reply := <-done
			if err := <-errs; err != nil || reply.Text != "partial" || reply.OK == hard || (reply.FailureEvent != nil) != hard {
				t.Fatalf("terminal semantics lost: %+v %v", reply, err)
			}
		})
	}
}

type retiringQueryTransport struct {
	*blockedClientTransport
	terminal string
}

func (t *retiringQueryTransport) SendHiveMessage(_ context.Context, message HiveMessage, _ bool) error {
	for _, name := range []string{EventSpeak, t.terminal, EventSpeak} {
		t.streams.hive.publish(HiveMessage{MsgType: "cascade", Metadata: message.Metadata, Payload: map[string]any{"type": name, "data": map[string]any{"utterance": "partial"}, "context": map[string]any{}}})
	}
	t.sendStarted <- struct{}{}
	<-t.sendRelease
	return nil
}

func TestQueryTerminalReturnsWhileSendRetires(t *testing.T) {
	for _, terminal := range []string{EventPolicyDenied, EventQueryTimeout, "hive.query.complete"} {
		t.Run(terminal, func(t *testing.T) {
			transport := &retiringQueryTransport{blockedClientTransport: newBlockedClientTransport(), terminal: terminal}
			transport.ready.Store(true)
			transport.sendStarted = make(chan struct{}, 1)
			transport.sendRelease = make(chan struct{})
			defer close(transport.sendRelease)
			client := &Client{Transport: transport, ConnectTimeout: 20 * time.Millisecond}
			reply, err := client.Query(context.Background(), "hello", QueryOptions{Timeout: 100 * time.Millisecond})
			if err != nil || reply.Text != "partial" || reply.OK != (terminal == "hive.query.complete") || len(reply.Events) != 2 {
				t.Fatalf("terminal reply lost or changed: %+v %v", reply, err)
			}
			if err := client.Connect(context.Background()); !errors.Is(err, ErrTimeout) {
				t.Fatal("retiring query lost ownership", err)
			}
		})
	}
}

func TestQueryReturnsAcceptedRuntimeSessionWithRequestedFallback(t *testing.T) {
	for _, runtimeSession := range []string{"", " ", "runtime-session"} {
		t.Run("runtime="+runtimeSession, func(t *testing.T) {
			transport := &queryDispatchTransport{blockedClientTransport: newBlockedClientTransport(), sent: make(chan HiveMessage, 1)}
			transport.ready.Store(true)
			done := make(chan Reply, 1)
			errs := make(chan error, 1)
			go func() {
				reply, err := (&Client{Transport: transport}).Query(context.Background(), "hello", QueryOptions{Timeout: time.Second, QueryID: "owned", SessionID: "requested"})
				done <- reply
				errs <- err
			}()
			<-transport.sent
			publish := func(queryID, name, sessionID string) {
				transport.streams.hive.publish(HiveMessage{MsgType: "cascade", Metadata: map[string]any{"query_id": queryID}, Payload: map[string]any{"type": name, "data": map[string]any{"utterance": "answer"}, "context": map[string]any{"session_id": sessionID}}})
			}
			publish("foreign", EventSpeak, "foreign-session")
			publish("owned", EventSpeak, runtimeSession)
			publish("owned", "hive.query.complete", "")
			reply := <-done
			expected := runtimeSession
			if expected == "" || expected == " " {
				expected = "requested"
			}
			if err := <-errs; err != nil || reply.SessionID != expected {
				t.Fatalf("wrong reply session: %+v %v", reply, err)
			}
		})
	}
}
