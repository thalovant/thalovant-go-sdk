package thalovant

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

type MqttTopicSet struct {
	C2S    string
	S2C    string
	Status string
}

func MQTTTopicsForIdentity(identity Identity) (MqttTopicSet, error) {
	if identity.MQTT == nil {
		return MqttTopicSet{}, fmt.Errorf("%w: identity does not include MQTT broker credentials", ErrConnection)
	}
	credentials := identity.MQTT
	satelliteID := identity.AccessKey
	if credentials.HashTopics {
		digest := sha256.Sum256([]byte(identity.AccessKey))
		satelliteID = hex.EncodeToString(digest[:])[:16]
	}
	if credentials.C2STopic != "" && credentials.S2CTopic != "" {
		status := credentials.StatusTopic
		if status == "" {
			status = siblingMQTTTopic(credentials.C2STopic, "status")
		}
		return MqttTopicSet{C2S: credentials.C2STopic, S2C: credentials.S2CTopic, Status: status}, nil
	}
	raw := strings.Trim(strings.TrimSpace(credentials.TopicPrefix), "/")
	base := ""
	if raw != "" {
		switch {
		case strings.Contains(raw, "/c2s/"):
			return MqttTopicSet{C2S: raw, S2C: siblingMQTTTopic(raw, "s2c"), Status: siblingMQTTTopic(raw, "status")}, nil
		case strings.Contains(raw, "/s2c/"):
			return MqttTopicSet{C2S: siblingMQTTTopic(raw, "c2s"), S2C: raw, Status: siblingMQTTTopic(raw, "status")}, nil
		case strings.Contains(raw, "/status/"):
			return MqttTopicSet{C2S: siblingMQTTTopic(raw, "c2s"), S2C: siblingMQTTTopic(raw, "s2c"), Status: raw}, nil
		default:
			parts := strings.Split(raw, "/")
			last := parts[len(parts)-1]
			if last == identity.AccessKey || last == credentials.Username || last == satelliteID {
				base = strings.Join(parts[:len(parts)-1], "/")
			} else {
				base = raw
			}
		}
	} else if credentials.HubID != "" {
		base = "hivemind/" + strings.Trim(credentials.HubID, "/")
	}
	if base == "" {
		return MqttTopicSet{}, fmt.Errorf("%w: MQTT credentials must include topic_prefix, hub_id, or explicit c2s/s2c topics", ErrConnection)
	}
	return MqttTopicSet{
		C2S:    base + "/c2s/" + satelliteID,
		S2C:    base + "/s2c/" + satelliteID,
		Status: base + "/status/" + satelliteID,
	}, nil
}

func siblingMQTTTopic(topic string, segment string) string {
	for _, current := range []string{"/c2s/", "/s2c/", "/status/"} {
		if strings.Contains(topic, current) {
			return strings.Replace(topic, current, "/"+segment+"/", 1)
		}
	}
	return topic
}
