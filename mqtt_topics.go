package thalovant

import (
	"fmt"
	"strings"
)

type MqttTopicSet struct {
	Inbound  string
	Outbound string
	Status   string
}

// MQTTTopicsForIdentity derives the data-plane topic set from the identity's
// MQTT credentials. TopicPrefix is the full base -- hivemind/<hub-id>/<access-key>
// -- and the channels append a fixed suffix to it: publish requests go to
// <prefix>/in, subscribe replies arrive on <prefix>/out, and the retained
// presence/LWT lives on <prefix>/status.
func MQTTTopicsForIdentity(identity Identity) (MqttTopicSet, error) {
	if identity.MQTT == nil {
		return MqttTopicSet{}, fmt.Errorf("%w: identity does not include MQTT broker credentials", ErrConnection)
	}
	base := strings.Trim(strings.TrimSpace(identity.MQTT.TopicPrefix), "/")
	if base == "" {
		return MqttTopicSet{}, fmt.Errorf("%w: MQTT credentials must include topic_prefix", ErrConnection)
	}
	// Reject MQTT wildcards (# and +) and ASCII control characters (< 0x20,
	// including NUL): a topic_prefix carrying them would build a malformed or
	// silently widened subscribe/publish topic.
	for i := 0; i < len(base); i++ {
		if c := base[i]; c == '#' || c == '+' || c < 0x20 {
			return MqttTopicSet{}, fmt.Errorf("%w: MQTT topic_prefix contains characters that are not valid in an MQTT topic", ErrConnection)
		}
	}
	return MqttTopicSet{
		Inbound:  base + "/in",
		Outbound: base + "/out",
		Status:   base + "/status",
	}, nil
}
