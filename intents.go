package thalovant

// What a hub can be asked: the intent inventory, over the client's own session.
//
// The hub runtime keeps an intent manifest (OVOS-INTENT-4 section 10): every
// intent a skill registered, per language, and on request the registration
// itself, which for a template intent carries the sentences from the skill's
// locale files, slots and all -- "what is the weather in {location}". This
// file asks that manifest and shapes the answer, so a satellite, an installer
// or an agent shows a person what they can say without a control-plane token.
//
// Two queries, correlated by context.request_id like every other request:
//
//   - ovos.intent.list {"lang": <tag>} -> ovos.intent.list.response
//     {"ok", "intents": [{skill_id, intent_name, lang, method, enabled,
//     session_id}]}. method is "template" (sample sentences) or "keyword"
//     (keyword sets). A runtime may attach each entry's "definition" when
//     asked with "include_definitions"; when it does not, the client
//     describes each intent individually.
//   - ovos.intent.describe {"skill_id", "intent_name", "lang"} ->
//     ovos.intent.describe.response {"ok", "definitions": [{method,
//     definition}]} or {"ok": false, "error"}.
//
// A hub whose connection may not publish a type answers hive.policy.denied
// naming it; that becomes a *PolicyDeniedError at once rather than a timeout.
// The engines' own manifests (intent.service.adapt.manifest.get and
// intent.service.padatious.manifest.get, names only, no language) are the
// fallback for a hub allowed for those alone.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// IntentSourceManifest marks an inventory read from the hub runtime's
	// intent manifest: sentences per language.
	IntentSourceManifest = "intent-manifest"
	// IntentSourceEngines marks the names-only fallback read from the
	// engines' own manifests; the inventory's Denied then names the query
	// the hub refused.
	IntentSourceEngines = "engine-manifests"
	// DefaultIntentTimeout bounds each intent query when
	// IntentOptions.Timeout is zero.
	DefaultIntentTimeout = 5 * time.Second
	// DescribeBatch is how many describes go out together. A hub with 69
	// intents in two languages is 138 requests and, with every reply
	// delivered twice, 276 inbound events -- more than a transport's reply
	// channel holds, and a burst the hub never asked for. Batching also
	// bounds the deadline: a hub answering nothing fails after one batch
	// rather than holding every request open.
	DescribeBatch = 32
)

var intentEngineByMethod = map[string]string{
	"template": "padatious",
	"keyword":  "adapt",
}

// IntentOptions tunes Client.Intents, Client.ListIntents and
// Client.DescribeIntent. Timeout bounds each query the hub is sent and
// defaults to DefaultIntentTimeout when zero. Describe, when nil or true,
// has Client.Intents fetch every template intent's sentences; false leaves
// the inventory with names and engines only. Fallback, when nil or true,
// has Client.Intents read the engines' own manifests when the hub refuses
// ovos.intent.list; false returns the *PolicyDeniedError instead.
// IncludeDefinitions asks the runtime to attach each row's definition to
// the ovos.intent.list reply in Client.ListIntents; Client.Intents sets it
// itself whenever it describes.
type IntentOptions struct {
	Timeout            time.Duration
	Describe           *bool
	Fallback           *bool
	IncludeDefinitions bool
}

func intentOptions(opts []IntentOptions) IntentOptions {
	merged := IntentOptions{}
	for _, opt := range opts {
		if opt.Timeout != 0 {
			merged.Timeout = opt.Timeout
		}
		if opt.Describe != nil {
			merged.Describe = opt.Describe
		}
		if opt.Fallback != nil {
			merged.Fallback = opt.Fallback
		}
		if opt.IncludeDefinitions {
			merged.IncludeDefinitions = true
		}
	}
	if merged.Timeout <= 0 {
		merged.Timeout = DefaultIntentTimeout
	}
	return merged
}

func (o IntentOptions) describe() bool {
	return o.Describe == nil || *o.Describe
}

func (o IntentOptions) fallback() bool {
	return o.Fallback == nil || *o.Fallback
}

// SameLanguage reports whether two language tags name the same language:
// "fr-fr" and "fr_FR" do.
func SameLanguage(a, b string) bool {
	return foldLanguage(a) == foldLanguage(b)
}

func foldLanguage(tag string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(tag)), "_", "-")
}

func intentEngine(method string) string {
	if engine, ok := intentEngineByMethod[method]; ok {
		return engine
	}
	if method == "" {
		return "unknown"
	}
	return method
}

// IntentRegistration is one row of the hub's intent manifest. Definition is
// set only when the runtime attached it to the listing.
type IntentRegistration struct {
	SkillID    string         `json:"skill_id"`
	IntentName string         `json:"intent_name"`
	Lang       string         `json:"lang"`
	Method     string         `json:"method"`
	Enabled    bool           `json:"enabled"`
	SessionID  string         `json:"session_id"`
	Definition map[string]any `json:"definition,omitempty"`
}

// Engine names the intent engine behind the registration: "padatious" for a
// template intent, "adapt" for a keyword one.
func (r IntentRegistration) Engine() string {
	return intentEngine(r.Method)
}

func registrationFromMap(raw map[string]any) (IntentRegistration, bool) {
	skillID := strings.TrimSpace(stringValue(raw["skill_id"]))
	intentName := strings.TrimSpace(stringValue(raw["intent_name"]))
	if skillID == "" || intentName == "" {
		return IntentRegistration{}, false
	}
	sessionID := stringValue(raw["session_id"])
	if sessionID == "" {
		sessionID = "default"
	}
	var definition map[string]any
	if value, ok := raw["definition"].(map[string]any); ok {
		definition = cloneMap(value)
	}
	return IntentRegistration{
		SkillID:    skillID,
		IntentName: intentName,
		Lang:       stringValue(raw["lang"]),
		Method:     stringValue(raw["method"]),
		Enabled:    raw["enabled"] != false,
		SessionID:  sessionID,
		Definition: definition,
	}, true
}

func registrationsFromEvent(event Event) []IntentRegistration {
	rows := anySlice(event.Data["intents"])
	entries := make([]IntentRegistration, 0, len(rows))
	for _, row := range rows {
		raw, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if entry, ok := registrationFromMap(raw); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// IntentDefinition is a registration as the skill made it, from
// ovos.intent.describe. Samples are the sentences a template intent answers
// to, slots in braces; Raw is the whole definition as the hub sent it.
type IntentDefinition struct {
	SkillID    string         `json:"skill_id"`
	IntentName string         `json:"intent_name"`
	Lang       string         `json:"lang"`
	Method     string         `json:"method"`
	Samples    []string       `json:"samples"`
	Raw        map[string]any `json:"raw"`
}

// Engine names the intent engine behind the definition: "padatious" for a
// template intent, "adapt" for a keyword one.
func (d IntentDefinition) Engine() string {
	return intentEngine(d.Method)
}

func definitionFromMap(item map[string]any) (IntentDefinition, bool) {
	definition, ok := item["definition"].(map[string]any)
	if !ok {
		return IntentDefinition{}, false
	}
	skillID := strings.TrimSpace(stringValue(definition["skill_id"]))
	intentName := strings.TrimSpace(stringValue(definition["intent_name"]))
	if skillID == "" || intentName == "" {
		return IntentDefinition{}, false
	}
	method := stringValue(item["method"])
	if method == "" {
		method = stringValue(definition["method"])
	}
	return IntentDefinition{
		SkillID:    skillID,
		IntentName: intentName,
		Lang:       stringValue(definition["lang"]),
		Method:     method,
		Samples:    samplesFromDefinition(definition),
		Raw:        cloneMap(definition),
	}, true
}

func definitionsFromEvent(event Event) []IntentDefinition {
	items := anySlice(event.Data["definitions"])
	found := make([]IntentDefinition, 0, len(items))
	for _, item := range items {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if definition, ok := definitionFromMap(raw); ok {
			found = append(found, definition)
		}
	}
	return found
}

func samplesFromDefinition(definition map[string]any) []string {
	var samples []string
	for _, item := range anySlice(definition["samples"]) {
		text, ok := item.(string)
		if !ok {
			continue
		}
		if text = strings.TrimSpace(text); text != "" {
			samples = append(samples, text)
		}
	}
	return samples
}

// HubIntent is one thing a hub can be asked, with the sentences that ask it,
// per language. Phrases is keyed by the language tag the inventory was asked
// for; Languages lists those keys in the order they were asked. A names-only
// inventory carries neither.
type HubIntent struct {
	SkillID   string              `json:"skill_id"`
	Name      string              `json:"name"`
	Engine    string              `json:"engine"`
	Enabled   bool                `json:"enabled"`
	Languages []string            `json:"languages"`
	Phrases   map[string][]string `json:"phrases"`
}

// ID is the intent's "<skill_id>:<name>" name, as the engines' manifests
// spell it.
func (i HubIntent) ID() string {
	return i.SkillID + ":" + i.Name
}

// PhrasesFor returns the sentences that reach the intent in one language,
// matched with SameLanguage, or nil when the hub registered none.
func (i HubIntent) PhrasesFor(lang string) []string {
	for _, candidate := range i.Languages {
		if SameLanguage(candidate, lang) {
			return i.Phrases[candidate]
		}
	}
	for candidate, sentences := range i.Phrases {
		if SameLanguage(candidate, lang) {
			return sentences
		}
	}
	return nil
}

// Examples returns a few sentences worth showing: whole ones before ones
// with a slot, shorter ones first. An empty lang means the first language
// the intent has; a limit of zero or less returns the whole pool.
func (i HubIntent) Examples(lang string, limit int) []string {
	var pool []string
	switch {
	case lang != "":
		pool = i.PhrasesFor(lang)
	case len(i.Languages) > 0:
		pool = i.Phrases[i.Languages[0]]
	}
	if limit <= 0 {
		return pool
	}
	ranked := append([]string(nil), pool...)
	sort.SliceStable(ranked, func(a, b int) bool {
		slotA := strings.Contains(ranked[a], "{")
		slotB := strings.Contains(ranked[b], "{")
		if slotA != slotB {
			return !slotA
		}
		return len(ranked[a]) < len(ranked[b])
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

// HubSkillIntents groups the intents one skill registered.
type HubSkillIntents struct {
	SkillID string      `json:"skill_id"`
	Intents []HubIntent `json:"intents"`
}

// Languages lists every language one of the skill's intents has, in the
// order they were asked.
func (s HubSkillIntents) Languages() []string {
	seen := map[string]struct{}{}
	var languages []string
	for _, intent := range s.Intents {
		for _, lang := range intent.Languages {
			if _, ok := seen[lang]; ok {
				continue
			}
			seen[lang] = struct{}{}
			languages = append(languages, lang)
		}
	}
	return languages
}

// HubIntentInventory is everything a hub can be asked, grouped by skill.
//
// Source says how it was read: IntentSourceManifest carries sentences per
// language; IntentSourceEngines is the names-only fallback, and Denied then
// names the query the hub refused.
type HubIntentInventory struct {
	Languages []string          `json:"languages"`
	Skills    []HubSkillIntents `json:"skills"`
	Source    string            `json:"source"`
	Denied    []string          `json:"denied"`
}

// Intents flattens the inventory: every skill's intents, skills in order.
func (inv HubIntentInventory) Intents() []HubIntent {
	var intents []HubIntent
	for _, skill := range inv.Skills {
		intents = append(intents, skill.Intents...)
	}
	return intents
}

// HasPhrases reports whether any intent carries a sentence, which a
// names-only inventory never does.
func (inv HubIntentInventory) HasPhrases() bool {
	for _, intent := range inv.Intents() {
		for _, sentences := range intent.Phrases {
			if len(sentences) > 0 {
				return true
			}
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// the client
// ----------------------------------------------------------------------------

// Intents returns everything the hub can be asked, per language, grouped by
// skill.
//
// It reads the runtime's intent manifest over this session, so no
// control-plane credential is involved. Each intent carries the sentences a
// person says to reach it, as the skill wrote them, "{slot}" placeholders
// included. A nil or empty languages asks for "en-us"; tags are trimmed and a
// language repeated under another spelling ("en-US", "en_us") is asked once,
// under the first spelling given.
//
// The hub's queries are correlated by request id like Ask; a reply delivered
// more than once is taken once. Unless the runtime attached definitions to
// the listing, every template registration is described, DescribeBatch of
// them in flight at a time, and one the hub does not describe in time
// carries no sentences.
//
// A hub that refuses ovos.intent.list is asked for the engines' own
// manifests instead, unless IntentOptions.Fallback is false: the result then
// carries names only, Source set to IntentSourceEngines and Denied naming
// the refused query. A hub refusing those too, or any refusal with the
// fallback off, returns a *PolicyDeniedError; a hub that stays silent returns
// an error wrapping ErrTimeout.
//
// Like Ask, it reads the transport's event channel, so it must not run
// concurrently with Ask or another intent call on the same client.
func (c *Client) Intents(ctx context.Context, languages []string, opts ...IntentOptions) (HubIntentInventory, error) {
	options := intentOptions(opts)
	asked, err := askedLanguages(languages)
	if err != nil {
		return HubIntentInventory{}, err
	}

	listed := make(map[string][]IntentRegistration, len(asked))
	for _, lang := range asked {
		rows, err := c.ListIntents(ctx, lang, IntentOptions{
			Timeout:            options.Timeout,
			IncludeDefinitions: options.describe(),
		})
		if err != nil {
			var denied *PolicyDeniedError
			if !options.fallback() || !errors.As(err, &denied) || denied.DeniedType != EventIntentList {
				return HubIntentInventory{}, err
			}
			names, err := c.intentNames(ctx, asked[0], options.Timeout)
			if err != nil {
				return HubIntentInventory{}, err
			}
			return inventoryFromNames(names, asked, denied.DeniedType), nil
		}
		listed[lang] = rows
	}

	var wanted []intentKey
	for _, lang := range asked {
		for _, row := range listed[lang] {
			if row.Enabled && row.Definition == nil && row.Method == "template" {
				wanted = append(wanted, intentKey{row.SkillID, row.IntentName, lang})
			}
		}
	}
	described := map[intentKey][]IntentDefinition{}
	if options.describe() && len(wanted) > 0 {
		found, err := c.describeMany(ctx, wanted, options.Timeout, DescribeBatch)
		if err != nil {
			return HubIntentInventory{}, err
		}
		described = found
	}

	byName := map[string]*HubIntent{}
	var order []string
	for _, lang := range asked {
		for _, row := range listed[lang] {
			id := row.SkillID + ":" + row.IntentName
			intent, ok := byName[id]
			if !ok {
				intent = &HubIntent{
					SkillID: row.SkillID,
					Name:    row.IntentName,
					Engine:  row.Engine(),
					Phrases: map[string][]string{},
				}
				byName[id] = intent
				order = append(order, id)
			}
			intent.Enabled = intent.Enabled || row.Enabled
			var sentences []string
			if row.Definition != nil {
				sentences = samplesFromDefinition(row.Definition)
			} else {
				for _, definition := range described[intentKey{row.SkillID, row.IntentName, lang}] {
					if len(definition.Samples) > 0 {
						sentences = definition.Samples
						break
					}
				}
			}
			// An intent registered under both engines has two rows for the
			// language; the keyword row carries no sentences and must not
			// erase the template row's, whichever order they arrive in.
			if _, ok := intent.Phrases[lang]; !ok {
				intent.Languages = append(intent.Languages, lang)
				intent.Phrases[lang] = sentences
			} else if len(sentences) > 0 {
				intent.Phrases[lang] = sentences
			}
		}
	}
	intents := make([]HubIntent, 0, len(order))
	for _, id := range order {
		intents = append(intents, *byName[id])
	}
	return HubIntentInventory{
		Languages: asked,
		Skills:    groupBySkill(intents),
		Source:    IntentSourceManifest,
		Denied:    []string{},
	}, nil
}

// ListIntents returns the hub's intent manifest for one language, one row
// per registration. An empty lang asks for "en-us". With
// IntentOptions.IncludeDefinitions the runtime is asked to attach each row's
// definition; a runtime that honours it fills IntentRegistration.Definition.
// Like Ask, it reads the transport's event channel, so it must not run
// concurrently with Ask or another intent call on the same client.
func (c *Client) ListIntents(ctx context.Context, lang string, opts ...IntentOptions) ([]IntentRegistration, error) {
	options := intentOptions(opts)
	if strings.TrimSpace(lang) == "" {
		lang = "en-us"
	}
	data := Data{"lang": lang}
	if options.IncludeDefinitions {
		data["include_definitions"] = true
	}
	event, err := c.requestReply(ctx, EventIntentList, EventIntentListResponse, data, lang, options.Timeout)
	if err != nil {
		return nil, err
	}
	return registrationsFromEvent(event), nil
}

// DescribeIntent returns every registration behind one intent in one
// language, keyword ones first, sentences included for a template intent. An
// empty lang asks for "en-us". A registration the hub does not know yields an
// empty list, not an error. Like Ask, it reads the transport's event channel,
// so it must not run concurrently with Ask or another intent call on the
// same client.
func (c *Client) DescribeIntent(ctx context.Context, skillID, intentName, lang string, opts ...IntentOptions) ([]IntentDefinition, error) {
	options := intentOptions(opts)
	skillID = strings.TrimSpace(skillID)
	intentName = strings.TrimSpace(intentName)
	if skillID == "" || intentName == "" {
		return nil, fmt.Errorf("describe intent requires a skill id and an intent name")
	}
	if strings.TrimSpace(lang) == "" {
		lang = "en-us"
	}
	data := Data{"skill_id": skillID, "intent_name": intentName, "lang": lang}
	event, err := c.requestReply(ctx, EventIntentDescribe, EventIntentDescribeResponse, data, lang, options.Timeout)
	if err != nil {
		return nil, err
	}
	if event.Data["ok"] == false {
		return []IntentDefinition{}, nil
	}
	return definitionsFromEvent(event), nil
}

// askedLanguages trims the tags and asks each language once: "en-us",
// "en-US" and "en_us" are one language, kept under the first spelling seen,
// in the order given. Nothing given means "en-us".
func askedLanguages(languages []string) ([]string, error) {
	if len(languages) == 0 {
		return []string{"en-us"}, nil
	}
	asked := make([]string, 0, len(languages))
	for _, lang := range languages {
		tag := strings.TrimSpace(lang)
		if tag == "" || containsLanguage(asked, tag) {
			continue
		}
		asked = append(asked, tag)
	}
	if len(asked) == 0 {
		return nil, fmt.Errorf("intents requires at least one language")
	}
	return asked, nil
}

func containsLanguage(tags []string, lang string) bool {
	for _, tag := range tags {
		if SameLanguage(tag, lang) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// the wire
// ----------------------------------------------------------------------------

// intentKey names one registration: a skill's intent in one language.
type intentKey struct {
	skillID    string
	intentName string
	lang       string
}

// intentQueryContext is the context every intent query carries: the request
// id that correlates the reply, and the language, which the hub echoes.
func (c *Client) intentQueryContext(lang, requestID string) Context {
	return ContextWithCorrelation(Context{"lang": lang}, "", c.Identity.SiteID, lang, requestID)
}

// requestReply sends one bus query and returns its reply, matched by request
// id the way Ask matches its events. A reply may arrive more than once; the
// first one wins and repeats are dropped by the next query. A
// hive.policy.denied naming the query returns a *PolicyDeniedError at once.
func (c *Client) requestReply(ctx context.Context, queryType, replyType string, data Data, lang string, timeout time.Duration) (Event, error) {
	if err := c.Connect(ctx); err != nil {
		return Event{}, err
	}
	requestID := NewRequestID()
	eventContext := c.intentQueryContext(lang, requestID)
	events := c.Transport.Events()
	// Whatever is queued now was sent before the query and cannot answer it:
	// the previous query's repeated replies, for one.
	drainEvents(events)
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.Emit(queryCtx, queryType, data, eventContext); err != nil {
		return Event{}, err
	}
	for {
		select {
		case <-queryCtx.Done():
			return Event{}, fmt.Errorf("%w: hub did not answer %s within %s", ErrTimeout, queryType, timeout)
		case event := <-events:
			if denied := policyDenial(event, queryType); denied != nil {
				return Event{}, denied
			}
			if event.Name == replyType && EventMatchesContext(event, eventContext) {
				return event, nil
			}
		}
	}
}

// describeMany describes many registrations, at most batch of them in
// flight: one window per batch, one request id per registration, replies
// matched by that id -- or by the definition's own skill, intent and
// language when a hub does not echo the id -- and repeats dropped. A batch
// of zero or less sends them all at once.
//
// The deadline covers each batch, so a hub that answers nothing fails after
// one batch rather than holding every request open. A partial answer is
// still an answer: within a batch the hub answered, the registrations it
// did not describe in time are simply absent from the result.
func (c *Client) describeMany(ctx context.Context, wanted []intentKey, timeout time.Duration, batch int) (map[intentKey][]IntentDefinition, error) {
	wanted = uniqueIntentKeys(wanted)
	found := map[intentKey][]IntentDefinition{}
	if len(wanted) == 0 {
		return found, nil
	}
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}
	if batch <= 0 || batch > len(wanted) {
		batch = len(wanted)
	}
	for start := 0; start < len(wanted); start += batch {
		end := start + batch
		if end > len(wanted) {
			end = len(wanted)
		}
		if err := c.describeBatch(ctx, wanted[start:end], timeout, found); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// describeBatch sends one batch of describes and collects their replies into
// found. Whatever is queued when it starts was sent before its requests and
// cannot answer them -- the previous batch's repeated replies, in practice.
func (c *Client) describeBatch(ctx context.Context, wanted []intentKey, timeout time.Duration, found map[intentKey][]IntentDefinition) error {
	events := c.Transport.Events()
	drainEvents(events)
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	byRequest := make(map[string]intentKey, len(wanted))
	for _, key := range wanted {
		requestID := NewRequestID()
		byRequest[requestID] = key
		data := Data{"skill_id": key.skillID, "intent_name": key.intentName, "lang": key.lang}
		if err := c.Emit(queryCtx, EventIntentDescribe, data, c.intentQueryContext(key.lang, requestID)); err != nil {
			return err
		}
	}
	for answered := 0; answered < len(wanted); {
		select {
		case <-queryCtx.Done():
			if answered == 0 {
				return fmt.Errorf("%w: hub did not answer %s within %s", ErrTimeout, EventIntentDescribe, timeout)
			}
			return nil
		case event := <-events:
			if denied := policyDenial(event, EventIntentDescribe); denied != nil {
				return denied
			}
			if event.Name != EventIntentDescribeResponse {
				continue
			}
			definitions := definitionsFromEvent(event)
			key, ok := byRequest[event.RequestID()]
			if !ok && len(definitions) > 0 {
				// No request id came back: the definition names what it
				// describes.
				key, ok = keyForDefinition(wanted, definitions[0])
			}
			if !ok {
				continue
			}
			if _, seen := found[key]; seen {
				continue
			}
			if event.Data["ok"] == false {
				found[key] = nil
			} else {
				found[key] = definitions
			}
			answered++
		}
	}
	return nil
}

func keyForDefinition(wanted []intentKey, definition IntentDefinition) (intentKey, bool) {
	for _, candidate := range wanted {
		if candidate.skillID == definition.SkillID && candidate.intentName == definition.IntentName && SameLanguage(candidate.lang, definition.Lang) {
			return candidate, true
		}
	}
	return intentKey{}, false
}

func uniqueIntentKeys(keys []intentKey) []intentKey {
	seen := make(map[intentKey]struct{}, len(keys))
	unique := make([]intentKey, 0, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	return unique
}

// engineNames is one engine's own manifest: the names it registered.
type engineNames struct {
	engine string
	names  []string
}

// intentNames reads the engines' own manifests, adapt then padatious: names
// only, and the same names whatever the language asked, because an intent's
// name is the same in every language. The fallback for a hub allowed for
// these queries but not the intent manifest. The order matters: the first
// engine to name an intent decides its engine.
func (c *Client) intentNames(ctx context.Context, lang string, timeout time.Duration) ([]engineNames, error) {
	manifests := []struct {
		engine    string
		queryType string
		replyType string
	}{
		{"adapt", EventAdaptManifestGet, EventAdaptManifest},
		{"padatious", EventPadatiousManifestGet, EventPadatiousManifest},
	}
	found := make([]engineNames, 0, len(manifests))
	for _, manifest := range manifests {
		event, err := c.requestReply(ctx, manifest.queryType, manifest.replyType, Data{"lang": lang}, lang, timeout)
		if err != nil {
			return nil, err
		}
		names := []string{}
		for _, item := range anySlice(event.Data["intents"]) {
			if text, ok := item.(string); ok && text != "" {
				names = append(names, text)
			}
		}
		found = append(found, engineNames{engine: manifest.engine, names: names})
	}
	return found, nil
}

func inventoryFromNames(manifests []engineNames, languages []string, denied string) HubIntentInventory {
	byName := map[string]HubIntent{}
	var order []string
	for _, manifest := range manifests {
		for _, raw := range manifest.names {
			skillID, intentName := splitIntentName(raw)
			id := skillID + ":" + intentName
			// The first engine to name an intent decides its engine, as on the
			// manifest path; adapt is asked before padatious.
			if _, ok := byName[id]; ok {
				continue
			}
			order = append(order, id)
			byName[id] = HubIntent{
				SkillID:   skillID,
				Name:      intentName,
				Engine:    manifest.engine,
				Enabled:   true,
				Languages: []string{},
				Phrases:   map[string][]string{},
			}
		}
	}
	intents := make([]HubIntent, 0, len(order))
	for _, id := range order {
		intents = append(intents, byName[id])
	}
	return HubIntentInventory{
		Languages: languages,
		Skills:    groupBySkill(intents),
		Source:    IntentSourceEngines,
		Denied:    []string{denied},
	}
}

// splitIntentName splits an engine manifest's "<skill_id>:<intent_name>";
// a name with no skill part is returned whole as the intent name.
func splitIntentName(raw string) (string, string) {
	skillID, intentName, found := strings.Cut(raw, ":")
	if !found || intentName == "" {
		return "", raw
	}
	return skillID, intentName
}

// groupBySkill sorts intents into skills, skills by id and intents by name.
func groupBySkill(intents []HubIntent) []HubSkillIntents {
	bySkill := map[string][]HubIntent{}
	for _, intent := range intents {
		bySkill[intent.SkillID] = append(bySkill[intent.SkillID], intent)
	}
	skillIDs := make([]string, 0, len(bySkill))
	for skillID := range bySkill {
		skillIDs = append(skillIDs, skillID)
	}
	sort.Strings(skillIDs)
	skills := make([]HubSkillIntents, 0, len(skillIDs))
	for _, skillID := range skillIDs {
		members := bySkill[skillID]
		sort.SliceStable(members, func(a, b int) bool {
			return members[a].Name < members[b].Name
		})
		skills = append(skills, HubSkillIntents{SkillID: skillID, Intents: members})
	}
	return skills
}

// policyDenial returns the *PolicyDeniedError a hive.policy.denied event
// carries when it names the given query type, and nil for any other event.
func policyDenial(event Event, queryType string) *PolicyDeniedError {
	if event.Name != EventPolicyDenied {
		return nil
	}
	if stringValue(event.Data["denied_type"]) != queryType {
		return nil
	}
	return policyDeniedFromEvent(event)
}

// drainEvents discards every event already queued on the channel without
// blocking.
func drainEvents(events <-chan Event) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}

// anySlice returns a list-shaped value as []any: the wire delivers []any,
// while a caller building events by hand may use a typed slice.
func anySlice(raw any) []any {
	switch value := raw.(type) {
	case []any:
		return value
	case []string:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = item
		}
		return out
	case []map[string]any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = item
		}
		return out
	}
	return nil
}
