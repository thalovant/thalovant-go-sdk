package thalovant

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// HubSession owns one connection. Either call Run in a goroutine, which keeps
// the link up by policy until the session closes, or call Probe at ProbeDelay
// intervals yourself. Warm is asynchronous; foreground calls bypass the
// unattended retry ladder. No admitted Ask or Emit is replayed after an
// ambiguous transport failure.
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

	settleWindow time.Duration
	refusalGrace time.Duration
	handlers     map[string][]*sessionHandler
	watchers     []*stateWatcher
	up           bool
	wake         chan struct{}
}

// sessionHandler is one On registration; its address is its identity.
// removed is set when it is unsubscribed, so a delivery that copied it just
// before does not call it after the unsubscribe has returned.
type sessionHandler struct {
	handle  func(Event)
	removed atomic.Bool
}

// stateWatcher is one OnStateChange registration.
type stateWatcher struct{ notify func(up bool) }

// DefaultHubSettle is how long a link opened by Connect or Run must stay up
// before it counts. A hub that does not know a client's static key says so
// only by closing right after the handshake.
const DefaultHubSettle = 750 * time.Millisecond

// DefaultHubRefusalGrace is how long Run treats a hub refusing the
// credentials as "not admitted yet" before returning the refusal. A new
// connection is refused until its hub admits it, about ninety seconds.
const DefaultHubRefusalGrace = 600 * time.Second

// hubLinkCheck is how often Run looks at a held link. Transports do not
// signal a drop, so this is how a dropped link is noticed.
const hubLinkCheck = 250 * time.Millisecond

// HubSessionOption adjusts a HubSession built by NewHubSession.
type HubSessionOption func(*HubSession)

// WithSettle sets how long a link opened by Connect or Run must stay up
// before it counts (DefaultHubSettle); 0 turns the check off. A close inside
// the window with no status, 1000, 1005 or 1008 is the hub refusing the
// credentials (ErrHubRefused); any other is a drop. Negative values are
// ignored.
func WithSettle(window time.Duration) HubSessionOption {
	return func(s *HubSession) {
		if window >= 0 {
			s.settleWindow = window
		}
	}
}

// WithRefusalGrace sets how long Run keeps retrying a hub that refuses the
// credentials before it returns the refusal (DefaultHubRefusalGrace).
// Nonpositive values are ignored.
func WithRefusalGrace(grace time.Duration) HubSessionOption {
	return func(s *HubSession) {
		if grace > 0 {
			s.refusalGrace = grace
		}
	}
}

func NewHubSession(connect func(context.Context) (HubSessionClient, error), policy HubSessionPolicy, options ...HubSessionOption) (*HubSession, error) {
	if connect == nil {
		return nil, errors.New("hub session requires a client factory")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	session := &HubSession{
		connect: connect, policy: policy, clock: time.Now, gate: make(chan struct{}, 1), retryWait: policy.Retry,
		settleWindow: DefaultHubSettle, refusalGrace: DefaultHubRefusalGrace,
		handlers: map[string][]*sessionHandler{}, wake: make(chan struct{}),
	}
	for _, option := range options {
		if option != nil {
			option(session)
		}
	}
	return session, nil
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
	notify := s.stateChangeLocked(false)
	s.mu.Unlock()
	notify()
	return s.cleanup(ctx)
}

// stateChangeLocked records whether the link is up and returns what tells the
// watchers, to run once s.mu is released.
func (s *HubSession) stateChangeLocked(up bool) func() {
	if s.up == up {
		return func() {}
	}
	s.up = up
	watchers := append([]*stateWatcher(nil), s.watchers...)
	return func() {
		for _, watcher := range watchers {
			func() {
				defer func() { _ = recover() }()
				watcher.notify(up)
			}()
		}
	}
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
				var handlers []*sessionHandler
				if s.generation == generation && !s.closed {
					s.events.publish(event)
					handlers = append(handlers, s.handlers[event.Name]...)
				}
				s.mu.Unlock()
				for _, handler := range handlers {
					deliver(handler, event)
				}
			}
		}
	}()
	return fresh, nil
}

// deliver runs one On handler. A handler that panics is skipped rather than
// taking the session's delivery, and every later event, down with it.
func deliver(handler *sessionHandler, event Event) {
	if handler.removed.Load() {
		return
	}
	defer func() { _ = recover() }()
	handler.handle(event)
}

// On calls handler for every event named eventType the session receives, on
// the client it holds now and on every client it builds after a reconnect,
// until the returned function is called; once it has returned, no delivery
// calls handler again. Handlers run one at a time, in order, on the session's
// delivery goroutine, so a handler that does slow work starts a goroutine of
// its own. Events and their maps are read-only.
func (s *HubSession) On(eventType string, handler func(Event)) (unsubscribe func()) {
	if handler == nil || strings.TrimSpace(eventType) == "" {
		return func() {}
	}
	entry := &sessionHandler{handle: handler}
	s.mu.Lock()
	s.handlers[eventType] = append(s.handlers[eventType], entry)
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.removed.Store(true)
			s.mu.Lock()
			defer s.mu.Unlock()
			current := s.handlers[eventType]
			for index, candidate := range current {
				if candidate == entry {
					s.handlers[eventType] = append(current[:index:index], current[index+1:]...)
					break
				}
			}
			if len(s.handlers[eventType]) == 0 {
				delete(s.handlers, eventType)
			}
		})
	}
}

// OnStateChange calls notify with true when the link comes up and false when
// it goes down, until the returned function is called. It runs on the
// goroutine that changed the state while that goroutine holds the session, so
// it must return promptly and must not call Ask, Emit, Reply, Connect or Close
// itself; start a goroutine for that.
func (s *HubSession) OnStateChange(notify func(up bool)) (unsubscribe func()) {
	if notify == nil {
		return func() {}
	}
	watcher := &stateWatcher{notify: notify}
	s.mu.Lock()
	s.watchers = append(s.watchers, watcher)
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for index, candidate := range s.watchers {
				if candidate == watcher {
					s.watchers = append(s.watchers[:index:index], s.watchers[index+1:]...)
					break
				}
			}
		})
	}
}

// Connected reports whether the session holds a client whose link is up.
func (s *HubSession) Connected() bool {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	return client != nil && Alive(client)
}

// Connect makes one attempt: it returns with a live link, or says why there is
// none. A link the session already holds is kept. A new one must stay up for
// the settle window (WithSettle); a hub that closes it inside the window with
// no status, 1000, 1005 or 1008 has refused the credentials, which is
// ErrHubRefused (always with ErrConnection). Any other failure is
// ErrConnection or ErrTimeout.
func (s *HubSession) Connect(ctx context.Context) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil {
		if Alive(client) {
			return nil
		}
		if err := s.drop(ctx); err != nil {
			return err
		}
	}
	fresh, err := s.ensure(ctx)
	if err != nil {
		return err
	}
	if err := s.settle(ctx, fresh); err != nil {
		return errors.Join(err, s.drop(ctx))
	}
	s.markUp(fresh)
	return nil
}

// markUp tells the watchers the link is up, when client is still the one held.
func (s *HubSession) markUp(client HubSessionClient) {
	s.mu.Lock()
	notify := func() {}
	if client != nil && s.client == client {
		notify = s.stateChangeLocked(true)
	}
	s.mu.Unlock()
	notify()
}

// settle waits out the settle window on a link that just opened.
func (s *HubSession) settle(ctx context.Context, client HubSessionClient) error {
	if s.settleWindow <= 0 {
		return nil
	}
	window := time.NewTimer(s.settleWindow)
	defer window.Stop()
	check := time.NewTicker(25 * time.Millisecond)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
		case <-window.C:
			if Alive(client) {
				return nil
			}
			return s.closedEarly(client)
		case <-check.C:
			if !Alive(client) {
				return s.closedEarly(client)
			}
		}
	}
}

// closedEarly is the error for a link that closed inside the settle window.
func (s *HubSession) closedEarly(client HubSessionClient) error {
	if refuser, ok := client.(interface{ ClosedRefused() bool }); ok && refuser.ClosedRefused() {
		return fmt.Errorf("%w: %w: the hub closed the link right after the handshake: it does not accept these credentials, or not yet", ErrConnection, ErrHubRefused)
	}
	return fmt.Errorf("%w: the hub closed the link right after the handshake", ErrConnection)
}

// Run keeps the link up until the session closes or ctx ends; start it in a
// goroutine of its own. It makes the first attempt at once, and after every
// attempt does what a LinkSupervisor decides: a link that drops is dialled
// again at once; a failed attempt waits the retry ladder (Retry, doubling up
// to RetryCeiling); a hub that refuses the credentials is retried the same way
// until the refusals have lasted the refusal grace (WithRefusalGrace), since a
// new connection is refused until its hub admits it, and then Run returns the
// refusal (ErrHubRefused); a hub whose key changed ends Run at once with that
// error (ErrHubKeyChanged), since retrying cannot change it. A held link is
// looked at continually. Run returns nil once the session is closed, and
// ctx's error when ctx ends first.
func (s *HubSession) Run(ctx context.Context) error {
	supervisor := NewLinkSupervisor(s.policy, s.refusalGrace)
	for {
		if s.isClosed() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.Connected() {
			if err := s.watch(ctx); err != nil {
				return err
			}
			if s.isClosed() {
				return nil
			}
			// Dropped: the decision is to dial again at once.
			supervisor.After(LinkDropped, time.Now())
			continue
		}
		err := s.Connect(ctx)
		switch {
		case err != nil && s.isClosed():
			return nil
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		}
		decision := supervisor.After(linkOutcome(err), time.Now())
		switch decision.Action {
		case LinkHold:
			continue
		case LinkGiveUp:
			return err
		}
		if err := s.pause(ctx, decision.Wait); err != nil {
			return err
		}
	}
}

// linkOutcome reads an attempt's error as the supervisor's outcome.
func linkOutcome(err error) LinkOutcome {
	switch {
	case err == nil:
		return LinkUp
	case errors.Is(err, ErrHubKeyChanged):
		return LinkKeyChanged
	case errors.Is(err, ErrHubRefused):
		return LinkRefused
	}
	return LinkFailed
}

// LinkOutcome is what one attempt to hold a link came to.
type LinkOutcome string

// The outcomes a LinkSupervisor decides on.
const (
	// LinkUp: the link is up.
	LinkUp LinkOutcome = "up"
	// LinkDropped: an established link went down.
	LinkDropped LinkOutcome = "dropped"
	// LinkFailed: the hub or the network could not be reached.
	LinkFailed LinkOutcome = "failed"
	// LinkRefused: the hub turned the credentials away (ErrHubRefused).
	LinkRefused LinkOutcome = "refused"
	// LinkKeyChanged: the hub's key is not the pinned one (ErrHubKeyChanged).
	LinkKeyChanged LinkOutcome = "key_changed"
)

// LinkAction is what to do after an attempt.
type LinkAction string

// The actions a LinkSupervisor decides.
const (
	// LinkHold keeps the link that is up.
	LinkHold LinkAction = "hold"
	// LinkRetry dials again after LinkDecision.Wait.
	LinkRetry LinkAction = "retry"
	// LinkGiveUp stops; LinkDecision.Reason says why.
	LinkGiveUp LinkAction = "give_up"
)

// LinkDecision is a LinkSupervisor's answer to one outcome.
type LinkDecision struct {
	Action LinkAction
	// Wait is how long to wait before dialling again, for LinkRetry.
	Wait time.Duration
	// Reason is the outcome that ended the link, for LinkGiveUp:
	// LinkRefused or LinkKeyChanged.
	Reason LinkOutcome
}

// LinkSupervisor decides how a long-lived link is kept up, as a pure function
// of what happened and when; HubSession's Run asks it after every attempt, and
// every SDK follows the same rules (link-keeping-vectors.json):
//
//   - LinkUp: hold, and start the ladder and the refusal clock afresh;
//   - LinkDropped: dial again at once;
//   - LinkFailed: wait the ladder's step -- the policy's Retry, doubling to
//     RetryCeiling -- and stop counting refusals;
//   - LinkRefused: wait the ladder's step the same way, until the refusals
//     have lasted the refusal grace since the first of them (inclusive), then
//     give up;
//   - LinkKeyChanged: give up at once, since retrying cannot change it.
//
// A LinkSupervisor is not safe for concurrent use.
type LinkSupervisor struct {
	policy       HubSessionPolicy
	refusalGrace time.Duration
	wait         time.Duration
	refusedSince time.Time
	refusing     bool
}

// NewLinkSupervisor is a supervisor for policy that gives refusals
// refusalGrace (DefaultHubRefusalGrace when nonpositive).
func NewLinkSupervisor(policy HubSessionPolicy, refusalGrace time.Duration) *LinkSupervisor {
	if refusalGrace <= 0 {
		refusalGrace = DefaultHubRefusalGrace
	}
	return &LinkSupervisor{policy: policy, refusalGrace: refusalGrace, wait: policy.Retry}
}

// After is the decision after outcome, observed at now; only the differences
// between the times it is given matter.
func (l *LinkSupervisor) After(outcome LinkOutcome, now time.Time) LinkDecision {
	switch outcome {
	case LinkUp:
		l.wait, l.refusing = l.policy.Retry, false
		return LinkDecision{Action: LinkHold}
	case LinkDropped:
		return LinkDecision{Action: LinkRetry}
	case LinkKeyChanged:
		return LinkDecision{Action: LinkGiveUp, Reason: LinkKeyChanged}
	case LinkRefused:
		if !l.refusing {
			l.refusing, l.refusedSince = true, now
		}
		if now.Sub(l.refusedSince) >= l.refusalGrace {
			return LinkDecision{Action: LinkGiveUp, Reason: LinkRefused}
		}
	default:
		l.refusing = false
	}
	wait := l.wait
	l.wait = l.policy.NextWait(l.wait)
	return LinkDecision{Action: LinkRetry, Wait: wait}
}

// watch returns once the held link has dropped (and has been let go), the
// session has closed, or ctx has ended.
func (s *HubSession) watch(ctx context.Context) error {
	check := time.NewTicker(hubLinkCheck)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake:
			return nil
		case <-check.C:
			s.mu.Lock()
			client := s.client
			s.mu.Unlock()
			if client == nil {
				return nil
			}
			if !Alive(client) {
				if err := s.acquire(ctx); err != nil {
					return err
				}
				s.mu.Lock()
				same := s.client == client
				s.mu.Unlock()
				if same {
					// A client that will not close stays retired, and the next
					// attempt closes it before it dials again.
					_ = s.drop(ctx)
				}
				s.release()
				return nil
			}
		}
	}
}

// pause waits between two attempts, returning early when the session closes.
func (s *HubSession) pause(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.wake:
		return nil
	case <-timer.C:
		return nil
	}
}

func (s *HubSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
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
		if client, err := s.ensure(ctx); err == nil {
			s.markUp(client)
		}
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
	s.markUp(client)
	err = operation(client)
	// A remote refusal proves a live, authenticated session, and a call the
	// caller withdrew (its context ended) says nothing about the link unless
	// the link went down with it.
	if err != nil && !errors.Is(err, ErrRuntime) && (ctx.Err() == nil || !Alive(client)) {
		err = errors.Join(err, s.drop(ctx))
	}
	return err
}

// Reply answers an event the hub sent, back along the route it came
// (OVOS-MSG-1 §5.2): see Client.Reply. Like Emit, it is never replayed.
func (s *HubSession) Reply(ctx context.Context, event Event, msgType string, data Data, eventContext Context) error {
	msgType = strings.TrimSpace(msgType)
	if msgType == "" {
		return fmt.Errorf("%w: a reply needs a message type", ErrRuntime)
	}
	return s.call(ctx, func(c HubSessionClient) error { return c.Emit(ctx, msgType, data, replyTo(event, eventContext)) })
}

func (s *HubSession) Ask(ctx context.Context, text string, options AskOptions) (reply Reply, err error) {
	err = s.call(ctx, func(c HubSessionClient) error { var e error; reply, e = c.AskWithOptions(ctx, text, options); return e })
	return
}
func (s *HubSession) Emit(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return s.call(ctx, func(c HubSessionClient) error { return c.Emit(ctx, eventType, data, eventContext) })
}

// Close retires the session permanently, stops Run, and waits for its
// admitted call.
func (s *HubSession) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		close(s.wake)
	}
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
