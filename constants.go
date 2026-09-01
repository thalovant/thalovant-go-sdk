package thalovant

const (
	EventRecognizerLoopUtterance = "recognizer_loop:utterance"
	EventSpeak                   = "speak"
	EventOvosUtteranceSpeak      = "ovos.utterance.speak"
	EventUtteranceHandled        = "ovos.utterance.handled"
	// EventIntentUnmatched is the current OVOS bus event fired when an utterance
	// matches no intent. EventIntentFailure is the legacy Mycroft name for the
	// same signal; both are kept so old and new runtimes are recognised.
	EventIntentUnmatched = "ovos.intent.unmatched"
	EventIntentFailure   = "complete_intent_failure"
	EventPolicyDenied    = "hive.policy.denied"
	EventQueryTimeout    = "hive.query.timeout"
	DefaultUserAgent     = userAgent
)

var failureEvents = map[string]struct{}{
	EventIntentUnmatched: {},
	EventIntentFailure:   {},
	EventPolicyDenied:    {},
	EventQueryTimeout:    {},
}
