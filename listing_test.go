package thalovant

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestListingPythonGoldenCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/listing-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Cases []struct {
			Kind, Text, Lang string
			Phrases          []string
			Expected         json.RawMessage
		}
	}
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, row := range data.Cases {
		if row.Kind == "rank" {
			var want []string
			if err = json.Unmarshal(row.Expected, &want); err != nil {
				t.Fatal(err)
			}
			if got := defaultListing.Rank(row.Phrases, row.Lang); !reflect.DeepEqual(got, want) {
				t.Errorf("rank %s: %v != %v", row.Lang, got, want)
			}
			continue
		}
		var want string
		if err = json.Unmarshal(row.Expected, &want); err != nil {
			t.Fatal(err)
		}
		got := AsSentence(row.Text, row.Lang)
		if row.Kind == "speakable" {
			got = SpeakableWithLanguage(row.Text, nil, row.Lang)
		}
		if got != want {
			t.Errorf("%s %s %q: %q != %q", row.Kind, row.Lang, row.Text, got, want)
		}
	}
}
func TestListingOVOSLanguageGoldenCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/language-matching-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Cases []struct {
			Target    string
			Available []string
			Expected  *string
		}
	}
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, row := range data.Cases {
		got, ok := ClosestLanguage(row.Target, row.Available)
		if row.Expected == nil {
			if ok {
				t.Errorf("%s %v: unexpected %s", row.Target, row.Available, got)
			}
		} else if !ok || got != *row.Expected {
			t.Errorf("%s %v: %s,%v != %s", row.Target, row.Available, got, ok, *row.Expected)
		}
	}
}
func TestListingSelectedLocaleAndRenderedLimit(t *testing.T) {
	i := HubIntent{Languages: []string{"fr-FR", "en-US"}, Phrases: map[string][]string{"fr-FR": {"volume {level} pour cent"}, "en-US": {"volume {level} percent"}}}
	if got := i.ExamplesWithOptions("", 2, IntentExampleOptions{Sentence: true}); !reflect.DeepEqual(got, []string{"Volume cinquante pour cent."}) {
		t.Fatal(got)
	}
	if got := i.ExamplesWithOptions("en-GB", 2, IntentExampleOptions{Sentence: true, Slots: map[string]string{"level": "ten"}}); !reflect.DeepEqual(got, []string{"Volume ten percent."}) {
		t.Fatal(got)
	}
	i.Phrases = map[string][]string{"en-US": {"[please]", "(repeat|say) that (again|)", "[please] repeat that", "volume [to] {level} percent"}}
	i.Languages = nil
	for _, limit := range []int{0, 2} {
		if got := i.ExamplesWithOptions("en-US", limit, IntentExampleOptions{Sentence: true}); !reflect.DeepEqual(got, []string{"Repeat that.", "Volume fifty percent."}) {
			t.Fatal(got)
		}
	}
	if got := i.Examples("en-US", 0); !reflect.DeepEqual(got, i.Phrases["en-US"]) {
		t.Fatal(got)
	}
}
func TestListingCustomRulesSnapshotAndOptionalQuestions(t *testing.T) {
	data := ListingData{SentenceEnds: ".!?", Languages: map[string]ListingLanguage{"xq": {QuestionPatterns: []string{"(?i)^is it", "(?m)^can it"}, SlotExamples: map[string]string{"thing": "the widget"}}}}
	listing, err := NewListingRules(&data)
	if err != nil {
		t.Fatal(err)
	}
	data.Languages["xq"].QuestionPatterns[0] = "never"
	data.Languages["xq"].SlotExamples["thing"] = "changed"
	if got := listing.AsSentence("is it ready", "xq"); got != "Is it ready?" {
		t.Fatal(got)
	}
	if got := listing.AsSentence("can it work", "xq"); got != "Can it work?" {
		t.Fatal(got)
	}
	if got := listing.Speakable("open {thing}", nil, "xq-ZZ"); got != "open the widget" {
		t.Fatal(got)
	}
	copy := listing.LanguageData("xq")
	copy.QuestionPatterns[0] = "broken"
	if got := listing.AsSentence("is it ready", "xq"); got != "Is it ready?" {
		t.Fatal(got)
	}
	anywhere, err := NewListingRules(&ListingData{SentenceEnds: ".!?", Languages: map[string]ListingLanguage{"xq": {QuestionWordsAnywhere: []string{"plim"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := anywhere.AsSentence("go plim now", "xq"); got != "Go plim now?" {
		t.Fatal(got)
	}
	if got := anywhere.AsSentence("go home", "xq"); got != "Go home." {
		t.Fatal(got)
	}
	if got := listing.AsSentence("what time is it", "en"); got != "What time is it" {
		t.Fatal(got)
	}
}
func TestListingNoDataAndRegexBounds(t *testing.T) {
	bare, err := NewListingRules(nil)
	if err != nil || bare.Available() {
		t.Fatal(err)
	}
	if got := bare.AsSentence("do i need a jacket", "en"); got != "Do i need a jacket" {
		t.Fatal(got)
	}
	if got := bare.Speakable("volume [to] {level} percent", nil, "en"); got != "volume level percent" {
		t.Fatal(got)
	}
	if _, err := NewListingRules(&ListingData{Languages: map[string]ListingLanguage{"xq": {QuestionPatterns: []string{"("}}}}); err == nil {
		t.Fatal("invalid regex accepted")
	}
	expensive, err := NewListingRules(&ListingData{SentenceEnds: ".!?", Languages: map[string]ListingLanguage{"xq": {QuestionPatterns: []string{"(a+)+$"}}}})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("a", 100000) + "x"
	started := time.Now()
	if _, err := expensive.Asks(text, "xq"); err == nil {
		t.Fatal("expected bounded matching error")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("regex exceeded bounded matching window")
	}
	if got := expensive.AsSentence(text, "xq"); got != "A"+text[1:] {
		t.Fatal("failed regex guessed punctuation")
	}
}
