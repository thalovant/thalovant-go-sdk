package thalovant

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// EventOptions scopes an event waiter or listener. A matching request ID takes
// precedence over a hub-assigned session ID; ID-less replies retain the shared
// legacy session fallback. Predicate runs after event-name and context filtering.
type EventOptions struct {
	Timeout   time.Duration
	Context   Context
	SessionID string
	RequestID string
	Predicate func(Event) bool
}

// ListenOptions bounds a listener's duration, event count and buffered backlog.
// Zero Timeout and MaxEvents leave lifetime/count to the caller's context and
// Close. Capacity defaults to 256 and is capped at 65536. Predicates should return
// promptly; cancellation unsubscribes immediately even if a predicate is pending.
type ListenOptions struct {
	EventOptions
	MaxEvents int
	Capacity  int
}

// WaitForEvent connects and waits for one matching event within one deadline.
// The default deadline is 12 seconds, including connection establishment.
func (c *Client) WaitForEvent(ctx context.Context, eventName string, options EventOptions) (Event, error) {
	if options.Timeout == 0 {
		options.Timeout = 12 * time.Second
	}
	stream, err := c.Listen(ctx, eventName, ListenOptions{EventOptions: options, MaxEvents: 1})
	if err != nil {
		return Event{}, err
	}
	defer stream.Close()
	event, open := <-stream.C
	if !open {
		return Event{}, subscriptionError(stream.Err())
	}
	return event, nil
}

// Listen connects and returns an independent filtered stream. Range over C and
// inspect Err afterward; reaching MaxEvents or calling Close is successful.
// Timeout, caller cancellation, disconnect and overflow close the stream with
// an explicit error. Legacy custom transports need EventSubscriber for isolation.
func (c *Client) Listen(ctx context.Context, eventName string, options ListenOptions) (*Subscription[Event], error) {
	eventName = strings.TrimSpace(eventName)
	if eventName == "" {
		return nil, fmt.Errorf("listen requires a non-empty event name")
	}
	if options.Timeout < 0 || options.MaxEvents < 0 || options.Capacity < 0 {
		return nil, fmt.Errorf("listen timeout, max events and capacity must not be negative")
	}
	var cancel context.CancelFunc
	if options.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, options.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	capacity := options.Capacity
	if capacity == 0 {
		capacity = 256
	}
	if capacity > 65536 {
		capacity = 65536
	}
	expected := ContextWithCorrelation(options.Context, options.SessionID, "", "", options.RequestID)
	source := c.SubscribeEvents(capacity)
	// Retain authenticated events emitted immediately before Connect returns.
	if err := c.Connect(ctx); err != nil {
		source.Close()
		cancel()
		return nil, err
	}
	output := make(chan Event, capacity)
	stream := &Subscription[Event]{C: output}
	done := make(chan struct{})
	var completed sync.Once
	finish := func(err error) {
		completed.Do(func() {
			stream.mu.Lock()
			stream.err = err
			close(output)
			close(done)
			stream.mu.Unlock()
			source.Close()
			cancel()
		})
	}
	stream.stop = func() { finish(nil) }
	// Cancellation and source overflow remain observable even while a custom
	// predicate or health getter is still running on the collection worker.
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				finish(fmt.Errorf("%w: %w", ErrTimeout, ctx.Err()))
				return
			case <-ticker.C:
				if err := source.Err(); err != nil {
					finish(err)
					return
				}
			}
		}
	}()
	go func() {
		defer func() {
			if recover() != nil {
				finish(fmt.Errorf("%w: event predicate or transport callback panicked", ErrRuntime))
			}
		}()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		delivered := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				health := c.Transport.Healthcheck()
				if !health.Connected || !health.HandshakeComplete {
					finish(ErrConnection)
					return
				}
			case event, open := <-source.C:
				if !open {
					finish(subscriptionError(source.Err()))
					return
				}
				if event.Name != eventName || !EventMatchesContext(event, expected) {
					continue
				}
				if options.Predicate != nil && !options.Predicate(event) {
					continue
				}
				stream.mu.Lock()
				if err := ctx.Err(); err != nil {
					stream.mu.Unlock()
					finish(fmt.Errorf("%w: %w", ErrTimeout, err))
					return
				}
				select {
				case <-done:
					stream.mu.Unlock()
					return
				default:
				}
				select {
				case output <- event:
					stream.mu.Unlock()
				default:
					stream.mu.Unlock()
					finish(ErrEventOverflow)
					return
				}
				delivered++
				if options.MaxEvents > 0 && delivered >= options.MaxEvents {
					finish(nil)
					return
				}
			}
		}
	}()
	return stream, nil
}
