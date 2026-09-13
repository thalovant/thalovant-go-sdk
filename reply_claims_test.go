package thalovant

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestReplyClaimVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/reply-claim-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Cases []struct {
			Name     string
			Handled  bool
			Failed   bool
			Contexts []Context
			Expected struct {
				PipelineIDs []string `json:"pipeline_ids"`
				SkillIDs    []string `json:"skill_ids"`
				Claimed     bool
			}
		}
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, row := range data.Cases {
		t.Run(row.Name, func(t *testing.T) {
			reply := Reply{Handled: row.Handled, OK: row.Handled && !row.Failed}
			if row.Failed {
				reply.FailureEvent = &Event{Name: "failure"}
			}
			for _, context := range row.Contexts {
				reply.Events = append(reply.Events, Event{Name: "speak", Context: context})
			}
			if !reflect.DeepEqual(reply.PipelineIDs(), row.Expected.PipelineIDs) || !reflect.DeepEqual(reply.SkillIDs(), row.Expected.SkillIDs) || reply.Claimed() != row.Expected.Claimed {
				t.Fatalf("unexpected metadata: %v %v %v", reply.PipelineIDs(), reply.SkillIDs(), reply.Claimed())
			}
		})
	}
}
