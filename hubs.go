package thalovant

import "strings"

// HubDisplayName returns what to call a hub on a screen somebody is reading.
//
// Every control-plane read in this SDK returns raw JSON, so each caller picks
// its own fields -- and on 2026-09-15 a phone offered somebody a list of rooms
// called "ops-copilot", "daily-desk", "news-stream". Those are slugs. The app
// was not careless: it read name and preferred it over slug, and on that
// deployment name holds the slug. The name a person was shown when the hub was
// made lives in spec.catalog.title.
//
// One place to get that wrong is better than one per app.
func HubDisplayName(hub map[string]any) string {
	if spec, ok := hub["spec"].(map[string]any); ok {
		if catalog, ok := spec["catalog"].(map[string]any); ok {
			if title := text(catalog["title"]); title != "" {
				return title
			}
		}
	}

	name := text(hub["name"])
	slug := text(hub["slug"])
	// A name that is exactly the slug is the slug.
	if name != "" && name != slug {
		return name
	}

	identifier := name
	if identifier == "" {
		identifier = slug
	}
	if identifier == "" {
		return "A Thalovant hub"
	}
	words := strings.FieldsFunc(identifier, func(r rune) bool { return r == '-' || r == '_' })
	for i, word := range words {
		// By rune, not by byte: word[:1] takes the first BYTE, which cuts a
		// multi-byte character in half and produces mojibake for any hub
		// somebody named in their own language.
		runes := []rune(word)
		words[i] = strings.ToUpper(string(runes[0])) + string(runes[1:])
	}
	readable := strings.Join(words, " ")
	if readable == "" {
		return "A Thalovant hub"
	}
	return readable
}

func text(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}
