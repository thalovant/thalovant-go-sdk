package thalovant

// Binary frames, and the hive's own frame kinds.
//
// A hub answers speak:synth by rendering the utterance and sending the audio
// back, so a client with no synthesiser of its own can still speak; a file
// arrives the same way. The expectations are contracts/conformance's
// binary-vectors.json and mesh-vectors.json, and the frames themselves are
// binary-frames.json -- hivemind-bus-client's own encoder output, so this is
// tested against the wire a hub actually puts out rather than against a reading
// of the specification.

import (
	"bytes"
	"encoding/base64"
	"slices"
	"sort"
	"strconv"
	"testing"
)

func TestBinaryPayloadKindsAreTheOnesTheVectorsName(t *testing.T) {
	spec := conformanceVectors(t, "binary-vectors.json")
	named := map[string]string{}
	for wire, name := range BinaryPayloadKinds {
		named[strconv.Itoa(wire)] = name
	}
	for wire, name := range spec["payload_kinds"].(map[string]any) {
		if named[wire] != name.(string) {
			t.Fatalf("payload type %s: got %q, want %q", wire, named[wire], name)
		}
	}
	if len(named) != len(spec["payload_kinds"].(map[string]any)) {
		t.Fatalf("named %d payload types, the vectors name %d", len(named), len(spec["payload_kinds"].(map[string]any)))
	}
}

func TestAPayloadTypeNobodyNamedArrivesUnderItsNumber(t *testing.T) {
	// Only 0-15 can travel: the wire field is four bits. The naming has to hold
	// for every number all the same -- it is the last thing between a payload
	// type nobody has named yet and a frame that disappears.
	spec := conformanceVectors(t, "binary-vectors.json")
	for wire, name := range spec["unnamed_kind_names"].(map[string]any) {
		number, err := strconv.Atoi(wire)
		if err != nil {
			t.Fatalf("unnamed_kind_names key %q is not a number", wire)
		}
		if got := BinaryKindName(number); got != name.(string) {
			t.Fatalf("payload type %s: got %q, want %q", wire, got, name)
		}
	}
}

func TestTheReferenceEncodersFramesDecodeHere(t *testing.T) {
	frames := conformanceVectors(t, "binary-frames.json")
	for _, row := range frames["cases"].([]any) {
		test := row.(map[string]any)
		raw, err := base64.StdEncoding.DecodeString(test["frame"].(string))
		if err != nil {
			t.Fatalf("%v: %v", test["name"], err)
		}
		message, err := DecodeHiveBinaryFrame(raw)
		if err != nil {
			t.Fatalf("%v: %v", test["name"], err)
		}
		if message.MsgType != "bin" || message.Binary == nil {
			t.Fatalf("%v: got %q with binary %v", test["name"], message.MsgType, message.Binary)
		}
		if message.Binary.Kind != test["expected_kind"].(string) {
			t.Fatalf("%v: kind %q, want %q", test["name"], message.Binary.Kind, test["expected_kind"])
		}
		clip, err := base64.StdEncoding.DecodeString(test["expected_payload"].(string))
		if err != nil {
			t.Fatalf("%v: %v", test["name"], err)
		}
		if !bytes.Equal(message.Binary.Data, clip) {
			t.Fatalf("%v: the clip did not survive the decode", test["name"])
		}
		for key, want := range test["expected_metadata"].(map[string]any) {
			if message.Binary.Metadata[key] != want {
				t.Fatalf("%v: metadata %s = %v, want %v", test["name"], key, message.Binary.Metadata[key], want)
			}
		}
	}
}

func TestABinarizedBusFrameIsStillText(t *testing.T) {
	// Only BINARY carries bytes; every other type binarized on the wire is JSON
	// and has to keep decoding as it always did.
	frames := conformanceVectors(t, "binary-frames.json")
	raw, err := base64.StdEncoding.DecodeString(frames["bus_frame"].(string))
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodeHiveBinaryFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.MsgType != "bus" || message.Binary != nil {
		t.Fatalf("got %q with binary %v", message.MsgType, message.Binary)
	}
	if message.Payload["type"] != "speak" {
		t.Fatalf("payload %v", message.Payload)
	}
}

func TestEveryCaseTheBinaryVectorsDescribeDecodesAsItSays(t *testing.T) {
	spec := conformanceVectors(t, "binary-vectors.json")
	frames := conformanceVectors(t, "binary-frames.json")
	byName := map[string]map[string]any{}
	for _, row := range frames["cases"].([]any) {
		test := row.(map[string]any)
		byName[test["name"].(string)] = test
	}
	for _, row := range spec["cases"].([]any) {
		test := row.(map[string]any)
		frame, known := byName[test["name"].(string)]
		if !known {
			t.Fatalf("%v: the vectors describe a case the frames do not carry", test["name"])
		}
		raw, _ := base64.StdEncoding.DecodeString(frame["frame"].(string))
		message, err := DecodeHiveBinaryFrame(raw)
		if err != nil {
			t.Fatalf("%v: %v", test["name"], err)
		}
		expected := test["expected"].(map[string]any)
		if message.Binary.Kind != expected["kind"].(string) {
			t.Fatalf("%v: kind %q, want %q", test["name"], message.Binary.Kind, expected["kind"])
		}
		// An empty name is no name: rendering "" would put a blank filename in
		// front of somebody as though the hub had chosen it.
		for field, got := range map[string]string{
			"utterance": message.Binary.Utterance,
			"lang":      message.Binary.Lang,
			"file_name": message.Binary.FileName,
		} {
			want, _ := expected[field].(string)
			if got != want {
				t.Fatalf("%v: %s = %q, want %q", test["name"], field, got, want)
			}
		}
	}
}

func TestEveryKindTheMeshVectorsDeclareHasACase(t *testing.T) {
	// A declared kind with no case is a rule written down and never checked.
	spec := conformanceVectors(t, "mesh-vectors.json")
	seen := map[string]bool{}
	for _, row := range spec["cases"].([]any) {
		seen[row.(map[string]any)["kind"].(string)] = true
	}
	var missing []string
	for _, list := range []string{"kinds", "refused_kinds"} {
		for _, kind := range spec[list].([]any) {
			if !seen[kind.(string)] {
				missing = append(missing, kind.(string))
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("declared with no case: %v", missing)
	}
}

func TestOnlyTheMeshKindsAreSubscribable(t *testing.T) {
	spec := conformanceVectors(t, "mesh-vectors.json")
	for _, row := range spec["cases"].([]any) {
		test := row.(map[string]any)
		kind := test["kind"].(string)
		accepted := test["expected"].(map[string]any)["accepted"].(bool)
		if got := slices.Contains(HiveKinds, kind); got != accepted {
			t.Fatalf("%v: subscribable=%v, want %v", test["name"], got, accepted)
		}
	}
}
