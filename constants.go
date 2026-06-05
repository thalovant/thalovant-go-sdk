package thalovant

const (
	EventRecognizerLoopUtterance = "recognizer_loop:utterance"
	EventSpeak                   = "speak"
	EventUtteranceHandled        = "ovos.utterance.handled"
	EventIntentFailure           = "complete_intent_failure"
	EventPolicyDenied            = "hive.policy.denied"
	DefaultUserAgent             = "ThalovantGoSDK/0.1.0"
)

var failureEvents = map[string]struct{}{
	EventIntentFailure: {},
	EventPolicyDenied:  {},
}
