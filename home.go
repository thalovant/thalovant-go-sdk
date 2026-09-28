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
	"html"
	"slices"
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

// PlainSpeech is speech a device can say as it is: markup removed, HTML
// entities decoded, whitespace collapsed and trimmed.
func PlainSpeech(text string) string {
	if text == "" {
		return ""
	}
	return strings.Join(strings.FieldsFunc(html.UnescapeString(StripSSML(text)), problemBlank), " ")
}

// HomeHandler answers one request. It runs under a context that ends when the
// handler's time is up; an error, a panic or running out of time is answered
// for it (failed_to_handle, failed_to_handle, timeout).
type HomeHandler func(ctx context.Context, request HomeRequest) (HomeAnswer, error)

// Replier sends a reply to an event, back along the route it came. Client and
// HubSession are Repliers.
type Replier interface {
	Reply(ctx context.Context, event Event, msgType string, data Data, eventContext Context) error
}

// AnswerHomeRequest answers one request: it runs handler, then replies with
// whatever happened, and returns the payload it sent.
//
// The handler has timeout (DefaultHomeHandlerTimeout when nonpositive), under
// a context derived from ctx, and the reply has what is left of the hub's
// HomeRequestTimeout after it. When it fails the answer is failed_to_handle,
// and when it is still running at the deadline the answer is timeout; a
// handler that ignores its context is left to finish on its own goroutine,
// and what it returns then is dropped. When ctx itself ends first, nothing is
// sent and ctx's error is returned: the link is going away.
func AnswerHomeRequest(ctx context.Context, replier Replier, event Event, handler HomeHandler, timeout time.Duration) (Data, error) {
	if replier == nil {
		return nil, fmt.Errorf("%w: answering a home request needs somewhere to reply", ErrRuntime)
	}
	request := HomeRequestFromEvent(event)
	if timeout <= 0 {
		timeout = DefaultHomeHandlerTimeout
	}
	// The hub's bound covers the handler and the reply together, so the reply
	// gets what is left of it rather than a budget of its own: an answer
	// delivered after the hub has given up is only noise. A handler timeout
	// set past the hub's bound extends it, since the caller asked for that.
	replyCtx, cancel := context.WithTimeout(ctx, max(HomeRequestTimeout, timeout))
	defer cancel()
	answer, err := runHomeHandler(ctx, request, handler, timeout)
	if err != nil {
		return nil, err
	}
	payload := HomeResponse(request, answer)
	if err := replier.Reply(replyCtx, event, HomeResponseEvent, payload, nil); err != nil {
		return payload, err
	}
	return payload, nil
}

// runHomeHandler is the handler's answer, or the one the SDK gives for it.
func runHomeHandler(ctx context.Context, request HomeRequest, handler HomeHandler, timeout time.Duration) (HomeAnswer, error) {
	failed := HomeAnswer{ResponseType: HomeError, ErrorCode: HomeErrorFailedToHandle}
	if handler == nil {
		return failed, nil
	}
	handlerCtx, cancel := context.WithTimeout(ctx, timeout)
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

// HomeAnswerOptions tunes AnswerHomeRequests.
type HomeAnswerOptions struct {
	// Timeout is how long each handler has; DefaultHomeHandlerTimeout when
	// zero.
	Timeout time.Duration
	// OnReplyError, when set, hears about an answer that could not be sent.
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
			_, err := AnswerHomeRequest(ctx, link, event, handler, opts.Timeout)
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
