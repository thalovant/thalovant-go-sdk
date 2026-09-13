package thalovant

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

type HubSessionPolicy struct{ Retry, RetryCeiling, Probe, ProbeDown time.Duration }

func DefaultHubSessionPolicy() HubSessionPolicy {
	return HubSessionPolicy{10 * time.Second, 120 * time.Second, 60 * time.Second, 5 * time.Second}
}
func (p HubSessionPolicy) Validate() error {
	if p.Retry <= 0 || p.RetryCeiling < p.Retry || p.Probe <= 0 || p.ProbeDown <= 0 {
		return errors.New("invalid hub session policy")
	}
	return nil
}
func (p HubSessionPolicy) NextWait(current time.Duration) time.Duration {
	if current >= p.RetryCeiling/2 {
		return p.RetryCeiling
	}
	return current * 2
}

type HubSessionClient interface {
	AskWithOptions(context.Context, string, AskOptions) (Reply, error)
	Emit(context.Context, string, Data, Context) error
	Close(context.Context) error
	ConnectionInfo() TransportConnectionInfo
	SubscribeEvents(int) *Subscription[Event]
}

func Alive(client HubSessionClient) bool {
	return client != nil && client.ConnectionInfo().Phase != ConnectionClosed && client.ConnectionInfo().Phase != ConnectionError
}

// HubSession owns one connection. Hosts call Probe at ProbeDelay intervals.
// Warm is asynchronous; foreground calls bypass the unattended retry ladder.
// No admitted Ask or Emit is replayed after an ambiguous transport failure.
type HubSession struct {
	policy          HubSessionPolicy
	connect         func(context.Context) (HubSessionClient, error)
	clock           func() time.Time
	gate            chan struct{}
	mu              sync.Mutex
	client, retired HubSessionClient
	closed, warming bool
	retryAt         time.Time
	retryWait       time.Duration
	generation      uint64
	relay           *Subscription[Event]
	relayStop       chan struct{}
	events          eventStream[Event]
}

func NewHubSession(connect func(context.Context) (HubSessionClient, error), policy HubSessionPolicy) (*HubSession, error) {
	if connect == nil {
		return nil, errors.New("hub session requires a client factory")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &HubSession{connect: connect, policy: policy, clock: time.Now, gate: make(chan struct{}, 1), retryWait: policy.Retry}, nil
}
func (s *HubSession) Held() bool               { s.mu.Lock(); defer s.mu.Unlock(); return s.client != nil }
func (s *HubSession) RetryAt() time.Time       { s.mu.Lock(); defer s.mu.Unlock(); return s.retryAt }
func (s *HubSession) RetryWait() time.Duration { s.mu.Lock(); defer s.mu.Unlock(); return s.retryWait }
func (s *HubSession) ProbeDelay() time.Duration {
	if s.Held() {
		return s.policy.Probe
	}
	return s.policy.ProbeDown
}
func (s *HubSession) acquire(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.gate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *HubSession) release() { <-s.gate }

// A failed close may still own a live transport. Retain it and block a new
// connection until cleanup succeeds; callers can retry Close with a fresh context.
func (s *HubSession) cleanup(ctx context.Context) error {
	if s.retired != nil {
		if err := s.retired.Close(ctx); err != nil {
			return err
		}
		s.retired = nil
	}
	return nil
}
func (s *HubSession) drop(ctx context.Context) error {
	s.mu.Lock()
	if s.client != nil {
		s.retired = s.client
		s.client = nil
		s.generation++
	}
	if s.relayStop != nil {
		close(s.relayStop)
		s.relayStop = nil
	}
	if s.relay != nil {
		s.relay.Close()
		s.relay = nil
	}
	s.mu.Unlock()
	return s.cleanup(ctx)
}
func (s *HubSession) ensure(ctx context.Context) (HubSessionClient, error) {
	s.mu.Lock()
	closed, client := s.closed, s.client
	s.mu.Unlock()
	if closed {
		return nil, ErrConnection
	}
	if err := s.cleanup(ctx); err != nil {
		return nil, err
	}
	if client != nil {
		return client, nil
	}
	fresh, err := s.connect(ctx)
	if err == nil && fresh == nil {
		err = errors.New("hub factory returned no client")
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	s.mu.Lock()
	if err == nil && s.closed {
		err = ErrConnection
	}
	if err != nil {
		s.retryAt = s.clock().Add(s.retryWait)
		s.retryWait = s.policy.NextWait(s.retryWait)
		s.mu.Unlock()
		if fresh != nil {
			s.retired = fresh
			err = errors.Join(err, s.cleanup(ctx))
		}
		return nil, err
	}
	s.client = fresh
	s.retryAt = time.Time{}
	s.retryWait = s.policy.Retry
	s.generation++
	generation := s.generation
	sub := fresh.SubscribeEvents(256)
	s.relay = sub
	stop := make(chan struct{})
	s.relayStop = stop
	s.mu.Unlock()
	go func() {
		for {
			select {
			case <-stop:
				return
			case event, ok := <-sub.C:
				if !ok {
					s.mu.Lock()
					if s.generation == generation && !s.closed {
						s.failEvents(subscriptionError(sub.Err()))
					}
					s.mu.Unlock()
					return
				}
				s.mu.Lock()
				if s.generation == generation && !s.closed {
					s.events.publish(event)
				}
				s.mu.Unlock()
			}
		}
	}()
	return fresh, nil
}
func (s *HubSession) SubscribeEvents(capacity int) *Subscription[Event] {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.events.subscribe(capacity)
	if s.closed {
		s.failEvents(ErrConnection)
	}
	return sub
}
func (s *HubSession) failEvents(err error) {
	s.events.mu.Lock()
	defer s.events.mu.Unlock()
	for sub, entry := range s.events.entries {
		sub.mu.Lock()
		sub.err = err
		sub.mu.Unlock()
		delete(s.events.entries, sub)
		close(entry.queue)
	}
}
func (s *HubSession) Warm(ctx context.Context) bool {
	s.mu.Lock()
	if s.closed || s.warming || s.clock().Before(s.retryAt) {
		s.mu.Unlock()
		return false
	}
	s.warming = true
	s.mu.Unlock()
	go func() {
		defer func() { s.mu.Lock(); s.warming = false; s.mu.Unlock() }()
		if s.acquire(ctx) != nil {
			return
		}
		defer s.release()
		_, _ = s.ensure(ctx)
	}()
	return true
}
func (s *HubSession) Probe(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
	default:
		return nil
	}
	s.mu.Lock()
	client, closed := s.client, s.closed
	s.mu.Unlock()
	if closed {
		s.release()
		return nil
	}
	var err error
	if client != nil && !Alive(client) {
		err = s.drop(ctx)
	}
	s.release()
	if err == nil && !s.Held() {
		s.Warm(ctx)
	}
	return err
}
func (s *HubSession) call(ctx context.Context, operation func(HubSessionClient) error) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil && !Alive(client) {
		if err := s.drop(ctx); err != nil {
			return err
		}
	}
	client, err := s.ensure(ctx)
	if err != nil {
		return err
	}
	err = operation(client)
	if err != nil && !errors.Is(err, ErrRuntime) {
		err = errors.Join(err, s.drop(ctx))
	}
	return err
}
func (s *HubSession) Ask(ctx context.Context, text string, options AskOptions) (reply Reply, err error) {
	err = s.call(ctx, func(c HubSessionClient) error { var e error; reply, e = c.AskWithOptions(ctx, text, options); return e })
	return
}
func (s *HubSession) Emit(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return s.call(ctx, func(c HubSessionClient) error { return c.Emit(ctx, eventType, data, eventContext) })
}

// Close retires the session permanently and waits for its admitted call.
func (s *HubSession) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.failEvents(ErrConnection)
	s.mu.Unlock()
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	return s.drop(ctx)
}
func HubHostname(master string) string {
	master = strings.TrimSpace(master)
	if master == "" {
		return ""
	}
	if !strings.Contains(master, "://") {
		master = "wss://" + master
	}
	parsed, err := url.Parse(master)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// OriginAttempt leaves the URL host, certificate validation and SNI unchanged.
// The factory owns a transport-local dial override and cleans up failed attempts.
type OriginAttempt struct {
	Address, Host                    string
	HandshakeTimeout, ConnectTimeout time.Duration
}
type OriginPreference struct {
	Address                    string
	HandshakeTimeout, Cooldown time.Duration
	mu                         sync.Mutex
	quietUntil                 time.Time
}

func (o *OriginPreference) CoolingDown() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return time.Now().Before(o.quietUntil)
}
func (o *OriginPreference) Connect(ctx context.Context, build func(context.Context, OriginAttempt) (HubSessionClient, error), options OriginAttempt) (HubSessionClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.HandshakeTimeout <= 0 || o.Cooldown <= 0 {
		return nil, errors.New("origin budgets must be positive")
	}
	if o.Address != "" && options.Host != "" && !o.CoolingDown() {
		attempt := options
		attempt.Address = o.Address
		attempt.HandshakeTimeout = o.HandshakeTimeout
		client, err := build(ctx, attempt)
		if err == nil {
			return client, nil
		}
		if client != nil {
			if cleanup := client.Close(ctx); cleanup != nil {
				return nil, errors.Join(err, cleanup)
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		o.mu.Lock()
		o.quietUntil = time.Now().Add(o.Cooldown)
		o.mu.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	options.Address = ""
	return build(ctx, options)
}
