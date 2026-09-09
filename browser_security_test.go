package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestBrowserCommandRejectsUnsafeTargetsAndKeepsURLAsOneArgument(t *testing.T) {
	for _, target := range []string{"--help", "file:///tmp/launch", "javascript:alert(1)", "data:text/html,hello", "https://user:synthetic-password@example.invalid", "https://", "https:opaque", "https://example.invalid/\n--option", " https://example.invalid", "https://example.invalid/ space"} {
		for _, platform := range []string{"linux", "darwin", "windows"} {
			if cmd, err := browserCommand(target, platform); cmd != nil || !errors.Is(err, ErrAPI) {
				t.Fatal("unsafe browser command accepted", platform, cmd, err)
			}
		}
	}
	target := "https://example.invalid/activate?user_code=SYNTHETIC&next=echo%20synthetic"
	for _, tc := range []struct {
		platform string
		args     []string
	}{
		{"linux", []string{"xdg-open", target}}, {"darwin", []string{"open", target}}, {"windows", []string{"rundll32", "url.dll,FileProtocolHandler", target}},
	} {
		cmd, err := browserCommand(target, tc.platform)
		if err != nil || !reflect.DeepEqual(cmd.Args, tc.args) {
			t.Fatal("URL was interpreted as command structure", tc.platform, cmd, err)
		}
	}
}
func TestDeviceFlowRejectsUnsafeVerificationURLsBeforePromptOrLaunch(t *testing.T) {
	original := openBrowser
	opened := 0
	openBrowser = func(string) error { opened++; return nil }
	defer func() { openBrowser = original }()
	for _, field := range []string{"verification_uri", "verification_uri_complete"} {
		for _, target := range []string{"--help", "javascript:alert(1)", "https://user:synthetic-password@example.invalid"} {
			var grant map[string]any
			if err := json.Unmarshal([]byte(testDeviceGrant), &grant); err != nil {
				t.Fatal(err)
			}
			grant[field] = target
			raw, err := json.Marshal(grant)
			if err != nil {
				t.Fatal(err)
			}
			calls := &deviceFlowCalls{}
			server := newDeviceFlowServer(t, string(raw), []scriptedReply{{http.StatusOK, testDeviceToken}}, calls)
			prompted := false
			_, err = NewControlPlane(server.URL, "").LoginWithBrowser(context.Background(), DeviceLoginOptions{Prompt: func(map[string]any) { prompted = true }})
			server.Close()
			if !errors.Is(err, ErrAPI) || prompted || opened != 0 || len(calls.token) != 0 {
				t.Fatal("invalid verification target escaped validation", field, err, prompted, opened, len(calls.token))
			}
		}
	}
}
