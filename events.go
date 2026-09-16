package thalovant

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

type Context map[string]any
type Data map[string]any

type Event struct {
	Name    string
	Data    Data
	Context Context
	Raw     any
}

type Reply struct {
	DroppedMedia int
	Text         string
	Utterances   []string
	Handled      bool
	OK           bool
	SessionID    string
	RequestID    string
	Events       []Event
	FailureEvent *Event
}

// PipelineIDs reports nonempty string stage stamps in first-seen order.
func (r Reply) PipelineIDs() []string { return r.contextIdentifiers("pipeline_id") }

// SkillIDs reports nonempty string skill stamps in first-seen order.
func (r Reply) SkillIDs() []string { return r.contextIdentifiers("skill_id") }

// Claimed is advisory: an OK reply from any non-fallback stage is claimed.
// An older hub without stage stamps retains its existing OK behavior.
func (r Reply) Claimed() bool {
	if !r.Handled || !r.OK || r.FailureEvent != nil {
		return false
	}
	stages := r.PipelineIDs()
	if len(stages) == 0 {
		return true
	}
	for _, stage := range stages {
		if !strings.Contains(stage, "fallback") {
			return true
		}
	}
	return false
}

func (r Reply) contextIdentifiers(key string) []string {
	result := make([]string, 0)
	seen := make(map[string]bool)
	for _, event := range r.Events {
		value, ok := event.Context[key].(string)
		if ok && value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func (e Event) Text() string {
	if val, ok := e.Data["utterance"].(string); ok {
		return val
	}
	if val, ok := e.Data["text"].(string); ok {
		return val
	}
	utterances := e.Utterances()
	if len(utterances) > 0 {
		return utterances[0]
	}
	return ""
}

func (e Event) Utterances() []string {
	raw, ok := e.Data["utterances"]
	if !ok {
		if val, ok := e.Data["utterance"].(string); ok {
			return []string{val}
		}
		return nil
	}
	if val, ok := raw.(string); ok {
		return []string{val}
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if val, ok := item.(string); ok {
			out = append(out, val)
		}
	}
	return out
}

func (e Event) SessionID() string {
	return SessionIDFromContext(e.Context)
}

func (e Event) RequestID() string {
	if val := RequestIDFromContext(e.Context); val != "" {
		return val
	}
	return requestIDFromMap(e.Data)
}

func (e Event) IsFailure() bool {
	_, ok := failureEvents[e.Name]
	return ok
}

func NewSessionID() string {
	return newID("thalovant-session-")
}

func NewRequestID() string {
	return newID("thalovant-request-")
}

func UtterancePayload(text, lang string) Data {
	return Data{"utterances": []string{text}, "lang": lang}
}

func MergeContext(base, extra Context) Context {
	merged := Context{}
	for key, val := range base {
		merged[key] = val
	}
	for key, val := range extra {
		if key == "session" {
			session := sessionFromContext(merged)
			if next, ok := val.(map[string]any); ok {
				for k, v := range next {
					session[k] = v
				}
				merged["session"] = session
				continue
			}
		}
		merged[key] = val
	}
	return merged
}

// HiveKinds are the hive's own frame kinds, which a client may subscribe to.
//
// query and cascade are deliberately absent: they are this client's own
// request/response traffic and Ask already owns them, so subscribing to one
// would quietly compete for the same replies.
var HiveKinds = []string{"broadcast", "propagate", "escalate", "intercom", "rendezvous"}

// ConversationSessionFields are the session fields a client carries from one
// turn of a conversation to the next.
//
// A hub keeps nothing for a named session: OVOS-SESSION-2 §2.2 makes the
// orchestrator stateless for those, so the carrier a client sends is the whole
// snapshot and whatever the last turn activated is discarded the moment it
// ends. Without converse_handlers the converse pipeline has no skill to poll
// and every follow-up reaches the fallback instead of the skill that just
// answered.
//
// An allow-list, not a deny-list. Deliberately absent: the caller's own
// per-turn settings (lang, pipeline, site_id), because a client that decides
// the language per utterance would otherwise be pinned to whichever one the
// conversation opened in; and the live device flags, which describe a moment
// that has passed by the time the next turn is sent.
var ConversationSessionFields = []string{
	"converse_handlers",
	"active_handlers",
	"active_skills",
	"context",
	"utterance_states",
	"response_mode",
}

func carriedValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// CarryConversation fills the conversation fields of session from the hub's
// last reply.
//
// This turn's own values win: a field the caller set is never overwritten, only
// one it left out is taken from the turn before.
func CarryConversation(previous, session map[string]any) map[string]any {
	carried := make(map[string]any, len(session)+len(ConversationSessionFields))
	for key, value := range session {
		carried[key] = value
	}
	if previous == nil {
		return carried
	}
	for _, field := range ConversationSessionFields {
		if _, present := carried[field]; present {
			continue
		}
		if value, ok := previous[field]; ok && carriedValue(value) {
			carried[field] = value
		}
	}
	return carried
}

func ContextWithCorrelation(raw Context, sessionID, siteID, lang, requestID string) Context {
	next := MergeContext(raw, nil)
	session := sessionFromContext(next)
	if sessionID != "" {
		session["session_id"] = sessionID
	}
	if siteID != "" {
		if _, ok := session["site_id"]; !ok {
			session["site_id"] = siteID
		}
	}
	if lang != "" {
		if _, ok := session["lang"]; !ok {
			session["lang"] = lang
		}
	}
	if requestID != "" {
		next["request_id"] = requestID
		next["thalovant_request_id"] = requestID
		session["request_id"] = requestID
	}
	if len(session) > 0 {
		next["session"] = session
	}
	return next
}

func EventMatchesContext(event Event, expected Context) bool {
	// The request id decides, when both sides carry one. A hub does not echo a
	// client-declared session id: it substitutes its own. Observed against a
	// live hub on 2026-09-03 -- sent "observe-me", every reply came back as
	// "71048b7f-e7b0-4360-8fb5-a03816f78617" -- so comparing session ids
	// rejected replies the request id had already identified as ours, and Ask
	// waited out its whole timeout while the hub had answered and emitted
	// ovos.utterance.handled.
	expectedRequest := RequestIDFromContext(expected)
	if expectedRequest != "" && event.RequestID() != "" {
		return event.RequestID() == expectedRequest
	}
	// No request id on one side or the other: fall back to the session, which
	// is all a caller had before request ids existed. Deliberately lenient --
	// a reply carrying no request id is not evidence either way.
	expectedSession := SessionIDFromContext(expected)
	if expectedSession != "" && event.SessionID() != "" && event.SessionID() != expectedSession {
		return false
	}
	return true
}

func SessionIDFromContext(context Context) string {
	session := sessionFromContext(context)
	if val, ok := session["session_id"].(string); ok {
		return val
	}
	if val, ok := context["session_id"].(string); ok {
		return val
	}
	return ""
}

func RequestIDFromContext(context Context) string {
	if val := requestIDFromMap(context); val != "" {
		return val
	}
	return requestIDFromMap(sessionFromContext(context))
}

func sessionFromContext(context Context) map[string]any {
	if context == nil {
		return map[string]any{}
	}
	if session, ok := context["session"].(map[string]any); ok {
		clone := map[string]any{}
		for key, val := range session {
			clone[key] = val
		}
		return clone
	}
	return map[string]any{}
}

func requestIDFromMap(values map[string]any) string {
	for _, key := range []string{"request_id", "thalovant_request_id", "correlation_id"} {
		if val, ok := values[key].(string); ok {
			return val
		}
	}
	return ""
}

func newID(prefix string) string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return prefix + "unknown"
	}
	return prefix + hex.EncodeToString(raw)
}

func (e Event) Lang() string {
	for _, v := range []any{e.Data["lang"], e.Context["lang"], sessionFromContext(e.Context)["lang"]} {
		if v != nil && fmt.Sprint(v) != "" {
			return fmt.Sprint(v)
		}
	}
	return ""
}
func (e Event) IsAudio() bool { return e.Name == EventAudioQueue }
func (e Event) HasAudio() bool {
	v, ok := e.Data["binary_data"].(string)
	return e.IsAudio() && ok && v != ""
}

// AudioBytes decodes embedded hex only. It never fetches a skill path or URL.
func (e Event) AudioBytes() ([]byte, error) { return e.AudioBytesWithLimit(MaxAudioClipBytes) }
func (e Event) AudioBytesWithLimit(maxBytes int) ([]byte, error) {
	encoded, ok := e.Data["binary_data"].(string)
	if !e.IsAudio() || !ok || encoded == "" {
		return nil, fmt.Errorf("missing embedded audio")
	}
	if maxBytes < 0 || (len(encoded)+1)/2 > maxBytes {
		return nil, fmt.Errorf("embedded audio exceeds byte limit")
	}
	var compact strings.Builder
	for _, c := range encoded {
		if strings.ContainsRune(" \t\n\r\v\f", c) {
			if compact.Len()%2 != 0 {
				return nil, fmt.Errorf("invalid embedded audio hex")
			}
			continue
		}
		compact.WriteRune(c)
	}
	decoded, err := hex.DecodeString(compact.String())
	if err != nil {
		return nil, fmt.Errorf("invalid embedded audio hex")
	}
	return decoded, nil
}
func (r Reply) Lang() string {
	for _, e := range r.Events {
		if lang := e.Lang(); lang != "" {
			return lang
		}
	}
	return ""
}
func (r Reply) HasAudio() bool {
	for _, e := range r.Events {
		if e.IsAudio() {
			return true
		}
	}
	return false
}
func (r Reply) MediaEvents() []Event {
	var result []Event
	for _, e := range r.Events {
		if e.IsAudio() || e.Name == EventSpeak || e.Name == EventOvosUtteranceSpeak {
			result = append(result, e)
		}
	}
	return result
}

type replyMediaBudget struct {
	chars, dropped int
	seen           map[string]bool
}

func (b *replyMediaBudget) accept(e Event) bool {
	if !e.IsAudio() {
		return true
	}
	// Maps retain their identity across duplicate deliveries, including value Event copies.
	key := fmt.Sprintf("%p", e.Data)
	if e.Data != nil && b.seen[key] {
		return false
	}
	encoded, ok := e.Data["binary_data"].(string)
	if !ok || len(encoded) > MaxAudioClipBytes*2 || b.chars+len(encoded) > MaxReplyMediaBytes*2 {
		b.dropped++
		return false
	}
	if b.seen == nil {
		b.seen = map[string]bool{}
	}
	b.seen[key] = true
	b.chars += len(encoded)
	return true
}
