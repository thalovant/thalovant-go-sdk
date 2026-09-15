package thalovant

import "testing"

// What a phone calls a hub. It called them slugs until 2026-09-15.

func TestAHubIsCalledWhatAPersonWasShown(t *testing.T) {
	// Exactly what a phone was offered: name IS the slug, and the readable
	// title sits in the catalog entry.
	hub := map[string]any{
		"name": "ops-copilot",
		"slug": "ops-copilot",
		"spec": map[string]any{"catalog": map[string]any{"title": "Ops Copilot"}},
	}
	if got := HubDisplayName(hub); got != "Ops Copilot" {
		t.Fatalf("HubDisplayName = %q, want Ops Copilot", got)
	}
}

func TestARealNameWinsWhenThereIsNoCatalogEntry(t *testing.T) {
	hub := map[string]any{"name": "The Kitchen", "slug": "kitchen"}
	if got := HubDisplayName(hub); got != "The Kitchen" {
		t.Fatalf("HubDisplayName = %q", got)
	}
}

func TestASlugIsMadeReadableRatherThanShownRaw(t *testing.T) {
	for hub, want := range map[string]string{"daily-desk": "Daily Desk", "local_pulse": "Local Pulse"} {
		if got := HubDisplayName(map[string]any{"slug": hub}); got != want {
			t.Fatalf("HubDisplayName(%q) = %q, want %q", hub, got, want)
		}
	}
	got := HubDisplayName(map[string]any{"name": "news-stream", "slug": "news-stream"})
	if got != "News Stream" {
		t.Fatalf("HubDisplayName = %q, want News Stream", got)
	}
}

func TestAHubDescribedWithNothingStillSaysSomething(t *testing.T) {
	for _, hub := range []map[string]any{
		{"id": "1"},
		{"name": "", "slug": "   "},
		{"spec": "nonsense"},
		{"spec": map[string]any{"catalog": []any{}}},
	} {
		if got := HubDisplayName(hub); got != "A Thalovant hub" {
			t.Fatalf("HubDisplayName(%v) = %q", hub, got)
		}
	}
}

func TestASlugIsCapitalisedByRuneNotByByte(t *testing.T) {
	// word[:1] takes the first BYTE, which cuts a multi-byte character in half.
	if got := HubDisplayName(map[string]any{"slug": "école-du-soir"}); got != "École Du Soir" {
		t.Fatalf("HubDisplayName = %q, want École Du Soir", got)
	}
	if got := HubDisplayName(map[string]any{"slug": "日本-hub"}); got != "日本 Hub" {
		t.Fatalf("HubDisplayName = %q", got)
	}
}
