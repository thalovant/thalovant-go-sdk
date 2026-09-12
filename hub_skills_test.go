package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHubSkillsSharedRuntimeRequestsAndResume(t *testing.T) {
	var paths, methods []string
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		methods = append(methods, r.Method)
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing auth")
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/operations/") {
			polls++
			status := "applied"
			if polls > 1 {
				status = "ready"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "op-1", "status": status})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/history") {
			if r.URL.Query().Get("limit") != "200" {
				t.Error("wrong limit")
			}
			_, _ = w.Write([]byte(`{"data":[{"kind":"event","actor_email":null},{"kind":"operation","status":"failed"}]}`))
			return
		}
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"hub_id":"h/1","runtime_group_id":"shared","data":[]}`))
			return
		}
		if r.Method != "DELETE" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["version"] != "1.2.0" {
				t.Errorf("wrong version: %v", body)
			}
		}
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"operation_id":"op-1","skill":"s/1","state":"installing"}`))
	}))
	defer server.Close()
	c := NewControlPlane(server.URL, "token")
	ctx := context.Background()
	if _, err := c.ListHubSkills(ctx, "h/1"); err != nil {
		t.Fatal(err)
	}
	history, err := c.ListHubSkillHistory(ctx, "h/1", 200)
	if err != nil || len(history["data"].([]any)) != 2 {
		t.Fatal(history, err)
	}
	accepted, err := c.InstallHubSkill(ctx, "h/1", "s/1", "1.2.0", HubSkillWaitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done, err := c.WaitForHubSkillOperation(ctx, accepted, HubSkillWaitOptions{PollInterval: time.Millisecond})
	if err != nil || done["state"] != "installed" {
		t.Fatal(done, err)
	}
	if accepted["state"] != "installing" {
		t.Fatal("mutated caller's accepted response")
	}
	if _, err = c.UpdateHubSkill(ctx, "h/1", "s/1", "1.2.0", HubSkillWaitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.RemoveHubSkill(ctx, "h/1", "s/1", HubSkillWaitOptions{}); err != nil {
		t.Fatal(err)
	}
	if paths[0] != "/v1/hubs/h%2F1/skills" || paths[len(paths)-1] != "/v1/hubs/h%2F1/skills/s%2F1" {
		t.Fatal(paths)
	}
	if strings.Join(methods, ",") != "GET,GET,POST,GET,GET,PATCH,DELETE" {
		t.Fatal(methods)
	}
}
func TestHubSkillPollFailuresRetainAcceptanceAndNeverReplay(t *testing.T) {
	for _, status := range []string{"failed", "timed_out", "applied", "http-error"} {
		t.Run(status, func(t *testing.T) {
			writes, reads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					writes++
					w.WriteHeader(202)
					_, _ = w.Write([]byte(`{"operation_id":"op-1","state":"installing"}`))
					return
				}
				reads++
				if status == "http-error" {
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"detail":"private-data"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "op-1", "status": status})
			}))
			defer server.Close()
			c := NewControlPlane(server.URL, "token")
			accepted, err := c.InstallHubSkill(context.Background(), "h", "s", "latest", HubSkillWaitOptions{Wait: true, Timeout: 100 * time.Millisecond, PollInterval: time.Second})
			if err == nil || accepted["operation_id"] != "op-1" || !strings.Contains(err.Error(), "op-1") || strings.Contains(err.Error(), "private-data") {
				t.Fatal(accepted, err)
			}
			if writes != 1 || reads != 1 {
				t.Fatal(writes, reads)
			}
		})
	}
}
func TestHubSkillValidationAndCancellationSendNothing(t *testing.T) {
	c := &ControlPlane{APIURL: "https://invalid.example", AccessToken: "token"}
	for _, limit := range []int{0, 201} {
		if _, err := c.ListHubSkillHistory(context.Background(), "h", limit); err == nil {
			t.Fatal("accepted invalid limit")
		}
	}
	if _, err := c.InstallHubSkill(context.Background(), "h", "s", "latest", HubSkillWaitOptions{Timeout: -1}); err == nil {
		t.Fatal("accepted invalid wait")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	accepted := map[string]any{"operation_id": "op-1", "state": "removing"}
	_, err := c.WaitForHubSkillOperation(ctx, accepted, HubSkillWaitOptions{})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "op-1") {
		t.Fatal(err)
	}
}
