package thalovant

import "testing"

// A hub substitutes its own session id; the request id is what correlates.
//
// Observed against a live hub on 2026-09-03: a client declaring
// session_id="observe-me" gets every reply back carrying the hub's own uuid.
// Comparing session ids rejected replies the request id had already identified
// as ours, so Ask waited out its whole timeout while the hub had answered.
// Verified across a matched skill, an unmatched fallback and a French
// utterance: the request id is present and equal on every event in all three.

func natEvent(sessionID, requestID string) Event {
	ctx := Context{}
	if sessionID != "" {
		ctx["session"] = map[string]any{"session_id": sessionID}
	}
	if requestID != "" {
		ctx["request_id"] = requestID
	}
	return Event{Name: EventUtteranceHandled, Data: Data{}, Context: ctx}
}

func natAsked(sessionID, requestID string) Context {
	return ContextWithCorrelation(Context{}, sessionID, "", "", requestID)
}

func TestMatchingRequestIDWinsOverSubstitutedSession(t *testing.T) {
	e := natEvent("71048b7f-e7b0-4360-8fb5-a03816f78617", "req-1")
	if !EventMatchesContext(e, natAsked("observe-me", "req-1")) {
		t.Fatal("a reply whose request id matches must be accepted despite a substituted session")
	}
}

func TestWrongRequestIDRejectedEvenIfSessionsAgree(t *testing.T) {
	if EventMatchesContext(natEvent("same", "req-2"), natAsked("same", "req-1")) {
		t.Fatal("a mismatched request id must still be rejected")
	}
}

func TestWithoutRequestIDsTheSessionStillDecides(t *testing.T) {
	if !EventMatchesContext(natEvent("s1", ""), natAsked("s1", "")) {
		t.Fatal("matching sessions must be accepted when neither side has a request id")
	}
	if EventMatchesContext(natEvent("s2", ""), natAsked("s1", "")) {
		t.Fatal("mismatched sessions must be rejected when neither side has a request id")
	}
}

func TestReplyWithoutRequestIDFallsBackToSession(t *testing.T) {
	if !EventMatchesContext(natEvent("s1", ""), natAsked("s1", "req-1")) {
		t.Fatal("an unlabelled reply on the right session must be accepted")
	}
}
