package thalovant

// Carrying a conversation between the turns of a named session.
//
// A hub keeps nothing for one: OVOS-SESSION-2 §2.2 makes the orchestrator
// stateless, so the carrier a client sends is the whole snapshot and whatever
// the last turn activated is discarded the moment it ends. Without
// converse_handlers the converse pipeline has no skill to poll and every
// follow-up reaches the fallback instead of the skill that just answered.
//
// The cases are contracts/conformance/conversation-vectors.json and
// mesh-vectors.json, shared with every other SDK so that being on par is
// something a machine checks rather than something a digest asserts.

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
)

func conformanceVectors(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return spec
}

func TestCarryMatchesConversationVectors(t *testing.T) {
	spec := conformanceVectors(t, "conversation-vectors.json")
	for _, row := range spec["cases"].([]any) {
		test := row.(map[string]any)
		previous, _ := test["previous"].(map[string]any)
		session, _ := test["session"].(map[string]any)
		expected, _ := test["expected"].(map[string]any)
		got := CarryConversation(previous, session)
		// Recorded before the assert: what this SDK produced, not a
		// restatement of what the vector says it should have.
		recordConformance(t, "conversation-vectors.json", test["name"].(string), got)
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("%v: got %v, want %v", test["name"], got, expected)
		}
	}
}

func TestCarriedFieldsAreTheOnesTheVectorsName(t *testing.T) {
	spec := conformanceVectors(t, "conversation-vectors.json")
	var want []string
	for _, field := range spec["carried_fields"].([]any) {
		want = append(want, field.(string))
	}
	got := append([]string(nil), ConversationSessionFields...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("carried fields: got %v, want %v", got, want)
	}
}

func TestForbiddenFieldsNeverTravel(t *testing.T) {
	// A remembered lang would pin a bilingual conversation to whichever
	// language it opened in, which is the failure this list prevents.
	spec := conformanceVectors(t, "conversation-vectors.json")
	for _, raw := range spec["never_carried"].([]any) {
		field := raw.(string)
		for _, carried := range ConversationSessionFields {
			if carried == field {
				t.Fatalf("%s must never be carried", field)
			}
		}
	}
}

func TestHiveKindsAreTheOnesTheVectorsName(t *testing.T) {
	spec := conformanceVectors(t, "mesh-vectors.json")
	var want []string
	for _, kind := range spec["kinds"].([]any) {
		want = append(want, kind.(string))
	}
	got := append([]string(nil), HiveKinds...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hive kinds: got %v, want %v", got, want)
	}
}

func TestOwnTrafficIsNotAHiveKind(t *testing.T) {
	// query and cascade belong to Ask; subscribing to one here would quietly
	// compete for the same replies.
	spec := conformanceVectors(t, "mesh-vectors.json")
	for _, raw := range spec["refused_kinds"].([]any) {
		refused := raw.(string)
		for _, kind := range HiveKinds {
			if kind == refused {
				t.Fatalf("%s must not be a hive kind", refused)
			}
		}
	}
}

func TestBothSessionAliasesSurviveAFullConversationStore(t *testing.T) {
	// With the store full, filing the two ids as separate entries let the
	// second evict the first -- and because ranging a Go map picks an
	// arbitrary key, which one it dropped was not even predictable. A caller
	// continuing under the request id then found no carry.
	c := &Client{}
	carried := Context{"session": map[string]any{
		"session_id": "x", "converse_handlers": []any{"skill.a"},
	}}
	for i := 0; i < maxRememberedConversations; i++ {
		c.rememberConversation([]string{fmt.Sprintf("filler-%d", i)}, carried)
	}
	if got := len(c.conversations); got != maxRememberedConversations {
		t.Fatalf("filler entries: got %d, want %d", got, maxRememberedConversations)
	}

	c.rememberConversation([]string{"sat-1", "hub:sat-1"}, carried)

	for _, id := range []string{"sat-1", "hub:sat-1"} {
		next := c.continueConversation(Context{}, id)
		session, _ := next["session"].(map[string]any)
		if session == nil || session["converse_handlers"] == nil {
			t.Fatalf("%s lost its carry: %#v", id, next)
		}
	}
	// One conversation, two names: the bound counts conversations.
	seen := map[uint64]bool{}
	for _, entry := range c.conversations {
		seen[entry.seq] = true
	}
	if len(seen) > maxRememberedConversations {
		t.Fatalf("distinct conversations: got %d, want <= %d", len(seen), maxRememberedConversations)
	}
}

func TestConversationAliasesAreBounded(t *testing.T) {
	// A hub that answers under a fresh translated id every turn would
	// otherwise grow one group for ever.
	c := &Client{}
	carried := Context{"session": map[string]any{
		"session_id": "x", "converse_handlers": []any{"skill.a"},
	}}
	for i := 0; i < maxConversationAliases*3; i++ {
		c.rememberConversation([]string{"sat-1", fmt.Sprintf("hub-%d", i)}, carried)
	}
	entry := c.conversations["sat-1"]
	if entry == nil {
		t.Fatal("the request id must always survive: it is what the next turn sends")
	}
	if len(entry.group) > maxConversationAliases {
		t.Fatalf("aliases: got %d, want <= %d", len(entry.group), maxConversationAliases)
	}
}

func TestAWhitespaceHubAliasIsNotAKey(t *testing.T) {
	// " " is a key unrelated conversations would all share, so storing it
	// merges their carried state. A hub answering with whitespace has told us
	// nothing; only a real id becomes an alias.
	c := &Client{}
	first := Context{"session": map[string]any{
		"session_id": "a", "converse_handlers": []any{"skill.a"},
	}}
	second := Context{"session": map[string]any{
		"session_id": "b", "converse_handlers": []any{"skill.b"},
	}}
	c.rememberConversation([]string{"sat-1", " "}, first)
	c.rememberConversation([]string{"sat-2", " "}, second)

	if _, shared := c.conversations[" "]; shared {
		t.Fatal("whitespace must not be stored as an alias")
	}
	for id, want := range map[string]string{"sat-1": "skill.a", "sat-2": "skill.b"} {
		next := c.continueConversation(Context{}, id)
		session, _ := next["session"].(map[string]any)
		handlers, _ := session["converse_handlers"].([]any)
		if len(handlers) != 1 || handlers[0] != want {
			t.Fatalf("%s carried %#v, want %s", id, handlers, want)
		}
	}
}
