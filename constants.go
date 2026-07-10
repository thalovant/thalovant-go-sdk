package thalovant

const (
	EventRecognizerLoopUtterance = "recognizer_loop:utterance"
	EventSpeak                   = "speak"
	EventOvosUtteranceSpeak      = "ovos.utterance.speak"
	EventUtteranceHandled        = "ovos.utterance.handled"
	EventIntentFailure           = "complete_intent_failure"
	EventPolicyDenied            = "hive.policy.denied"
	EventQueryTimeout            = "hive.query.timeout"
	DefaultUserAgent             = "ThalovantGoSDK/0.2.15"
)

var failureEvents = map[string]struct{}{
	EventIntentFailure: {},
	EventPolicyDenied:  {},
	EventQueryTimeout:  {},
}
