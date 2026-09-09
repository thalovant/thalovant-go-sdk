package thalovant

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAskCollectsDelayedSpeechAfterSoftMissAndHandled(t *testing.T) {
	for _, first := range []string{EventIntentUnmatched, EventIntentFailure, EventUtteranceHandled} {
		t.Run(first, func(t *testing.T) {
			transport := newBlockedClientTransport()
			transport.ready.Store(true)
			client := &Client{Transport: transport}
			result := make(chan Reply, 1)
			errs := make(chan error, 1)
			go func() {
				reply, err := client.AskWithOptions(context.Background(), "hello", AskOptions{RequestOptions: RequestOptions{Timeout: time.Second}, ReplySettle: 30 * time.Millisecond, EmptyReplyWait: 200 * time.Millisecond})
				result <- reply
				errs <- err
			}()
			q := <-transport.emitted
			transport.streams.bus.publish(Event{Name: first, Data: Data{}, Context: q.context})
			select {
			case <-result:
				t.Fatal("completed before delayed speech")
			case <-time.After(20 * time.Millisecond):
			}
			for _, utterance := range []string{" hello  world ", "hello world", "second"} {
				transport.streams.bus.publish(Event{Name: EventSpeak, Data: Data{"utterance": utterance}, Context: q.context})
			}
			reply := <-result
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			if reply.Text != "hello world second" || !reply.OK || reply.FailureEvent != nil || len(reply.Utterances) != 2 {
				t.Fatalf("unexpected recovered reply: %+v", reply)
			}
		})
	}
}
func TestAskEmptyCompletionFailsAndHardDenialRetainsPartialReply(t *testing.T) {
	for _, withSpeech := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "partial denial"}[withSpeech], func(t *testing.T) {
			transport := newBlockedClientTransport()
			transport.ready.Store(true)
			client := &Client{Transport: transport}
			result := make(chan Reply, 1)
			errs := make(chan error, 1)
			go func() {
				reply, err := client.AskWithOptions(context.Background(), "hello", AskOptions{RequestOptions: RequestOptions{Timeout: time.Second}, EmptyReplyWait: 20 * time.Millisecond})
				result <- reply
				errs <- err
			}()
			q := <-transport.emitted
			if withSpeech {
				transport.streams.bus.publish(Event{Name: EventSpeak, Data: Data{"utterance": "partial"}, Context: q.context})
				transport.streams.bus.publish(Event{Name: EventPolicyDenied, Context: q.context})
			} else {
				transport.streams.bus.publish(Event{Name: EventUtteranceHandled, Context: q.context})
			}
			reply := <-result
			err := <-errs
			if !withSpeech && !errors.Is(err, ErrTimeout) {
				t.Fatal("empty reply accepted", err)
			}
			if withSpeech && (err != nil || reply.OK || reply.Text != "partial" || reply.FailureEvent == nil) {
				t.Fatalf("hard denial lost: %+v %v", reply, err)
			}
		})
	}
}
func TestFallbackProbeIncludesBlockedSendAndRetainsCleanup(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	transport.sendStarted = make(chan struct{}, 1)
	transport.sendRelease = make(chan struct{})
	client := &Client{Transport: transport, ConnectTimeout: 25 * time.Millisecond}
	result := make(chan error, 1)
	started := time.Now()
	go func() {
		fallbacks, err := client.ListFallbacks(context.Background(), 1500*time.Millisecond)
		if fallbacks != nil {
			err = errors.New("silence became known inventory")
		}
		result <- err
	}()
	awaitSignal(t, transport.sendStarted)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("optional probe exceeded its whole-operation budget")
	}
	if err := client.Connect(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatal("pending send lost ownership", err)
	}
	close(transport.sendRelease)
	awaitCleanup(t, transport)
}

type queryDispatchTransport struct {
	*blockedClientTransport
	sent chan HiveMessage
}

func (t *queryDispatchTransport) SendHiveMessage(_ context.Context, message HiveMessage, _ bool) error {
	t.sent <- message
	return nil
}
func TestQuerySoftMissCanRecoverButForeignQueryCannotComplete(t *testing.T) {
	transport := &queryDispatchTransport{blockedClientTransport: newBlockedClientTransport(), sent: make(chan HiveMessage, 1)}
	transport.ready.Store(true)
	client := &Client{Transport: transport}
	result := make(chan Reply, 1)
	errs := make(chan error, 1)
	go func() {
		reply, err := client.Query(context.Background(), "hello", QueryOptions{Timeout: time.Second, QueryID: "owned-query"})
		result <- reply
		errs <- err
	}()
	<-transport.sent
	publish := func(id, name, text string) {
		transport.streams.hive.publish(HiveMessage{MsgType: "cascade", Metadata: map[string]any{"query_id": id}, Payload: map[string]any{"type": name, "data": map[string]any{"utterance": text}, "context": map[string]any{}}})
	}
	publish("owned-query", EventIntentUnmatched, "")
	publish("foreign-query", EventPolicyDenied, "")
	publish("foreign-query", "hive.query.complete", "")
	select {
	case <-result:
		t.Fatal("soft miss or foreign query ended collection")
	case <-time.After(20 * time.Millisecond):
	}
	publish("owned-query", EventSpeak, "recovered")
	publish("owned-query", "hive.query.complete", "")
	reply := <-result
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if !reply.OK || reply.Text != "recovered" || reply.FailureEvent != nil {
		t.Fatalf("recovery failed: %+v", reply)
	}
}
