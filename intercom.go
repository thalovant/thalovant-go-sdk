package thalovant

// Intercom: say a short text aloud on a household hub's speakers.
//
// An announcement is a text of at most 280 characters, spoken by each chosen
// speaker's own text-to-speech. It is announce-only: no two-way audio, and
// nothing listens. It is NOT a certified safety system and does not replace
// smoke or CO alarms, medical alert devices or emergency services.
//
// The platform never stores the text: the API keeps its SHA-256 and length,
// the targets, each speaker's result and the actor. These are the routes under
// /v1/hubs/{hub_id}/intercom on ControlPlane (Announce, GetAnnouncement,
// ListAnnouncements, ListIntercomSpeakers, UpdateIntercomSpeaker,
// GetIntercomSettings, SetIntercomSettings). The names say Announce and
// Intercom, never a bare Intercom for the HTTP calls: the HiveMind mesh
// already has an intercom frame, which is a different thing.
//
// Scopes: intercom:read for the reads, intercom:send for Announce, and
// hubs:write (owner or admin of the hub) for settings and speakers. A refusal
// is an *APIError; errors.Is reaches ErrIntercomDisabled, ErrHubUnreachable,
// ErrRateLimited, ErrPlan and ErrAuth.
//
// Every status is open: a value the SDK does not know is kept as sent.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits of the intercom contract.
const (
	// MaxAnnouncementText is the longest announcement, in characters.
	MaxAnnouncementText = 280
	// MaxAnnouncementSpeakers is the most speakers one announcement may name.
	MaxAnnouncementSpeakers = 50
	// MaxRepeatTimes is the most repeats of an urgent announcement.
	MaxRepeatTimes = 10
	// MinRepeatIntervalSeconds and MaxRepeatIntervalSeconds bound the repeat interval.
	MinRepeatIntervalSeconds = 5
	MaxRepeatIntervalSeconds = 120
	// MaxIntercomRoomLength, MaxIntercomGroupLength and MaxIntercomGroups bound a speaker's policy.
	MaxIntercomRoomLength  = 64
	MaxIntercomGroupLength = 32
	MaxIntercomGroups      = 8
)

// IntercomPriority is how urgent an announcement is.
type IntercomPriority string

const (
	// IntercomPriorityNormal waits for the hub's answer.
	IntercomPriorityNormal IntercomPriority = "normal"
	// IntercomPriorityUrgent may repeat until somebody acknowledges.
	IntercomPriorityUrgent IntercomPriority = "urgent"
)

// AnnouncementStatus is where an announcement is. "sending" while an urgent
// one is still repeating. Open: a value the API adds later is kept as sent.
type AnnouncementStatus string

const (
	AnnouncementCompleted AnnouncementStatus = "completed"
	AnnouncementPartial   AnnouncementStatus = "partial"
	AnnouncementFailed    AnnouncementStatus = "failed"
	AnnouncementSending   AnnouncementStatus = "sending"
)

// DeliveryStatus is what happened for one speaker. Open.
type DeliveryStatus string

const (
	DeliveryDelivered           DeliveryStatus = "delivered"
	DeliveryAcked               DeliveryStatus = "acked"
	DeliveryOffline             DeliveryStatus = "offline"
	DeliverySkippedDND          DeliveryStatus = "skipped_dnd"
	DeliverySkippedNotReceiving DeliveryStatus = "skipped_not_receiving"
	DeliveryFailed              DeliveryStatus = "failed"
)

// IntercomUnavailableReason says why intercom cannot be enabled for a hub:
// "plan", "kid_hub" or "public_hub". Open.
type IntercomUnavailableReason string

// IntercomSettings is a hub's intercom switch.
type IntercomSettings struct {
	Enabled     bool `json:"enabled"`
	PlanAllowed bool `json:"plan_allowed"`
	// UnavailableReason is "" when intercom can be enabled.
	UnavailableReason IntercomUnavailableReason `json:"unavailable_reason"`
}

// IntercomQuietHours are HH:MM quiet hours in an IANA timezone (UTC when nil).
type IntercomQuietHours struct {
	Start    string  `json:"start"`
	End      string  `json:"end"`
	Timezone *string `json:"timezone"`
}

// IntercomDND is do-not-disturb: a normal announcement is skipped inside quiet
// hours; an urgent one is not.
type IntercomDND struct {
	Enabled    bool                `json:"enabled"`
	QuietHours *IntercomQuietHours `json:"quiet_hours,omitempty"`
}

// IntercomPolicy is a speaker's intercom policy. Receive is false until
// somebody turns it on.
type IntercomPolicy struct {
	Receive bool        `json:"receive"`
	Room    *string     `json:"room"`
	Groups  []string    `json:"groups"`
	DND     IntercomDND `json:"dnd"`
}

// IntercomSpeaker is a client of the hub that may be spoken to.
type IntercomSpeaker struct {
	ClientID   string         `json:"client_id"`
	Slug       string         `json:"slug"`
	Name       string         `json:"name"`
	Online     bool           `json:"online"`
	LastSeenAt *string        `json:"last_seen_at"`
	Intercom   IntercomPolicy `json:"intercom"`
}

// IntercomDelivery is what happened for one speaker.
type IntercomDelivery struct {
	ClientID string         `json:"client_id"`
	Slug     string         `json:"slug"`
	Name     string         `json:"name"`
	Status   DeliveryStatus `json:"status"`
	At       *string        `json:"at"`
}

// Announcement is an announcement and its deliveries. The text is never part
// of it: the platform keeps none.
type Announcement struct {
	ID         string             `json:"id"`
	Status     AnnouncementStatus `json:"status"`
	Priority   IntercomPriority   `json:"priority"`
	CreatedAt  string             `json:"created_at"`
	Deliveries []IntercomDelivery `json:"deliveries"`
	// AckedBy names the speaker that acknowledged an urgent announcement; nil when none did.
	AckedBy *string `json:"acked_by"`
}

// Sending reports whether an urgent announcement is still repeating.
func (a Announcement) Sending() bool { return a.Status == AnnouncementSending }

// AnnouncementList is a page of announcements, newest first. NextCursor is ""
// on the last page.
type AnnouncementList struct {
	Items      []Announcement
	NextCursor string
}

// IntercomTargets is whom to speak to: exactly one of All, Speakers, Rooms or Groups.
type IntercomTargets struct {
	// All speaks on every speaker that receives.
	All bool
	// Speakers are client ids or slugs (at most 50).
	Speakers []string
	Rooms    []string
	Groups   []string
}

// AllSpeakers targets every speaker that receives.
func AllSpeakers() IntercomTargets { return IntercomTargets{All: true} }

// SpeakerTargets targets speakers by client id or slug.
func SpeakerTargets(speakers ...string) IntercomTargets { return IntercomTargets{Speakers: speakers} }

// RoomTargets targets speakers by room.
func RoomTargets(rooms ...string) IntercomTargets { return IntercomTargets{Rooms: rooms} }

// GroupTargets targets speakers by group.
func GroupTargets(groups ...string) IntercomTargets { return IntercomTargets{Groups: groups} }

// IntercomRepeat is how an urgent announcement repeats: Times 1-10,
// IntervalSeconds 5-120.
type IntercomRepeat struct {
	Times           int `json:"times"`
	IntervalSeconds int `json:"interval_seconds"`
}

// AnnounceOptions is one announcement.
type AnnounceOptions struct {
	// Text is 1-280 characters, trimmed, with no control characters. It is
	// spoken aloud in a home.
	Text    string
	Targets IntercomTargets
	// Priority is "normal" when empty.
	Priority IntercomPriority
	// Repeat is for urgent announcements only.
	Repeat *IntercomRepeat
	// Lang is a language tag such as "en-US"; "" leaves it to the hub.
	Lang string
	// IdempotencyKey is sent as Idempotency-Key when set; it is never generated.
	IdempotencyKey string
	// Wait polls a "sending" announcement with GetAnnouncement until it
	// completes. The polls need intercom:read as well as intercom:send.
	Wait bool
	// WaitTimeout is the longest wait: Times*Interval+15s when zero; it must
	// not be negative. The deadline bounds each poll, not only the sleep
	// between polls. On timeout the latest answer is returned, still "sending".
	WaitTimeout time.Duration
	// PollInterval is the time between polls: 2s when zero; it must not be
	// negative.
	PollInterval time.Duration
}

// IntercomDNDUpdate is a do-not-disturb change. The API replaces the whole
// do-not-disturb setting with it and stores a missing enabled as false, so
// Enabled is required (nil is refused) rather than left to default to off:
// an update that only carries QuietHours must still say whether DND is on.
// A nil QuietHours stores the default quiet hours (22:00-07:00, UTC).
type IntercomDNDUpdate struct {
	Enabled    *bool
	QuietHours *IntercomQuietHours
}

// IntercomSpeakerUpdate is a change to a speaker's policy: only what is set is
// sent. At least one field is required.
type IntercomSpeakerUpdate struct {
	Receive *bool
	// Room sets the room; ClearRoom sends null instead. Not both.
	Room      *string
	ClearRoom bool
	// Groups replaces the groups when non-nil (an empty non-nil slice clears them).
	Groups []string
	DND    *IntercomDNDUpdate
}

// AnnouncementListOptions pages through announcements.
type AnnouncementListOptions struct {
	// Limit is a positive count; 0 leaves it to the API.
	Limit int
	// Cursor continues from a page's NextCursor.
	Cursor string
}

var intercomClock = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func intercomErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrAPI}, args...)...)
}

// AnnouncementText returns the text as it is sent: trimmed, 1-280 characters,
// no control characters.
func AnnouncementText(text string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", intercomErr("an announcement's text must not be empty")
	}
	if utf8.RuneCountInString(trimmed) > MaxAnnouncementText {
		return "", intercomErr("an announcement's text is at most %d characters", MaxAnnouncementText)
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return "", intercomErr("an announcement's text must not contain control characters")
		}
	}
	return trimmed, nil
}

func intercomTargetsBody(targets IntercomTargets) (map[string]any, error) {
	chosen := 0
	for _, set := range []bool{targets.All, len(targets.Speakers) > 0, len(targets.Rooms) > 0, len(targets.Groups) > 0} {
		if set {
			chosen++
		}
	}
	if chosen != 1 {
		return nil, intercomErr("choose exactly one of all, speakers, rooms or groups as targets")
	}
	if targets.All {
		return map[string]any{"all": true}, nil
	}
	kind, items := "speakers", targets.Speakers
	switch {
	case len(targets.Rooms) > 0:
		kind, items = "rooms", targets.Rooms
	case len(targets.Groups) > 0:
		kind, items = "groups", targets.Groups
	}
	for _, item := range items {
		if strings.TrimSpace(item) == "" {
			return nil, intercomErr("a target in %s must be a non-empty string", kind)
		}
	}
	if kind == "speakers" && len(items) > MaxAnnouncementSpeakers {
		return nil, intercomErr("an announcement names at most %d speakers", MaxAnnouncementSpeakers)
	}
	return map[string]any{kind: append([]string(nil), items...)}, nil
}

// announceBody is the POST .../intercom/announce body. It holds the text: never log it.
func announceBody(opts AnnounceOptions) (map[string]any, error) {
	priority := opts.Priority
	if priority == "" {
		priority = IntercomPriorityNormal
	}
	if priority != IntercomPriorityNormal && priority != IntercomPriorityUrgent {
		return nil, intercomErr(`priority must be "normal" or "urgent"`)
	}
	text, err := AnnouncementText(opts.Text)
	if err != nil {
		return nil, err
	}
	targets, err := intercomTargetsBody(opts.Targets)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"text": text, "targets": targets, "priority": string(priority)}
	if opts.Repeat != nil {
		if priority != IntercomPriorityUrgent {
			return nil, intercomErr("repeat is only for urgent announcements")
		}
		if opts.Repeat.Times < 1 || opts.Repeat.Times > MaxRepeatTimes {
			return nil, intercomErr("repeat times must be an integer from 1 to %d", MaxRepeatTimes)
		}
		if opts.Repeat.IntervalSeconds < MinRepeatIntervalSeconds || opts.Repeat.IntervalSeconds > MaxRepeatIntervalSeconds {
			return nil, intercomErr("repeat interval_seconds must be an integer from %d to %d", MinRepeatIntervalSeconds, MaxRepeatIntervalSeconds)
		}
		body["repeat"] = map[string]any{"times": opts.Repeat.Times, "interval_seconds": opts.Repeat.IntervalSeconds}
	}
	if opts.Lang != "" {
		if strings.TrimSpace(opts.Lang) == "" {
			return nil, intercomErr("lang must not be blank")
		}
		body["lang"] = opts.Lang
	}
	return body, nil
}

// speakerUpdateBody is the PATCH .../intercom/speakers/{id} body: only what is set.
func speakerUpdateBody(update IntercomSpeakerUpdate) (map[string]any, error) {
	body := map[string]any{}
	if update.Receive != nil {
		body["receive"] = *update.Receive
	}
	if update.Room != nil && update.ClearRoom {
		return nil, intercomErr("set a room or clear it, not both")
	}
	if update.Room != nil {
		if strings.TrimSpace(*update.Room) == "" || utf8.RuneCountInString(*update.Room) > MaxIntercomRoomLength {
			return nil, intercomErr("room is 1-%d characters", MaxIntercomRoomLength)
		}
		body["room"] = *update.Room
	}
	if update.ClearRoom {
		body["room"] = nil
	}
	if update.Groups != nil {
		if len(update.Groups) > MaxIntercomGroups {
			return nil, intercomErr("groups is a list of at most %d names", MaxIntercomGroups)
		}
		for _, group := range update.Groups {
			if strings.TrimSpace(group) == "" || utf8.RuneCountInString(group) > MaxIntercomGroupLength {
				return nil, intercomErr("a group name is 1-%d characters", MaxIntercomGroupLength)
			}
		}
		body["groups"] = append([]string{}, update.Groups...)
	}
	if update.DND != nil {
		if update.DND.Enabled == nil {
			return nil, intercomErr("dnd.Enabled is required: true or false (the API replaces the whole do-not-disturb setting, so a missing value would switch it off)")
		}
		dnd := map[string]any{"enabled": *update.DND.Enabled}
		if hours := update.DND.QuietHours; hours != nil {
			if !intercomClock.MatchString(hours.Start) || !intercomClock.MatchString(hours.End) {
				return nil, intercomErr("quiet_hours start and end must be HH:MM")
			}
			dnd["quiet_hours"] = map[string]any{"start": hours.Start, "end": hours.End, "timezone": hours.Timezone}
		}
		body["dnd"] = dnd
	}
	if len(body) == 0 {
		return nil, intercomErr("pass at least one of receive, room, groups or dnd")
	}
	return body, nil
}

// intercomPath is the escaped path under a hub's intercom routes; a blank id
// is refused before anything is sent.
func intercomPath(hubID string, rest ...string) (string, error) {
	if strings.TrimSpace(hubID) == "" {
		return "", intercomErr("a hub id is required")
	}
	return strings.Join(append([]string{"/v1/hubs/" + url.PathEscape(hubID) + "/intercom"}, rest...), "/"), nil
}

func intercomSegment(value, name string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", intercomErr("%s is required", name)
	}
	return url.PathEscape(value), nil
}

// GetIntercomSettings reads a hub's intercom switch and whether it can be
// turned on. Requires intercom:read.
func (c *ControlPlane) GetIntercomSettings(ctx context.Context, hubID string) (IntercomSettings, error) {
	path, err := intercomPath(hubID, "settings")
	if err != nil {
		return IntercomSettings{}, err
	}
	var settings IntercomSettings
	if err := c.controlInto(ctx, http.MethodGet, path, nil, nil, &settings); err != nil {
		return IntercomSettings{}, err
	}
	return settings, nil
}

// SetIntercomSettings turns a hub's intercom on or off. Requires hubs:write
// (owner or admin of the hub). A plan without intercom is ErrPlan; a kid or
// public hub is a 422 *APIError.
func (c *ControlPlane) SetIntercomSettings(ctx context.Context, hubID string, enabled bool) (IntercomSettings, error) {
	path, err := intercomPath(hubID, "settings")
	if err != nil {
		return IntercomSettings{}, err
	}
	var settings IntercomSettings
	if err := c.controlInto(ctx, http.MethodPut, path, map[string]any{"enabled": enabled}, nil, &settings); err != nil {
		return IntercomSettings{}, err
	}
	return settings, nil
}

// ListIntercomSpeakers lists every client of the hub as a speaker, with
// presence and policy. Requires intercom:read.
func (c *ControlPlane) ListIntercomSpeakers(ctx context.Context, hubID string) ([]IntercomSpeaker, error) {
	path, err := intercomPath(hubID, "speakers")
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Items *[]IntercomSpeaker `json:"items"`
	}
	if err := c.controlInto(ctx, http.MethodGet, path, nil, nil, &envelope); err != nil {
		return nil, err
	}
	if envelope.Items == nil {
		return nil, &APIError{Detail: "unexpected intercom speaker listing shape"}
	}
	return *envelope.Items, nil
}

// UpdateIntercomSpeaker changes a speaker's policy: only what update sets is
// sent. A speaker is spoken to only when Receive is true. Requires hubs:write.
func (c *ControlPlane) UpdateIntercomSpeaker(ctx context.Context, hubID, clientID string, update IntercomSpeakerUpdate) (IntercomSpeaker, error) {
	body, err := speakerUpdateBody(update)
	if err != nil {
		return IntercomSpeaker{}, err
	}
	segment, err := intercomSegment(clientID, "a client id")
	if err != nil {
		return IntercomSpeaker{}, err
	}
	path, err := intercomPath(hubID, "speakers", segment)
	if err != nil {
		return IntercomSpeaker{}, err
	}
	var speaker IntercomSpeaker
	if err := c.controlInto(ctx, http.MethodPatch, path, body, nil, &speaker); err != nil {
		return IntercomSpeaker{}, err
	}
	return speaker, nil
}

// Announce says opts.Text aloud on a hub's speakers. The platform keeps only
// the text's hash and length. Not a certified safety system.
//
// A normal announcement waits up to ten seconds for the hub and returns its
// result. An urgent one with Repeat returns at once as "sending"; set Wait to
// poll GetAnnouncement until it completes (or WaitTimeout passes, which
// returns the latest answer, still "sending"). Set IdempotencyKey, and keep
// it, to retry after an uncertain outcome without speaking twice.
//
// Wait needs intercom:read as well as intercom:send: the polls are
// GetAnnouncement. A poll can fail after the announcement was accepted (a
// token with only intercom:send is refused at the first poll; a 429; the
// network). Announce then returns the last accepted announcement, whose ID
// lets the caller poll it later, together with the poll's error: a non-nil
// error with a non-empty Announcement.ID means the announcement exists and a
// retry without the same IdempotencyKey may speak it again. WaitTimeout and
// PollInterval are checked before anything is sent, and WaitTimeout bounds
// each poll, so a stalled read cannot carry the wait past it.
//
// Requires intercom:send. Intercom off is ErrIntercomDisabled, an unreachable
// hub ErrHubUnreachable (the text may or may not have been spoken: retry with
// the same IdempotencyKey), a rate limit ErrRateLimited (RetryAfter says how
// long), a plan without intercom ErrPlan (HTTP 402, or 403 plan_limit).
func (c *ControlPlane) Announce(ctx context.Context, hubID string, opts AnnounceOptions) (Announcement, error) {
	body, err := announceBody(opts)
	if err != nil {
		return Announcement{}, err
	}
	path, err := intercomPath(hubID, "announce")
	if err != nil {
		return Announcement{}, err
	}
	if opts.PollInterval < 0 {
		return Announcement{}, intercomErr("PollInterval must not be negative")
	}
	if opts.WaitTimeout < 0 {
		return Announcement{}, intercomErr("WaitTimeout must not be negative")
	}
	var headers map[string]string
	if opts.IdempotencyKey != "" {
		if strings.TrimSpace(opts.IdempotencyKey) == "" {
			return Announcement{}, intercomErr("IdempotencyKey must not be blank")
		}
		headers = map[string]string{"Idempotency-Key": opts.IdempotencyKey}
	}
	var announcement Announcement
	if err := c.controlInto(ctx, http.MethodPost, path, body, headers, &announcement); err != nil {
		return Announcement{}, err
	}
	if announcement.ID == "" || announcement.Status == "" {
		return Announcement{}, &APIError{Detail: "unexpected announcement shape"}
	}
	if !opts.Wait || !announcement.Sending() {
		return announcement, nil
	}
	timeout := opts.WaitTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
		if opts.Repeat != nil {
			timeout += time.Duration(opts.Repeat.Times*opts.Repeat.IntervalSeconds) * time.Second
		}
	}
	poll := opts.PollInterval
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for announcement.Sending() {
		wait := time.Until(deadline)
		if wait <= 0 {
			break
		}
		if wait > poll {
			wait = poll
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return announcement, ctx.Err()
		case <-timer.C:
		}
		// With nothing left after the sleep, no read starts at all.
		if time.Until(deadline) <= 0 {
			break
		}
		// Each read is bounded by what is left of the wait: a read the API is
		// slow to answer must not carry the wait past its deadline.
		readCtx, cancel := context.WithDeadline(ctx, deadline)
		latest, err := c.GetAnnouncement(readCtx, hubID, announcement.ID)
		ranOut := readCtx.Err() != nil && ctx.Err() == nil
		cancel()
		if err != nil {
			if ranOut {
				break // the latest answer, still "sending"
			}
			// The accepted announcement is kept: its ID is the caller's way back.
			return announcement, err
		}
		announcement = latest
	}
	return announcement, nil
}

// GetAnnouncement reads one announcement and its deliveries; poll it while
// "sending". Requires intercom:read.
func (c *ControlPlane) GetAnnouncement(ctx context.Context, hubID, announcementID string) (Announcement, error) {
	segment, err := intercomSegment(announcementID, "an announcement id")
	if err != nil {
		return Announcement{}, err
	}
	path, err := intercomPath(hubID, "announcements", segment)
	if err != nil {
		return Announcement{}, err
	}
	var announcement Announcement
	if err := c.controlInto(ctx, http.MethodGet, path, nil, nil, &announcement); err != nil {
		return Announcement{}, err
	}
	if announcement.ID == "" || announcement.Status == "" {
		return Announcement{}, &APIError{Detail: "unexpected announcement shape"}
	}
	return announcement, nil
}

// ListAnnouncements lists a hub's announcements, newest first; continue with
// NextCursor. Requires intercom:read.
func (c *ControlPlane) ListAnnouncements(ctx context.Context, hubID string, opts AnnouncementListOptions) (AnnouncementList, error) {
	path, err := intercomPath(hubID, "announcements")
	if err != nil {
		return AnnouncementList{}, err
	}
	if opts.Limit < 0 {
		return AnnouncementList{}, intercomErr("limit must be a positive integer")
	}
	query := url.Values{}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		if strings.TrimSpace(opts.Cursor) == "" {
			return AnnouncementList{}, intercomErr("cursor must not be blank")
		}
		query.Set("cursor", opts.Cursor)
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var envelope struct {
		Items      *[]Announcement `json:"items"`
		Data       *[]Announcement `json:"data"`
		NextCursor string          `json:"next_cursor"`
		Cursor     string          `json:"cursor"`
	}
	if err := c.controlInto(ctx, http.MethodGet, path, nil, nil, &envelope); err != nil {
		return AnnouncementList{}, err
	}
	rows := envelope.Items
	if rows == nil {
		rows = envelope.Data
	}
	if rows == nil {
		return AnnouncementList{}, &APIError{Detail: "unexpected announcement listing shape"}
	}
	cursor := envelope.NextCursor
	if cursor == "" {
		cursor = envelope.Cursor
	}
	return AnnouncementList{Items: *rows, NextCursor: cursor}, nil
}
