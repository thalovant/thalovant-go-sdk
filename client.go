package thalovant

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Client struct {
	Identity  Identity
	Transport *HTTPTransport
}

func NewClient(identity Identity) *Client {
	return &Client{Identity: identity, Transport: NewHTTPTransport(identity)}
}

func NewClientFromFile(path string) (*Client, error) {
	identity, err := IdentityFromFile(path)
	if err != nil {
		return nil, err
	}
	return NewClient(identity), nil
}

func NewClientFromEnv() (*Client, error) {
	identity, err := IdentityFromEnv("THALOVANT_")
	if err != nil {
		return nil, err
	}
	return NewClient(identity), nil
}

func (c *Client) Connect(ctx context.Context) error {
	return c.Transport.Connect(ctx)
}

func (c *Client) Close(ctx context.Context) error {
	return c.Transport.Disconnect(ctx)
}

func (c *Client) Healthcheck() TransportHealth {
	return c.Transport.Healthcheck()
}

func (c *Client) Emit(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return c.Transport.EmitBus(ctx, eventType, data, eventContext)
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
		case event := <-c.Transport.BusEvents:
			if !EventMatchesContext(event, eventContext) {
				continue
			}
			events = append(events, event)
			switch event.Name {
			case EventSpeak:
				if event.Text() != "" {
					fragments = append(fragments, event.Text())
				}
			case EventIntentFailure, EventPolicyDenied:
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
