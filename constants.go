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
	// The hub runtime's intent manifest (OVOS-INTENT-4 section 10) and the
	// engines' own manifests, read by Client.Intents, Client.ListIntents and
	// Client.DescribeIntent. See intents.go.
	EventIntentList             = "ovos.intent.list"
	EventIntentListResponse     = "ovos.intent.list.response"
	EventIntentDescribe         = "ovos.intent.describe"
	EventIntentDescribeResponse = "ovos.intent.describe.response"
	EventAdaptManifestGet       = "intent.service.adapt.manifest.get"
	EventAdaptManifest          = "intent.service.adapt.manifest"
	EventPadatiousManifestGet   = "intent.service.padatious.manifest.get"
	EventPadatiousManifest      = "intent.service.padatious.manifest"
)

var failureEvents = map[string]struct{}{
	EventIntentUnmatched: {},
	EventIntentFailure:   {},
	EventPolicyDenied:    {},
	EventQueryTimeout:    {},
}
