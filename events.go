package thalovant

import (
	"crypto/rand"
	"encoding/hex"
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
	Text         string
	Utterances   []string
	Handled      bool
	OK           bool
	SessionID    string
	RequestID    string
	Events       []Event
	FailureEvent *Event
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
	expectedSession := SessionIDFromContext(expected)
	if expectedSession != "" && event.SessionID() != "" && event.SessionID() != expectedSession {
		return false
	}
	expectedRequest := RequestIDFromContext(expected)
	if expectedRequest != "" && event.RequestID() != "" && event.RequestID() != expectedRequest {
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
