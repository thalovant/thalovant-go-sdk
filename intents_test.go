package thalovant

// The intent inventory, against a hub that behaves like the one observed.
//
// Shapes copied from a live runtime on 2026-09-05: ovos.intent.list.response
// rows, ovos.intent.describe.response definitions carrying samples as the
// skill's locale files wrote them, hive.policy.denied for a type the
// connection may not publish, and every reply delivered twice.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	weatherSkill = "thalovant-skill-weather.thalovant"
	shadowSkill  = "thalovant-skill-custos-shadow.thalovant"
)

// intentSample is one registration the fake hub holds: a skill's intent in
// one language, with the sentences its locale files wrote. An empty method
// is a template intent.
type intentSample struct {
	lang    string
	skillID string
	name    string
	method  string
	samples []string
}

// observedRegistrations is what the hub registered. Weather speaks both
// languages; the shadow skill only English.
var observedRegistrations = []intentSample{
	{lang: "en-us", skillID: weatherSkill, name: "current.weather", samples: []string{
		"what is the weather",
		"what is the weather in {location}",
		"how is it outside",
	}},
	{lang: "en-us", skillID: shadowSkill, name: "custos.incidents", samples: []string{
		"are there incidents",
		"any incidents",
	}},
	{lang: "fr-fr", skillID: weatherSkill, name: "current.weather", samples: []string{
		"quel temps fait-il",
		"quelle est la météo à {location}",
		"quelle est la météo",
	}},
}

var allowedTypes = []string{EventRecognizerLoopUtterance, EventSpeak}

type emittedQuery struct {
	eventType string
	data      Data
	context   Context
}

// intentHub is a hub session: it answers the manifest, or refuses it, twice
// over. Replies are delivered from their own goroutine the way a transport's
// read loop does, so a query never blocks on its own answers.
type intentHub struct {
	registrations     []intentSample
	refuse            map[string]bool
	silent            map[string]bool
	definitionsInList bool
	echoRequestID     bool
	repeats           int
	deaf              func(eventType string, data Data) bool
	events            chan Event
	mu                sync.Mutex
	connected         bool
	emitted           []emittedQuery
}

func newIntentHub() *intentHub {
	return &intentHub{
		registrations: observedRegistrations,
		refuse:        map[string]bool{},
		silent:        map[string]bool{},
		echoRequestID: true,
		repeats:       2,
		events:        make(chan Event, 256),
	}
}

func (h *intentHub) Connect(context.Context) error {
	h.connected = true
	return nil
}

func (h *intentHub) Disconnect(context.Context) error {
	h.connected = false
	return nil
}

func (h *intentHub) Healthcheck() TransportHealth {
	return TransportHealth{Connected: h.connected, HandshakeComplete: h.connected, TransportAlive: h.connected}
}

func (h *intentHub) ConnectionInfo() TransportConnectionInfo {
	return TransportConnectionInfo{Phase: ConnectionReady}
}

func (h *intentHub) Events() <-chan Event {
	return h.events
}

func (h *intentHub) EmitBus(_ context.Context, eventType string, data Data, eventContext Context) error {
	h.mu.Lock()
	h.emitted = append(h.emitted, emittedQuery{eventType: eventType, data: cloneMap(data), context: MergeContext(eventContext, nil)})
	h.mu.Unlock()
	if h.refuse[eventType] {
		h.deliver(EventPolicyDenied, Data{
			"denied_type": eventType,
			"code":        "acl_disallowed_type",
			"reason":      eventType + " not in allowed_types",
			"data":        map[string]any{"msg_type": eventType, "allowed": anySlice(allowedTypes)},
		}, eventContext)
		return nil
	}
	if h.silent[eventType] || (h.deaf != nil && h.deaf(eventType, data)) {
		return nil
	}
	lang := stringValue(data["lang"])
	switch eventType {
	case EventIntentList:
		rows := []any{}
		for _, sample := range h.registrations {
			if sample.lang != lang {
				continue
			}
			row := map[string]any{
				"skill_id":    sample.skillID,
				"intent_name": sample.name,
				"lang":        storedLanguage(lang),
				"method":      sampleMethod(sample),
				"enabled":     true,
				"session_id":  "default",
			}
			if h.definitionsInList && data["include_definitions"] == true {
				row["definition"] = definitionOf(sample)
			}
			rows = append(rows, row)
		}
		h.deliver(EventIntentListResponse, Data{"ok": true, "intents": rows}, eventContext)
	case EventIntentDescribe:
		definitions := []any{}
		for _, sample := range h.registrations {
			if sample.lang != lang || sample.skillID != data["skill_id"] || sample.name != data["intent_name"] {
				continue
			}
			definitions = append(definitions, map[string]any{"method": sampleMethod(sample), "definition": definitionOf(sample)})
		}
		// Keyword registrations come first, as the hub orders them.
		sort.SliceStable(definitions, func(a, b int) bool {
			return definitions[a].(map[string]any)["method"] == "keyword" && definitions[b].(map[string]any)["method"] != "keyword"
		})
		if len(definitions) == 0 {
			h.deliver(EventIntentDescribeResponse, Data{"ok": false, "error": "unknown intent"}, eventContext)
			return nil
		}
		h.deliver(EventIntentDescribeResponse, Data{"ok": true, "definitions": definitions}, eventContext)
	case EventAdaptManifestGet:
		h.deliver(EventAdaptManifest, Data{"intents": []any{}}, eventContext)
	case EventPadatiousManifestGet:
		seen := map[string]struct{}{}
		names := []any{}
		for _, sample := range h.registrations {
			id := sample.skillID + ":" + sample.name
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			names = append(names, id)
		}
		sort.Slice(names, func(a, b int) bool { return names[a].(string) < names[b].(string) })
		h.deliver(EventPadatiousManifest, Data{"intents": names}, eventContext)
	}
	return nil
}

// deliver hands a reply to the client, repeats times, with the request
// context echoed unless the hub is one that drops the id.
func (h *intentHub) deliver(name string, data Data, eventContext Context) {
	replyContext := MergeContext(eventContext, nil)
	if !h.echoRequestID {
		session := sessionFromContext(replyContext)
		delete(session, "request_id")
		replyContext["session"] = session
		delete(replyContext, "request_id")
		delete(replyContext, "thalovant_request_id")
	}
	go func() {
		for i := 0; i < h.repeats; i++ {
			h.events <- Event{Name: name, Data: data, Context: replyContext}
		}
	}()
}

func (h *intentHub) queries(eventType string) []emittedQuery {
	h.mu.Lock()
	defer h.mu.Unlock()
	var matching []emittedQuery
	for _, query := range h.emitted {
		if query.eventType == eventType {
			matching = append(matching, query)
		}
	}
	return matching
}

// storedLanguage is how the runtime standardises the tag it stores:
// "fr-fr" is answered as "fr-FR".
func storedLanguage(lang string) string {
	if lang == "fr-fr" {
		return "fr-FR"
	}
	return lang
}

func sampleMethod(sample intentSample) string {
	if sample.method == "" {
		return "template"
	}
	return sample.method
}

func definitionOf(sample intentSample) map[string]any {
	definition := map[string]any{
		"skill_id":    sample.skillID,
		"intent_name": sample.name,
		"lang":        sample.lang,
	}
	if sampleMethod(sample) == "template" {
		definition["samples"] = anySlice(sample.samples)
		definition["blacklist"] = []any{}
		definition["slot_blacklist"] = map[string]any{}
	} else {
		definition["keywords"] = anySlice(sample.samples)
	}
	return definition
}

func intentClient(hub *intentHub) *Client {
	return &Client{Identity: Identity{SiteID: "site"}, Transport: hub}
}

func intentByID(t *testing.T, inventory HubIntentInventory, id string) HubIntent {
	t.Helper()
	for _, intent := range inventory.Intents() {
		if intent.ID() == id {
			return intent
		}
	}
	t.Fatalf("inventory has no intent %q: %+v", id, inventory)
	return HubIntent{}
}

func intentIDs(inventory HubIntentInventory) []string {
	var ids []string
	for _, intent := range inventory.Intents() {
		ids = append(ids, intent.ID())
	}
	return ids
}

func boolPointer(value bool) *bool {
	return &value
}

func TestIntentsCarryTheSentencesPerLanguage(t *testing.T) {
	inventory, err := intentClient(newIntentHub()).Intents(context.Background(), []string{"en-us", "fr-fr"})
	if err != nil {
		t.Fatal(err)
	}

	if inventory.Source != IntentSourceManifest || len(inventory.Denied) != 0 {
		t.Fatalf("expected a manifest inventory with nothing denied, got source %q denied %v", inventory.Source, inventory.Denied)
	}
	if !reflect.DeepEqual(inventory.Languages, []string{"en-us", "fr-fr"}) {
		t.Fatalf("unexpected languages: %v", inventory.Languages)
	}
	skillIDs := []string{}
	for _, skill := range inventory.Skills {
		skillIDs = append(skillIDs, skill.SkillID)
	}
	if !reflect.DeepEqual(skillIDs, []string{shadowSkill, weatherSkill}) {
		t.Fatalf("skills must be sorted by id, got %v", skillIDs)
	}
	weather := inventory.Skills[1].Intents[0]
	if weather.ID() != weatherSkill+":current.weather" || weather.Engine != "padatious" || !weather.Enabled {
		t.Fatalf("unexpected weather intent: %+v", weather)
	}
	french := []string{"quel temps fait-il", "quelle est la météo à {location}", "quelle est la météo"}
	if !reflect.DeepEqual(weather.PhrasesFor("fr-FR"), french) {
		t.Fatalf("PhrasesFor must fold the tag's case, got %v", weather.PhrasesFor("fr-FR"))
	}
	if !reflect.DeepEqual(inventory.Skills[1].Languages(), []string{"en-us", "fr-fr"}) {
		t.Fatalf("unexpected weather languages: %v", inventory.Skills[1].Languages())
	}
	shadow := inventory.Skills[0]
	if !reflect.DeepEqual(shadow.Languages(), []string{"en-us"}) {
		t.Fatalf("the hub said the shadow skill has no French, got %v", shadow.Languages())
	}
	if len(shadow.Intents[0].PhrasesFor("fr-fr")) != 0 {
		t.Fatalf("expected no French sentences for the shadow skill, got %v", shadow.Intents[0].PhrasesFor("fr-fr"))
	}
	if !inventory.HasPhrases() {
		t.Fatal("a manifest inventory carries sentences")
	}
}

func TestIntentExamplesPreferWholeSentencesAndRespectTheLimit(t *testing.T) {
	inventory, err := intentClient(newIntentHub()).Intents(context.Background(), []string{"en-us"})
	if err != nil {
		t.Fatal(err)
	}
	weather := intentByID(t, inventory, weatherSkill+":current.weather")

	cases := []struct {
		name  string
		lang  string
		limit int
		want  []string
	}{
		{name: "whole sentences first, shorter first", lang: "en-us", limit: 2, want: []string{"how is it outside", "what is the weather"}},
		{name: "a zero limit is the whole pool", lang: "en-us", limit: 0, want: weather.PhrasesFor("en-us")},
		{name: "no language means the first one", lang: "", limit: 1, want: []string{"how is it outside"}},
		{name: "a language the hub did not list", lang: "de-de", limit: 2, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := weather.Examples(tc.lang, tc.limit); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Examples(%q, %d) = %v, want %v", tc.lang, tc.limit, got, tc.want)
			}
		})
	}
}

func TestEveryRegistrationIsDescribedAtOnceAndRepeatsAreDropped(t *testing.T) {
	hub := newIntentHub()
	hub.repeats = 3
	inventory, err := intentClient(hub).Intents(context.Background(), []string{"en-us", "fr-fr"})
	if err != nil {
		t.Fatal(err)
	}

	describes := map[string]int{}
	for _, query := range hub.queries(EventIntentDescribe) {
		describes[fmt.Sprint(query.data["skill_id"], query.data["intent_name"], query.data["lang"])]++
	}
	if len(describes) != 3 {
		t.Fatalf("expected the three registrations to be described, got %v", describes)
	}
	for key, count := range describes {
		if count != 1 {
			t.Fatalf("registration %s was described %d times", key, count)
		}
	}
	if len(inventory.Intents()) != 2 {
		t.Fatalf("expected two intents, got %v", intentIDs(inventory))
	}
	for _, eventType := range []string{EventIntentList, EventIntentDescribe} {
		for _, query := range hub.queries(eventType) {
			if RequestIDFromContext(query.context) == "" {
				t.Fatalf("%s must be correlated by request id, got context %v", eventType, query.context)
			}
			if query.context["lang"] != query.data["lang"] {
				t.Fatalf("%s must carry the language in its context, got %v", eventType, query.context)
			}
		}
	}
}

func TestDefinitionsAttachedToTheListingSkipTheDescribes(t *testing.T) {
	hub := newIntentHub()
	hub.definitionsInList = true
	inventory, err := intentClient(hub).Intents(context.Background(), []string{"fr-fr"})
	if err != nil {
		t.Fatal(err)
	}

	if described := hub.queries(EventIntentDescribe); len(described) != 0 {
		t.Fatalf("a listing carrying definitions needs no describe, got %d", len(described))
	}
	if got := inventory.Intents()[0].PhrasesFor("fr-fr"); len(got) == 0 || got[0] != "quel temps fait-il" {
		t.Fatalf("unexpected sentences: %v", got)
	}
	listed := hub.queries(EventIntentList)
	if len(listed) != 1 || !reflect.DeepEqual(listed[0].data, Data{"lang": "fr-fr", "include_definitions": true}) {
		t.Fatalf("unexpected listing queries: %+v", listed)
	}
}

func TestADualRegistrationKeepsItsSentencesWhicheverRowCarriesThem(t *testing.T) {
	hub := newIntentHub()
	hub.definitionsInList = true
	hub.registrations = []intentSample{
		{lang: "en-us", skillID: weatherSkill, name: "current.weather", samples: []string{"what is the weather"}},
		{lang: "en-us", skillID: weatherSkill, name: "current.weather", method: "keyword", samples: []string{"weather"}},
	}
	inventory, err := intentClient(hub).Intents(context.Background(), []string{"en-us"})
	if err != nil {
		t.Fatal(err)
	}

	if len(inventory.Intents()) != 1 {
		t.Fatalf("two registrations of one intent are one intent, got %v", intentIDs(inventory))
	}
	weather := inventory.Intents()[0]
	if !reflect.DeepEqual(weather.PhrasesFor("en-us"), []string{"what is the weather"}) {
		t.Fatalf("the keyword row must not erase the template's sentences, got %v", weather.PhrasesFor("en-us"))
	}
	if weather.Engine != "padatious" {
		t.Fatalf("the first row listed names the engine, got %q", weather.Engine)
	}
}

func TestARefusalIsAPolicyErrorNotATimeout(t *testing.T) {
	hub := newIntentHub()
	hub.refuse[EventIntentList] = true
	started := time.Now()
	_, err := intentClient(hub).Intents(context.Background(), []string{"en-us"}, IntentOptions{Fallback: boolPointer(false)})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("a refusal must not wait for the timeout, took %s", elapsed)
	}

	var denied *PolicyDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("expected a *PolicyDeniedError, got %v", err)
	}
	if !errors.Is(err, ErrRuntime) {
		t.Fatalf("a policy denial is a runtime error, got %v", err)
	}
	if denied.DeniedType != EventIntentList || denied.Code != "acl_disallowed_type" {
		t.Fatalf("unexpected denial: %+v", denied)
	}
	if !reflect.DeepEqual(denied.Allowed, allowedTypes) {
		t.Fatalf("the denial must carry the allowed types, got %v", denied.Allowed)
	}
	if !strings.Contains(err.Error(), EventIntentList) || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("the message must name the type and the connection settings, got %q", err)
	}
	if listed := hub.queries(EventIntentList); len(listed) != 1 {
		t.Fatalf("expected one refused listing, got %d", len(listed))
	}
}

func TestTheFallbackListsNamesAndSaysWhatWasRefused(t *testing.T) {
	hub := newIntentHub()
	hub.refuse[EventIntentList] = true
	inventory, err := intentClient(hub).Intents(context.Background(), []string{"en-us", "fr-fr"})
	if err != nil {
		t.Fatal(err)
	}

	if inventory.Source != IntentSourceEngines {
		t.Fatalf("expected the engines' manifests as the source, got %q", inventory.Source)
	}
	if !reflect.DeepEqual(inventory.Denied, []string{EventIntentList}) {
		t.Fatalf("the inventory must name the refused query, got %v", inventory.Denied)
	}
	if inventory.HasPhrases() {
		t.Fatal("a names-only inventory carries no sentences")
	}
	if !reflect.DeepEqual(inventory.Languages, []string{"en-us", "fr-fr"}) {
		t.Fatalf("unexpected languages: %v", inventory.Languages)
	}
	want := []string{shadowSkill + ":custos.incidents", weatherSkill + ":current.weather"}
	if !reflect.DeepEqual(intentIDs(inventory), want) {
		t.Fatalf("unexpected intents: %v", intentIDs(inventory))
	}
	if engine := inventory.Intents()[0].Engine; engine != "padatious" {
		t.Fatalf("the manifest that listed the name names the engine, got %q", engine)
	}
	// Names carry no language, so the engines are asked once, not per language.
	for _, eventType := range []string{EventAdaptManifestGet, EventPadatiousManifestGet} {
		if asked := hub.queries(eventType); len(asked) != 1 {
			t.Fatalf("expected %s to be asked once, got %d", eventType, len(asked))
		}
	}
}

func TestAHubRefusingEverythingFailsEvenWithTheFallback(t *testing.T) {
	hub := newIntentHub()
	hub.refuse[EventIntentList] = true
	hub.refuse[EventAdaptManifestGet] = true
	_, err := intentClient(hub).Intents(context.Background(), []string{"en-us"})

	var denied *PolicyDeniedError
	if !errors.As(err, &denied) || denied.DeniedType != EventAdaptManifestGet {
		t.Fatalf("expected the fallback's own refusal, got %v", err)
	}
}

func TestARefusedDescribeIsReportedNotSwallowed(t *testing.T) {
	hub := newIntentHub()
	hub.refuse[EventIntentDescribe] = true
	_, err := intentClient(hub).Intents(context.Background(), []string{"en-us"})

	var denied *PolicyDeniedError
	if !errors.As(err, &denied) || denied.DeniedType != EventIntentDescribe {
		t.Fatalf("expected the describe refusal, got %v", err)
	}
}

func TestASilentHubTimesOutOnTheListing(t *testing.T) {
	hub := newIntentHub()
	hub.silent[EventIntentList] = true
	_, err := intentClient(hub).Intents(context.Background(), []string{"en-us"}, IntentOptions{Timeout: 200 * time.Millisecond})

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected a timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), EventIntentList) {
		t.Fatalf("the timeout must name the query, got %q", err)
	}
}

func TestADescribeThatNeverComesLeavesThatIntentWithoutSentences(t *testing.T) {
	hub := newIntentHub()
	hub.deaf = func(eventType string, data Data) bool {
		return eventType == EventIntentDescribe && data["skill_id"] == shadowSkill
	}
	inventory, err := intentClient(hub).Intents(context.Background(), []string{"en-us"}, IntentOptions{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	if len(intentByID(t, inventory, weatherSkill+":current.weather").PhrasesFor("en-us")) == 0 {
		t.Fatal("the intent the hub described must keep its sentences")
	}
	if got := intentByID(t, inventory, shadowSkill+":custos.incidents").PhrasesFor("en-us"); len(got) != 0 {
		t.Fatalf("the intent the hub never described must carry no sentences, got %v", got)
	}
}

func TestAReplyWithoutARequestIDIsStillTaken(t *testing.T) {
	// A hub that does not echo the request id is not evidence of anything: the
	// listing is taken on its name and a definition names what it describes.
	hub := newIntentHub()
	hub.echoRequestID = false
	hub.repeats = 1
	inventory, err := intentClient(hub).Intents(context.Background(), []string{"en-us"})
	if err != nil {
		t.Fatal(err)
	}

	if !inventory.HasPhrases() {
		t.Fatalf("expected the sentences to be matched by definition, got %+v", inventory)
	}
	if len(intentByID(t, inventory, shadowSkill+":custos.incidents").PhrasesFor("en-us")) != 2 {
		t.Fatalf("every describe must find its intent, got %+v", inventory.Intents())
	}
}

func TestDescribeIsSkippedWhenNotWanted(t *testing.T) {
	hub := newIntentHub()
	inventory, err := intentClient(hub).Intents(context.Background(), []string{"en-us"}, IntentOptions{Describe: boolPointer(false)})
	if err != nil {
		t.Fatal(err)
	}

	if described := hub.queries(EventIntentDescribe); len(described) != 0 {
		t.Fatalf("Describe false must not describe, got %d describes", len(described))
	}
	if inventory.HasPhrases() || len(inventory.Intents()) != 2 {
		t.Fatalf("expected names and engines only, got %+v", inventory.Intents())
	}
	if listed := hub.queries(EventIntentList); len(listed) != 1 || listed[0].data["include_definitions"] != nil {
		t.Fatalf("a listing that will not be described asks for no definitions, got %+v", listed)
	}
}

func TestLowLevelCallsExposeTheManifestRowsAndDefinitions(t *testing.T) {
	client := intentClient(newIntentHub())
	rows, err := client.ListIntents(context.Background(), "fr-fr")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the one French registration, got %+v", rows)
	}
	row := rows[0]
	if row.SkillID != weatherSkill || row.IntentName != "current.weather" || row.Engine() != "padatious" || !row.Enabled {
		t.Fatalf("unexpected row: %+v", row)
	}
	if !SameLanguage(row.Lang, "fr-fr") || row.Lang == "fr-fr" {
		t.Fatalf("the runtime answers the tag it stores, got %q", row.Lang)
	}
	if row.SessionID != "default" || row.Definition != nil {
		t.Fatalf("unexpected row: %+v", row)
	}

	definitions, err := client.DescribeIntent(context.Background(), weatherSkill, "current.weather", "fr-fr")
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || definitions[0].Samples[0] != "quel temps fait-il" || definitions[0].Engine() != "padatious" {
		t.Fatalf("unexpected definitions: %+v", definitions)
	}
	if blacklist, ok := definitions[0].Raw["blacklist"].([]any); !ok || len(blacklist) != 0 {
		t.Fatalf("Raw must carry the whole definition, got %+v", definitions[0].Raw)
	}

	unknown, err := client.DescribeIntent(context.Background(), shadowSkill, "custos.incidents", "fr-fr")
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 0 {
		t.Fatalf("an unknown registration is an empty answer, got %+v", unknown)
	}

	if _, err := client.DescribeIntent(context.Background(), "", "current.weather", "fr-fr"); err == nil {
		t.Fatal("a describe without a skill id must be refused")
	}
}

func TestListIntentsHonoursIncludeDefinitions(t *testing.T) {
	hub := newIntentHub()
	hub.definitionsInList = true
	rows, err := intentClient(hub).ListIntents(context.Background(), "en-us", IntentOptions{IncludeDefinitions: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 {
		t.Fatalf("expected two English registrations, got %+v", rows)
	}
	for _, row := range rows {
		if row.Definition == nil || len(samplesFromDefinition(row.Definition)) == 0 {
			t.Fatalf("expected the definition on the row, got %+v", row)
		}
	}
}

func TestInventoryJSONIsCompleteAndSnakeCase(t *testing.T) {
	inventory, err := intentClient(newIntentHub()).Intents(context.Background(), []string{"en-us", "fr-fr"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}

	if payload["source"] != IntentSourceManifest || !reflect.DeepEqual(payload["languages"], []any{"en-us", "fr-fr"}) {
		t.Fatalf("unexpected payload: %s", raw)
	}
	if denied, ok := payload["denied"].([]any); !ok || len(denied) != 0 {
		t.Fatalf("denied must be an empty list, not null: %s", raw)
	}
	var weather map[string]any
	for _, raw := range payload["skills"].([]any) {
		if skill := raw.(map[string]any); skill["skill_id"] == weatherSkill {
			weather = skill
		}
	}
	if weather == nil {
		t.Fatalf("payload lacks the weather skill: %s", raw)
	}
	intent := weather["intents"].([]any)[0].(map[string]any)
	if intent["name"] != "current.weather" || intent["engine"] != "padatious" || intent["enabled"] != true {
		t.Fatalf("unexpected intent payload: %v", intent)
	}
	if !reflect.DeepEqual(intent["languages"], []any{"en-us", "fr-fr"}) {
		t.Fatalf("unexpected intent languages: %v", intent["languages"])
	}
	french := intent["phrases"].(map[string]any)["fr-fr"].([]any)
	if french[0] != "quel temps fait-il" {
		t.Fatalf("unexpected French phrases: %v", french)
	}
}

func TestLanguagesDefaultToEnglish(t *testing.T) {
	cases := []struct {
		name      string
		languages []string
		wantLang  string
		wantErr   bool
	}{
		{name: "nil", languages: nil, wantLang: "en-us"},
		{name: "empty", languages: []string{}, wantLang: "en-us"},
		{name: "explicit", languages: []string{"fr-fr"}, wantLang: "fr-fr"},
		{name: "blank only", languages: []string{" "}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newIntentHub()
			_, err := intentClient(hub).Intents(context.Background(), tc.languages)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error for languages that name nothing")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			listed := hub.queries(EventIntentList)
			if len(listed) != 1 || listed[0].data["lang"] != tc.wantLang {
				t.Fatalf("expected one listing in %q, got %+v", tc.wantLang, listed)
			}
		})
	}
}

func TestLowLevelCallsDefaultTheLanguageToEnglish(t *testing.T) {
	hub := newIntentHub()
	client := intentClient(hub)
	if _, err := client.ListIntents(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DescribeIntent(context.Background(), weatherSkill, "current.weather", ""); err != nil {
		t.Fatal(err)
	}
	for _, eventType := range []string{EventIntentList, EventIntentDescribe} {
		if asked := hub.queries(eventType); len(asked) != 1 || asked[0].data["lang"] != "en-us" {
			t.Fatalf("expected %s in en-us, got %+v", eventType, asked)
		}
	}
}

func TestIntentOptionsMerge(t *testing.T) {
	cases := []struct {
		name         string
		opts         []IntentOptions
		wantTimeout  time.Duration
		wantDescribe bool
		wantFallback bool
		wantInclude  bool
	}{
		{name: "none", wantTimeout: DefaultIntentTimeout, wantDescribe: true, wantFallback: true},
		{name: "zero value", opts: []IntentOptions{{}}, wantTimeout: DefaultIntentTimeout, wantDescribe: true, wantFallback: true},
		{name: "negative timeout is the default", opts: []IntentOptions{{Timeout: -time.Second}}, wantTimeout: DefaultIntentTimeout, wantDescribe: true, wantFallback: true},
		{name: "flags off", opts: []IntentOptions{{Timeout: time.Second, Describe: boolPointer(false), Fallback: boolPointer(false), IncludeDefinitions: true}}, wantTimeout: time.Second, wantInclude: true},
		{name: "later options win", opts: []IntentOptions{{Timeout: time.Second, Describe: boolPointer(false)}, {Timeout: 2 * time.Second, Describe: boolPointer(true)}}, wantTimeout: 2 * time.Second, wantDescribe: true, wantFallback: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := intentOptions(tc.opts)
			if merged.Timeout != tc.wantTimeout || merged.describe() != tc.wantDescribe || merged.fallback() != tc.wantFallback || merged.IncludeDefinitions != tc.wantInclude {
				t.Fatalf("intentOptions(%+v) = %+v", tc.opts, merged)
			}
		})
	}
}

func TestSameLanguageFoldsCaseAndSeparators(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"fr-fr", "fr_FR", true},
		{"fr-FR", " fr-fr ", true},
		{"en-us", "en-gb", false},
		{"", "", true},
		{"en", "en-us", false},
	}
	for _, tc := range cases {
		if got := SameLanguage(tc.a, tc.b); got != tc.want {
			t.Fatalf("SameLanguage(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSplitIntentName(t *testing.T) {
	cases := []struct {
		raw       string
		wantSkill string
		wantName  string
	}{
		{"skill-weather:current.weather", "skill-weather", "current.weather"},
		{"bare", "", "bare"},
		{"a:b:c", "a", "b:c"},
		{"trailing:", "", "trailing:"},
	}
	for _, tc := range cases {
		skillID, name := splitIntentName(tc.raw)
		if skillID != tc.wantSkill || name != tc.wantName {
			t.Fatalf("splitIntentName(%q) = %q, %q; want %q, %q", tc.raw, skillID, name, tc.wantSkill, tc.wantName)
		}
	}
}

func TestRegistrationFromMap(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want IntentRegistration
		ok   bool
	}{
		{
			name: "a full row",
			raw:  map[string]any{"skill_id": weatherSkill, "intent_name": "current.weather", "lang": "en-us", "method": "keyword", "enabled": false, "session_id": "abc"},
			want: IntentRegistration{SkillID: weatherSkill, IntentName: "current.weather", Lang: "en-us", Method: "keyword", Enabled: false, SessionID: "abc"},
			ok:   true,
		},
		{
			name: "defaults",
			raw:  map[string]any{"skill_id": " " + weatherSkill + " ", "intent_name": "current.weather"},
			want: IntentRegistration{SkillID: weatherSkill, IntentName: "current.weather", Enabled: true, SessionID: "default"},
			ok:   true,
		},
		{
			name: "an attached definition",
			raw:  map[string]any{"skill_id": weatherSkill, "intent_name": "current.weather", "definition": map[string]any{"samples": []any{"hi"}}},
			want: IntentRegistration{SkillID: weatherSkill, IntentName: "current.weather", Enabled: true, SessionID: "default", Definition: map[string]any{"samples": []any{"hi"}}},
			ok:   true,
		},
		{name: "no intent name", raw: map[string]any{"skill_id": weatherSkill, "intent_name": " "}},
		{name: "no skill id", raw: map[string]any{"intent_name": "current.weather"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := registrationFromMap(tc.raw)
			if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("registrationFromMap(%v) = %+v, %v; want %+v, %v", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestPolicyDeniedErrorMessageAndUnwrap(t *testing.T) {
	cases := []struct {
		name       string
		err        *PolicyDeniedError
		wantDetail string
	}{
		{name: "reason", err: &PolicyDeniedError{DeniedType: EventIntentList, Code: "acl_disallowed_type", Reason: "not in allowed_types"}, wantDetail: "not in allowed_types"},
		{name: "code only", err: &PolicyDeniedError{DeniedType: EventIntentList, Code: "acl_disallowed_type"}, wantDetail: "acl_disallowed_type"},
		{name: "neither", err: &PolicyDeniedError{DeniedType: EventIntentList}, wantDetail: "refused by the hub's policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := tc.err.Error()
			for _, want := range []string{ErrRuntime.Error(), EventIntentList, tc.wantDetail, "connection settings"} {
				if !strings.Contains(message, want) {
					t.Fatalf("message %q lacks %q", message, want)
				}
			}
			wrapped := fmt.Errorf("intents: %w", tc.err)
			var denied *PolicyDeniedError
			if !errors.As(wrapped, &denied) || denied != tc.err {
				t.Fatalf("errors.As must find the denial through a wrapper, got %v", wrapped)
			}
			if !errors.Is(wrapped, ErrRuntime) {
				t.Fatalf("a policy denial must match ErrRuntime, got %v", wrapped)
			}
		})
	}
}

func TestPolicyDeniedFromEventReadsTheObservedShape(t *testing.T) {
	event := Event{Name: EventPolicyDenied, Data: Data{
		"denied_type": EventIntentList,
		"code":        "acl_disallowed_type",
		"reason":      "ovos.intent.list not in allowed_types",
		"data":        map[string]any{"msg_type": EventIntentList, "allowed": []any{"speak", 42}},
	}}
	denied := policyDeniedFromEvent(event)
	want := &PolicyDeniedError{DeniedType: EventIntentList, Code: "acl_disallowed_type", Reason: "ovos.intent.list not in allowed_types", Allowed: []string{"speak"}}
	if !reflect.DeepEqual(denied, want) {
		t.Fatalf("policyDeniedFromEvent = %+v, want %+v", denied, want)
	}
	if policyDenial(event, EventIntentDescribe) != nil {
		t.Fatal("a denial of another type is not this query's")
	}
	if policyDenial(Event{Name: EventSpeak, Data: Data{"denied_type": EventIntentList}}, EventIntentList) != nil {
		t.Fatal("only hive.policy.denied is a denial")
	}
}

func TestIntentEventNamesMatchTheContract(t *testing.T) {
	cases := map[string]string{
		EventIntentList:             "ovos.intent.list",
		EventIntentListResponse:     "ovos.intent.list.response",
		EventIntentDescribe:         "ovos.intent.describe",
		EventIntentDescribeResponse: "ovos.intent.describe.response",
		EventAdaptManifestGet:       "intent.service.adapt.manifest.get",
		EventAdaptManifest:          "intent.service.adapt.manifest",
		EventPadatiousManifestGet:   "intent.service.padatious.manifest.get",
		EventPadatiousManifest:      "intent.service.padatious.manifest",
		IntentSourceManifest:        "intent-manifest",
		IntentSourceEngines:         "engine-manifests",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("constant %q must be %q", got, want)
		}
	}
}
