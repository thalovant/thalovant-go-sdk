package thalovant

import (
	"errors"
	"sync"
)

// ErrEventOverflow means a subscriber did not keep up. The subscription is
// closed instead of silently losing replies or blocking the Noise reader.
var ErrEventOverflow = errors.New("runtime event subscription overflow")

// Subscription owns an independent, bounded stream. Read until C closes,
// then inspect Err; call Close when done. Events and their maps are read-only.
type Subscription[T any] struct {
	C    <-chan T
	mu   sync.Mutex
	err  error
	stop func()
}

func (s *Subscription[T]) Err() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *Subscription[T]) Close() {
	if s.stop != nil {
		s.stop()
	}
}

type streamEntry[T any] struct {
	queue        chan T
	subscription *Subscription[T]
}
type eventStream[T any] struct {
	mu      sync.Mutex
	entries map[*Subscription[T]]streamEntry[T]
}

func (b *eventStream[T]) subscribe(capacity int) *Subscription[T] {
	if capacity <= 0 {
		capacity = 256
	}
	if capacity > 65536 {
		capacity = 65536
	}
	queue := make(chan T, capacity)
	sub := &Subscription[T]{C: queue}
	sub.stop = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, exists := b.entries[sub]; exists {
			delete(b.entries, sub)
			close(queue)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = make(map[*Subscription[T]]streamEntry[T])
	}
	b.entries[sub] = streamEntry[T]{queue, sub}
	return sub
}

func (b *eventStream[T]) publish(value T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub, entry := range b.entries {
		select {
		case entry.queue <- value:
		default:
			sub.mu.Lock()
			sub.err = ErrEventOverflow
			sub.mu.Unlock()
			delete(b.entries, sub)
			close(entry.queue)
		}
	}
}

type runtimeStreams struct {
	bus  eventStream[Event]
	hive eventStream[HiveMessage]
}

// EventSubscriber is optional for custom transports, preserving RuntimeTransport.
// Built-in transports implement it. Custom transports should implement it when
// concurrent calls or independent passive subscribers are required.
type EventSubscriber interface {
	SubscribeEvents(capacity int) *Subscription[Event]
}
type HiveMessageSubscriber interface {
	SubscribeHiveMessages(capacity int) *Subscription[HiveMessage]
}

func (t *HTTPTransport) SubscribeEvents(capacity int) *Subscription[Event] {
	return t.streams.bus.subscribe(capacity)
}
func (t *HTTPTransport) SubscribeHiveMessages(capacity int) *Subscription[HiveMessage] {
	return t.streams.hive.subscribe(capacity)
}
func (t *MQTTTransport) SubscribeEvents(capacity int) *Subscription[Event] {
	return t.streams.bus.subscribe(capacity)
}
func (t *MQTTTransport) SubscribeHiveMessages(capacity int) *Subscription[HiveMessage] {
	return t.streams.hive.subscribe(capacity)
}
func (t *WSSTransport) SubscribeEvents(capacity int) *Subscription[Event] {
	return t.streams.bus.subscribe(capacity)
}
func (t *WSSTransport) SubscribeHiveMessages(capacity int) *Subscription[HiveMessage] {
	return t.streams.hive.subscribe(capacity)
}

func (c *Client) SubscribeEvents(capacity int) *Subscription[Event] {
	if source, ok := c.Transport.(EventSubscriber); ok {
		return source.SubscribeEvents(capacity)
	}
	return &Subscription[Event]{C: c.Transport.Events()}
}

func subscribeHiveMessages(transport hiveMessageTransport) *Subscription[HiveMessage] {
	if source, ok := transport.(HiveMessageSubscriber); ok {
		return source.SubscribeHiveMessages(256)
	}
	return &Subscription[HiveMessage]{C: transport.HiveMessages()}
}

func subscriptionError(err error) error {
	if err != nil {
		return err
	}
	return ErrConnection
}
