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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
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
	hours := &IntercomQuietHours{Start: "21:00", End: "06:30"}
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
	if err != nil || len(listed) != 1 || !listed[0].Online || listed[0].Intercom.DND.QuietHours.Start != "22:00" || *listed[0].Intercom.Room != "kitchen" {
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
		for _, other := range []error{ErrIntercomDisabled, ErrHubUnreachable, ErrNotCancellable, ErrRateLimited, ErrPlan, ErrAuth} {
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
		{Text: "a\x07b", Targets: all}, {Text: "a b", Targets: all},
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
	if _, err := AnnouncementText(strings.Repeat("\U0001F600", 280)); err != nil {
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
