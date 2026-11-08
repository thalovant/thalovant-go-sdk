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
	"reflect"
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
// timeout). A handler should return once its context ends: Go cannot stop a
// goroutine, so one that does not keeps it until it returns (see
// AnswerHomeRequest for the bound on those).
type HomeHandler func(ctx context.Context, request HomeRequest) (HomeAnswer, error)

// Replier sends a reply to an event, back along the route it came. Client and
// HubSession are Repliers.
//
// Reply must return once ctx ends, as Client's and HubSession's do: the
// answer to a home request has to be withdrawn at the hub's bound.
// AnswerHomeRequest stops waiting at the bound whatever Reply does, but a
// Reply that ignores ctx keeps its goroutine until it returns.
type Replier interface {
	Reply(ctx context.Context, event Event, msgType string, data Data, eventContext Context) error
}

// homeOverrunLimit is how many handlers, and apart from them how many
// replies, may still be running past their time before the next request
// stops starting another. A handler or a Replier that ignores its context
// keeps its goroutine until it returns, and without a limit a stuck one would
// leave a goroutine behind for every request. Four, as the Python reference
// runs at most four plain handlers at once.
const homeOverrunLimit = 4

// overrunCount is where a goroutine still running past its time is counted.
type overrunCount interface {
	// full reports whether homeOverrunLimit goroutines are counted.
	full() bool
	// add counts delta more (or, negative, fewer) goroutines.
	add(delta int)
}

// homeOverruns counts the goroutines of one kind that ran past their time and
// have not returned yet.
type homeOverruns struct {
	mu      sync.Mutex
	running int
}

// full reports whether the count has reached homeOverrunLimit.
func (o *homeOverruns) full() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.running >= homeOverrunLimit
}

// add moves the count by delta.
func (o *homeOverruns) add(delta int) {
	o.mu.Lock()
	o.running += delta
	o.mu.Unlock()
}

// replierOverruns counts the replies still running past the bound apart for
// each Replier, so one that is stuck holds up only its own answers. An entry
// is dropped once its count is back to zero, so a Replier is never kept alive
// by being counted.
type replierOverruns struct {
	mu      sync.Mutex
	running map[any]int
}

// count is replier's own count.
func (r *replierOverruns) count(replier Replier) overrunCount {
	return replierCount{counts: r, key: replierKey(replier)}
}

// replierKey is what a Replier is counted under: itself when it is a
// pointer, a channel or an unsafe pointer, which are always safe to compare;
// otherwise its type, which every value of that type then shares. A struct
// value holding a slice would panic as a map key.
func replierKey(replier Replier) any {
	switch reflect.ValueOf(replier).Kind() {
	case reflect.Pointer, reflect.Chan, reflect.UnsafePointer:
		return replier
	default:
		return reflect.TypeOf(replier)
	}
}

// replierCount is one Replier's entry in a replierOverruns.
type replierCount struct {
	counts *replierOverruns
	key    any
}

// full reports whether this Replier has homeOverrunLimit replies counted.
func (c replierCount) full() bool {
	c.counts.mu.Lock()
	defer c.counts.mu.Unlock()
	return c.counts.running[c.key] >= homeOverrunLimit
}

// add moves this Replier's count by delta, dropping the entry at zero.
func (c replierCount) add(delta int) {
	c.counts.mu.Lock()
	defer c.counts.mu.Unlock()
	if c.counts.running == nil {
		c.counts.running = map[any]int{}
	}
	if running := c.counts.running[c.key] + delta; running > 0 {
		c.counts.running[c.key] = running
	} else {
		delete(c.counts.running, c.key)
	}
}

// homeBounds are where AnswerHomeRequest counts what runs past its time:
// handlers and replies apart, so a stuck handler never keeps an answer from
// being sent.
type homeBounds struct {
	handlers homeOverruns
	// replies counts a subscription's replies, which all go through its one
	// link.
	replies homeOverruns
	// byReplier, when set, counts replies per Replier instead: calls of
	// AnswerHomeRequest on its own may each name a different one.
	byReplier *replierOverruns
}

// replyCount is where a reply through replier is counted.
func (b *homeBounds) replyCount(replier Replier) overrunCount {
	if b.byReplier != nil {
		return b.byReplier.count(replier)
	}
	return &b.replies
}

// sharedHomeBounds are the counts of AnswerHomeRequest called on its own,
// with no subscription of its own.
var sharedHomeBounds = &homeBounds{byReplier: &replierOverruns{}}

// homeRun is one goroutine's place in a count. It takes a place when its
// caller stops waiting for it (abandoned) and gives it back when the
// goroutine returns (returned), whichever order the two come in.
type homeRun struct {
	mu          sync.Mutex
	count       overrunCount
	finished    bool
	overrunning bool
}

// abandoned counts the goroutine as running past its time, unless it has
// already returned.
func (r *homeRun) abandoned() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished && !r.overrunning {
		r.overrunning = true
		r.count.add(1)
	}
}

// returned marks the goroutine as done and gives its place back if it held
// one.
func (r *homeRun) returned() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = true
	if r.overrunning {
		r.overrunning = false
		r.count.add(-1)
	}
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
//
// Go cannot stop a goroutine, so a handler that ignores its context, or a
// Replier that ignores its own, keeps its goroutine until it returns. While
// four handlers are still running past their time, a request is answered
// timeout at once and its handler is not started; while four replies are
// still running past the bound, no reply is started and the error says so.
// Neither can then leave a goroutine behind for every request. Calls of
// AnswerHomeRequest on its own share one count of handlers and keep one
// count of replies for each Replier; each AnswerHomeRequests subscription
// has its own counts.
func AnswerHomeRequest(ctx context.Context, replier Replier, event Event, handler HomeHandler, opts HomeAnswerOptions) (Data, error) {
	return answerHomeRequest(ctx, replier, event, handler, opts, sharedHomeBounds)
}

// answerHomeRequest is AnswerHomeRequest, counting what runs past its time
// in bounds.
func answerHomeRequest(ctx context.Context, replier Replier, event Event, handler HomeHandler, opts HomeAnswerOptions, bounds *homeBounds) (Data, error) {
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
	answer, err := runHomeHandler(ctx, request, handler, handlerDeadline, &bounds.handlers)
	if err != nil {
		return nil, err
	}
	payload := HomeResponse(request, answer)
	if !time.Now().Before(hubDeadline) {
		return nil, nil // no time left: the hub has given up on this request
	}
	replies := bounds.replyCount(replier)
	if replies.full() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %d home replies through this Replier are still running past the hub's bound; this answer was not sent", ErrTimeout, homeOverrunLimit)
	}
	replyCtx, cancel := context.WithDeadline(ctx, hubDeadline)
	defer cancel()
	// Sent on its own goroutine, so a Replier that does not honour its context
	// still cannot hold this past the bound.
	run := &homeRun{count: replies}
	sent := make(chan error, 1)
	go func() {
		defer run.returned()
		sent <- replier.Reply(replyCtx, event, HomeResponseEvent, payload, nil)
	}()
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
		run.abandoned()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil // withdrawn at the bound
	}
}

// runHomeHandler is the handler's answer, or the one the SDK gives for it.
// overruns counts the handlers still running past their time.
func runHomeHandler(ctx context.Context, request HomeRequest, handler HomeHandler, deadline time.Time, overruns overrunCount) (HomeAnswer, error) {
	failed := HomeAnswer{ResponseType: HomeError, ErrorCode: HomeErrorFailedToHandle}
	if handler == nil {
		return failed, nil
	}
	timedOut := HomeAnswer{ResponseType: HomeError, ErrorCode: HomeErrorTimeout}
	if overruns.full() {
		// Every place is held by a handler that has not returned: starting
		// another would only leave one more goroutine behind.
		if err := ctx.Err(); err != nil {
			return HomeAnswer{}, err
		}
		return timedOut, nil
	}
	handlerCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	type outcome struct {
		answer HomeAnswer
		err    error
	}
	// Buffered, so a handler that finishes after the deadline never blocks.
	done := make(chan outcome, 1)
	run := &homeRun{count: overruns}
	go func() {
		// Deferred first, so it runs last: the place is given back only once
		// the handler has returned, a panic included.
		defer run.returned()
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- outcome{err: fmt.Errorf("%w: the home handler panicked: %v", ErrRuntime, recovered)}
			}
		}()
		answer, err := handler(handlerCtx, request)
		done <- outcome{answer, err}
	}()
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
		run.abandoned()
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
// answers still running; those send nothing. The subscription has its own
// counts of handlers and replies still running past their time (see
// AnswerHomeRequest): four of either and requests are answered without
// starting another. Its replies all go through link, so they are one count.
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
	bounds := &homeBounds{}
	unsubscribe := link.On(HomeRequestEvent, func(event Event) {
		if ctx.Err() != nil {
			return // stopped: nothing new starts
		}
		go func() {
			_, err := answerHomeRequest(ctx, link, event, handler, opts, bounds)
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
