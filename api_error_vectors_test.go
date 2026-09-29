package thalovant

// What a control-plane error carries, against the vectors every SDK shares:
// contracts/conformance/api-error-vectors.json in the Python SDK, vendored
// here unchanged and pinned by the parity contract. The API answers a refusal
// with a Problem+JSON body whose structured fields say what to do next -- the
// images a caller may pin instead, the plan's numbers -- and a line cut at 256
// runes is not where anybody can read them. Each case is served by a real
// loopback HTTP server and read back through the public client, so what is
// recorded is what a caller gets.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

type apiErrorVector struct {
	Name     string `json:"name"`
	Response struct {
		Status      int    `json:"status"`
		ContentType string `json:"content_type"`
		Body        string `json:"body"`
	} `json:"response"`
	Expect          map[string]any `json:"expect"`
	MessageExcludes []string       `json:"message_excludes"`
}

func loadAPIErrorVectors(t *testing.T, name string) []apiErrorVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []apiErrorVector `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) == 0 {
		t.Fatalf("%s has no cases", name)
	}
	return vectors.Cases
}

// answering is a loopback API that answers every request with the vector's
// status, Content-Type and body, byte for byte. The device-flow authorize
// call is the one exception, so a sign-in reaches its token poll.
func answering(t *testing.T, vector apiErrorVector) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/device/authorize" {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"device_code":"device-code-1","user_code":"WDJB-MJHT","verification_uri":"https://dash.thalovant.com/activate","expires_in":900,"interval":0}`))
			return
		}
		w.Header().Set("content-type", vector.Response.ContentType)
		w.WriteHeader(vector.Response.Status)
		_, _ = w.Write([]byte(vector.Response.Body))
	}))
	t.Cleanup(server.Close)
	return server
}

func refusedAPIError(t *testing.T, err error) *APIError {
	t.Helper()
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("want an error matching ErrAPI, got %T %v", err, err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T %v", err, err)
	}
	return apiErr
}

// producedAPIError spells what the error carries the way the vectors do. Go
// has no optional string, so an absent code or detail is "" here and null in
// every other SDK and in the vectors; no case expects a present-but-empty
// one, because a blank code or detail is absent by the rules.
func producedAPIError(apiErr *APIError) map[string]any {
	var problem any
	if apiErr.Problem != nil {
		problem = apiErr.Problem
	}
	return map[string]any{
		"status":  apiErr.StatusCode,
		"code":    absentIfEmpty(apiErr.Code),
		"detail":  absentIfEmpty(apiErr.ProblemDetail),
		"problem": problem,
	}
}

func assertAPIErrorMatches(t *testing.T, produced map[string]any, expect map[string]any) {
	t.Helper()
	if status, _ := expect["status"].(float64); produced["status"] != int(status) {
		t.Errorf("status = %v, want %v", produced["status"], expect["status"])
	}
	for _, field := range []string{"code", "detail", "problem"} {
		if !reflect.DeepEqual(produced[field], expect[field]) {
			got, _ := json.Marshal(produced[field])
			want, _ := json.Marshal(expect[field])
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
}

// displayForms is everything a caller sees when an error is printed or
// logged: the message, and what fmt and log make of it by default.
func displayForms(err error) map[string]string {
	return map[string]string{
		"Error()":  err.Error(),
		"Sprint":   fmt.Sprint(err),
		"%v":       fmt.Sprintf("%v", err),
		"%+v":      fmt.Sprintf("%+v", err),
		"%s":       fmt.Sprintf("%s", err),
		"%q":       fmt.Sprintf("%q", err),
		"wrapped":  fmt.Errorf("release failed: %w", err).Error(),
		"Detail":   refusedDetail(err),
		"Sprintln": fmt.Sprintln(err),
	}
}

func refusedDetail(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Detail
	}
	return ""
}

func TestAPIErrorCarriesWhatItsVectorNames(t *testing.T) {
	for _, vector := range loadAPIErrorVectors(t, "api-error-vectors.json") {
		t.Run(vector.Name, func(t *testing.T) {
			server := answering(t, vector)
			_, err := NewControlPlane(server.URL, "synthetic-token").GetHub(context.Background(), "hub-1")
			apiErr := refusedAPIError(t, err)
			produced := producedAPIError(apiErr)
			// Recorded before the assert: the record is what this SDK
			// produced, not a restatement of what the vector says it should
			// have.
			recordConformance(t, "api-error-vectors.json", vector.Name, produced)
			assertAPIErrorMatches(t, produced, vector.Expect)
			// The line Error() prints is the one it always printed.
			if want := fmt.Sprintf("%v: HTTP %d: %s", ErrAPI, vector.Response.Status, serverErrorDetail([]byte(vector.Response.Body))); err.Error() != want {
				t.Errorf("Error() = %q, want the unchanged line %q", err.Error(), want)
			}
			for _, echoed := range vector.MessageExcludes {
				for form, text := range displayForms(err) {
					if strings.Contains(text, echoed) {
						t.Errorf("%s repeats %q, which the body echoed back from the request: %s", form, echoed, text)
					}
				}
			}
		})
	}
}

// Every other place a non-2xx answer becomes an error goes through the same
// helper: the read half of a guarded configuration merge, and the device
// sign-in's token poll. Each must carry exactly what GetHub does.
func TestEveryControlPathCarriesTheSameAPIError(t *testing.T) {
	disabled := false
	paths := map[string]func(control *ControlPlane) error{
		"UpdateRuntimeGroupConfig": func(control *ControlPlane) error {
			_, err := control.UpdateRuntimeGroupConfig(context.Background(), "rg-1", map[string]any{"lang": "en-us"}, RuntimeGroupConfigOptions{})
			return err
		},
		"LoginWithBrowser": func(control *ControlPlane) error {
			_, err := control.LoginWithBrowser(context.Background(), DeviceLoginOptions{
				OpenBrowser: &disabled,
				Prompt:      func(map[string]any) {},
			})
			return err
		},
	}
	for _, vector := range loadAPIErrorVectors(t, "api-error-vectors.json") {
		for label, call := range paths {
			t.Run(label+"/"+vector.Name, func(t *testing.T) {
				server := answering(t, vector)
				err := call(NewControlPlane(server.URL, "synthetic-token"))
				assertAPIErrorMatches(t, producedAPIError(refusedAPIError(t, err)), vector.Expect)
			})
		}
	}
}

func TestAPIErrorMessageMayBeShortenedButTheDetailNeverIs(t *testing.T) {
	var vector apiErrorVector
	for _, candidate := range loadAPIErrorVectors(t, "api-error-vectors.json") {
		if candidate.Expect["code"] == "platform_image_required" {
			vector = candidate
		}
	}
	if vector.Name == "" {
		t.Fatal("no platform_image_required case in the vectors")
	}
	server := answering(t, vector)
	_, err := NewControlPlane(server.URL, "synthetic-token").GetHub(context.Background(), "hub-1")
	apiErr := refusedAPIError(t, err)

	// The display line is what it always was: one line, bounded, with an
	// ellipsis where it was cut.
	if !strings.HasPrefix(err.Error(), "thalovant api error: HTTP 403: Only an administrator can run an image") {
		t.Fatalf("Error() = %q", err.Error())
	}
	if !strings.HasSuffix(apiErr.Detail, "…") || len([]rune(apiErr.Detail)) != maxServerErrorDetail+1 {
		t.Fatalf("Detail should be cut at %d runes with an ellipsis: %q", maxServerErrorDetail, apiErr.Detail)
	}
	// The sentence the API wrote is whole, and every list it sent is there.
	want, _ := vector.Expect["detail"].(string)
	if apiErr.ProblemDetail != want || len([]rune(apiErr.ProblemDetail)) <= maxServerErrorDetail {
		t.Fatalf("ProblemDetail = %q, want the whole %d-rune sentence", apiErr.ProblemDetail, len([]rune(want)))
	}
	if !strings.HasPrefix(apiErr.ProblemDetail, strings.TrimSuffix(apiErr.Detail, "…")) {
		t.Fatal("Detail should be the start of the same sentence")
	}
	if apiErr.Code != "platform_image_required" {
		t.Fatalf("Code = %q", apiErr.Code)
	}
	allowed, _ := apiErr.Problem["allowed_images"].(map[string]any)
	if !reflect.DeepEqual(allowed["core"], []any{"ghcr.io/thalovant/ovos-core:2026.09.2", "ghcr.io/thalovant/ovos-core:2026.09.3-alpha.1"}) {
		t.Fatalf("allowed_images = %v", apiErr.Problem["allowed_images"])
	}
	if !reflect.DeepEqual(apiErr.Problem["allowed_repositories"], map[string]any{"core": "ghcr.io/thalovant/ovos-core"}) {
		t.Fatalf("allowed_repositories = %v", apiErr.Problem["allowed_repositories"])
	}
	refused, _ := apiErr.Problem["refused_images"].(map[string]any)
	if refused["bus"] != "docker.io/example/ovos-messagebus:custom" {
		t.Fatalf("refused_images = %v", apiErr.Problem["refused_images"])
	}
}

func TestAPIErrorBuiltTheOldWayStillReadsTheOldWay(t *testing.T) {
	var err error = &APIError{StatusCode: 412, Detail: "ETag mismatch"}
	if err.Error() != "thalovant api error: HTTP 412: ETag mismatch" || fmt.Sprintf("%v", err) != err.Error() || fmt.Sprintf("%+v", err) != err.Error() {
		t.Fatalf("Error() = %q", err.Error())
	}
	if !errors.Is(err, ErrAPI) {
		t.Fatal("an APIError literal still matches ErrAPI")
	}
	apiErr := refusedAPIError(t, err)
	if apiErr.Code != "" || apiErr.ProblemDetail != "" || apiErr.Problem != nil {
		t.Fatalf("the new fields are empty on an old literal: %+v", *apiErr)
	}
}

func TestProblemFieldsFollowTheReferenceRules(t *testing.T) {
	cases := []struct {
		name         string
		problem      map[string]any
		code, detail string
	}{
		{"nothing", nil, "", ""},
		{"top level wins over the envelope", map[string]any{"code": "outer", "detail": map[string]any{"code": "inner", "detail": "inner sentence"}}, "outer", "inner sentence"},
		{"kept exactly, not trimmed", map[string]any{"code": " plan_limit ", "detail": "  two  spaces\n"}, " plan_limit ", "  two  spaces\n"},
		// Python's str.strip() counts the information separators as
		// whitespace; unicode.IsSpace does not.
		{"a separator alone is blank", map[string]any{"code": "\x1c\x1f", "detail": "　 "}, "", ""},
		{"a detail that is a list is no sentence", map[string]any{"detail": []any{"a"}}, "", ""},
	}
	for _, tc := range cases {
		code, detail := problemFields(tc.problem)
		if code != tc.code || detail != tc.detail {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, code, detail, tc.code, tc.detail)
		}
	}
}

// A vector set that quietly lost its non-JSON or its nested case would still
// pass, so the shapes the rules name are checked to be there.
func TestAPIErrorVectorsCoverEveryShapeTheRulesName(t *testing.T) {
	has := map[string]bool{}
	for _, vector := range loadAPIErrorVectors(t, "api-error-vectors.json") {
		expect := vector.Expect
		code, _ := expect["code"].(string)
		detail, _ := expect["detail"].(string)
		problem, _ := expect["problem"].(map[string]any)
		has["no problem"] = has["no problem"] || expect["problem"] == nil
		has["code without detail"] = has["code without detail"] || (code != "" && expect["detail"] == nil)
		has["detail without code"] = has["detail without code"] || (detail != "" && expect["code"] == nil)
		_, nested := problem["detail"].(map[string]any)
		has["nested detail"] = has["nested detail"] || nested
		_, list := problem["detail"].([]any)
		has["detail list"] = has["detail list"] || list
		has["detail over 256"] = has["detail over 256"] || len([]rune(detail)) > maxServerErrorDetail
		has["line break"] = has["line break"] || strings.Contains(detail, "\n")
		has["message excludes"] = has["message excludes"] || len(vector.MessageExcludes) > 0
	}
	for _, shape := range []string{"no problem", "code without detail", "detail without code", "nested detail", "detail list", "detail over 256", "line break", "message excludes"} {
		if !has[shape] {
			t.Errorf("no vector covers %s", shape)
		}
	}
}

// %#v used to print the whole Problem map, echoed request included.
func TestAPIErrorGoStringLeavesProblemOut(t *testing.T) {
	const secret = "synthetic-echoed-secret-do-not-log"
	err := apiErrorFromResponse(422, []byte(`{"detail":[{"loc":["body","password"],"msg":"too short","input":"`+secret+`"}],"code":"validation_error"}`), nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Problem == nil {
		t.Fatalf("not an APIError with a problem: %#v", err)
	}
	for _, printed := range []string{
		fmt.Sprintf("%#v", err),
		fmt.Sprintf("%#v", apiErr),
		fmt.Sprintf("%#v", &DeviceLoginDeniedError{APIError: apiErr}),
		fmt.Sprintf("%+v", err),
		fmt.Sprintf("%v", err),
	} {
		if strings.Contains(printed, secret) {
			t.Errorf("printed the echoed input: %s", printed)
		}
	}
	if printed := fmt.Sprintf("%#v", apiErr); !strings.Contains(printed, "StatusCode:422") || !strings.Contains(printed, `Code:"validation_error"`) {
		t.Errorf("%%#v lost the status or the code: %s", printed)
	}
	if printed := fmt.Sprintf("%#v", (*APIError)(nil)); printed != "(*thalovant.APIError)(nil)" {
		t.Errorf("nil: %s", printed)
	}
}
