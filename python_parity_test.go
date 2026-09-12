package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	for _, pair := range [][2]any{{0, 0}, {91, 0}, {0, 181}, {"NaN", 1}, {"bad", 2}} {
		if _, ok := BuildLocation(LocationOptions{City: "Toronto", Latitude: pair[0], Longitude: pair[1]})["coordinate"]; ok {
			t.Fatal(pair)
		}
	}
}
func TestPythonSpeakableRanking(t *testing.T) {
	if got := Speakable("did i (already |)ask (about|for|to|) {thing}", nil); got != "did i ask about thing" {
		t.Fatal(got)
	}
	i := HubIntent{Phrases: map[string][]string{"en-us": {"{x}", "a complete sentence", "[please]", "(x|y)", "x"}}}
	if got := i.ExamplesWithOptions("en-us", 2, IntentExampleOptions{Speakable: true}); !reflect.DeepEqual(got, []string{"x", "a complete sentence"}) {
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
