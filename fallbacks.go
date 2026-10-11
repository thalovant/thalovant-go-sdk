package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// EventFallbackList and EventFallbackListResponse name the query ovos-core
// #951 added and #988 took out again the same day. No runtime answers it, so
// nothing here sends it any more; the names stay exported so code that
// imports them keeps compiling. Fallbacks are read from EventSkillsList.
const EventFallbackList = "ovos.skills.fallback.list"
const EventFallbackListResponse = "ovos.skills.fallback.list.response"

// EventSkillsList asks the runtime for every skill it loaded and what each
// declared it can do (OVOS-INTENT-4 section 10.3): {"ok", "skills":
// [{skill_id, session_id, capabilities, intents}]}. A skill whose
// capabilities include FallbackCapability is offered whatever nothing else
// matched.
const EventSkillsList = "ovos.skills.list"
const EventSkillsListResponse = "ovos.skills.list.response"

// FallbackCapability is the capability a skill declares when it takes what
// nothing else matched.
const FallbackCapability = "fallback"

// FallbackProbeTimeout caps the fallback probe in IntentsWithCapabilities,
// whatever the caller's own timeout.
const FallbackProbeTimeout = 1500 * time.Millisecond

// HubFallback is a skill that answers whatever nothing else matched. It has
// no sentences to publish, so a caller can learn that it exists and nothing
// more -- which is still the difference between "no skill handles this" and
// "something will try".
//
// No runtime serves an order. ovos.skills.list says which skills declared the
// fallback capability and not in what order they are tried, and the query
// that carried one was reverted. So PriorityKnown is false and Priority is 0
// on every fallback this SDK reads: Priority is not a rank, and a list of
// fallbacks is in skill id order, which says nothing about who goes first.
// Priority keeps its int64 type so existing code compiles; read it only when
// PriorityKnown is true. In JSON an unknown priority is null, as in every
// other SDK, and a non-null number read back sets PriorityKnown.
type HubFallback struct {
	SkillID       string `json:"skill_id"`
	Priority      int64  `json:"priority"`
	PriorityKnown bool   `json:"-"`
}

// MarshalJSON writes {"skill_id", "priority"}, with priority null unless
// PriorityKnown.
func (f HubFallback) MarshalJSON() ([]byte, error) {
	var priority *int64
	if f.PriorityKnown {
		priority = &f.Priority
	}
	return json.Marshal(struct {
		SkillID  string `json:"skill_id"`
		Priority *int64 `json:"priority"`
	}{f.SkillID, priority})
}

// UnmarshalJSON reads what MarshalJSON writes: a null or absent priority
// leaves PriorityKnown false.
func (f *HubFallback) UnmarshalJSON(data []byte) error {
	var raw struct {
		SkillID  string `json:"skill_id"`
		Priority *int64 `json:"priority"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*f = HubFallback{SkillID: raw.SkillID}
	if raw.Priority != nil {
		f.Priority, f.PriorityKnown = *raw.Priority, true
	}
	return nil
}

// HubIntentCapabilities enriches the existing inventory without changing
// HubIntentInventory struct literals. Unknown fallbacks do not mean none.
type HubIntentCapabilities struct {
	Inventory      HubIntentInventory `json:"inventory"`
	Fallbacks      []HubFallback      `json:"fallbacks"`
	FallbacksKnown bool               `json:"fallbacks_known"`
}

// MayAnswer conservatively avoids declaring a language unsupported just
// because no registered intent has phrases. It is not a language guarantee.
func (c HubIntentCapabilities) MayAnswer(lang string) bool {
	for _, intent := range c.Inventory.Intents() {
		if intent.Enabled && len(intent.PhrasesFor(lang)) != 0 {
			return true
		}
	}
	return len(c.Fallbacks) != 0 || !c.FallbacksKnown
}

// ListFallbacks asks ovos.skills.list which skills answer whatever nothing
// else matched: the hub's own skills (session "default", or none named) that
// declared FallbackCapability, sorted by skill id, each once, with no
// priority (see HubFallback). It used to ask ovos.skills.fallback.list,
// which no hub ever answered.
//
// nil and a non-nil empty slice are different answers. Empty means the hub
// said it has no fallbacks; nil means it could not say -- a timeout, a
// *PolicyDeniedError (a connection not granted ovos.skills.list), ok: false,
// or a "skills" that is not a list. Caller cancellation and transport errors
// still propagate.
func (c *Client) ListFallbacks(ctx context.Context, timeout time.Duration) ([]HubFallback, error) {
	if timeout <= 0 {
		timeout = DefaultIntentTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	event, err := c.requestReply(probeCtx, EventSkillsList, EventSkillsListResponse, Data{}, "", timeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var denied *PolicyDeniedError
		if errors.Is(err, ErrTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &denied) {
			return nil, nil
		}
		return nil, err
	}
	if ok, exists := event.Data["ok"].(bool); exists && !ok {
		return nil, nil
	}
	rows, ok := event.Data["skills"].([]any)
	if !ok {
		return nil, nil
	}
	return fallbacksFromSkills(rows), nil
}

// fallbacksFromSkills keeps the rows of an ovos.skills.list answer that are
// this hub's own fallback skills, in skill id order. A row in another session
// is somebody else's ("default" is where the runtime loads every installed
// skill, and a row naming no session is taken as the hub's); a malformed row
// is skipped and the rest kept.
func fallbacksFromSkills(rows []any) []HubFallback {
	found := map[string]struct{}{}
	for _, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			continue
		}
		skill, ok := row["skill_id"].(string)
		if !ok || strings.TrimSpace(skill) == "" {
			continue
		}
		if session, present := row["session_id"]; present && session != nil && session != "" && session != "default" {
			continue
		}
		capabilities, ok := row["capabilities"].([]any)
		if !ok {
			continue
		}
		for _, capability := range capabilities {
			if capability == FallbackCapability {
				found[strings.TrimSpace(skill)] = struct{}{}
				break
			}
		}
	}
	skills := make([]string, 0, len(found))
	for skill := range found {
		skills = append(skills, skill)
	}
	sort.Strings(skills)
	result := make([]HubFallback, 0, len(skills))
	for _, skill := range skills {
		result = append(result, HubFallback{SkillID: skill})
	}
	return result
}

// IntentsWithCapabilities adds the optional fallback-handler probe, bounded
// to FallbackProbeTimeout (1.5 seconds). Existing Intents remains available for callers that only
// need the manifest and its established return type.
func (c *Client) IntentsWithCapabilities(ctx context.Context, languages []string, opts ...IntentOptions) (HubIntentCapabilities, error) {
	inventory, err := c.Intents(ctx, languages, opts...)
	if err != nil {
		return HubIntentCapabilities{}, err
	}
	timeout := min(intentOptions(opts).Timeout, FallbackProbeTimeout)
	fallbacks, err := c.ListFallbacks(ctx, timeout)
	if err != nil {
		return HubIntentCapabilities{}, err
	}
	return HubIntentCapabilities{Inventory: inventory, Fallbacks: fallbacks, FallbacksKnown: fallbacks != nil}, nil
}
