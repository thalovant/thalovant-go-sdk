package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestPythonRequestHints(t *testing.T) {
	base := Context{"session": map[string]any{"session_id": "kept", "pipeline": []string{"old"}}}
	location := BuildLocation(LocationOptions{City: " Montréal ", Country: " ca ", Latitude: "45.5", Longitude: "-73.5"})
	result := RequestContext(base, RequestContextOptions{STTLang: " fr ", Pipeline: []string{" ", "intent"}, Location: location})
	if result["stt_lang"] != "fr" || location["country_code"] != "CA" {
		t.Fatal(result)
	}
	if !reflect.DeepEqual(base["session"].(map[string]any)["pipeline"], []string{"old"}) {
		t.Fatal("mutated caller")
	}
	if RequestContext(nil, RequestContextOptions{}) != nil || BuildLocation(LocationOptions{}) != nil {
		t.Fatal("empty hints")
	}
	withoutPipeline := RequestContext(base, RequestContextOptions{STTLang: "fr"})
	withoutPipeline["session"].(map[string]any)["session_id"] = "changed"
	if base["session"].(map[string]any)["session_id"] != "kept" {
		t.Fatal("context helper shared the caller's session map")
	}
	for _, pair := range [][2]any{{0, 0}, {91, 0}, {0, 181}, {"NaN", 1}, {"bad", 2}} {
		if _, ok := BuildLocation(LocationOptions{City: "Toronto", Latitude: pair[0], Longitude: pair[1]})["coordinate"]; ok {
			t.Fatal(pair)
		}
	}
}

type configSnapshotTransport func(*http.Request) (*http.Response, error)

func (f configSnapshotTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConfigSnapshotsCompletePayloadBeforeReads(t *testing.T) {
	config := map[string]any{"nested": map[string]any{"value": "original"}, "number": int64(9007199254740993)}
	personas := map[string]any{"default": map[string]any{"name": "original"}}
	writes := 0
	api := NewControlPlane("https://example.test", "test")
	api.HTTPClient = &http.Client{Transport: configSnapshotTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 200, "{}"
		if r.Method == "GET" {
			config["nested"].(map[string]any)["value"] = "changed"
			personas["default"].(map[string]any)["name"] = "changed"
			body = fmt.Sprintf(`{"config":{"stored":9007199254740993},"revision":"%064x"}`, 1)
		} else {
			var payload map[string]any
			decoder := json.NewDecoder(r.Body)
			decoder.UseNumber()
			if err := decoder.Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["config"].(map[string]any)["nested"].(map[string]any)["value"] != "original" || payload["personas"].(map[string]any)["default"].(map[string]any)["name"] != "original" {
				t.Fatal(payload)
			}
			if payload["config"].(map[string]any)["number"] != json.Number("9007199254740993") {
				t.Fatal("lost integer precision", payload)
			}
			if payload["config"].(map[string]any)["stored"] != json.Number("9007199254740993") {
				t.Fatal("lost stored integer precision", payload)
			}
			writes++
			if writes == 1 {
				status = 412
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	if _, err := api.UpdateRuntimeGroupConfig(context.Background(), "g", config, RuntimeGroupConfigOptions{Personas: personas}); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatal(writes)
	}
}
func TestPythonSpeakableRanking(t *testing.T) {
	if got := Speakable("did i (already |)ask (about|for|to|) {thing}", nil); got != "did i ask about thing" {
		t.Fatal(got)
	}
	i := HubIntent{Phrases: map[string][]string{"en-us": {"{x}", "a complete sentence", "[please]", "(x|y)", "x"}}}
	if got := i.ExamplesWithOptions("en-us", 2, IntentExampleOptions{Speakable: true}); !reflect.DeepEqual(got, []string{"a complete sentence", "x"}) {
		t.Fatal(got)
	}
}
func TestPythonAudioContractEdgeCases(t *testing.T) {
	// Python bounds encoded input before decoding; whitespace consumes that budget.
	if _, err := (Event{Name: EventAudioQueue, Data: Data{"binary_data": "00 "}}).AudioBytesWithLimit(1); err == nil {
		t.Fatal("encoded upper bound was not enforced")
	}
	empty, err := (Event{Name: EventAudioQueue, Data: Data{"binary_data": " \t"}}).AudioBytes()
	if err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
	var budget replyMediaBudget
	first := Event{Name: EventAudioQueue, Data: Data{"binary_data": "00"}}
	second := Event{Name: EventAudioQueue, Data: Data{"binary_data": "00"}}
	if !budget.accept(first) || !budget.accept(second) || budget.accept(first) || budget.dropped != 0 {
		t.Fatal("distinct clips must retain repetitions; duplicate object dispatch is ignored")
	}
	intent := HubIntent{Phrases: map[string][]string{"en-us": {"{name}", "a complete sentence"}}}
	if got := intent.ExamplesWithOptions("en-us", 1, IntentExampleOptions{Speakable: true, Slots: map[string]string{"name": "Ada"}}); !reflect.DeepEqual(got, []string{"a complete sentence"}) {
		t.Fatal(got)
	}
}
func TestPythonAudioBoundsAndOrdering(t *testing.T) {
	e := Event{Name: EventAudioQueue, Data: Data{"binary_data": "00 ff\n10", "lang": "fr"}}
	bytes, err := e.AudioBytes()
	if err != nil || !reflect.DeepEqual(bytes, []byte{0, 255, 16}) || e.Lang() != "fr" {
		t.Fatal(bytes, err)
	}
	for _, value := range []any{nil, "", "0", "0 0", "gg", "https://example.com", "00\u00a0ff"} {
		if _, err := (Event{Name: EventAudioQueue, Data: Data{"binary_data": value}}).AudioBytes(); err == nil {
			t.Fatal(value)
		}
	}
	var budget replyMediaBudget
	if !budget.accept(e) || budget.accept(e) {
		t.Fatal("duplicate delivery")
	}
	clip := strings.Repeat("00", MaxAudioClipBytes)
	for j := 0; j < 4; j++ {
		budget.accept(Event{Name: EventAudioQueue, Data: Data{"binary_data": clip}})
	}
	budget.accept(Event{Name: EventAudioQueue, Data: Data{"binary_data": clip + "00"}})
	if budget.dropped != 2 {
		t.Fatal(budget.dropped)
	}
	r := Reply{Events: []Event{e, {Name: EventSpeak, Data: Data{"utterance": "hello"}}}}
	if r.Lang() != "fr" || !r.HasAudio() || len(r.MediaEvents()) != 2 {
		t.Fatal(r)
	}
}
func TestPythonConfigConflictPreservesConcurrentKeys(t *testing.T) {
	reads, writes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			reads++
			_, _ = fmt.Fprintf(w, `{"config":{"nested":{"original":true,"concurrent":%t}},"revision":"%064x"}`, reads > 1, reads)
			return
		}
		if r.Method != "PUT" {
			t.Error(r.Method)
		}
		writes++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if writes == 1 {
			w.WriteHeader(412)
			return
		}
		nested := body["config"].(map[string]any)["nested"].(map[string]any)
		if nested["original"] != true || nested["concurrent"] != true || nested["caller"] != true || body["expected_revision"] != fmt.Sprintf("%064x", 2) {
			t.Error(body)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	delta := map[string]any{"nested": map[string]any{"caller": true}}
	_, err := NewControlPlane(server.URL, "test").UpdateRuntimeGroupConfig(context.Background(), "x", delta, RuntimeGroupConfigOptions{})
	if err != nil || reads != 2 || writes != 2 || len(delta["nested"].(map[string]any)) != 1 {
		t.Fatal(err, reads, writes, delta)
	}
}
func TestPythonConfigFailClosedAndBoundedRetries(t *testing.T) {
	for _, status := range []int{400, 401, 403, 405, 409, 412, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			reads, writes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					reads++
					_, _ = fmt.Fprintf(w, `{"config":{},"revision":"%064x"}`, 1)
					return
				}
				writes++
				if r.Method != "PUT" {
					t.Error(r.Method)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			_, err := NewControlPlane(server.URL, "test").UpdateRuntimeGroupConfig(context.Background(), "x", nil, RuntimeGroupConfigOptions{})
			var apiError *APIError
			expected := 1
			if status == 412 {
				expected = 3
			}
			if !errors.Is(err, ErrAPI) || !errors.As(err, &apiError) || apiError.StatusCode != status || reads != expected || writes != expected {
				t.Fatal(err, reads, writes)
			}
		})
	}
	for _, body := range []string{`{"config":{}}`, `{"config":{},"revision":"bad"}`, fmt.Sprintf(`{"config":[],"revision":"%064x"}`, 1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Error("unsafe write")
			}
			_, _ = w.Write([]byte(body))
		}))
		_, err := NewControlPlane(server.URL, "test").UpdateRuntimeGroupConfig(context.Background(), "x", nil, RuntimeGroupConfigOptions{})
		server.Close()
		if err == nil {
			t.Fatal(body)
		}
	}
}
