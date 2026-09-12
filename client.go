package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Client struct {
	Identity       Identity
	Transport      RuntimeTransport
	ConnectTimeout time.Duration
	connectionGate contextMutex
	replyIDsMu     sync.Mutex
	replyIDs       map[replyCorrelation]struct{}
}

// Ask request IDs and cascade query IDs are independent matching namespaces.
type replyCorrelation struct {
	query bool
	id    string
}

func (c *Client) reserveReplyID(query bool, id string) (func(), error) {
	c.replyIDsMu.Lock()
	defer c.replyIDsMu.Unlock()
	key := replyCorrelation{query: query, id: id}
	if _, active := c.replyIDs[key]; active {
		return nil, fmt.Errorf("%w: reply correlation ID is already active on this client", ErrRuntime)
	}
	if c.replyIDs == nil {
		c.replyIDs = make(map[replyCorrelation]struct{})
	}
	c.replyIDs[key] = struct{}{}
	return func() {
		c.replyIDsMu.Lock()
		defer c.replyIDsMu.Unlock()
		delete(c.replyIDs, key)
	}, nil
}

type ClientOptions struct {
	Protocol       HubProtocol
	ConnectTimeout time.Duration
}

func NewClient(identity Identity) *Client {
	return &Client{Identity: identity, Transport: NewHTTPTransport(identity)}
}

func NewClientWithOptions(identity Identity, opts ClientOptions) (*Client, error) {
	protocol := opts.Protocol
	if protocol == "" {
		selected, err := defaultRuntimeProtocol(identity)
		if err != nil {
			return nil, err
		}
		protocol = selected
	}
	switch protocol {
	case ProtocolHTTPS:
		return &Client{Identity: identity, Transport: NewHTTPTransport(identity), ConnectTimeout: opts.ConnectTimeout}, nil
	case ProtocolWSS:
		if identity.EndpointFor(ProtocolWSS) == "" {
			return nil, fmt.Errorf("%w: identity does not include a WSS endpoint", ErrProtocol)
		}
		return &Client{Identity: identity, Transport: NewWSSTransport(identity), ConnectTimeout: opts.ConnectTimeout}, nil
	case ProtocolMQTT:
		transport, err := NewMQTTTransport(identity)
		if err != nil {
			return nil, err
		}
		return &Client{Identity: identity, Transport: transport, ConnectTimeout: opts.ConnectTimeout}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported protocol %s", ErrProtocol, protocol)
	}
}

func NewClientFromFile(path string) (*Client, error) {
	identity, err := IdentityFromFile(path)
	if err != nil {
		return nil, err
	}
	return NewClientWithOptions(identity, ClientOptions{})
}

func NewClientFromEnv() (*Client, error) {
	identity, err := IdentityFromEnv("THALOVANT_")
	if err != nil {
		return nil, err
	}
	return NewClientWithOptions(identity, ClientOptions{})
}

func NewClientFromConfig(path string, profile string) (*Client, error) {
	identity, err := IdentityFromConfig(path, profile)
	if err != nil {
		return nil, err
	}
	return NewClientWithOptions(identity, ClientOptions{})
}

func defaultRuntimeProtocol(identity Identity) (HubProtocol, error) {
	for _, protocol := range DefaultProtocolPreference {
		switch protocol {
		case ProtocolWSS:
			if identity.SupportsProtocol(ProtocolWSS) && identity.EndpointFor(ProtocolWSS) != "" {
				return ProtocolWSS, nil
			}
		case ProtocolHTTPS:
			if identity.SupportsProtocol(ProtocolHTTPS) || identity.EndpointFor(ProtocolHTTPS) != "" {
				return ProtocolHTTPS, nil
			}
		case ProtocolMQTT:
			if identity.SupportsProtocol(ProtocolMQTT) && identity.MQTT != nil {
				return ProtocolMQTT, nil
			}
		}
	}
	return "", fmt.Errorf("%w: identity does not include a usable WSS, HTTPS, or MQTT endpoint", ErrProtocol)
}

func (c *Client) Connect(ctx context.Context) error {
	timeout := c.connectTimeout()
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.connectionGate.Lock(connectCtx); err != nil {
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	// The worker retains ownership through cleanup, even if a custom
	// transport ignores cancellation. A later caller cannot race its teardown.
	result := make(chan error, 1)
	go func() {
		defer c.connectionGate.Unlock()
		var err error
		health := c.Transport.Healthcheck()
		if connectCtx.Err() != nil {
			err = fmt.Errorf("%w: %w", ErrTimeout, connectCtx.Err())
		} else if health.Connected && health.HandshakeComplete {
			result <- nil
			return
		} else {
			err = c.Transport.Connect(connectCtx)
		}
		if err == nil {
			health := c.Transport.Healthcheck()
			if !health.Connected || !health.HandshakeComplete {
				err = fmt.Errorf("%w: transport returned before authenticated readiness", ErrConnection)
			}
		}
		if connectCtx.Err() != nil {
			err = fmt.Errorf("%w: %w", ErrTimeout, connectCtx.Err())
		}
		if err != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = c.Transport.Disconnect(cleanupCtx)
			cleanupCancel()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if connectCtx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrTimeout, connectCtx.Err())
		}
		return err
	case <-connectCtx.Done():
		return fmt.Errorf("%w: %w", ErrTimeout, connectCtx.Err())
	}
}

// ConnectWithInfo includes diagnostic collection in the connection deadline.
// A timed-out custom getter retains operation ownership until it returns.
func (c *Client) ConnectWithInfo(ctx context.Context) (TransportConnectionInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, c.connectTimeout())
	defer cancel()
	connectErr := c.Connect(ctx)
	if ctx.Err() != nil {
		return TransportConnectionInfo{}, errors.Join(connectErr, fmt.Errorf("%w: %w", ErrTimeout, ctx.Err()))
	}
	information := make(chan TransportConnectionInfo, 1)
	if err := c.runOwned(ctx, false, func() error {
		information <- c.Transport.ConnectionInfo()
		return nil
	}); err != nil {
		return TransportConnectionInfo{}, errors.Join(connectErr, err)
	}
	return <-information, connectErr
}

func (c *Client) ConnectionInfo() TransportConnectionInfo {
	return c.Transport.ConnectionInfo()
}

func (c *Client) Close(ctx context.Context) error {
	closeCtx, cancel := context.WithTimeout(ctx, c.connectTimeout())
	defer cancel()
	return c.runOwned(closeCtx, false, func() error { return c.Transport.Disconnect(closeCtx) })
}

// runOwned bounds the caller while retaining the transport until a custom
// implementation has actually stopped. A queued caller never owns cleanup.
func (c *Client) runOwned(ctx context.Context, cleanupOnError bool, operation func() error) error {
	if err := c.connectionGate.Lock(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	result := make(chan error, 1)
	go func() {
		defer c.connectionGate.Unlock()
		err := operation()
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
		}
		if err != nil && cleanupOnError {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = c.Transport.Disconnect(cleanupCtx)
			cancel()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
		}
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
	}
}

func (c *Client) Healthcheck() TransportHealth {
	return c.Transport.Healthcheck()
}

func (c *Client) Emit(ctx context.Context, eventType string, data Data, eventContext Context) error {
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return c.runOwned(sendCtx, true, func() error {
		return c.Transport.EmitBus(sendCtx, eventType, data, c.contextWithIdentityMetadata(eventContext))
	})
}

func (c *Client) contextWithIdentityMetadata(eventContext Context) Context {
	if len(c.Identity.Metadata) == 0 {
		return eventContext
	}
	context := MergeContext(eventContext, nil)
	metadata := mapValue(context["metadata"])
	for key, value := range c.Identity.Metadata {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	context["metadata"] = metadata
	return context
}

func (c *Client) SendUtterance(ctx context.Context, text string, opts RequestOptions) error {
	prompt := strings.TrimSpace(text)
	if prompt == "" {
		return fmt.Errorf("send utterance requires non-empty text")
	}
	lang := opts.Lang
	if lang == "" {
		lang = "en-us"
	}
	requestID := opts.RequestID
	if requestID == "" {
		requestID = NewRequestID()
	}
	eventContext := ContextWithCorrelation(opts.Context, opts.SessionID, c.Identity.SiteID, lang, requestID)
	return c.Emit(ctx, EventRecognizerLoopUtterance, UtterancePayload(prompt, lang), eventContext)
}

func (c *Client) SendAction(ctx context.Context, payload string, opts ActionOptions) error {
	prompt := strings.TrimSpace(payload)
	if prompt == "" {
		return fmt.Errorf("send action requires non-empty payload")
	}
	requestOpts := RequestOptions{
		Lang:      opts.Lang,
		Context:   MergeContext(opts.Context, Context{"input": map[string]any{"kind": "action", "title": opts.Title, "payload": prompt}}),
		SessionID: opts.SessionID,
		RequestID: opts.RequestID,
	}
	return c.SendUtterance(ctx, prompt, requestOpts)
}

func (c *Client) SendCode(ctx context.Context, value string, opts CodeOptions) error {
	code := strings.TrimSpace(value)
	if code == "" {
		return fmt.Errorf("send code requires non-empty value")
	}
	lang := opts.Lang
	if lang == "" {
		lang = "en-us"
	}
	requestID := opts.RequestID
	if requestID == "" {
		requestID = NewRequestID()
	}
	kind := opts.Kind
	if kind == "" {
		kind = "code"
	}
	input := map[string]any{"kind": kind, "label": opts.Label, "value": code, "exact": true}
	eventContext := ContextWithCorrelation(MergeContext(opts.Context, Context{"input": input}), opts.SessionID, c.Identity.SiteID, lang, requestID)
	data := UtterancePayload(code, lang)
	data["input"] = input
	return c.Emit(ctx, EventRecognizerLoopUtterance, data, eventContext)
}

// AskOptions extends RequestOptions without changing existing keyed or unkeyed
// RequestOptions literals. Zero settlement values use the family defaults.
type AskOptions struct {
	STTLang  string
	Pipeline []string
	Location map[string]any
	RequestOptions
	ReplySettle    time.Duration
	EmptyReplyWait time.Duration
}

func (c *Client) Ask(ctx context.Context, text string, opts RequestOptions) (Reply, error) {
	return c.AskWithOptions(ctx, text, AskOptions{RequestOptions: opts})
}

func (c *Client) AskWithOptions(ctx context.Context, text string, opts AskOptions) (Reply, error) {
	prompt := strings.TrimSpace(text)
	if prompt == "" {
		return Reply{}, fmt.Errorf("ask requires non-empty text")
	}
	lang := opts.Lang
	if lang == "" {
		lang = "en-us"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	settleWait := opts.ReplySettle
	if settleWait == 0 {
		settleWait = 250 * time.Millisecond
	}
	emptyWait := opts.EmptyReplyWait
	if emptyWait == 0 {
		emptyWait = 5 * time.Second
	}
	requestID := opts.RequestID
	if requestID == "" {
		requestID = NewRequestID()
	}
	releaseID, err := c.reserveReplyID(false, requestID)
	if err != nil {
		return Reply{}, err
	}
	defer releaseID()
	eventContext := ContextWithCorrelation(RequestContext(opts.Context, RequestContextOptions{STTLang: opts.STTLang, Pipeline: opts.Pipeline, Location: opts.Location}), opts.SessionID, c.Identity.SiteID, lang, requestID)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		return Reply{}, err
	}
	sub := c.SubscribeEvents(256)
	defer sub.Close()
	var events []Event
	var mediaBudget replyMediaBudget
	var fragments []string
	var hardFailure, softFailure *Event
	var settling *time.Timer
	var settled <-chan time.Time
	emptyStarted := false
	settleStarted := false
	schedule := func(delay time.Duration) {
		if settling != nil {
			settling.Stop()
		}
		settling = time.NewTimer(max(delay, 0))
		settled = settling.C
	}
	defer func() {
		if settling != nil {
			settling.Stop()
		}
	}()
	finish := func() (Reply, error) {
		if err := ctx.Err(); err == context.Canceled {
			return Reply{}, fmt.Errorf("%w: %w", ErrTimeout, err)
		}
		if err := sub.Err(); err != nil {
			return Reply{}, subscriptionError(err)
		}
		failure := hardFailure
		if failure == nil && len(fragments) == 0 {
			failure = softFailure
		}
		if failure != nil && len(fragments) == 0 {
			return Reply{}, fmt.Errorf("%w: %s", ErrRuntime, failure.Name)
		}
		if len(fragments) == 0 {
			if err := ctx.Err(); err != nil {
				return Reply{}, fmt.Errorf("%w: %w", ErrTimeout, err)
			}
			return Reply{}, fmt.Errorf("%w: hub finished without a speak reply", ErrTimeout)
		}
		sessionID := SessionIDFromContext(eventContext)
		for _, event := range events {
			if id := event.SessionID(); strings.TrimSpace(id) != "" {
				sessionID = id
				break
			}
		}
		return Reply{Text: strings.Join(fragments, " "), Utterances: fragments, Handled: failure == nil, OK: failure == nil, SessionID: sessionID, RequestID: requestID, Events: events, DroppedMedia: mediaBudget.dropped, FailureEvent: failure}, nil
	}
	accept := func(event Event) bool {
		if event.RequestID() != requestID {
			return false
		}
		if !mediaBudget.accept(event) {
			return false
		}
		events = append(events, event)
		switch event.Name {
		case EventSpeak, EventOvosUtteranceSpeak:
			appendFragment(&fragments, event.Text())
			if !settleStarted && len(fragments) > 0 {
				settleStarted = true
				schedule(settleWait)
			}
		case EventPolicyDenied, EventQueryTimeout:
			hardFailure = &event
			return true
		case EventIntentUnmatched, EventIntentFailure:
			softFailure = &event
			if !emptyStarted && !settleStarted {
				emptyStarted = true
				schedule(emptyWait)
			}
		case EventUtteranceHandled:
			if !emptyStarted && !settleStarted {
				emptyStarted = true
				schedule(emptyWait)
			}
		}
		return false
	}
	// The collector remains active while an admitted send retires. runOwned
	// retains transport ownership even when this caller has already finished.
	sendResult := make(chan error, 1)
	go func() {
		sendResult <- c.Emit(ctx, EventRecognizerLoopUtterance, UtterancePayload(prompt, lang), eventContext)
	}()
	drain := func() error {
		// Snapshot the backlog so a continuing producer cannot postpone a
		// deadline indefinitely. Stop at the first hard terminal event.
		for queued := len(sub.C); queued > 0; queued-- {
			select {
			case event, open := <-sub.C:
				if !open {
					return subscriptionError(sub.Err())
				}
				if accept(event) {
					return nil
				}
			default:
				return nil
			}
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return Reply{}, fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
			}
			if err := drain(); err != nil {
				return Reply{}, err
			}
			return finish()
		case err := <-sendResult:
			sendResult = nil
			if err != nil {
				if ctx.Err() == context.Canceled {
					return Reply{}, err
				}
				if drainErr := drain(); drainErr != nil {
					return Reply{}, drainErr
				}
				if hardFailure != nil || ctx.Err() == context.DeadlineExceeded {
					return finish()
				}
				return Reply{}, err
			}
		case <-settled:
			if err := drain(); err != nil {
				return Reply{}, err
			}
			return finish()
		case event, open := <-sub.C:
			if !open {
				return Reply{}, subscriptionError(sub.Err())
			}
			if accept(event) {
				return finish()
			}
		}
	}
}

func (c *Client) Query(ctx context.Context, text string, opts QueryOptions) (Reply, error) {
	prompt := strings.TrimSpace(text)
	if prompt == "" {
		return Reply{}, fmt.Errorf("query requires non-empty text")
	}
	transport, ok := c.Transport.(hiveMessageTransport)
	if !ok {
		return Reply{}, fmt.Errorf("%w: this transport does not support HiveMind query frames", ErrRuntime)
	}
	lang := opts.Lang
	if lang == "" {
		lang = "en-us"
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 12 * time.Second
	}
	requestID := opts.RequestID
	if requestID == "" {
		requestID = NewRequestID()
	}
	queryID := opts.QueryID
	if queryID == "" {
		queryID = requestID
	}
	releaseID, err := c.reserveReplyID(true, queryID)
	if err != nil {
		return Reply{}, err
	}
	defer releaseID()
	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID = NewSessionID()
	}
	eventContext := ContextWithCorrelation(opts.Context, sessionID, c.Identity.SiteID, lang, requestID)
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.Connect(queryCtx); err != nil {
		return Reply{}, err
	}
	events := []Event{}
	var mediaBudget replyMediaBudget
	fragments := []string{}
	var failure, softFailure *Event
	sub := subscribeHiveMessages(transport)
	defer sub.Close()
	messages := sub.C
	inner := HiveMessage{
		MsgType: "bus",
		Payload: map[string]any{
			"type":    EventRecognizerLoopUtterance,
			"data":    UtterancePayload(prompt, lang),
			"context": eventContext,
		},
		Metadata: map[string]any{},
		Route:    []any{},
	}
	sendResult := make(chan error, 1)
	go func() {
		sendResult <- c.runOwned(queryCtx, true, func() error {
			return transport.SendHiveMessage(queryCtx, HiveMessage{
				MsgType:  "query",
				Payload:  hiveMessagePayload(inner),
				Metadata: map[string]any{"query_id": queryID},
				Route:    []any{},
			}, true)
		})
	}()
	finish := func() (Reply, error) {
		if err := queryCtx.Err(); err == context.Canceled {
			return Reply{}, fmt.Errorf("%w: %w", ErrTimeout, err)
		}
		if err := sub.Err(); err != nil {
			return Reply{}, subscriptionError(err)
		}
		if failure == nil && len(fragments) == 0 {
			failure = softFailure
		}
		if failure != nil && len(fragments) == 0 {
			return Reply{}, fmt.Errorf("%w: %s", ErrRuntime, failure.Name)
		}
		if len(fragments) == 0 {
			return Reply{}, fmt.Errorf("%w: hub finished the query without a speak reply", ErrTimeout)
		}
		replySessionID := SessionIDFromContext(eventContext)
		for _, event := range events {
			if id := event.SessionID(); strings.TrimSpace(id) != "" {
				replySessionID = id
				break
			}
		}
		return Reply{
			Text:         strings.Join(fragments, " "),
			Utterances:   fragments,
			Handled:      failure == nil,
			OK:           failure == nil,
			SessionID:    replySessionID,
			RequestID:    requestID,
			Events:       events,
			DroppedMedia: mediaBudget.dropped,
			FailureEvent: failure,
		}, nil
	}
	accept := func(message HiveMessage) bool {
		if queryIDFromHiveMessage(message) != queryID {
			return false
		}
		event, ok := eventFromQueryHiveMessage(message)
		if !ok {
			return false
		}
		if !mediaBudget.accept(event) {
			return false
		}
		events = append(events, event)
		if event.Name == "hive.query.complete" {
			return true
		}
		switch event.Name {
		case EventSpeak, EventOvosUtteranceSpeak:
			appendFragment(&fragments, event.Text())
		case EventIntentUnmatched, EventIntentFailure:
			softFailure = &event
		case EventPolicyDenied, EventQueryTimeout:
			failure = &event
			return true
		}
		return false
	}
	drain := func() (bool, error) {
		for queued := len(messages); queued > 0; queued-- {
			select {
			case message, open := <-messages:
				if !open {
					return false, subscriptionError(sub.Err())
				}
				if accept(message) {
					return true, nil
				}
			default:
				return false, nil
			}
		}
		return false, nil
	}
	for {
		select {
		case <-queryCtx.Done():
			if queryCtx.Err() != context.Canceled {
				if terminal, err := drain(); err != nil {
					return Reply{}, err
				} else if terminal {
					return finish()
				}
			}
			return Reply{}, fmt.Errorf("%w: %w", ErrTimeout, queryCtx.Err())
		case err := <-sendResult:
			sendResult = nil
			if err != nil {
				if queryCtx.Err() != context.Canceled {
					if terminal, drainErr := drain(); drainErr != nil {
						return Reply{}, drainErr
					} else if terminal {
						return finish()
					}
				}
				return Reply{}, err
			}
		case message, open := <-messages:
			if !open {
				return Reply{}, subscriptionError(sub.Err())
			}
			if accept(message) {
				return finish()
			}
		}
	}
}

type hiveMessageTransport interface {
	SendHiveMessage(ctx context.Context, message HiveMessage, encrypt bool) error
	HiveMessages() <-chan HiveMessage
}

func (c *Client) connectTimeout() time.Duration {
	if c.ConnectTimeout > 0 {
		return c.ConnectTimeout
	}
	return 6 * time.Second
}

func (c *Client) Conversation(opts ConversationOptions) Conversation {
	if opts.SessionID == "" {
		opts.SessionID = NewSessionID()
	}
	if opts.Lang == "" {
		opts.Lang = "en-us"
	}
	return Conversation{Client: c, Options: opts}
}

type RequestOptions struct {
	Timeout   time.Duration
	Lang      string
	Context   Context
	SessionID string
	RequestID string
}

type QueryOptions struct {
	Timeout   time.Duration
	Lang      string
	Context   Context
	SessionID string
	RequestID string
	QueryID   string
}

type ActionOptions struct {
	Title     string
	Lang      string
	Context   Context
	SessionID string
	RequestID string
}

type CodeOptions struct {
	Kind      string
	Label     string
	Lang      string
	Context   Context
	SessionID string
	RequestID string
}

type ConversationOptions struct {
	SessionID string
	Lang      string
	Context   Context
}

type Conversation struct {
	Client  *Client
	Options ConversationOptions
}

func (c Conversation) Ask(ctx context.Context, text string, opts RequestOptions) (Reply, error) {
	opts.SessionID = c.Options.SessionID
	if opts.Lang == "" {
		opts.Lang = c.Options.Lang
	}
	opts.Context = MergeContext(c.Options.Context, opts.Context)
	return c.Client.Ask(ctx, text, opts)
}

func (c Conversation) Query(ctx context.Context, text string, opts QueryOptions) (Reply, error) {
	opts.SessionID = c.Options.SessionID
	if opts.Lang == "" {
		opts.Lang = c.Options.Lang
	}
	opts.Context = MergeContext(c.Options.Context, opts.Context)
	return c.Client.Query(ctx, text, opts)
}

func (c Conversation) SendUtterance(ctx context.Context, text string, opts RequestOptions) error {
	opts.SessionID = c.Options.SessionID
	if opts.Lang == "" {
		opts.Lang = c.Options.Lang
	}
	opts.Context = MergeContext(c.Options.Context, opts.Context)
	return c.Client.SendUtterance(ctx, text, opts)
}

func (c Conversation) SendAction(ctx context.Context, payload string, opts ActionOptions) error {
	opts.SessionID = c.Options.SessionID
	if opts.Lang == "" {
		opts.Lang = c.Options.Lang
	}
	opts.Context = MergeContext(c.Options.Context, opts.Context)
	return c.Client.SendAction(ctx, payload, opts)
}

func (c Conversation) SendCode(ctx context.Context, value string, opts CodeOptions) error {
	opts.SessionID = c.Options.SessionID
	if opts.Lang == "" {
		opts.Lang = c.Options.Lang
	}
	opts.Context = MergeContext(c.Options.Context, opts.Context)
	return c.Client.SendCode(ctx, value, opts)
}

func hiveMessagePayload(message HiveMessage) map[string]any {
	raw, err := json.Marshal(message)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func queryIDFromHiveMessage(message HiveMessage) string {
	for _, key := range []string{"query_id", "queryId"} {
		if value, ok := message.Metadata[key].(string); ok {
			return value
		}
	}
	return ""
}

func eventFromQueryHiveMessage(message HiveMessage) (Event, bool) {
	payload, ok := busPayloadFromHivePayload(message.Payload)
	if !ok {
		return Event{}, false
	}
	return Event{
		Name:    fmt.Sprint(payload["type"]),
		Data:    mapValue(payload["data"]),
		Context: mapValue(payload["context"]),
		Raw:     message,
	}, true
}

func busPayloadFromHivePayload(payload map[string]any) (map[string]any, bool) {
	if eventType, ok := payload["type"].(string); ok {
		return map[string]any{
			"type":    eventType,
			"data":    mapValue(payload["data"]),
			"context": mapValue(payload["context"]),
		}, true
	}
	if inner, ok := payload["payload"].(map[string]any); ok {
		return busPayloadFromHivePayload(inner)
	}
	return nil, false
}

func appendFragment(fragments *[]string, text string) {
	normalized := strings.Join(strings.Fields(text), " ")
	if normalized == "" {
		return
	}
	if len(*fragments) == 0 || (*fragments)[len(*fragments)-1] != normalized {
		*fragments = append(*fragments, normalized)
	}
}
