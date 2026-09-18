package thalovant

// What an ask does when the hub refuses it, against the vectors every SDK
// shares: contracts/conformance/refusal-vectors.json in the Python SDK,
// vendored here unchanged and pinned by the parity contract. A refusal becomes
// a typed error carrying the hub's code and, for a spent quota, its numbers;
// an unmatched intent is an unanswered question; and a denial with no request
// id is taken only by the ask that can be the one it refused.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type refusalVectors struct {
	Classification []struct {
		Name  string `json:"name"`
		Event struct {
			Type    string         `json:"type"`
			Data    map[string]any `json:"data"`
			Context map[string]any `json:"context"`
		} `json:"event"`
		Expect map[string]any `json:"expect"`
	} `json:"classification"`
	Correlation []struct {
		Name            string  `json:"name"`
		AsksInFlight    int     `json:"asks_in_flight"`
		QueriesInFlight int     `json:"queries_in_flight"`
		DeniedType      string  `json:"denied_type"`
		RequestID       *string `json:"request_id"`
		Taken           bool    `json:"taken"`
	} `json:"correlation"`
}

func loadRefusalVectors(t *testing.T, name string) refusalVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var vectors refusalVectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

func TestRefusalClassificationMatchesTheSharedVectors(t *testing.T) {
	for _, vector := range loadRefusalVectors(t, "refusal-vectors.json").Classification {
		t.Run(vector.Name, func(t *testing.T) {
			// Decoded into map[string]any as a transport does, so every
			// number arrives as float64 -- the trap wholeCount exists for.
			err := failureError(Event{Name: vector.Event.Type, Data: Data(vector.Event.Data), Context: Context(vector.Event.Context)})
			if !errors.Is(err, ErrRuntime) {
				t.Fatalf("every refusal still matches ErrRuntime: %v", err)
			}
			if vector.Expect["kind"] == "unanswered" {
				var unanswered *UnansweredError
				if !errors.As(err, &unanswered) {
					t.Fatalf("want UnansweredError, got %T %v", err, err)
				}
				return
			}
			var refused *PolicyDeniedError
			if !errors.As(err, &refused) {
				t.Fatalf("want PolicyDeniedError, got %T %v", err, err)
			}
			allowed := []any{}
			for _, entry := range refused.Allowed {
				allowed = append(allowed, entry)
			}
			var quota any
			if q := refused.Quota; q != nil {
				quota = map[string]any{"period": q.Period, "limit": float64(q.Limit), "used": float64(q.Used), "reset_after": float64(q.ResetAfter)}
			}
			produced := map[string]any{
				"kind": "refused", "denied_type": refused.DeniedType, "code": refused.Code,
				"reason": refused.Reason, "allowed": allowed, "quota": quota,
			}
			if !reflect.DeepEqual(produced, vector.Expect) {
				t.Fatalf("\nproduced %#v\nexpected %#v", produced, vector.Expect)
			}
		})
	}
}

func TestRefusalCorrelationMatchesTheSharedVectors(t *testing.T) {
	for _, vector := range loadRefusalVectors(t, "refusal-vectors.json").Correlation {
		t.Run(vector.Name, func(t *testing.T) {
			requestID := ""
			if vector.RequestID != nil {
				requestID = map[string]string{"own": "req-own", "other": "req-other"}[*vector.RequestID]
			}
			got := refusalBelongsToAsk(requestID, "req-own", vector.DeniedType, vector.AsksInFlight, vector.QueriesInFlight)
			if got != vector.Taken {
				t.Fatalf("taken = %v, want %v", got, vector.Taken)
			}
		})
	}
}

func TestRefusalVectorsCoverEveryKindOfRefusal(t *testing.T) {
	// A copy that quietly lost its quota or its unanswered case would still pass.
	vectors := loadRefusalVectors(t, "refusal-vectors.json")
	codes, kinds, taken := map[any]bool{}, map[any]bool{}, map[bool]bool{}
	for _, vector := range vectors.Classification {
		codes[vector.Expect["code"]] = true
		kinds[vector.Expect["kind"]] = true
	}
	for _, vector := range vectors.Correlation {
		taken[vector.Taken] = true
	}
	for _, code := range []string{PolicyCodeACL, PolicyCodeQuotaExceeded, PolicyCodeBackendUnavailable} {
		if !codes[code] {
			t.Fatalf("no vector for %s", code)
		}
	}
	if len(kinds) != 2 || !kinds["refused"] || !kinds["unanswered"] || len(taken) != 2 {
		t.Fatalf("vectors lost a kind: %v %v", kinds, taken)
	}
}

var quotaDenial = Data{
	"denied_type": EventRecognizerLoopUtterance, "code": PolicyCodeQuotaExceeded, "reason": "daily intent quota exceeded",
	"data": map[string]any{"period": "daily", "limit": float64(50), "used": float64(50), "reset_after": float64(36120)},
}

func TestAnUncorrelatedQuotaRefusalEndsTheAskAtOnce(t *testing.T) {
	// The production shape: denied at once, no request id, the numbers nested.
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	errs := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := (&Client{Transport: transport}).AskWithOptions(context.Background(), "what time is it", AskOptions{RequestOptions: RequestOptions{Timeout: 5 * time.Second}})
		errs <- err
	}()
	<-transport.emitted
	transport.streams.bus.publish(Event{Name: EventPolicyDenied, Data: quotaDenial, Context: Context{"source": "hivemind-core"}})
	err := <-errs
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("waited out the deadline (%v) instead of taking the refusal", elapsed)
	}
	var refused *PolicyDeniedError
	if !errors.As(err, &refused) || refused.Quota == nil {
		t.Fatalf("want a quota refusal, got %T %v", err, err)
	}
	if *refused.Quota != (Quota{Period: "daily", Limit: 50, Used: 50, ResetAfter: 36120}) {
		t.Fatalf("quota lost its numbers: %+v", *refused.Quota)
	}
	if strings.Contains(err.Error(), "dashboard") {
		t.Fatal("a spent day is not an allow-list to edit")
	}
}

func TestAnUnmatchedIntentIsUnansweredNotAFailure(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	errs := make(chan error, 1)
	go func() {
		_, err := (&Client{Transport: transport}).AskWithOptions(context.Background(), "book me a flight to the moon", AskOptions{RequestOptions: RequestOptions{Timeout: time.Second}, EmptyReplyWait: 50 * time.Millisecond})
		errs <- err
	}()
	q := <-transport.emitted
	transport.streams.bus.publish(Event{Name: EventIntentUnmatched, Data: Data{"utterance": "book me a flight to the moon"}, Context: q.context})
	var unanswered *UnansweredError
	if err := <-errs; !errors.As(err, &unanswered) {
		t.Fatalf("want UnansweredError, got %T %v", err, err)
	}
}

func TestWithTwoAsksInFlightAnUncorrelatedDenialFailsNeither(t *testing.T) {
	// Either could be the one refused; ending the wrong one fails a question
	// the hub never refused, so both are left to their deadlines.
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	client := &Client{Transport: transport}
	errs := make(chan error, 2)
	for _, prompt := range []string{"first", "second"} {
		go func() {
			_, err := client.AskWithOptions(context.Background(), prompt, AskOptions{RequestOptions: RequestOptions{Timeout: 300 * time.Millisecond}})
			errs <- err
		}()
		<-transport.emitted
	}
	transport.streams.bus.publish(Event{Name: EventPolicyDenied, Data: quotaDenial, Context: Context{"source": "hivemind-core"}})
	for range 2 {
		if err := <-errs; !errors.Is(err, ErrTimeout) {
			t.Fatalf("an ask took a denial that could have been the other's: %T %v", err, err)
		}
	}
}
