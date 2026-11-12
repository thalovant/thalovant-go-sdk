package thalovant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	icHub  = "0f8a7c3e-2b1d-4c5e-9a6f-1d2c3b4a5e6f"
	icAnn  = "7a1d6c1e-6f0b-4f43-9d7e-3f4f0d9a7c11"
	icBase = "/v1/hubs/" + icHub + "/intercom"
)

type icSeen struct {
	Method, Path, RawPath, Query, Authorization, IdempotencyKey string
	Body                                                        map[string]any
}

type icReply struct {
	Status  int
	Body    any
	Headers map[string]string
}

// icFake is a loopback fake API: replies map "METHOD path" to a queue, the
// last reply repeating.
type icFake struct {
	mu      sync.Mutex
	seen    []icSeen
	replies map[string][]icReply
	delays  map[string]time.Duration
}

func newIntercomAPI(t *testing.T) (*ControlPlane, *icFake) {
	t.Helper()
	fake := &icFake{replies: map[string][]icReply{}, delays: map[string]time.Duration{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		fake.mu.Lock()
		fake.seen = append(fake.seen, icSeen{
			Method: r.Method, Path: r.URL.Path, RawPath: r.URL.EscapedPath(), Query: r.URL.RawQuery,
			Authorization: r.Header.Get("Authorization"), IdempotencyKey: r.Header.Get("Idempotency-Key"), Body: body,
		})
		key := r.Method + " " + r.URL.EscapedPath()
		queue := fake.replies[key]
		reply := icReply{Status: 404, Body: map[string]any{"detail": "nope"}}
		if len(queue) > 1 {
			reply, fake.replies[key] = queue[0], queue[1:]
		} else if len(queue) == 1 {
			reply = queue[0]
		}
		delay := fake.delays[key]
		fake.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		for k, v := range reply.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(reply.Status)
		_ = json.NewEncoder(w).Encode(reply.Body)
	}))
	t.Cleanup(server.Close)
	return NewControlPlane(server.URL, "tok"), fake
}

func (f *icFake) reply(method, path string, replies ...icReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method+" "+path] = replies
}

// delay holds a route's answer back, until the client gives up on it.
func (f *icFake) delay(method, path string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays[method+" "+path] = d
}

func (f *icFake) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.seen))
	for i, seen := range f.seen {
		out[i] = seen.Method
	}
	return out
}

func (f *icFake) last() icSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[len(f.seen)-1]
}

func (f *icFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func icAnnouncement(status string, extra map[string]any) map[string]any {
	body := map[string]any{
		"id": icAnn, "status": status, "priority": "normal", "created_at": "2026-11-11T10:00:00Z",
		"deliveries": []any{
			map[string]any{"client_id": "c1", "slug": "kitchen", "name": "Kitchen", "status": "delivered", "at": "2026-11-11T10:00:01Z"},
			map[string]any{"client_id": "c2", "slug": "den", "name": "Den", "status": "brand_new_status", "at": nil},
		},
		"acked_by": nil,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestIntercomAnnounceSendsTheContractBody(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	got, err := api.Announce(context.Background(), icHub, AnnounceOptions{
		Text: "  Dinner is ready  ", Targets: RoomTargets("kitchen"), Lang: "en-US",
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := fake.last()
	want := `{"lang":"en-US","priority":"normal","targets":{"rooms":["kitchen"]},"text":"Dinner is ready"}`
	gotBody, _ := json.Marshal(seen.Body)
	if string(gotBody) != want {
		t.Fatalf("body = %s, want %s", gotBody, want)
	}
	if seen.Authorization != "Bearer tok" || seen.IdempotencyKey != "" {
		t.Fatalf("headers: %+v", seen)
	}
	if got.Status != AnnouncementCompleted || got.Sending() || got.AckedBy != nil {
		t.Fatalf("announcement: %+v", got)
	}
	if got.Deliveries[1].Status != DeliveryStatus("brand_new_status") || got.Deliveries[0].Status != DeliveryDelivered {
		t.Fatalf("deliveries: %+v", got.Deliveries)
	}
}

func TestIntercomUrgentRepeatIdempotencyAndWait(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 202, Body: icAnnouncement("sending", map[string]any{"priority": "urgent"})})
	fake.reply("GET", icBase+"/announcements/"+icAnn,
		icReply{Status: 200, Body: icAnnouncement("sending", nil)},
		icReply{Status: 200, Body: icAnnouncement("completed", map[string]any{"acked_by": "Den"})})
	got, err := api.Announce(context.Background(), icHub, AnnounceOptions{
		Text: "Wake up", Targets: AllSpeakers(), Priority: IntercomPriorityUrgent,
		Repeat: &IntercomRepeat{Times: 2, IntervalSeconds: 5}, IdempotencyKey: "key-1",
		Wait: true, PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != AnnouncementCompleted || got.AckedBy == nil || *got.AckedBy != "Den" {
		t.Fatalf("got %+v", got)
	}
	fake.mu.Lock()
	post := fake.seen[0]
	gets := 0
	for _, s := range fake.seen {
		if s.Method == "GET" {
			gets++
		}
	}
	fake.mu.Unlock()
	if post.IdempotencyKey != "key-1" || post.Body["repeat"].(map[string]any)["times"].(float64) != 2 {
		t.Fatalf("post: %+v", post)
	}
	if gets != 2 {
		t.Fatalf("polled %d times", gets)
	}
}

func TestIntercomWaitTimeoutReturnsTheLatestAnswer(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 202, Body: icAnnouncement("sending", nil)})
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 200, Body: icAnnouncement("sending", nil)})
	got, err := api.Announce(context.Background(), icHub, AnnounceOptions{
		Text: "x", Targets: GroupTargets("kids"), Priority: IntercomPriorityUrgent,
		Repeat: &IntercomRepeat{Times: 1, IntervalSeconds: 5}, Wait: true, WaitTimeout: 40 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	if err != nil || !got.Sending() {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestIntercomWaitStopsWhenTheContextEnds(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 202, Body: icAnnouncement("sending", nil)})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := api.Announce(ctx, icHub, AnnounceOptions{Text: "x", Targets: AllSpeakers(), Wait: true, PollInterval: time.Hour})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func icUrgent(opts AnnounceOptions) AnnounceOptions {
	opts.Text, opts.Targets, opts.Priority, opts.Wait = "x", AllSpeakers(), IntercomPriorityUrgent, true
	opts.Repeat = &IntercomRepeat{Times: 1, IntervalSeconds: 5}
	return opts
}

func TestIntercomWaitDeadlineBoundsAStalledRead(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 202, Body: icAnnouncement("sending", nil)})
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	fake.delay("GET", icBase+"/announcements/"+icAnn, 5*time.Second) // a read that never answers in time
	started := time.Now()
	// context.Background() has no deadline of its own: only WaitTimeout can end the read.
	got, err := api.Announce(context.Background(), icHub, icUrgent(AnnounceOptions{WaitTimeout: 300 * time.Millisecond, PollInterval: 10 * time.Millisecond}))
	if err != nil || !got.Sending() || got.ID != icAnn {
		t.Fatalf("got %+v, %v", got, err) // the latest answer: not an error, not a hang
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the wait took %v", took)
	}
}

func TestIntercomNoReadStartsOnceTheDeadlinePassedDuringTheSleep(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 202, Body: icAnnouncement("sending", nil)})
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	got, err := api.Announce(context.Background(), icHub, icUrgent(AnnounceOptions{WaitTimeout: 40 * time.Millisecond, PollInterval: time.Hour}))
	if err != nil || !got.Sending() {
		t.Fatalf("got %+v, %v", got, err)
	}
	if methods := fake.methods(); len(methods) != 1 || methods[0] != "POST" {
		t.Fatalf("requests = %v", methods) // the sleep was cut to the deadline, and no read followed
	}
}

func TestIntercomAFailedPollKeepsTheAcceptedAnnouncement(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 202, Body: icAnnouncement("sending", nil)})
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 403, Body: map[string]any{"detail": "Insufficient scopes"}})
	got, err := api.Announce(context.Background(), icHub, icUrgent(AnnounceOptions{IdempotencyKey: "k-1", PollInterval: 5 * time.Millisecond}))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 403 || !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	// The announcement exists: its ID and status come back with the poll's error.
	if got.ID != icAnn || !got.Sending() {
		t.Fatalf("the accepted announcement was lost: %+v", got)
	}
	if methods := fake.methods(); len(methods) != 2 || methods[0] != "POST" || methods[1] != "GET" {
		t.Fatalf("requests = %v", methods)
	}
}

func TestIntercomAZeroWaitTimeoutMeansTheDefault(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 202, Body: icAnnouncement("sending", nil)})
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	got, err := api.Announce(context.Background(), icHub, icUrgent(AnnounceOptions{PollInterval: 5 * time.Millisecond}))
	if err != nil || got.Status != AnnouncementCompleted {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestIntercomDNDNeedsAnExplicitEnabled(t *testing.T) {
	api, fake := newIntercomAPI(t)
	speaker := map[string]any{"client_id": "c1", "slug": "k", "name": "K", "online": true, "intercom": map[string]any{}}
	fake.reply("PATCH", icBase+"/speakers/c1", icReply{Status: 200, Body: speaker})
	zone := "Europe/Paris"
	hours := &IntercomQuietHours{Start: "21:00", End: "06:30", Timezone: &zone}
	if _, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "c1", IntercomSpeakerUpdate{DND: &IntercomDNDUpdate{QuietHours: hours}}); !errors.Is(err, ErrAPI) {
		t.Fatalf("a DND update without Enabled was accepted: %v", err)
	}
	if fake.count() != 0 {
		t.Fatal("something was sent")
	}
	for _, enabled := range []bool{true, false} {
		e := enabled
		if _, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "c1", IntercomSpeakerUpdate{DND: &IntercomDNDUpdate{Enabled: &e, QuietHours: hours}}); err != nil {
			t.Fatal(err)
		}
		if got := fake.last().Body["dnd"].(map[string]any)["enabled"]; got != enabled {
			t.Fatalf("enabled = %v, want %v", got, enabled)
		}
	}
}

func TestIntercomGetAndListAnnouncements(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	fake.reply("GET", icBase+"/announcements", icReply{Status: 200, Body: map[string]any{"items": []any{icAnnouncement("completed", nil)}, "next_cursor": "c2"}})
	one, err := api.GetAnnouncement(context.Background(), icHub, icAnn)
	if err != nil || one.ID != icAnn {
		t.Fatalf("%+v %v", one, err)
	}
	page, err := api.ListAnnouncements(context.Background(), icHub, AnnouncementListOptions{Limit: 5, Cursor: "c 1"})
	if err != nil || page.NextCursor != "c2" || len(page.Items) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	if q := fake.last().Query; q != "cursor=c+1&limit=5" {
		t.Fatalf("query = %q", q)
	}
	fake.reply("GET", icBase+"/announcements", icReply{Status: 200, Body: map[string]any{"items": []any{}}})
	if page, err = api.ListAnnouncements(context.Background(), icHub, AnnouncementListOptions{}); err != nil || page.NextCursor != "" {
		t.Fatalf("%+v %v", page, err)
	}
}

func TestIntercomSpeakersAndPatchSendOnlyWhatIsSet(t *testing.T) {
	api, fake := newIntercomAPI(t)
	speaker := map[string]any{
		"client_id": "c-1", "slug": "kitchen", "name": "Kitchen", "online": true, "last_seen_at": nil,
		"intercom": map[string]any{"receive": true, "room": "kitchen", "groups": []any{"downstairs"},
			"dnd": map[string]any{"enabled": true, "quiet_hours": map[string]any{"start": "22:00", "end": "07:00", "timezone": nil}}},
	}
	fake.reply("GET", icBase+"/speakers", icReply{Status: 200, Body: map[string]any{"items": []any{speaker}}})
	fake.reply("PATCH", icBase+"/speakers/c-1", icReply{Status: 200, Body: speaker})
	listed, err := api.ListIntercomSpeakers(context.Background(), icHub)
	if err != nil || len(listed.Items) != 1 || !listed.Items[0].Online || listed.Items[0].Intercom.DND.QuietHours.Start != "22:00" || *listed.Items[0].Intercom.Room != "kitchen" {
		t.Fatalf("%+v %v", listed, err)
	}
	yes := true
	if _, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "c-1", IntercomSpeakerUpdate{Receive: &yes, ClearRoom: true}); err != nil {
		t.Fatal(err)
	}
	seen := fake.last()
	if seen.RawPath != icBase+"/speakers/c-1" {
		t.Fatalf("path = %q", seen.RawPath)
	}
	body, _ := json.Marshal(seen.Body)
	if string(body) != `{"receive":true,"room":null}` {
		t.Fatalf("body = %s", body)
	}
	zone := "Europe/Paris"
	if _, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "c-1", IntercomSpeakerUpdate{
		DND: &IntercomDNDUpdate{Enabled: &yes, QuietHours: &IntercomQuietHours{Start: "22:00", End: "07:00", Timezone: &zone}},
	}); err != nil {
		t.Fatal(err)
	}
	if fake.last().Body["dnd"].(map[string]any)["quiet_hours"].(map[string]any)["timezone"] != "Europe/Paris" {
		t.Fatalf("dnd: %+v", fake.last().Body)
	}
	if _, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "c-1", IntercomSpeakerUpdate{Groups: []string{}}); err != nil {
		t.Fatal(err)
	}
	if g, ok := fake.last().Body["groups"].([]any); !ok || len(g) != 0 {
		t.Fatalf("empty groups must be sent: %+v", fake.last().Body)
	}
}

func TestIntercomSettingsAndAnIDIsOnePercentEncodedSegment(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("GET", icBase+"/settings", icReply{Status: 200, Body: map[string]any{"enabled": false, "plan_allowed": false, "unavailable_reason": "plan"}})
	fake.reply("PUT", icBase+"/settings", icReply{Status: 200, Body: map[string]any{"enabled": true, "plan_allowed": true, "unavailable_reason": nil}})
	fake.reply("GET", "/v1/hubs/a%20b%25c/intercom/settings", icReply{Status: 200, Body: map[string]any{"enabled": false, "plan_allowed": true}})
	got, err := api.GetIntercomSettings(context.Background(), icHub)
	if err != nil || got.UnavailableReason != "plan" || got.Enabled {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err = api.SetIntercomSettings(context.Background(), icHub, true); err != nil || !got.Enabled || got.UnavailableReason != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if fake.last().Body["enabled"] != true {
		t.Fatalf("body: %+v", fake.last().Body)
	}
	if got, err = api.GetIntercomSettings(context.Background(), "a b%c"); err != nil || !got.PlanAllowed {
		t.Fatalf("%+v %v", got, err)
	}
	if fake.last().RawPath != "/v1/hubs/a%20b%25c/intercom/settings" {
		t.Fatalf("path = %q", fake.last().RawPath)
	}
}

func TestIntercomErrorsAreTyped(t *testing.T) {
	cases := []struct {
		status  int
		body    map[string]any
		headers map[string]string
		target  error
	}{
		{403, map[string]any{"code": "intercom_disabled", "detail": "off"}, nil, ErrIntercomDisabled},
		{422, map[string]any{"code": "intercom_disabled", "detail": "kid hub"}, nil, ErrIntercomDisabled},
		{502, map[string]any{"code": "hub_unreachable", "detail": "no hub"}, nil, ErrHubUnreachable},
		{409, map[string]any{"code": "not_cancellable", "detail": "done"}, nil, ErrNotCancellable},
		{409, map[string]any{"code": "something_else", "detail": "no"}, nil, nil},
		{429, map[string]any{"code": "rate_limited", "detail": "slow"}, map[string]string{"Retry-After": "30"}, ErrRateLimited},
		{403, map[string]any{"code": "plan_limit", "detail": "plan"}, nil, ErrPlan},
		{402, map[string]any{"detail": "API access requires a paid plan."}, nil, ErrPlan},
		{403, map[string]any{"detail": "Insufficient scopes"}, nil, ErrAuth},
		{422, map[string]any{"code": "validation", "detail": "bad"}, nil, nil},
	}
	for _, tc := range cases {
		api, fake := newIntercomAPI(t)
		fake.reply("POST", icBase+"/announce", icReply{Status: tc.status, Body: tc.body, Headers: tc.headers})
		_, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hello", Targets: AllSpeakers()})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status {
			t.Fatalf("%d: err = %v", tc.status, err)
		}
		for _, other := range []error{ErrIntercomDisabled, ErrHubUnreachable, ErrNotCancellable, ErrRateLimited, ErrPlan, ErrAuth,
			ErrIntercomUnavailable, ErrIntercomPlatformDisabled, ErrIntercomNotConfigured, ErrUrgentScopeRequired, ErrUrgentBusy,
			ErrSkillNotInstalled, ErrNotASpeaker, ErrReservedName, ErrHubLocked, ErrImpersonationNotAllowed, ErrTooManyTargets, ErrManagedClient} {
			if want := other == tc.target; errors.Is(err, other) != want {
				t.Fatalf("%d %v: errors.Is(%v) = %v", tc.status, tc.body, other, !want)
			}
		}
		if tc.status == 429 && apiErr.RetryAfter != 30*time.Second {
			t.Fatalf("retry after = %v", apiErr.RetryAfter)
		}
	}
}

func TestIntercomNothingIsSentForInvalidInput(t *testing.T) {
	api, fake := newIntercomAPI(t)
	ctx := context.Background()
	all := AllSpeakers()
	many := make([]string, 51)
	for i := range many {
		many[i] = "s"
	}
	bad := []AnnounceOptions{
		{Text: "", Targets: all}, {Text: "   ", Targets: all}, {Text: strings.Repeat("x", 281), Targets: all},
		{Text: "a\x07b", Targets: all}, {Text: "a" + string(rune(0x2028)) + "b", Targets: all},
		{Text: "hi"}, {Text: "hi", Targets: IntercomTargets{All: true, Rooms: []string{"a"}}},
		{Text: "hi", Targets: SpeakerTargets(many...)}, {Text: "hi", Targets: RoomTargets(" ")},
		{Text: "hi", Targets: all, Repeat: &IntercomRepeat{Times: 2, IntervalSeconds: 10}},
		{Text: "hi", Targets: all, Priority: "loud"},
		{Text: "hi", Targets: all, IdempotencyKey: "  "},
		{Text: "hi", Targets: all, Wait: true, WaitTimeout: -time.Second},
		{Text: "hi", Targets: all, Wait: true, PollInterval: -time.Second},
	}
	for _, repeat := range []IntercomRepeat{{0, 10}, {11, 10}, {2, 4}, {2, 121}} {
		r := repeat
		bad = append(bad, AnnounceOptions{Text: "hi", Targets: all, Priority: IntercomPriorityUrgent, Repeat: &r})
	}
	for i, opts := range bad {
		if _, err := api.Announce(ctx, icHub, opts); !errors.Is(err, ErrAPI) {
			t.Fatalf("case %d: err = %v", i, err)
		}
	}
	if _, err := api.Announce(ctx, "  ", AnnounceOptions{Text: "hi", Targets: all}); err == nil {
		t.Fatal("blank hub id accepted")
	}
	if _, err := api.GetAnnouncement(ctx, icHub, ""); err == nil {
		t.Fatal("blank announcement id accepted")
	}
	yes := true
	room, long := "r", strings.Repeat("r", 65)
	for _, update := range []IntercomSpeakerUpdate{
		{}, {Receive: &yes, Room: &room, ClearRoom: true}, {Room: &long}, {Groups: make([]string, 9)},
		{Groups: []string{""}},
		{DND: &IntercomDNDUpdate{Enabled: &yes, QuietHours: &IntercomQuietHours{Start: "25:00", End: "07:00"}}},
		{DND: &IntercomDNDUpdate{}},
		{DND: &IntercomDNDUpdate{QuietHours: &IntercomQuietHours{Start: "21:00", End: "06:00"}}}, // enabled missing: never stored as false
	} {
		if _, err := api.UpdateIntercomSpeaker(ctx, icHub, "c1", update); err == nil {
			t.Fatalf("accepted %+v", update)
		}
	}
	if _, err := api.ListAnnouncements(ctx, icHub, AnnouncementListOptions{Limit: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
	if fake.count() != 0 {
		t.Fatalf("%d requests were sent", fake.count())
	}
}

func TestIntercomAnnouncementTextCountsCharacters(t *testing.T) {
	if _, err := AnnouncementText(strings.Repeat(string(rune(0x1F600)), 280)); err != nil {
		t.Fatal(err)
	}
	if got, _ := AnnouncementText("  hi \n"); got != "hi" {
		t.Fatalf("got %q", got)
	}
}

func TestIntercomMalformedAnswersAreAPIErrors(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: map[string]any{"status": "completed"}})
	if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hi", Targets: AllSpeakers()}); err == nil {
		t.Fatal("an answer without an id was accepted")
	}
	fake.reply("GET", icBase+"/announcements", icReply{Status: 200, Body: map[string]any{"nope": 1}})
	if _, err := api.ListAnnouncements(context.Background(), icHub, AnnouncementListOptions{}); err == nil {
		t.Fatal("a listing without items was accepted")
	}
	fake.reply("GET", icBase+"/speakers", icReply{Status: 200, Body: map[string]any{"nope": 1}})
	if _, err := api.ListIntercomSpeakers(context.Background(), icHub); err == nil {
		t.Fatal("a speaker listing without items was accepted")
	}
}

// --- Amendment 2: cancel, replay, ids, redaction, the 502 announcement id, doc rules -------------------------

var icUnsafeIDs = []string{".", "..", "", "   ", "a/b", "/", "a?b", "a#b", `a\b`, "a\x00b", "a\nb", "a\tb", "a\x7fb", "a\u0085b", "../x", "x/.."}

func TestIntercomDotSegmentsAndUnsafeIDsAreRefusedBeforeAnyRequest(t *testing.T) {
	api, fake := newIntercomAPI(t)
	ctx := context.Background()
	yes := true
	for _, bad := range icUnsafeIDs {
		calls := map[string]func() error{
			"GetIntercomSettings":  func() error { _, err := api.GetIntercomSettings(ctx, bad); return err },
			"SetIntercomSettings":  func() error { _, err := api.SetIntercomSettings(ctx, bad, true); return err },
			"ListIntercomSpeakers": func() error { _, err := api.ListIntercomSpeakers(ctx, bad); return err },
			"UpdateIntercomSpeaker(hub)": func() error {
				_, err := api.UpdateIntercomSpeaker(ctx, bad, "c1", IntercomSpeakerUpdate{Receive: &yes})
				return err
			},
			"UpdateIntercomSpeaker(client)": func() error {
				_, err := api.UpdateIntercomSpeaker(ctx, icHub, bad, IntercomSpeakerUpdate{Receive: &yes})
				return err
			},
			"Announce": func() error {
				_, err := api.Announce(ctx, bad, AnnounceOptions{Text: "hi", Targets: AllSpeakers()})
				return err
			},
			"GetAnnouncement(hub)":             func() error { _, err := api.GetAnnouncement(ctx, bad, icAnn); return err },
			"GetAnnouncement(announcement)":    func() error { _, err := api.GetAnnouncement(ctx, icHub, bad); return err },
			"CancelAnnouncement(hub)":          func() error { _, err := api.CancelAnnouncement(ctx, bad, icAnn); return err },
			"CancelAnnouncement(announcement)": func() error { _, err := api.CancelAnnouncement(ctx, icHub, bad); return err },
			"ListAnnouncements":                func() error { _, err := api.ListAnnouncements(ctx, bad, AnnouncementListOptions{}); return err },
		}
		for name, call := range calls {
			if err := call(); !errors.Is(err, ErrAPI) {
				t.Errorf("%s(%q): err = %v", name, bad, err)
			}
		}
	}
	if fake.count() != 0 {
		t.Fatalf("%d requests were sent", fake.count())
	}
}

func TestIntercomOrdinaryIDsPassTheIDCheck(t *testing.T) {
	for _, good := range []string{icHub, icAnn, "client-1", "c_1", "a.b", "...", "a..b", ".hidden", "name with space", strings.Repeat("x", 200)} {
		if err := validIntercomID(good, "an id"); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
}

func TestIntercomCancelPostsToTheCancelRouteAndReadsHubConfirmed(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announcements/"+icAnn+"/cancel", icReply{Status: 200, Body: icAnnouncement("cancelled", map[string]any{"priority": "urgent", "hub_confirmed": true})})
	got, err := api.CancelAnnouncement(context.Background(), icHub, icAnn)
	if err != nil {
		t.Fatal(err)
	}
	seen := fake.last()
	if seen.Method != "POST" || seen.Path != icBase+"/announcements/"+icAnn+"/cancel" || seen.Body != nil || seen.Authorization != "Bearer tok" {
		t.Fatalf("request = %+v", seen)
	}
	if got.Status != AnnouncementCancelled || got.Sending() || got.Priority != IntercomPriorityUrgent {
		t.Fatalf("announcement = %+v", got)
	}
	if got.HubConfirmed == nil || !*got.HubConfirmed {
		t.Fatalf("HubConfirmed = %v", got.HubConfirmed)
	}
}

func TestIntercomHubConfirmedIsARealBooleanOrNil(t *testing.T) {
	for _, tc := range []struct {
		sent any
		want *bool
	}{{false, new(bool)}, {true, ptrBool(true)}, {"yes", nil}, {1, nil}, {nil, nil}} {
		api, fake := newIntercomAPI(t)
		fake.reply("POST", icBase+"/announcements/"+icAnn+"/cancel", icReply{Status: 200, Body: icAnnouncement("cancelled", map[string]any{"hub_confirmed": tc.sent})})
		got, err := api.CancelAnnouncement(context.Background(), icHub, icAnn)
		if err != nil {
			t.Fatalf("%v: %v", tc.sent, err)
		}
		if (got.HubConfirmed == nil) != (tc.want == nil) || (tc.want != nil && *got.HubConfirmed != *tc.want) {
			t.Fatalf("hub_confirmed %v read as %v", tc.sent, got.HubConfirmed)
		}
	}
}

func ptrBool(v bool) *bool { return &v }

func TestIntercomHubConfirmedIsOptionalOnEveryOtherAnswer(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	got, err := api.GetAnnouncement(context.Background(), icHub, icAnn)
	if err != nil || got.HubConfirmed != nil {
		t.Fatalf("%+v %v", got, err)
	}
	var list struct{ Items []Announcement }
	if err := json.Unmarshal([]byte(`{"Items":[{"id":"a","status":"cancelled","hub_confirmed":false}]}`), &list); err != nil || list.Items[0].HubConfirmed == nil || *list.Items[0].HubConfirmed {
		t.Fatalf("%+v %v", list, err)
	}
	out, _ := json.Marshal(Announcement{ID: "a", Status: AnnouncementCompleted})
	if strings.Contains(string(out), "hub_confirmed") {
		t.Fatalf("an absent hub_confirmed is omitted: %s", out)
	}
}

func TestIntercomACancelTheHubCouldNotConfirmIsStillCancelled(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announcements/"+icAnn+"/cancel", icReply{Status: 200, Body: icAnnouncement("cancelled", map[string]any{"hub_confirmed": false})})
	got, err := api.CancelAnnouncement(context.Background(), icHub, icAnn)
	if err != nil || got.Status != AnnouncementCancelled || got.HubConfirmed == nil || *got.HubConfirmed {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestIntercomCancelOfAFinishedAnnouncementIsErrNotCancellable(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announcements/"+icAnn+"/cancel", icReply{Status: 409, Body: map[string]any{"code": "not_cancellable", "detail": "It already completed."}})
	_, err := api.CancelAnnouncement(context.Background(), icHub, icAnn)
	var apiErr *APIError
	if !errors.Is(err, ErrNotCancellable) || !errors.As(err, &apiErr) || apiErr.StatusCode != 409 || apiErr.Code != "not_cancellable" {
		t.Fatalf("err = %v", err)
	}
	fake.reply("POST", icBase+"/announcements/"+icAnn+"/cancel", icReply{Status: 403, Body: map[string]any{"detail": "Insufficient scopes"}})
	if _, err := api.CancelAnnouncement(context.Background(), icHub, icAnn); !errors.Is(err, ErrAuth) || errors.Is(err, ErrNotCancellable) {
		t.Fatalf("scope err = %v", err)
	}
}

func TestIntercomA502NamesTheAnnouncementTheAPIRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"top", map[string]any{"code": "hub_unreachable", "detail": "no hub", "announcement_id": icAnn}, icAnn},
		{"inside detail", map[string]any{"detail": map[string]any{"code": "hub_unreachable", "detail": "no hub", "announcement_id": icAnn}}, icAnn},
		{"absent", map[string]any{"code": "hub_unreachable", "detail": "no hub"}, ""},
		{"not a string", map[string]any{"code": "hub_unreachable", "detail": "no hub", "announcement_id": 7}, ""},
		{"empty", map[string]any{"code": "hub_unreachable", "detail": "no hub", "announcement_id": ""}, ""},
	} {
		api, fake := newIntercomAPI(t)
		fake.reply("POST", icBase+"/announce", icReply{Status: 502, Body: tc.body})
		_, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hello", Targets: AllSpeakers(), IdempotencyKey: "k-1"})
		var apiErr *APIError
		if !errors.Is(err, ErrHubUnreachable) || !errors.As(err, &apiErr) || apiErr.StatusCode != 502 {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		if got := apiErr.AnnouncementID(); got != tc.want {
			t.Errorf("%s: AnnouncementID() = %q, want %q", tc.name, got, tc.want)
		}
	}
	if (&APIError{StatusCode: 502}).AnnouncementID() != "" {
		t.Fatal("an error with no body names no announcement")
	}
}

func TestIntercomAReplayedFailedAnnouncementStaysFailedAndTheKeyIsSentEachTime(t *testing.T) {
	// The API replays the stored announcement for the same (hub, key) with
	// Idempotency-Replayed: true and never sends again; the SDK neither fails
	// nor retries behind the caller's back.
	api, fake := newIntercomAPI(t)
	key := "11111111-1111-4111-8111-111111111111"
	fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("failed", nil), Headers: map[string]string{"Idempotency-Replayed": "true"}})
	opts := AnnounceOptions{Text: "Dinner is ready", Targets: AllSpeakers(), IdempotencyKey: key}
	first, err1 := api.Announce(context.Background(), icHub, opts)
	again, err2 := api.Announce(context.Background(), icHub, opts)
	if err1 != nil || err2 != nil || first.Status != AnnouncementFailed || again.Status != AnnouncementFailed {
		t.Fatalf("%+v %v / %+v %v", first, err1, again, err2)
	}
	if fake.count() != 2 || fake.last().IdempotencyKey != key {
		t.Fatalf("%d requests, last key %q", fake.count(), fake.last().IdempotencyKey)
	}
}

func TestIntercomAnnounceOptionsNeverPrintTheText(t *testing.T) {
	const secret = "Take your pills at nine"
	opts := AnnounceOptions{Text: secret, Targets: RoomTargets("kitchen"), Priority: IntercomPriorityUrgent,
		Repeat: &IntercomRepeat{Times: 3, IntervalSeconds: 20}, Lang: "en-US", IdempotencyKey: "k-1", Wait: true}
	ptr := &opts
	nested := struct{ Options AnnounceOptions }{opts}
	list := []AnnounceOptions{opts}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("sending", "options", opts)
	slog.New(slog.NewTextHandler(&logged, nil)).Info("sending", "options", ptr)
	outputs := []string{
		fmt.Sprint(opts), fmt.Sprintf("%v", opts), fmt.Sprintf("%+v", opts), fmt.Sprintf("%#v", opts), fmt.Sprintf("%s", opts),
		fmt.Sprintf("%q", opts), fmt.Sprintf("%d", opts), fmt.Sprintf("%x", opts), fmt.Sprintf("%v", ptr), fmt.Sprintf("%+v", ptr),
		fmt.Sprintf("%#v", ptr), fmt.Sprintf("%v", nested), fmt.Sprintf("%+v", nested), fmt.Sprintf("%v", list), fmt.Sprintf("%#v", list),
		opts.String(), opts.GoString(), logged.String(),
	}
	for i, out := range outputs {
		if strings.Contains(out, secret) || strings.Contains(strings.ToLower(out), "pills") {
			t.Errorf("output %d prints the text: %s", i, out)
		}
	}
	if got := opts.String(); !strings.Contains(got, "<23 chars>") || !strings.Contains(got, "kitchen") || !strings.Contains(got, "urgent") {
		t.Fatalf("String() = %s", got)
	}
	if got := fmt.Sprintf("%#v", opts); !strings.HasPrefix(got, "thalovant.AnnounceOptions{") {
		t.Fatalf("GoString = %s", got)
	}
	// The text still goes out in the request.
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: secret, Targets: AllSpeakers()}); err != nil || fake.last().Body["text"] != secret {
		t.Fatalf("the request lost the text: %v %+v", err, fake.last().Body)
	}
}

func TestIntercomDocsDoNotUseAConstantKeyOrOverPromise(t *testing.T) {
	// A fixed key replays the stored announcement for 24 hours and sends nothing: examples generate a fresh one.
	constantKey := regexp.MustCompile(`IdempotencyKey:\s*"`)
	for _, name := range []string{"README.md", "QUICK_FACTS.md", "FAQ.md"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(raw), "\n") {
			if constantKey.MatchString(line) {
				t.Errorf("%s:%d uses a constant idempotency key: %s", name, number+1, strings.TrimSpace(line))
			}
		}
		lowered := strings.ToLower(string(raw))
		for _, phrase := range []string{"nothing listens", "never listens", "spoken once", "stops every announcement at once", "retry with the same", "no two-way audio, and nothing"} {
			if strings.Contains(lowered, phrase) {
				t.Errorf("%s says %q", name, phrase)
			}
		}
	}
	raw, _ := os.ReadFile("README.md")
	readme := strings.ToLower(string(raw))
	for _, word := range []string{"uuid", "idempotency-replayed", "cancelannouncement", "new key", "announcementid"} {
		if !strings.Contains(readme, word) {
			t.Errorf("README.md does not mention %q", word)
		}
	}
}

// ---------------------------------------------------------------------------
// The final API (platform security review): scope, speaker model, problem
// codes, one validation rule.
// ---------------------------------------------------------------------------

var icSpeakerDND = map[string]any{"enabled": false, "quiet_hours": map[string]any{"start": "22:00", "end": "07:00", "timezone": nil}}

func icSpeakerRow(extra map[string]any) map[string]any {
	row := map[string]any{
		"client_id": "d8d4", "slug": "kitchen-speaker", "name": "Kitchen Speaker", "kind": "voice_satellite",
		"can_play": true, "online": false, "last_seen_at": nil,
		"intercom": map[string]any{"receive": true, "room": "kitchen", "groups": []any{}, "dnd": icSpeakerDND},
	}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

func TestIntercomSpeakerListReadsKindCanPlayAndPolicySynced(t *testing.T) {
	api, fake := newIntercomAPI(t)
	link := icSpeakerRow(map[string]any{"client_id": "996f", "slug": "home-link", "name": "Home link", "kind": "home_assistant", "can_play": false})
	fake.reply("GET", icBase+"/speakers", icReply{Status: 200, Body: map[string]any{
		"items": []any{icSpeakerRow(nil), link}, "unavailable_reason": nil, "policy_synced": false}})
	listed, err := api.ListIntercomSpeakers(context.Background(), icHub)
	if err != nil {
		t.Fatal(err)
	}
	if listed.PolicySynced == nil || *listed.PolicySynced || listed.UnavailableReason != "" || len(listed.Items) != 2 {
		t.Fatalf("list: %+v", listed)
	}
	kitchen, home := listed.Items[0], listed.Items[1]
	if kitchen.Kind != "voice_satellite" || !kitchen.CanPlay || kitchen.PolicySynced != nil {
		t.Fatalf("kitchen: %+v", kitchen)
	}
	if home.Kind != "home_assistant" || home.CanPlay {
		t.Fatalf("home link: %+v", home)
	}
}

func TestIntercomASpeakerWithoutTheNewFieldsKeepsTheAPIsDefaults(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("GET", icBase+"/speakers", icReply{Status: 200, Body: map[string]any{"items": []any{map[string]any{"client_id": "c1"}}}})
	only, err := api.ListIntercomSpeakers(context.Background(), icHub)
	if err != nil || only.Items[0].Kind != "voice_satellite" || !only.Items[0].CanPlay || only.PolicySynced != nil || only.UnavailableReason != "" {
		t.Fatalf("%+v %v", only, err)
	}
	// Garbage is not trusted: only an explicit false says "cannot play".
	fake.reply("GET", icBase+"/speakers", icReply{Status: 200, Body: map[string]any{"items": []any{
		map[string]any{"client_id": "c1", "kind": 7, "can_play": "no", "policy_synced": "yes"}}, "policy_synced": "yes"}})
	odd, err := api.ListIntercomSpeakers(context.Background(), icHub)
	if err != nil {
		t.Fatal(err)
	}
	if got := odd.Items[0]; got.Kind != "voice_satellite" || !got.CanPlay || got.PolicySynced != nil || odd.PolicySynced != nil {
		t.Fatalf("%+v %+v", got, odd)
	}
}

func TestIntercomTheSpeakerPatchAnswerCarriesItsOwnPolicySynced(t *testing.T) {
	api, fake := newIntercomAPI(t)
	yes := true
	for _, synced := range []bool{false, true} {
		fake.reply("PATCH", icBase+"/speakers/d8d4", icReply{Status: 200, Body: icSpeakerRow(map[string]any{"policy_synced": synced})})
		got, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "d8d4", IntercomSpeakerUpdate{Receive: &yes})
		if err != nil || got.PolicySynced == nil || *got.PolicySynced != synced {
			t.Fatalf("%+v %v", got, err)
		}
	}
}

func TestIntercomThePlatformReasonIsAnUnavailableReason(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("GET", icBase+"/settings", icReply{Status: 200, Body: map[string]any{"enabled": false, "plan_allowed": true, "unavailable_reason": "platform"}})
	fake.reply("GET", icBase+"/speakers", icReply{Status: 200, Body: map[string]any{"items": []any{}, "unavailable_reason": "platform", "policy_synced": true}})
	settings, err := api.GetIntercomSettings(context.Background(), icHub)
	if err != nil || settings.UnavailableReason != IntercomUnavailablePlatform || settings.Enabled {
		t.Fatalf("%+v %v", settings, err)
	}
	listed, err := api.ListIntercomSpeakers(context.Background(), icHub)
	if err != nil || listed.UnavailableReason != IntercomUnavailablePlatform || listed.PolicySynced == nil || !*listed.PolicySynced {
		t.Fatalf("%+v %v", listed, err)
	}
	if IntercomUnavailablePlan != "plan" || IntercomUnavailableKidHub != "kid_hub" || IntercomUnavailablePublic != "public_hub" {
		t.Fatal("reason constants")
	}
}

func TestIntercomADeliveryKeepsItsDetailCode(t *testing.T) {
	body := icAnnouncement("failed", map[string]any{"deliveries": []any{
		map[string]any{"client_id": "c1", "slug": "k", "name": "K", "status": "failed", "detail": "hub_no_response", "at": nil},
		map[string]any{"client_id": "c2", "slug": "d", "name": "D", "status": "delivered", "detail": nil, "at": nil},
		map[string]any{"client_id": "c3", "slug": "e", "name": "E", "status": "failed", "detail": "a_code_from_the_future", "at": nil},
	}})
	api, fake := newIntercomAPI(t)
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 200, Body: body})
	got, err := api.GetAnnouncement(context.Background(), icHub, icAnn)
	if err != nil {
		t.Fatal(err)
	}
	if got.Deliveries[0].Detail != "hub_no_response" || got.Deliveries[1].Detail != "" || got.Deliveries[2].Detail != "a_code_from_the_future" {
		t.Fatalf("%+v", got.Deliveries)
	}
	if !KnownDeliveryDetail("hub_no_response") || KnownDeliveryDetail("a_code_from_the_future") || KnownDeliveryDetail("") {
		t.Fatal("KnownDeliveryDetail")
	}
}

func TestIntercomTheScopeAndProblemCodeNamesAreConstants(t *testing.T) {
	if IntercomReadScope != "intercom:read" || IntercomSendScope != "intercom:send" || IntercomUrgentScope != "intercom:urgent" {
		t.Fatal("scope names")
	}
	// Every code has a typed refusal, and no refusal is left without a code.
	want := map[string]error{
		"intercom_disabled": ErrIntercomDisabled, "intercom_unavailable": ErrIntercomUnavailable,
		"intercom_disabled_platform": ErrIntercomPlatformDisabled, "intercom_not_configured": ErrIntercomNotConfigured,
		"urgent_scope_required": ErrUrgentScopeRequired, "intercom_urgent_busy": ErrUrgentBusy,
		"skill_not_installed": ErrSkillNotInstalled, "not_a_speaker": ErrNotASpeaker, "reserved_name": ErrReservedName,
		"hub_locked": ErrHubLocked, "impersonation_not_allowed": ErrImpersonationNotAllowed,
		"too_many_targets": ErrTooManyTargets, "managed_client": ErrManagedClient, "not_cancellable": ErrNotCancellable,
		"hub_unreachable": ErrHubUnreachable,
	}
	got := map[string]error{
		IntercomCodeDisabled: nil, IntercomCodeUnavailable: nil, IntercomCodeDisabledPlatform: nil, IntercomCodeNotConfigured: nil,
		IntercomCodeUrgentScopeRequired: nil, IntercomCodeUrgentBusy: nil, IntercomCodeSkillNotInstalled: nil,
		IntercomCodeNotASpeaker: nil, IntercomCodeReservedName: nil, IntercomCodeHubLocked: nil,
		IntercomCodeImpersonationNotAllowed: nil, IntercomCodeTooManyTargets: nil, IntercomCodeManagedClient: nil,
		IntercomCodeNotCancellable: nil, IntercomCodeHubUnreachable: nil,
	}
	for code := range got {
		if want[code] == nil || intercomRefusals[code] != want[code] {
			t.Errorf("code %q: refusal %v, want %v", code, intercomRefusals[code], want[code])
		}
	}
	if len(got) != len(want) || len(intercomRefusals) != len(want) {
		t.Fatalf("%d constants, %d refusals, want %d", len(got), len(intercomRefusals), len(want))
	}
}

// -- announce: replayed -------------------------------------------------------

func TestIntercomAReplayedAnnouncementSaysSoAndAFirstAnswerDoesNot(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	first, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "Dinner", Targets: AllSpeakers(), IdempotencyKey: "k-1"})
	if err != nil || first.Replayed {
		t.Fatalf("%+v %v", first, err)
	}
	fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("completed", nil), Headers: map[string]string{"Idempotency-Replayed": "true"}})
	again, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "Dinner", Targets: AllSpeakers(), IdempotencyKey: "k-1"})
	if err != nil || !again.Replayed || again.Status != AnnouncementCompleted {
		t.Fatalf("%+v %v", again, err)
	}
}

func TestIntercomTheReplayedHeaderIsReadInAnyCaseAndOnlyWhenTrue(t *testing.T) {
	api, fake := newIntercomAPI(t)
	for _, tc := range []struct {
		value string
		want  bool
	}{{"TRUE", true}, {"true", true}, {"false", false}, {"1", false}} {
		fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("completed", nil), Headers: map[string]string{"Idempotency-Replayed": tc.value}})
		got, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "Dinner", Targets: AllSpeakers()})
		if err != nil || got.Replayed != tc.want {
			t.Fatalf("header %q: %+v %v", tc.value, got, err)
		}
	}
}

func TestIntercomAReplayedSendingAnnouncementStaysReplayedAfterWait(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 202, Body: icAnnouncement("sending", map[string]any{"priority": "urgent"}), Headers: map[string]string{"Idempotency-Replayed": "true"}})
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 200, Body: icAnnouncement("completed", map[string]any{"priority": "urgent"})})
	got, err := api.Announce(context.Background(), icHub, AnnounceOptions{
		Text: "Wake up", Targets: AllSpeakers(), Priority: IntercomPriorityUrgent, Repeat: &IntercomRepeat{Times: 2, IntervalSeconds: 5},
		IdempotencyKey: "k-2", Wait: true, PollInterval: 5 * time.Millisecond,
	})
	if err != nil || got.Status != AnnouncementCompleted || !got.Replayed {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestIntercomReadsAndCancelAreNeverReplayed(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("GET", icBase+"/announcements/"+icAnn, icReply{Status: 200, Body: icAnnouncement("completed", nil), Headers: map[string]string{"Idempotency-Replayed": "true"}})
	got, err := api.GetAnnouncement(context.Background(), icHub, icAnn)
	if err != nil || got.Replayed {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestIntercomAnIdempotencyKeyIsAtMost128PrintableASCIICharacters(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hi", Targets: AllSpeakers(), IdempotencyKey: strings.Repeat("k", 128)}); err != nil {
		t.Fatal(err)
	}
	if len(fake.last().IdempotencyKey) != 128 {
		t.Fatalf("key length %d", len(fake.last().IdempotencyKey))
	}
	sent := fake.count()
	for _, bad := range []string{strings.Repeat("k", 129), "caf" + string(rune(0xe9)), "a\r\nb", "   "} {
		if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hi", Targets: AllSpeakers(), IdempotencyKey: bad}); !errors.Is(err, ErrAPI) {
			t.Fatalf("key %q accepted: %v", bad, err)
		}
	}
	if fake.count() != sent {
		t.Fatal("a bad key sent a request")
	}
}

// -- the one text rule --------------------------------------------------------

func icRefusedTexts() []string {
	r := func(cp rune) string { return string(cp) }
	return []string{
		"a<b", "a>b", "<prosody>x</prosody>", "line\nbreak", "tab\there", "nul\x00 x", "bell\x07 x", "c1 " + r(0x85) + " next", "del\x7f x",
		"bidi " + r(0x202e) + " override", "isolate " + r(0x2066) + " x", "zero width " + r(0x200b) + " space", "bom " + r(0xfeff) + " x",
		"word joiner " + r(0x2060) + " x", "tag " + r(0xe0041) + " x", "private " + r(0xe000) + " x", "line " + r(0x2028) + " sep",
		"para " + r(0x2029) + " sep", "bad utf8 \xff\xfe x", "lone surrogate \xed\xa0\x80 x",
	}
}

func TestIntercomATextTheAPIWouldRefuseIsRefusedFirstAndNeverEchoed(t *testing.T) {
	api, fake := newIntercomAPI(t)
	for _, text := range icRefusedTexts() {
		_, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: text, Targets: AllSpeakers()})
		if !errors.Is(err, ErrAPI) {
			t.Fatalf("%q accepted: %v", text, err)
		}
		if strings.Contains(err.Error(), strings.TrimSpace(text)) {
			t.Fatalf("the error echoes the text: %v", err)
		}
	}
	if fake.count() != 0 {
		t.Fatalf("%d requests were sent", fake.count())
	}
}

func TestIntercomATextTheAPIAcceptsIsSent(t *testing.T) {
	r := func(cp rune) string { return string(cp) }
	for _, text := range []string{
		"Dinner is ready", "Caf" + r(0xe9) + " cr" + r(0xe8) + "me", r(0xab) + " bonjour " + r(0xbb),
		"family " + r(0x1F468) + r(0x200d) + r(0x1F469) + r(0x200d) + r(0x1F467),
		r(0x645) + r(0x6cc) + r(0x200c) + r(0x62e) + r(0x648) + r(0x627) + r(0x647) + r(0x645),
		"no" + r(0xa0) + "break space inside", "a & b, c; d: e", strings.Repeat("x", 280),
	} {
		api, fake := newIntercomAPI(t)
		fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("completed", nil)})
		if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: text, Targets: AllSpeakers()}); err != nil {
			t.Fatalf("%q refused: %v", text, err)
		}
		if fake.last().Body["text"] != text {
			t.Fatalf("sent %q, want %q", fake.last().Body["text"], text)
		}
	}
}

func TestIntercomTheTextIsTrimmedAndCountedAsSentWithoutNormalisation(t *testing.T) {
	// Deviation from the API and the Python and Node SDKs: no NFC (golang.org/x/text is not a dependency).
	if got, err := AnnouncementText("  " + string(rune(0xa0)) + " hi " + string(rune(0x3000)) + " "); err != nil || got != "hi" {
		t.Fatalf("%q %v", got, err)
	}
	decomposed := "e" + string(rune(0x301))
	if got, err := AnnouncementText(decomposed); err != nil || got != decomposed {
		t.Fatalf("a decomposed text is sent as it is: %q %v", got, err)
	}
	composed := strings.Repeat(string(rune(0xe9)), 280)
	if got, err := AnnouncementText(composed); err != nil || utf8.RuneCountInString(got) != 280 {
		t.Fatalf("%v", err)
	}
	if _, err := AnnouncementText(composed + string(rune(0xe9))); err == nil || !strings.Contains(err.Error(), "at most 280") {
		t.Fatalf("281 accepted: %v", err)
	}
	if _, err := AnnouncementText(" " + string(rune(0xa0)) + " "); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("a blank text: %v", err)
	}
}

func TestIntercomLangIsALanguageTag(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("completed", nil)})
	for _, good := range []string{"en", "en-US", "fr-CA", "zho-Hant"} {
		if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hi", Targets: AllSpeakers(), Lang: good}); err != nil {
			t.Fatalf("%q: %v", good, err)
		}
	}
	sent := fake.count()
	for _, bad := range []string{"e", "en_US", "english", "en-", "en-US-x-private-use-long", " en"} {
		if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hi", Targets: AllSpeakers(), Lang: bad}); !errors.Is(err, ErrAPI) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	if fake.count() != sent {
		t.Fatal("a bad lang sent a request")
	}
}

// -- targets ------------------------------------------------------------------

func TestIntercomATargetListHoldsAtMost50NamesOfAtMost64Characters(t *testing.T) {
	names := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "n" + strconv.Itoa(i)
		}
		return out
	}
	for kind, build := range map[string]func([]string) IntercomTargets{
		"speakers": func(n []string) IntercomTargets { return SpeakerTargets(n...) },
		"rooms":    func(n []string) IntercomTargets { return RoomTargets(n...) },
		"groups":   func(n []string) IntercomTargets { return GroupTargets(n...) },
	} {
		api, fake := newIntercomAPI(t)
		fake.reply("POST", icBase+"/announce", icReply{Status: 200, Body: icAnnouncement("completed", nil)})
		for _, ok := range [][]string{names(50), {strings.Repeat("n", 64)}} {
			if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hi", Targets: build(ok)}); err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
		}
		sent := fake.count()
		if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hi", Targets: build(names(51))}); err == nil || !strings.Contains(err.Error(), "at most 50") {
			t.Fatalf("%s: 51 names: %v", kind, err)
		}
		if _, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hi", Targets: build([]string{strings.Repeat("n", 65)})}); err == nil || !strings.Contains(err.Error(), "at most 64") {
			t.Fatalf("%s: a 65-character name: %v", kind, err)
		}
		if fake.count() != sent {
			t.Fatalf("%s: a refused target sent a request", kind)
		}
	}
}

// -- do not disturb -----------------------------------------------------------

func icQuiet(zone *string, start, end string) *IntercomDNDUpdate {
	yes := true
	return &IntercomDNDUpdate{Enabled: &yes, QuietHours: &IntercomQuietHours{Start: start, End: end, Timezone: zone}}
}

func TestIntercomAnUnusableDoNotDisturbIsRefusedBeforeAnythingIsSent(t *testing.T) {
	api, fake := newIntercomAPI(t)
	str := func(s string) *string { return &s }
	yes, no := true, false
	for name, dnd := range map[string]*IntercomDNDUpdate{
		"no window":                    {Enabled: &yes},
		"no zone":                      icQuiet(nil, "22:00", "07:00"),
		"unknown zone":                 icQuiet(str("Mars/Phobos"), "22:00", "07:00"),
		"padded after":                 icQuiet(str("Europe/Paris "), "22:00", "07:00"),
		"padded before":                icQuiet(str(" Europe/Paris"), "22:00", "07:00"),
		"empty zone":                   icQuiet(str(""), "22:00", "07:00"),
		"Local is not an IANA name":    icQuiet(str("Local"), "22:00", "07:00"),
		"too long":                     icQuiet(str("Europe/"+strings.Repeat("x", 60)), "22:00", "07:00"),
		"an empty window":              icQuiet(str("Europe/Paris"), "22:00", "22:00"),
		"a bad clock":                  icQuiet(str("Europe/Paris"), "25:00", "07:00"),
		"disabled, but a zone is bad":  {Enabled: &no, QuietHours: &IntercomQuietHours{Start: "22:00", End: "07:00", Timezone: str("Mars/Phobos")}},
		"disabled, but a clock is bad": {Enabled: &no, QuietHours: &IntercomQuietHours{Start: "99:00", End: "07:00"}},
	} {
		if _, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "c1", IntercomSpeakerUpdate{DND: dnd}); !errors.Is(err, ErrAPI) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if fake.count() != 0 {
		t.Fatalf("%d requests were sent", fake.count())
	}
}

func TestIntercomAValidDoNotDisturbIsSent(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("PATCH", icBase+"/speakers/c1", icReply{Status: 200, Body: icSpeakerRow(nil)})
	for _, zone := range []string{"Europe/Paris", "America/Argentina/Buenos_Aires", "UTC"} {
		z := zone
		if _, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "c1", IntercomSpeakerUpdate{DND: icQuiet(&z, "22:00", "07:00")}); err != nil {
			t.Fatalf("%s: %v", zone, err)
		}
		if got := fake.last().Body["dnd"].(map[string]any)["quiet_hours"].(map[string]any)["timezone"]; got != zone {
			t.Fatalf("timezone = %v", got)
		}
	}
	// Switched off, a window and a null zone are fine (the API stores them as they are).
	no := false
	if _, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "c1", IntercomSpeakerUpdate{
		DND: &IntercomDNDUpdate{Enabled: &no, QuietHours: &IntercomQuietHours{Start: "21:00", End: "06:30"}}}); err != nil {
		t.Fatal(err)
	}
	if got := fake.last().Body["dnd"].(map[string]any)["quiet_hours"].(map[string]any)["timezone"]; got != nil {
		t.Fatalf("timezone = %v", got)
	}
	if _, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "c1", IntercomSpeakerUpdate{DND: &IntercomDNDUpdate{Enabled: &no}}); err != nil {
		t.Fatal(err)
	}
	if body, _ := json.Marshal(fake.last().Body["dnd"]); string(body) != `{"enabled":false}` {
		t.Fatalf("dnd = %s", body)
	}
}

func TestIntercomTheZoneShapeIsTheFallbackOnlyWhereThereIsNoTimezoneDatabase(t *testing.T) {
	saved := loadLocation
	defer func() { loadLocation = saved }()
	loadLocation = func(name string) (*time.Location, error) { return nil, errors.New("unknown time zone " + name) }
	for _, ok := range []string{"Europe/Paris", "Etc/GMT+5", "UTC"} {
		if err := validIntercomTimezone(ok, "zone"); err != nil { // unknown to this machine, well formed: the API decides
			t.Fatalf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"Europe Paris", "Europe/Paris/..", "/x", "x/", "", "Local", "Europe/Paris "} {
		if err := validIntercomTimezone(bad, "zone"); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	// With a database (a known zone loads), an unknown name is refused.
	loadLocation = func(name string) (*time.Location, error) {
		if name == "America/New_York" || name == "Europe/Paris" {
			return time.UTC, nil
		}
		return nil, errors.New("unknown time zone " + name)
	}
	if err := validIntercomTimezone("Mars/Phobos", "zone"); err == nil {
		t.Fatal("an unknown zone was accepted where a database exists")
	}
	if err := validIntercomTimezone("Europe/Paris", "zone"); err != nil {
		t.Fatal(err)
	}
}

func TestIntercomTheSpeakerUpdateTakesOnlyTheDocumentedFields(t *testing.T) {
	got := map[string]bool{}
	typ := reflect.TypeOf(IntercomSpeakerUpdate{})
	for i := 0; i < typ.NumField(); i++ {
		got[typ.Field(i).Name] = true
	}
	want := map[string]bool{"Receive": true, "Room": true, "ClearRoom": true, "Groups": true, "DND": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields %v, want %v", got, want)
	}
	dnd := map[string]bool{}
	typ = reflect.TypeOf(IntercomDNDUpdate{})
	for i := 0; i < typ.NumField(); i++ {
		dnd[typ.Field(i).Name] = true
	}
	if !reflect.DeepEqual(dnd, map[string]bool{"Enabled": true, "QuietHours": true}) {
		t.Fatalf("dnd fields %v", dnd)
	}
}

// -- the new problem codes ----------------------------------------------------

type icProblemCase struct {
	status int
	code   string
	target error
	extra  map[string]any
	check  func(t *testing.T, e *APIError)
}

func icProblemCases() []icProblemCase {
	return []icProblemCase{
		{403, "urgent_scope_required", ErrUrgentScopeRequired, nil, nil},
		{429, "intercom_urgent_busy", ErrUrgentBusy, map[string]any{"scope": "hub", "retry_after_seconds": 30}, func(t *testing.T, e *APIError) {
			if e.UrgentScope() != "hub" || e.RetryAfter != 30*time.Second {
				t.Fatalf("scope %q retry %v", e.UrgentScope(), e.RetryAfter)
			}
		}},
		{409, "skill_not_installed", ErrSkillNotInstalled, nil, nil},
		{422, "not_a_speaker", ErrNotASpeaker, map[string]any{"kind": "home_assistant"}, func(t *testing.T, e *APIError) {
			if e.SpeakerKind() != "home_assistant" || len(e.NotSpeakers()) != 0 {
				t.Fatalf("kind %q not_speakers %v", e.SpeakerKind(), e.NotSpeakers())
			}
		}},
		{422, "not_a_speaker", ErrNotASpeaker, map[string]any{"not_speakers": []any{"home-link", "other", 7, ""}}, func(t *testing.T, e *APIError) {
			if e.SpeakerKind() != "" || !reflect.DeepEqual(e.NotSpeakers(), []string{"home-link", "other"}) {
				t.Fatalf("kind %q not_speakers %v", e.SpeakerKind(), e.NotSpeakers())
			}
		}},
		{422, "reserved_name", ErrReservedName, nil, nil},
		{403, "hub_locked", ErrHubLocked, nil, nil},
		{403, "impersonation_not_allowed", ErrImpersonationNotAllowed, nil, nil},
		{422, "too_many_targets", ErrTooManyTargets, map[string]any{"matched": 51}, func(t *testing.T, e *APIError) {
			if n, ok := e.Matched(); !ok || n != 51 {
				t.Fatalf("matched %d %v", n, ok)
			}
		}},
		{422, "intercom_unavailable", ErrIntercomUnavailable, map[string]any{"reason": "kid_hub"}, func(t *testing.T, e *APIError) {
			if e.Reason() != "kid_hub" {
				t.Fatalf("reason %q", e.Reason())
			}
		}},
		{503, "intercom_disabled_platform", ErrIntercomPlatformDisabled, nil, nil},
		{503, "intercom_not_configured", ErrIntercomNotConfigured, nil, nil},
		{403, "managed_client", ErrManagedClient, map[string]any{"refused_client_refs": []any{"client-1"}}, func(t *testing.T, e *APIError) {
			if !reflect.DeepEqual(e.RefusedClientRefs(), []string{"client-1"}) {
				t.Fatalf("refs %v", e.RefusedClientRefs())
			}
		}},
		{409, "not_cancellable", ErrNotCancellable, map[string]any{"announcement_status": "cancelled"}, func(t *testing.T, e *APIError) {
			if e.AnnouncementStatus() != "cancelled" {
				t.Fatalf("status %q", e.AnnouncementStatus())
			}
		}},
		{502, "hub_unreachable", ErrHubUnreachable, map[string]any{"announcement_id": icAnn}, func(t *testing.T, e *APIError) {
			if e.AnnouncementID() != icAnn {
				t.Fatalf("announcement %q", e.AnnouncementID())
			}
		}},
	}
}

func TestIntercomEachNewProblemCodeIsItsOwnTypedRefusal(t *testing.T) {
	all := []error{ErrIntercomUnavailable, ErrIntercomPlatformDisabled, ErrIntercomNotConfigured, ErrUrgentScopeRequired, ErrUrgentBusy,
		ErrSkillNotInstalled, ErrNotASpeaker, ErrReservedName, ErrHubLocked, ErrImpersonationNotAllowed, ErrTooManyTargets,
		ErrManagedClient, ErrNotCancellable, ErrHubUnreachable}
	for _, nested := range []bool{false, true} {
		for _, tc := range icProblemCases() {
			api, fake := newIntercomAPI(t)
			body := map[string]any{"status": tc.status, "code": tc.code, "message": "A sentence from the API."}
			for k, v := range tc.extra {
				body[k] = v
			}
			if nested {
				body = map[string]any{"detail": body}
			}
			fake.reply("POST", icBase+"/announce", icReply{Status: tc.status, Body: body})
			_, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "Dinner", Targets: AllSpeakers()})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status || apiErr.Code != tc.code {
				t.Fatalf("%s (nested %v): %v", tc.code, nested, err)
			}
			if !errors.Is(err, tc.target) {
				t.Fatalf("%s (nested %v): not %v", tc.code, nested, tc.target)
			}
			for _, other := range all {
				if other != tc.target && errors.Is(err, other) {
					t.Fatalf("%s matches %v too", tc.code, other)
				}
			}
			if tc.check != nil {
				tc.check(t, apiErr)
			}
		}
	}
}

func TestIntercomTheTypedRefusalsKeepTheirPlacesInTheHierarchy(t *testing.T) {
	kinds := map[string][]error{
		"urgent_scope_required":      {ErrUrgentScopeRequired, ErrAuth},                  // sign in again with the scope
		"intercom_urgent_busy":       {ErrUrgentBusy, ErrRateLimited},                    // still "a 429"
		"intercom_unavailable":       {ErrIntercomUnavailable, ErrIntercomDisabled},      // a kind of disabled
		"intercom_disabled_platform": {ErrIntercomPlatformDisabled, ErrIntercomDisabled}, // a kind of disabled
	}
	for code, targets := range kinds {
		err := &APIError{StatusCode: 400, Code: code}
		for _, target := range targets {
			if !errors.Is(err, target) {
				t.Errorf("%s is not %v", code, target)
			}
		}
		if !errors.Is(err, ErrAPI) {
			t.Errorf("%s is not ErrAPI", code)
		}
	}
	// The reverse does not hold: a plain 429, a plain disabled and a plain 403 are not the stronger kinds.
	if errors.Is(&APIError{StatusCode: 429}, ErrUrgentBusy) || errors.Is(&APIError{StatusCode: 403, Code: "intercom_disabled"}, ErrIntercomPlatformDisabled) ||
		errors.Is(&APIError{StatusCode: 403, ProblemDetail: "Insufficient scopes"}, ErrUrgentScopeRequired) {
		t.Fatal("a general refusal matched a stronger one")
	}
	for _, kind := range []error{ErrIntercomNotConfigured, ErrHubLocked, ErrManagedClient, ErrNotASpeaker} {
		if errors.Is(&APIError{StatusCode: 403}, kind) {
			t.Errorf("a plain 403 matches %v", kind)
		}
	}
	// A plain 403 "Insufficient scopes" is still ErrAuth and not the urgent kind.
	if !errors.Is(&APIError{StatusCode: 403, ProblemDetail: "Insufficient scopes"}, ErrAuth) {
		t.Fatal("insufficient scopes is no longer ErrAuth")
	}
}

func TestIntercomTheUrgentBusyRefusalCarriesTheRetryAfterHeader(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 429, Headers: map[string]string{"Retry-After": "30"},
		Body: map[string]any{"code": "intercom_urgent_busy", "message": "busy", "scope": "platform", "retry_after_seconds": 30}})
	_, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "Wake", Targets: AllSpeakers(), Priority: IntercomPriorityUrgent, Repeat: &IntercomRepeat{Times: 5, IntervalSeconds: 30}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !errors.Is(err, ErrUrgentBusy) || !errors.Is(err, ErrRateLimited) || apiErr.UrgentScope() != "platform" || apiErr.RetryAfter != 30*time.Second {
		t.Fatalf("%v", err)
	}
}

func TestIntercomANotASpeakerRefusalOnThePatch(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("PATCH", icBase+"/speakers/996f", icReply{Status: 422, Body: map[string]any{"status": 422, "code": "not_a_speaker", "message": "cannot play", "kind": "home_assistant"}})
	yes := true
	_, err := api.UpdateIntercomSpeaker(context.Background(), icHub, "996f", IntercomSpeakerUpdate{Receive: &yes})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !errors.Is(err, ErrNotASpeaker) || apiErr.SpeakerKind() != "home_assistant" {
		t.Fatalf("%v", err)
	}
}

func TestIntercomAProblemWithoutItsExtraMembersHasNoneOfThem(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announce", icReply{Status: 422, Body: map[string]any{"code": "too_many_targets", "message": "too many", "matched": "51"}})
	_, err := api.Announce(context.Background(), icHub, AnnounceOptions{Text: "hi", Targets: AllSpeakers()})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal(err)
	}
	if n, ok := apiErr.Matched(); ok || n != 0 {
		t.Fatalf("a string is not a count: %d %v", n, ok)
	}
	for _, bad := range []any{-1, 1.5, true, nil} {
		if _, ok := (&APIError{Problem: map[string]any{"matched": bad}}).Matched(); ok {
			t.Fatalf("%v read as a count", bad)
		}
	}
	empty := &APIError{}
	if empty.Reason() != "" || empty.UrgentScope() != "" || empty.SpeakerKind() != "" || empty.AnnouncementStatus() != "" ||
		empty.NotSpeakers() != nil || empty.RefusedClientRefs() != nil {
		t.Fatal("an error with no body has members")
	}
}

func TestIntercomTheCancel409NamesTheCurrentStatus(t *testing.T) {
	api, fake := newIntercomAPI(t)
	fake.reply("POST", icBase+"/announcements/"+icAnn+"/cancel", icReply{Status: 409, Body: map[string]any{
		"status": 409, "code": "not_cancellable", "message": "Only an announcement that is still sending can be cancelled.", "announcement_status": "completed"}})
	_, err := api.CancelAnnouncement(context.Background(), icHub, icAnn)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !errors.Is(err, ErrNotCancellable) || apiErr.AnnouncementStatus() != "completed" {
		t.Fatalf("%v", err)
	}
}

func TestIntercomTheCodesDecideWhateverTheStatus(t *testing.T) {
	// Like the other SDKs: the intercom problem code decides, not the status that carried it.
	for _, status := range []int{400, 500} {
		if !errors.Is(&APIError{StatusCode: status, Code: "hub_unreachable"}, ErrHubUnreachable) || !errors.Is(&APIError{StatusCode: status, Code: "not_cancellable"}, ErrNotCancellable) {
			t.Fatalf("status %d", status)
		}
	}
}

func TestIntercomTheGuidesNameTheUrgentScopeAndTheNewErrors(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"intercom:urgent", "ErrUrgentScopeRequired", "PolicySynced", "CanPlay", "Replayed", "ErrNotASpeaker",
		"ErrTooManyTargets", "ErrIntercomPlatformDisabled", "not a certified safety system"} {
		if !strings.Contains(strings.ToLower(string(raw)), strings.ToLower(needle)) {
			t.Errorf("README.md does not mention %q", needle)
		}
	}
}
