package thalovant

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

type dispatchTransport struct {
	*WSSTransport
	emitted chan emittedQuery
}

func (t *dispatchTransport) Connect(context.Context) error { return nil }
func (t *dispatchTransport) Healthcheck() TransportHealth {
	return TransportHealth{Connected: true, HandshakeComplete: true}
}
func (t *dispatchTransport) EmitBus(_ context.Context, name string, data Data, ctx Context) error {
	t.emitted <- emittedQuery{eventType: name, data: data, context: ctx}
	return nil
}

func TestConcurrentAskAndInventoryKeepIndependentReplies(t *testing.T) {
	transport := &dispatchTransport{WSSTransport: NewWSSTransport(Identity{}), emitted: make(chan emittedQuery, 3)}
	client := &Client{Transport: transport}
	passive := client.SubscribeEvents(32)
	defer passive.Close()
	results := make(chan error, 3)
	for _, prompt := range []string{"first", "second"} {
		go func(prompt string) {
			reply, err := client.Ask(context.Background(), prompt, RequestOptions{Timeout: time.Second, RequestID: prompt})
			if err == nil && reply.Text != prompt {
				err = errors.New("reply was stolen or mismatched")
			}
			results <- err
		}(prompt)
	}
	go func() {
		_, err := client.Intents(context.Background(), nil, IntentOptions{Timeout: time.Second, Describe: boolPointer(false)})
		results <- err
	}()
	queries := []emittedQuery{}
	for i := 0; i < 3; i++ {
		select {
		case q := <-transport.emitted:
			queries = append(queries, q)
		case <-time.After(2 * time.Second):
			t.Fatal("requests did not start concurrently")
		}
	}
	publish := func(name string, data Data, ctx Context) {
		dispatchNoiseMessage(transport.BusEvents, transport.HiveEvents, HiveMessage{MsgType: "bus", Payload: map[string]any{"type": name, "data": map[string]any(data), "context": map[string]any(ctx)}}, &transport.streams)
	}
	for _, q := range queries {
		if q.eventType == EventIntentList {
			publish(EventPolicyDenied, Data{"denied_type": EventIntentList}, Context{"request_id": "unrelated"})
			publish(EventIntentListResponse, Data{"ok": true, "intents": []any{}}, q.context)
		} else {
			publish(EventSpeak, Data{"utterance": q.context["request_id"]}, q.context)
		}
	}
	for _, q := range queries {
		if q.eventType != EventIntentList {
			publish(EventUtteranceHandled, Data{}, q.context)
		}
	}
	for i := 0; i < 3; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(passive.C) != 6 {
		t.Fatalf("passive observer missed events: %d", len(passive.C))
	}
}

func TestEventOverflowClosesOnlySlowSubscriber(t *testing.T) {
	transport := NewWSSTransport(Identity{})
	slow := transport.SubscribeEvents(1)
	defer slow.Close()
	fast := transport.SubscribeEvents(4)
	defer fast.Close()
	for i := 0; i < 3; i++ {
		transport.streams.bus.publish(Event{Name: EventSpeak})
	}
	for range slow.C {
	}
	if !errors.Is(slow.Err(), ErrEventOverflow) {
		t.Fatal("overflow was silent", slow.Err())
	}
	if len(fast.C) != 3 || fast.Err() != nil {
		t.Fatal("slow subscriber affected independent observer")
	}
	fast.Close()
	fast.Close()
	transport.streams.bus.publish(Event{})
}

type fallbackHub struct {
	*intentHub
	data Data
}

func (h *fallbackHub) EmitBus(ctx context.Context, name string, data Data, eventContext Context) error {
	if name == EventFallbackList && h.data != nil {
		h.deliver(EventFallbackListResponse, h.data, eventContext)
		return nil
	}
	return h.intentHub.EmitBus(ctx, name, data, eventContext)
}

func TestFallbackCapabilityKnownUnknownAndLanguageSafety(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  Data
		known bool
		count int
	}{
		{"known empty", Data{"fallbacks": []any{}}, true, 0},
		{"missing", Data{}, false, 0},
		{"negative", Data{"ok": false, "fallbacks": []any{}}, false, 0},
		{"handlers", Data{"fallbacks": []any{map[string]any{"skill_id": "z", "priority": 50.5}, map[string]any{"skill_id": "a", "priority": 1}, map[string]any{"skill_id": "bad", "priority": math.Inf(1)}}}, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &fallbackHub{intentHub: newIntentHub(), data: tc.data}
			client := &Client{Transport: hub}
			result, err := client.IntentsWithCapabilities(context.Background(), []string{"de-de"}, IntentOptions{Timeout: 50 * time.Millisecond, Describe: boolPointer(false)})
			if err != nil {
				t.Fatal(err)
			}
			if result.FallbacksKnown != tc.known || len(result.Fallbacks) != tc.count {
				t.Fatalf("unexpected capabilities: %+v", result)
			}
			if result.MayAnswer("de-DE") != (!tc.known || tc.count > 0) {
				t.Fatal("incorrect language inference")
			}
			if tc.count > 0 && result.Fallbacks[0].SkillID != "a" {
				t.Fatal("priority order not stable")
			}
		})
	}
	capabilities := HubIntentCapabilities{FallbacksKnown: true, Inventory: HubIntentInventory{Skills: []HubSkillIntents{{Intents: []HubIntent{{Enabled: false, Phrases: map[string][]string{"fr-FR": {"bonjour"}}}}}}}}
	if capabilities.MayAnswer("fr-fr") {
		t.Fatal("disabled intent implies language coverage")
	}
}

func TestSilentListingUsesEngineFallbackWithoutMaskingCancellation(t *testing.T) {
	hub := newIntentHub()
	hub.silent[EventIntentList] = true
	client := intentClient(hub)
	result, err := client.Intents(context.Background(), nil, IntentOptions{Timeout: 10 * time.Millisecond})
	if err != nil || result.Source != IntentSourceEngines || len(result.Denied) != 1 || result.Denied[0] != EventIntentList {
		t.Fatalf("silent listing fallback: %+v, %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = client.Intents(ctx, nil, IntentOptions{Timeout: 10 * time.Millisecond}); err == nil {
		t.Fatal("caller cancellation was swallowed")
	}
	if _, err = client.ListFallbacks(ctx, 10*time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatal("fallback probe swallowed cancellation", err)
	}
}
