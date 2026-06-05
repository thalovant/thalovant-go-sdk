package thalovant

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type DisplayItem struct {
	Kind    string
	Text    string
	Data    any
	Title   string
	Payload string
	URL     string
	Silent  bool
}

var ssmlPattern = regexp.MustCompile(`<{1}/?[^>]*>{1}`)

func StripSSML(text string) string {
	return ssmlPattern.ReplaceAllString(text, "")
}

func (e Event) DisplayText() string {
	return StripSSML(e.Text())
}

func (e Event) RichMedia() map[string]any {
	return RichMediaFromData(e.Data)
}

func (e Event) DisplayItems(maxTextChars int) []DisplayItem {
	return DisplayItemsFromEventData(e.Data, e.Name, maxTextChars)
}

func (r Reply) DisplayText() string {
	return StripSSML(r.Text)
}

func (r Reply) DisplayItems(maxTextChars int) []DisplayItem {
	var items []DisplayItem
	for _, event := range r.Events {
		items = append(items, event.DisplayItems(maxTextChars)...)
	}
	if len(items) == 0 && r.Text != "" {
		items = append(items, DisplayItem{Kind: "text", Text: r.DisplayText()})
	}
	return items
}

func RichMediaFromData(data Data) map[string]any {
	raw := firstExisting(data, "rich_media_data", "rich_media", "display")
	if media := mapValue(parseJSON(raw)); len(media) > 0 {
		return media
	}
	direct := map[string]any{}
	for _, key := range []string{"table", "attachment", "attachments", "quick_replies", "buttons", "image", "images"} {
		if val, ok := data[key]; ok {
			direct[key] = val
		}
	}
	return direct
}

func DisplayItemsFromEventData(data Data, eventName string, maxTextChars int) []DisplayItem {
	var items []DisplayItem
	if text := textFromData(data); text != "" {
		for _, chunk := range textChunks(StripSSML(text), maxTextChars) {
			items = append(items, DisplayItem{Kind: "text", Text: chunk, Silent: truthy(data["silent"]) || eventName == "write"})
		}
	}
	media := RichMediaFromData(data)
	if table := parseJSON(media["table"]); table != nil {
		items = append(items, DisplayItem{Kind: "table", Data: table})
	}
	for _, attachment := range attachmentValues(media) {
		payload := mapValue(attachment["payload"])
		url := stringValue(firstNonNil(payload["src"], payload["url"], attachment["src"], attachment["url"]))
		kind := stringValue(attachment["type"])
		if kind == "" {
			kind = "attachment"
		}
		itemKind := "attachment"
		if kind == "image" {
			itemKind = "image"
		}
		items = append(items, DisplayItem{Kind: itemKind, Data: attachment, Title: stringValue(attachment["title"]), URL: url})
	}
	choices := choiceValues(firstNonNil(media["quick_replies"], media["buttons"]))
	if len(choices) > 0 {
		items = append(items, DisplayItem{Kind: "choices", Data: choices})
	}
	for _, image := range asSlice(firstNonNil(media["image"], media["images"])) {
		url := stringValue(image)
		if raw := mapValue(image); len(raw) > 0 {
			url = stringValue(firstNonNil(raw["src"], raw["url"]))
		}
		if url != "" {
			items = append(items, DisplayItem{Kind: "image", Data: image, URL: url})
		}
	}
	return items
}

func textFromData(data Data) string {
	if val, ok := data["utterance"].(string); ok {
		return val
	}
	if val, ok := data["text"].(string); ok {
		return val
	}
	if val, ok := data["utterances"].(string); ok {
		return val
	}
	if raw, ok := data["utterances"].([]any); ok {
		var out []string
		for _, item := range raw {
			if val, ok := item.(string); ok {
				out = append(out, val)
			}
		}
		return strings.Join(out, " ")
	}
	return ""
}

func parseJSON(raw any) any {
	if text, ok := raw.(string); ok {
		var out any
		if err := json.Unmarshal([]byte(text), &out); err == nil {
			return out
		}
	}
	return raw
}

func firstExisting(data Data, keys ...string) any {
	for _, key := range keys {
		if val, ok := data[key]; ok {
			return val
		}
	}
	return nil
}

func firstNonNil(values ...any) any {
	for _, val := range values {
		if val != nil {
			return val
		}
	}
	return nil
}

func stringValue(raw any) string {
	if val, ok := raw.(string); ok {
		return val
	}
	if raw == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

func attachmentValues(media map[string]any) []map[string]any {
	raw := firstNonNil(media["attachments"], media["attachment"])
	if one := mapValue(raw); len(one) > 0 {
		return []map[string]any{one}
	}
	var out []map[string]any
	for _, item := range asSlice(raw) {
		if val := mapValue(item); len(val) > 0 {
			out = append(out, val)
		}
	}
	return out
}

func choiceValues(raw any) []map[string]any {
	var out []map[string]any
	for _, item := range asSlice(parseJSON(raw)) {
		if text, ok := item.(string); ok {
			out = append(out, map[string]any{"title": text, "payload": text, "data": text})
			continue
		}
		value := mapValue(item)
		if len(value) == 0 {
			continue
		}
		title := stringValue(firstNonNil(value["title"], value["label"], value["text"]))
		payload := stringValue(firstNonNil(value["payload"], value["value"], title))
		out = append(out, map[string]any{"title": title, "payload": payload, "data": value})
	}
	return out
}

func asSlice(raw any) []any {
	raw = parseJSON(raw)
	switch val := raw.(type) {
	case nil:
		return nil
	case []any:
		return val
	default:
		return []any{val}
	}
}

func textChunks(text string, maxChars int) []string {
	if maxChars <= 0 || len(text) <= maxChars {
		return []string{text}
	}
	var out []string
	remaining := text
	for len(remaining) > maxChars {
		index := strings.LastIndex(remaining[:maxChars], " ")
		if index <= 0 {
			index = maxChars
		}
		out = append(out, strings.TrimSpace(remaining[:index]))
		remaining = strings.TrimSpace(remaining[index:])
	}
	if remaining != "" {
		out = append(out, remaining)
	}
	return out
}
