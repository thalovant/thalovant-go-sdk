package thalovant

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type emptyDescribeReviewHub struct {
	*intentHub
	refused bool
	silence bool
}

func (h *emptyDescribeReviewHub) EmitBus(ctx context.Context, name string, data Data, eventContext Context) error {
	if name != EventIntentDescribe {
		return h.intentHub.EmitBus(ctx, name, data, eventContext)
	}
	if h.silence && data["intent_name"] == "second" {
		return nil
	}
	response := Data{"ok": true, "definitions": []any{}}
	if h.refused {
		response = Data{"ok": false, "error": "unknown intent"}
	}
	h.deliver(EventIntentDescribeResponse, response, eventContext)
	return nil
}

func TestDescribeEmptyAnswersDoNotHideUnansweredRegistrations(t *testing.T) {
	wanted := []intentKey{{"skill", "first", "en-us"}, {"skill", "second", "en-us"}}
	for _, refused := range []bool{false, true} {
		for _, batch := range []int{1, 2} {
			hub := &emptyDescribeReviewHub{intentHub: newIntentHub(), refused: refused, silence: true}
			_, err := (&Client{Transport: hub}).describeMany(context.Background(), wanted, 20*time.Millisecond, batch)
			if !errors.Is(err, ErrTimeout) {
				t.Errorf("refused=%v batch=%d: wanted timeout without a usable definition, got %v", refused, batch, err)
			}
			hub = &emptyDescribeReviewHub{intentHub: newIntentHub(), refused: refused}
			found, err := (&Client{Transport: hub}).describeMany(context.Background(), wanted, time.Second, batch)
			if err != nil || len(found) != 2 {
				t.Errorf("explicit answers for both registrations remain valid: got %v, %v", found, err)
			}
		}
	}
}

func TestEmptyHubETagFailsBeforeAnyRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	control := NewControlPlane(server.URL, "token")
	for _, etag := range []string{"", " \t "} {
		_, err := control.UpdateHub(context.Background(), "hub", nil, etag)
		if !errors.Is(err, ErrAPI) {
			t.Errorf("update must reject missing etag: %v", err)
		}
		if err := control.DeleteHub(context.Background(), "hub", etag); !errors.Is(err, ErrAPI) {
			t.Errorf("delete must reject missing etag: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid etags sent %d requests", calls.Load())
	}
}

func TestActiveReplyIDsRejectDuplicatesWithoutPublishing(t *testing.T) {
	for _, query := range []bool{false, true} {
		t.Run(map[bool]string{false: "ask", true: "query"}[query], func(t *testing.T) {
			transport := &queryDispatchTransport{blockedClientTransport: newBlockedClientTransport(), sent: make(chan HiveMessage, 8)}
			transport.ready.Store(true)
			client := &Client{Transport: transport}
			invoke := func(ctx context.Context, requestID, queryID string) error {
				if query {
					_, err := client.Query(ctx, "question", QueryOptions{Timeout: time.Second, RequestID: requestID, QueryID: queryID})
					return err
				}
				_, err := client.Ask(ctx, "question", RequestOptions{Timeout: time.Second, RequestID: requestID})
				return err
			}
			started := func() {
				t.Helper()
				if query {
					select {
					case <-transport.sent:
					case <-time.After(time.Second):
						t.Fatal("query never published")
					}
				} else {
					select {
					case <-transport.emitted:
					case <-time.After(time.Second):
						t.Fatal("ask never published")
					}
				}
			}
			firstCtx, firstCancel := context.WithCancel(context.Background())
			defer firstCancel()
			first := make(chan error, 1)
			go func() { first <- invoke(firstCtx, "shared", "query-shared") }()
			started()
			duplicateCtx, duplicateCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			duplicateRequestID := "shared"
			if query {
				duplicateRequestID = "another-request"
			}
			err := invoke(duplicateCtx, duplicateRequestID, "query-shared")
			duplicateCancel()
			if !errors.Is(err, ErrRuntime) {
				t.Errorf("duplicate must fail locally with ErrRuntime, got %v", err)
			}
			if len(transport.sent) != 0 || len(transport.emitted) != 0 {
				t.Errorf("duplicate request reached the transport")
			}
			firstCancel()
			<-first
			// Cancellation releases the collector's ID; it does not reserve it forever.
			nextCtx, nextCancel := context.WithCancel(context.Background())
			next := make(chan error, 1)
			go func() { next <- invoke(nextCtx, "shared", "query-shared") }()
			started()
			nextCancel()
			<-next
		})
	}
}

func TestAskAndQueryCorrelationNamespacesAreIndependent(t *testing.T) {
	transport := &queryDispatchTransport{blockedClientTransport: newBlockedClientTransport(), sent: make(chan HiveMessage, 2)}
	transport.ready.Store(true)
	client := &Client{Transport: transport}
	askCtx, cancelAsk := context.WithCancel(context.Background())
	defer cancelAsk()
	askDone := make(chan error, 1)
	go func() {
		_, err := client.Ask(askCtx, "ask", RequestOptions{RequestID: "same", Timeout: time.Second})
		askDone <- err
	}()
	select {
	case <-transport.emitted:
	case <-time.After(time.Second):
		t.Fatal("ask did not publish")
	}
	queryCtx, cancelQuery := context.WithCancel(context.Background())
	defer cancelQuery()
	queryDone := make(chan error, 1)
	go func() {
		_, err := client.Query(queryCtx, "query", QueryOptions{QueryID: "same", Timeout: time.Second})
		queryDone <- err
	}()
	select {
	case <-transport.sent:
	case err := <-queryDone:
		t.Fatalf("query namespace collided with ask: %v", err)
	case <-time.After(time.Second):
		t.Fatal("query did not publish")
	}
	cancelAsk()
	<-askDone
	_, err := client.Query(context.Background(), "duplicate", QueryOptions{QueryID: "same", Timeout: 20 * time.Millisecond})
	if !errors.Is(err, ErrRuntime) {
		t.Fatalf("disposing Ask must not free Query reservation: %v", err)
	}
	cancelQuery()
	<-queryDone
}
