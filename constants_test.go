package thalovant

import "testing"

// OVOS renamed the "no intent matched" bus event from the legacy Mycroft
// "complete_intent_failure" to "ovos.intent.unmatched" (upstream issue #22).
// Both names must classify as a terminal failure so the interaction/query loop
// fails promptly instead of waiting out its whole timeout.
func TestIntentUnmatchedAndLegacyNameAreFailures(t *testing.T) {
	failures := map[string]string{
		"current OVOS name":   EventIntentUnmatched,
		"legacy Mycroft name": EventIntentFailure,
	}
	for label, name := range failures {
		if name == "" {
			t.Fatalf("%s constant is empty", label)
		}
		if !(Event{Name: name}).IsFailure() {
			t.Fatalf("%s %q must be classified as a failure", label, name)
		}
	}

	if EventIntentUnmatched != "ovos.intent.unmatched" {
		t.Fatalf("EventIntentUnmatched must be the current OVOS name, got %q", EventIntentUnmatched)
	}
	if EventIntentFailure != "complete_intent_failure" {
		t.Fatalf("legacy EventIntentFailure must be retained, got %q", EventIntentFailure)
	}

	// A genuinely handled/unknown event stays a non-failure.
	if (Event{Name: EventUtteranceHandled}).IsFailure() {
		t.Fatal("EventUtteranceHandled must not be classified as a failure")
	}
}
