package thalovant

// The Home Assistant link: a hub asks a home, and the home always answers.
//
// A home skill on the hub sends thalovant.home.request to the account's Home
// Assistant connection; the integration hands the utterance to Home
// Assistant's conversation agent and answers with thalovant.home.response. The
// rules every SDK keeps (home-link-vectors.json in the parity contract):
//
//   - every request gets exactly one answer, within the hub's 10 seconds;
//   - the answer is a reply (OVOS-MSG-1 §5.2), so it goes back the way the
//     request came;
//   - speech is plain text, never markup;
//   - response_type is action_done, query_answer or error, and an error names
//     one error_code. When the SDK has to answer for a handler -- it failed,
//     it was too slow, it answered outside the contract -- the speech is
//     empty: the hub speaks its own sentence for the code, in the device's
//     language, which the SDK does not know.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// HomeRequestEvent is what a hub sends a Home Assistant link.
	HomeRequestEvent = "thalovant.home.request"
	// HomeResponseEvent is the one answer every request gets.
	HomeResponseEvent = "thalovant.home.response"
	// HomeRequestTimeout is how long the hub waits for an answer before it
	// treats the silence as a timeout.
	HomeRequestTimeout = 10 * time.Second
	// DefaultHomeHandlerTimeout is how long a handler has by default: a
	// second inside the hub's bound, so the SDK's own timeout answer still
	// lands before the hub gives up.
	DefaultHomeHandlerTimeout = HomeRequestTimeout - time.Second
)

// The response types a home answer may carry.
const (
	HomeActionDone  = "action_done"
	HomeQueryAnswer = "query_answer"
	HomeError       = "error"
)

// The error codes an error answer may name.
const (
	HomeErrorNoIntentMatch    = "no_intent_match"
	HomeErrorNoValidTargets   = "no_valid_targets"
	HomeErrorFailedToHandle   = "failed_to_handle"
	HomeErrorUnknown          = "unknown"
	HomeErrorTimeout          = "timeout"
	HomeErrorAgentUnavailable = "agent_unavailable"
)

// HomeResponseTypes lists every response type the contract allows, in its
// order. Each call returns a fresh slice.
func HomeResponseTypes() []string {
	return []string{HomeActionDone, HomeQueryAnswer, HomeError}
}

// HomeErrorCodes lists every error code the contract allows, in its order.
// Each call returns a fresh slice.
func HomeErrorCodes() []string {
	return []string{
		HomeErrorNoIntentMatch, HomeErrorNoValidTargets, HomeErrorFailedToHandle,
		HomeErrorUnknown, HomeErrorTimeout, HomeErrorAgentUnavailable,
	}
}

// HomeRequest is one thalovant.home.request: what was said, in which
// language. Absent fields are "".
type HomeRequest struct {
	RequestID      string
	Utterance      string
	Lang           string
	ConversationID string
	// Event is what the request arrived as; the answer is a reply to it.
	Event Event
}

// HomeRequestFromEvent reads a request out of the event it arrived as. Only a
// non-empty string counts as a value.
func HomeRequestFromEvent(event Event) HomeRequest {
	text := func(key string) string {
		value, _ := event.Data[key].(string)
		return value
	}
	return HomeRequest{
		RequestID:      text("request_id"),
		Utterance:      text("utterance"),
		Lang:           text("lang"),
		ConversationID: text("conversation_id"),
		Event:          event,
	}
}

// HomeAnswer is what a handler says back. Speech may carry markup; it is sent
// as plain text. An empty ResponseType is HomeActionDone. ErrorCode is sent
// only with HomeError. ConversationID, when empty, echoes the request's.
type HomeAnswer struct {
	Speech               string
	ResponseType         string
	ErrorCode            string
	ContinueConversation bool
	ConversationID       string
}

// HomeResponse is the thalovant.home.response payload for answer, held to the
// contract: an unknown response type or error code becomes HomeError with
// HomeErrorUnknown, keeping the speech.
func HomeResponse(request HomeRequest, answer HomeAnswer) Data {
	responseType := answer.ResponseType
	if responseType == "" {
		responseType = HomeActionDone
	}
	errorCode := ""
	if responseType == HomeError {
		errorCode = answer.ErrorCode
	}
	if !slices.Contains(HomeResponseTypes(), responseType) || (responseType == HomeError && !slices.Contains(HomeErrorCodes(), errorCode)) {
		responseType, errorCode = HomeError, HomeErrorUnknown
	}
	payload := Data{
		"request_id":            request.RequestID,
		"speech":                PlainSpeech(answer.Speech),
		"response_type":         responseType,
		"continue_conversation": answer.ContinueConversation,
	}
	if errorCode != "" {
		payload["error_code"] = errorCode
	}
	if conversationID := firstNonEmpty(answer.ConversationID, request.ConversationID); conversationID != "" {
		payload["conversation_id"] = conversationID
	}
	return payload
}

// PlainSpeech is speech a device can say as it is, made in this order, the
// same in every SDK (home-link-vectors.json):
//
//  1. markup removed, with StripSSML: only real tags, comments and processing
//     instructions, so "5 < 6 and 7 > 3" stays whole;
//  2. character references decoded once, left to right, with
//     DecodeReferences: numeric ones, the five XML entities and &nbsp;,
//     nothing else;
//  3. every run of Unicode White_Space collapsed to one space, and the ends
//     trimmed of it.
//
// Nothing comes from html.UnescapeString: its table of named references is
// not the one other SDKs decode.
func PlainSpeech(text string) string {
	if text == "" {
		return ""
	}
	// Every White_Space run is one space by now, so trimming spaces trims
	// exactly White_Space; U+001C..U+001F are not white space and stay.
	return strings.Trim(whiteSpace.ReplaceAllString(DecodeReferences(StripSSML(text)), " "), " ")
}

// whiteSpace is a run of Unicode White_Space characters, spelled out rather
// than as \s, which differs from one regex engine to the next.
var whiteSpace = regexp.MustCompile(`[\t\n\v\f\r \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]+`)

// characterReference is the one small set of references every SDK decodes:
// decimal and hexadecimal numeric references, the five XML entities, and
// &nbsp;. A reference needs its semicolon.
var characterReference = regexp.MustCompile(`&(?:#([0-9]{1,7})|#[xX]([0-9A-Fa-f]{1,6})|(amp|lt|gt|quot|apos|nbsp));`)

var namedReferences = map[string]string{"amp": "&", "lt": "<", "gt": ">", "quot": "\"", "apos": "'", "nbsp": "\u00a0"}

// DecodeReferences decodes the portable set of character references once,
// left to right: numeric references (&#72;, &#x48;, &#X48;) and the five XML
// entities plus &nbsp;. A numeric reference to no character -- 0, a surrogate,
// anything past U+10FFFF -- and every other named reference (&eacute;,
// &copy;) are left as written, since the libraries SDKs would otherwise use
// disagree about them.
func DecodeReferences(text string) string {
	if !strings.Contains(text, "&") {
		return text
	}
	return characterReference.ReplaceAllStringFunc(text, func(reference string) string {
		parts := characterReference.FindStringSubmatch(reference)
		if parts[3] != "" {
			return namedReferences[parts[3]]
		}
		var value int64
		var err error
		if parts[1] != "" {
			value, err = strconv.ParseInt(parts[1], 10, 32)
		} else {
			value, err = strconv.ParseInt(parts[2], 16, 32)
		}
		if err != nil || value == 0 || (value >= 0xD800 && value <= 0xDFFF) || value > 0x10FFFF {
			return reference
		}
		return string(rune(value))
	})
}

// HomeHandler answers one request. It runs on its own goroutine under a
// context that ends when the handler's time is up; an error, a panic or
// running out of time is answered for it (failed_to_handle, failed_to_handle,
// timeout).
type HomeHandler func(ctx context.Context, request HomeRequest) (HomeAnswer, error)

// Replier sends a reply to an event, back along the route it came. Client and
// HubSession are Repliers.
type Replier interface {
	Reply(ctx context.Context, event Event, msgType string, data Data, eventContext Context) error
}

// AnswerHomeRequest answers one request: it runs handler, then replies with
// whatever happened, and returns the payload it sent.
//
// Everything happens inside the hub's bound (opts.HubTimeout,
// HomeRequestTimeout by default), counted from the call: the hub gives up on a
// request after that, and an answer it has given up on only confuses the next
// one. The handler gets opts.Timeout (DefaultHomeHandlerTimeout by default) or
// what is left of the bound, whichever is less, under a context derived from
// ctx; the reply gets whatever the handler left. When the handler fails the
// answer is failed_to_handle, and when it is still running at its deadline
// the answer is timeout, sent at once: a handler that ignores its context is
// left to finish on its own goroutine, and what it returns then is dropped.
//
// A reply is never started after the bound, and one still waiting to be sent
// when it passes is withdrawn; a frame already being written is finished,
// since half of one would break the Noise stream. When there was no time to
// answer, AnswerHomeRequest returns a nil payload and a nil error. When ctx
// itself ends first, nothing is sent and ctx's error is returned: the link is
// going away.
func AnswerHomeRequest(ctx context.Context, replier Replier, event Event, handler HomeHandler, opts HomeAnswerOptions) (Data, error) {
	if replier == nil {
		return nil, fmt.Errorf("%w: answering a home request needs somewhere to reply", ErrRuntime)
	}
	started := time.Now()
	request := HomeRequestFromEvent(event)
	timeout, hubTimeout := opts.Timeout, opts.HubTimeout
	if timeout <= 0 {
		timeout = DefaultHomeHandlerTimeout
	}
	if hubTimeout <= 0 {
		hubTimeout = HomeRequestTimeout
	}
	hubDeadline := started.Add(hubTimeout)
	handlerDeadline := started.Add(timeout)
	if hubDeadline.Before(handlerDeadline) {
		handlerDeadline = hubDeadline
	}
	answer, err := runHomeHandler(ctx, request, handler, handlerDeadline)
	if err != nil {
		return nil, err
	}
	payload := HomeResponse(request, answer)
	if !time.Now().Before(hubDeadline) {
		return nil, nil // no time left: the hub has given up on this request
	}
	replyCtx, cancel := context.WithDeadline(ctx, hubDeadline)
	defer cancel()
	// Sent on its own goroutine, so a Replier that does not honour its context
	// still cannot hold this past the bound.
	sent := make(chan error, 1)
	go func() { sent <- replier.Reply(replyCtx, event, HomeResponseEvent, payload, nil) }()
	select {
	case err := <-sent:
		if err == nil {
			return payload, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(replyCtx.Err(), context.DeadlineExceeded) {
			return nil, nil // withdrawn at the bound
		}
		return payload, err
	case <-replyCtx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil // withdrawn at the bound
	}
}

// runHomeHandler is the handler's answer, or the one the SDK gives for it.
func runHomeHandler(ctx context.Context, request HomeRequest, handler HomeHandler, deadline time.Time) (HomeAnswer, error) {
	failed := HomeAnswer{ResponseType: HomeError, ErrorCode: HomeErrorFailedToHandle}
	if handler == nil {
		return failed, nil
	}
	handlerCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	type outcome struct {
		answer HomeAnswer
		err    error
	}
	// Buffered, so a handler that finishes after the deadline never blocks.
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- outcome{err: fmt.Errorf("%w: the home handler panicked: %v", ErrRuntime, recovered)}
			}
		}()
		answer, err := handler(handlerCtx, request)
		done <- outcome{answer, err}
	}()
	timedOut := HomeAnswer{ResponseType: HomeError, ErrorCode: HomeErrorTimeout}
	select {
	case result := <-done:
		switch {
		case ctx.Err() != nil:
			return HomeAnswer{}, ctx.Err()
		case result.err != nil && errors.Is(result.err, context.DeadlineExceeded) && errors.Is(handlerCtx.Err(), context.DeadlineExceeded):
			// It gave up because its time was up: that is a timeout, not a
			// failure.
			return timedOut, nil
		case result.err != nil:
			return failed, nil
		}
		return result.answer, nil
	case <-handlerCtx.Done():
		if err := ctx.Err(); err != nil {
			return HomeAnswer{}, err
		}
		return timedOut, nil
	}
}

// HomeLink is a connection a hub's requests arrive on: somewhere to register a
// handler for one event type, and to reply. HubSession is one.
type HomeLink interface {
	Replier
	On(eventType string, handler func(Event)) (unsubscribe func())
}

// HomeAnswerOptions tunes AnswerHomeRequest and AnswerHomeRequests.
type HomeAnswerOptions struct {
	// Timeout is how long each handler has at most; DefaultHomeHandlerTimeout
	// when zero.
	Timeout time.Duration
	// HubTimeout is the hub's bound on a request, counted from its arrival,
	// which the handler and the reply share; HomeRequestTimeout when zero.
	HubTimeout time.Duration
	// OnReplyError, when set, hears about an answer that could not be sent.
	// An answer there was no time left to send is not an error.
	OnReplyError func(request HomeRequest, err error)
}

// AnswerHomeRequests answers every thalovant.home.request that reaches link,
// each on a goroutine of its own so a slow one does not hold up the next, and
// returns a function that stops it. Stopping cancels the contexts of the
// answers still running; those send nothing.
//
// With a HubSession, requests keep arriving across reconnects while the
// session's Run keeps the link up:
//
//	session, _ := thalovant.NewHubSession(connect, thalovant.DefaultHubSessionPolicy())
//	stop := thalovant.AnswerHomeRequests(session, handler, thalovant.HomeAnswerOptions{})
//	defer stop()
//	go session.Run(ctx)
func AnswerHomeRequests(link HomeLink, handler HomeHandler, opts HomeAnswerOptions) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	unsubscribe := link.On(HomeRequestEvent, func(event Event) {
		if ctx.Err() != nil {
			return // stopped: nothing new starts
		}
		go func() {
			_, err := AnswerHomeRequest(ctx, link, event, handler, opts)
			if err != nil && ctx.Err() == nil && opts.OnReplyError != nil {
				opts.OnReplyError(HomeRequestFromEvent(event), err)
			}
		}()
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			unsubscribe()
			cancel()
		})
	}
}
