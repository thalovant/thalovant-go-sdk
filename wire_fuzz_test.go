package thalovant

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Exercise the untrusted frame parser with compressed, truncated and malformed
// metadata. Every accepted frame must survive canonical encode/decode unchanged.
func FuzzHiveBinaryFrame(f *testing.F) {
	for _, seed := range [][]byte{nil, {0}, {0xff, 0xff}, {0x83, 2, '{', '}', 0x78, 0x9c}} {
		f.Add(seed)
	}
	seed, err := EncodeHiveBinaryFrame(HiveMessage{MsgType: "bus", Payload: map[string]any{"type": "speak", "data": map[string]any{"utterance": "hello"}}, Metadata: map[string]any{"query_id": "synthetic"}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		message, err := DecodeHiveBinaryFrame(data)
		if err != nil {
			return
		}
		encoded, err := EncodeHiveBinaryFrame(message)
		if err != nil {
			return
		} // canonical metadata can exceed the wire limit
		decoded, err := DecodeHiveBinaryFrame(encoded)
		if err != nil {
			t.Fatal(err)
		}
		left, _ := json.Marshal(nonNilMap(message.Payload))
		right, _ := json.Marshal(decoded.Payload)
		if string(left) != string(right) || !reflect.DeepEqual(nonNilMap(message.Metadata), decoded.Metadata) || message.MsgType != decoded.MsgType {
			t.Fatal("accepted frame lost payload, metadata or type")
		}
	})
}
