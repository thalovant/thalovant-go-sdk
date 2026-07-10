package thalovant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Client struct {
	Identity       Identity
	Transport      RuntimeTransport
	ConnectTimeout time.Duration
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
	health := c.Transport.Healthcheck()
	if health.Connected && health.HandshakeComplete {
		return nil
	}
	timeout := c.connectTimeout()
	connectCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok && timeout > 0 {
		connectCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	err := c.Transport.Connect(connectCtx)
	if err != nil && connectCtx.Err() != nil {
		_ = c.Transport.Disconnect(context.Background())
		return fmt.Errorf("%w: hub connection did not complete within %s", ErrTimeout, timeout)
	}
	return err
}

func (c *Client) ConnectWithInfo(ctx context.Context) (TransportConnectionInfo, error) {
	err := c.Connect(ctx)
	return c.ConnectionInfo(), err
}

func (c *Client) ConnectionInfo() TransportConnectionInfo {
	return c.Transport.ConnectionInfo()
}

func (c *Client) Close(ctx context.Context) error {
	return c.Transport.Disconnect(ctx)
}

func (c *Client) Healthcheck() TransportHealth {
	return c.Transport.Healthcheck()
}

func (c *Client) Emit(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return c.Transport.EmitBus(ctx, eventType, data, c.contextWithIdentityMetadata(eventContext))
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

func (c *Client) Ask(ctx context.Context, text string, opts RequestOptions) (Reply, error) {
	prompt := strings.TrimSpace(text)
	if prompt == "" {
		return Reply{}, fmt.Errorf("ask requires non-empty text")
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
	eventContext := ContextWithCorrelation(opts.Context, opts.SessionID, c.Identity.SiteID, lang, requestID)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var events []Event
	var fragments []string
	var failure *Event
	if err := c.Emit(ctx, EventRecognizerLoopUtterance, UtterancePayload(prompt, lang), eventContext); err != nil {
		return Reply{}, err
	}
	for {
		select {
		case <-ctx.Done():
			return Reply{}, fmt.Errorf("%w: utterance handling timed out", ErrTimeout)
		case event := <-c.Transport.Events():
			if !EventMatchesContext(event, eventContext) {
				continue
			}
			events = append(events, event)
			switch event.Name {
			case EventSpeak, EventOvosUtteranceSpeak:
				if event.Text() != "" {
					fragments = append(fragments, event.Text())
				}
			case EventIntentFailure, EventPolicyDenied, EventQueryTimeout:
				failure = &event
			case EventUtteranceHandled:
				if failure != nil && len(fragments) == 0 {
					return Reply{}, fmt.Errorf("%w: %s", ErrRuntime, failure.Name)
				}
				return Reply{
					Text:       strings.Join(fragments, " "),
					Utterances: fragments,
					Handled:    failure == nil,
					OK:         failure == nil,
					SessionID:  SessionIDFromContext(eventContext),
					RequestID:  requestID,
					Events:     events,
				}, nil
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
	if err := c.Connect(ctx); err != nil {
		return Reply{}, err
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
	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID = NewSessionID()
	}
	eventContext := ContextWithCorrelation(opts.Context, sessionID, c.Identity.SiteID, lang, requestID)
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	events := []Event{}
	fragments := []string{}
	var failure *Event
	messages := transport.HiveMessages()
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
	if err := transport.SendHiveMessage(queryCtx, HiveMessage{
		MsgType:  "query",
		Payload:  hiveMessagePayload(inner),
		Metadata: map[string]any{"query_id": queryID},
		Route:    []any{},
	}, true); err != nil {
		return Reply{}, err
	}
	for {
		select {
		case <-queryCtx.Done():
			return Reply{}, fmt.Errorf("%w: query timed out", ErrTimeout)
		case message := <-messages:
			if queryIDFromHiveMessage(message) != queryID {
				continue
			}
			event, ok := eventFromQueryHiveMessage(message)
			if !ok {
				continue
			}
			events = append(events, event)
			if event.Name == "hive.query.complete" {
				if failure != nil && len(fragments) == 0 {
					return Reply{}, fmt.Errorf("%w: %s", ErrRuntime, failure.Name)
				}
				if len(fragments) == 0 {
					return Reply{}, fmt.Errorf("%w: hub finished the query without a speak reply", ErrTimeout)
				}
				return Reply{
					Text:         strings.Join(fragments, " "),
					Utterances:   fragments,
					Handled:      failure == nil,
					OK:           failure == nil,
					SessionID:    SessionIDFromContext(eventContext),
					RequestID:    requestID,
					Events:       events,
					FailureEvent: failure,
				}, nil
			}
			switch event.Name {
			case EventSpeak, EventOvosUtteranceSpeak:
				appendFragment(&fragments, event.Text())
			case EventIntentFailure, EventPolicyDenied, EventQueryTimeout:
				failure = &event
				if len(fragments) == 0 {
					return Reply{}, fmt.Errorf("%w: %s", ErrRuntime, event.Name)
				}
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
