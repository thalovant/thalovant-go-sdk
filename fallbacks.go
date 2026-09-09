package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"
)

const EventFallbackList = "ovos.skills.fallback.list"
const EventFallbackListResponse = "ovos.skills.fallback.list.response"

type HubFallback struct {
	SkillID  string `json:"skill_id"`
	Priority int64  `json:"priority"`
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

// ListFallbacks returns nil for unsupported, refused, silent or malformed
// discovery; a non-nil empty slice means the hub reported no handlers.
// Caller cancellation and transport errors still propagate.
func (c *Client) ListFallbacks(ctx context.Context, timeout time.Duration) ([]HubFallback, error) {
	if timeout <= 0 {
		timeout = DefaultIntentTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	event, err := c.requestReply(probeCtx, EventFallbackList, EventFallbackListResponse, Data{}, "", timeout)
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
	rows, ok := event.Data["fallbacks"].([]any)
	if !ok {
		return nil, nil
	}
	result := make([]HubFallback, 0, len(rows))
	for _, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			continue
		}
		skill, ok := row["skill_id"].(string)
		if !ok || skill == "" {
			continue
		}
		priority, valid := fallbackPriority(row["priority"])
		if valid {
			result = append(result, HubFallback{SkillID: skill, Priority: priority})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Priority == result[j].Priority {
			return result[i].SkillID < result[j].SkillID
		}
		return result[i].Priority < result[j].Priority
	})
	return result, nil
}

func fallbackPriority(raw any) (int64, bool) {
	switch value := raw.(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case bool:
		if value {
			return 1, true
		}
		return 0, true
	case json.Number:
		if rank, err := strconv.ParseInt(string(value), 10, 64); err == nil {
			return rank, true
		}
		valueFloat, err := value.Float64()
		if err != nil {
			return 0, false
		}
		return fallbackPriority(valueFloat)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value < -9223372036854775808.0 || value >= 9223372036854775808.0 {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, true
	}
}

// IntentsWithCapabilities adds the optional fallback-handler probe, bounded
// to 1.5 seconds. Existing Intents remains available for callers that only
// need the manifest and its established return type.
func (c *Client) IntentsWithCapabilities(ctx context.Context, languages []string, opts ...IntentOptions) (HubIntentCapabilities, error) {
	inventory, err := c.Intents(ctx, languages, opts...)
	if err != nil {
		return HubIntentCapabilities{}, err
	}
	timeout := min(intentOptions(opts).Timeout, 1500*time.Millisecond)
	fallbacks, err := c.ListFallbacks(ctx, timeout)
	if err != nil {
		return HubIntentCapabilities{}, err
	}
	return HubIntentCapabilities{Inventory: inventory, Fallbacks: fallbacks, FallbacksKnown: fallbacks != nil}, nil
}
