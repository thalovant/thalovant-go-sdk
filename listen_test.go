package thalovant

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func awaitListenerCount(t *testing.T, transport *blockedClientTransport, want int) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for {
		transport.streams.bus.mu.Lock()
		count := len(transport.streams.bus.entries)
		transport.streams.bus.mu.Unlock()
		if count == want {
			return
		}
		if time.Now().After(until) {
			t.Fatalf("listener count=%d, want%d", count, want)
		}
		time.Sleep(time.Millisecond)
	}
}
func eventForListener(request, session, text string) Event {
	return Event{Name: EventSpeak, Data: Data{"utterance": text}, Context: ContextWithCorrelation(nil, session, "", "", request)}
}
func TestWaitForEventConcurrentCorrelationAndListenerIsolation(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	client := &Client{Transport: transport}
	observer, err := client.Listen(context.Background(), EventSpeak, ListenOptions{EventOptions: EventOptions{Timeout: time.Second, SessionID: "hub-session", Predicate: func(event Event) bool { return event.Text() != "skip" }}, MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	replies := make(chan Event, 2)
	errs := make(chan error, 2)
	for _, request := range []string{"one", "two"} {
		go func(request string) {
			event, err := client.WaitForEvent(context.Background(), EventSpeak, EventOptions{Timeout: time.Second, Context: Context{"request_id": "overridden"}, RequestID: request, SessionID: "caller-session", Predicate: func(event Event) bool { return event.Text() == request }})
			replies <- event
			errs <- err
		}(request)
	}
	awaitListenerCount(t, transport, 3)
	transport.streams.bus.publish(eventForListener("foreign", "caller-session", "foreign"))
	transport.streams.bus.publish(eventForListener("two", "hub-session", "skip"))
	transport.streams.bus.publish(eventForListener("one", "hub-session", "one"))
	transport.streams.bus.publish(eventForListener("two", "hub-session", "two"))
	found := map[string]bool{}
	for i := 0; i < 2; i++ {
		event := <-replies
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		found[event.Text()] = true
	}
	if !found["one"] || !found["two"] {
		t.Fatal("concurrent waiters lost correlated events", found)
	}
	observed := []string{}
	for event := range observer.C {
		observed = append(observed, event.Text())
	}
	if observer.Err() != nil || len(observed) != 2 || observed[0] != "one" || observed[1] != "two" {
		t.Fatal(observed, observer.Err())
	}
	observer.Close()
	awaitListenerCount(t, transport, 0)
}
func TestListenKeepsLegacyIDlessSessionFallback(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	stream, err := (&Client{Transport: transport}).Listen(context.Background(), EventSpeak, ListenOptions{EventOptions: EventOptions{Timeout: time.Second, RequestID: "requested", SessionID: "expected"}, MaxEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	transport.streams.bus.publish(eventForListener("", "foreign", "wrong session"))
	transport.streams.bus.publish(eventForListener("foreign", "expected", "wrong request"))
	transport.streams.bus.publish(eventForListener("", "expected", "legacy"))
	event, open := <-stream.C
	if !open || event.Text() != "legacy" {
		t.Fatal(event, stream.Err())
	}
	if _, open := <-stream.C; open || stream.Err() != nil {
		t.Fatal("max events did not close successfully", stream.Err())
	}
	stream.Close()
	awaitListenerCount(t, transport, 0)
}
func TestListenCancellationUnsubscribesWhilePredicateIsBlocked(t *testing.T) {
	for _, explicitClose := range []bool{false, true} {
		transport := newBlockedClientTransport()
		transport.ready.Store(true)
		entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		defer releaseOnce()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stream, err := (&Client{Transport: transport}).Listen(ctx, EventSpeak, ListenOptions{EventOptions: EventOptions{Predicate: func(Event) bool { close(entered); <-release; close(returned); return true }}})
		if err != nil {
			t.Fatal(err)
		}
		transport.streams.bus.publish(eventForListener("", "", "pending"))
		awaitSignal(t, entered)
		if explicitClose {
			stream.Close()
		} else {
			cancel()
		}
		select {
		case _, open := <-stream.C:
			if open {
				t.Fatal("cancelled listener delivered pending predicate result")
			}
		case <-time.After(time.Second):
			t.Fatal("predicate defeated cancellation")
		}
		if explicitClose && stream.Err() != nil {
			t.Fatal(stream.Err())
		}
		if !explicitClose && !errors.Is(stream.Err(), context.Canceled) {
			t.Fatal(stream.Err())
		}
		stream.Close()
		awaitListenerCount(t, transport, 0)
		releaseOnce()
		awaitSignal(t, returned)
	}
}
func TestListenOverflowDoesNotAffectIndependentObserver(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	client := &Client{Transport: transport}
	slow, err := client.Listen(context.Background(), EventSpeak, ListenOptions{EventOptions: EventOptions{Timeout: time.Second}, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	fast, err := client.Listen(context.Background(), EventSpeak, ListenOptions{EventOptions: EventOptions{Timeout: time.Second}, MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	transport.streams.bus.publish(eventForListener("", "", "one"))
	until := time.Now().Add(time.Second)
	for len(slow.C) != 1 {
		if time.Now().After(until) {
			t.Fatal("first event not delivered")
		}
		time.Sleep(time.Millisecond)
	}
	transport.streams.bus.publish(eventForListener("", "", "two"))
	count := 0
	for range fast.C {
		count++
	}
	if count != 2 || fast.Err() != nil {
		t.Fatal("independent observer failed", count, fast.Err())
	}
	// Let the slow output fill before draining it, so the second send overflows.
	until = time.Now().Add(time.Second)
	for slow.Err() == nil {
		if time.Now().After(until) {
			t.Fatal("output overflow was silent")
		}
		time.Sleep(time.Millisecond)
	}
	for range slow.C {
	}
	if !errors.Is(slow.Err(), ErrEventOverflow) {
		t.Fatal(slow.Err())
	}
	slow.Close()
	fast.Close()
	awaitListenerCount(t, transport, 0)
}
func TestListenSourceOverflowClosesEvenWhenPredicateBlocks(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	stream, err := (&Client{Transport: transport}).Listen(context.Background(), EventSpeak, ListenOptions{Capacity: 1, EventOptions: EventOptions{Predicate: func(Event) bool { close(entered); <-release; return true }}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	transport.streams.bus.publish(eventForListener("", "", "one"))
	awaitSignal(t, entered)
	transport.streams.bus.publish(eventForListener("", "", "two"))
	transport.streams.bus.publish(eventForListener("", "", "three"))
	select {
	case _, open := <-stream.C:
		if open {
			t.Fatal("blocked predicate unexpectedly delivered")
		}
	case <-time.After(time.Second):
		t.Fatal("source overflow hidden behind predicate")
	}
	if !errors.Is(stream.Err(), ErrEventOverflow) {
		t.Fatal(stream.Err())
	}
	stream.Close()
	awaitListenerCount(t, transport, 0)
}
func TestListenerTimeoutAndDisconnectAreExplicit(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	client := &Client{Transport: transport}
	if _, err := client.WaitForEvent(context.Background(), EventSpeak, EventOptions{Timeout: 20 * time.Millisecond}); !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	awaitListenerCount(t, transport, 0)
	stream, err := client.Listen(context.Background(), EventSpeak, ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	transport.ready.Store(false)
	select {
	case _, open := <-stream.C:
		if open {
			t.Fatal("disconnected stream stayed open")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect was not reported")
	}
	if !errors.Is(stream.Err(), ErrConnection) {
		t.Fatal(stream.Err())
	}
	stream.Close()
	awaitListenerCount(t, transport, 0)
}
func TestWaitForEventDeadlineIncludesConnectAndRejectsInvalidOptions(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.connectStarted = make(chan struct{}, 1)
	transport.connectRelease = make(chan struct{})
	release := sync.OnceFunc(func() { close(transport.connectRelease) })
	defer release()
	client := &Client{Transport: transport, ConnectTimeout: time.Second}
	started := time.Now()
	if _, err := client.WaitForEvent(context.Background(), EventSpeak, EventOptions{Timeout: 20 * time.Millisecond}); !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("connect exceeded event deadline")
	}
	awaitListenerCount(t, transport, 0)
	release()
	awaitCleanup(t, transport)
	for _, options := range []ListenOptions{{MaxEvents: -1}, {Capacity: -1}, {EventOptions: EventOptions{Timeout: -1}}} {
		before := transport.connects.Load()
		if _, err := client.Listen(context.Background(), EventSpeak, options); err == nil {
			t.Fatal("invalid listener options accepted")
		}
		if transport.connects.Load() != before {
			t.Fatal("invalid options contacted transport")
		}
	}
}

type earlyEventTransport struct{ *blockedClientTransport }

func (t *earlyEventTransport) Connect(context.Context) error {
	t.ready.Store(true)
	t.streams.bus.publish(eventForListener("early", "hub-session", "authenticated early reply"))
	return nil
}
func TestWaitForEventRetainsAuthenticatedEventDuringConnect(t *testing.T) {
	transport := &earlyEventTransport{blockedClientTransport: newBlockedClientTransport()}
	event, err := (&Client{Transport: transport}).WaitForEvent(context.Background(), EventSpeak, EventOptions{Timeout: time.Second, RequestID: "early"})
	if err != nil || event.Text() != "authenticated early reply" {
		t.Fatal("early event was lost", event, err)
	}
	awaitListenerCount(t, transport.blockedClientTransport, 0)
}
