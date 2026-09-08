package thalovant

import "testing"

// Go randomizes map iteration order, so a payload carrying both spellings of
// one field used to send either value depending on the run. The canonical
// snake_case spelling is documented, so it is the one that must survive.
func TestSnakeCaseRequestPayloadPrefersTheCanonicalSpelling(t *testing.T) {
	renames := map[string]string{"ownerId": "owner_id"}
	for attempt := 0; attempt < 200; attempt++ {
		out := snakeCaseRequestPayload(map[string]any{
			"ownerId":  "from-camel",
			"owner_id": "from-snake",
		}, renames)
		if out["owner_id"] != "from-snake" {
			t.Fatalf("attempt %d: collision resolved to %v, want the canonical value", attempt, out["owner_id"])
		}
		if len(out) != 1 {
			t.Fatalf("attempt %d: expected one merged key, got %v", attempt, out)
		}
	}
}

func TestSnakeCaseRequestPayloadStillRenamesWithoutCollision(t *testing.T) {
	out := snakeCaseRequestPayload(map[string]any{
		"ownerId": "from-camel",
		"other":   1,
	}, map[string]string{"ownerId": "owner_id"})

	if out["owner_id"] != "from-camel" {
		t.Fatalf("rename lost: %v", out)
	}
	if _, present := out["ownerId"]; present {
		t.Fatalf("camelCase key should not survive: %v", out)
	}
	if out["other"] != 1 {
		t.Fatalf("unrelated key lost: %v", out)
	}
}
