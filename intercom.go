package thalovant

// Intercom: say a short text aloud on a household hub's speakers.
//
// An announcement is a text of at most 280 characters, spoken by each chosen
// speaker's own text-to-speech. It is announce-only: no two-way audio.
// Intercom does not record or store speech; while an urgent alert sounds, the
// hub only checks whether a reply is an acknowledgement. It is NOT a certified
// safety system and does not replace smoke or CO alarms, medical alert devices
// or emergency services.
//
// The API does not store the text: it keeps a keyed hash of it (HMAC-SHA256,
// server-side secret) and its length, the targets, each speaker's result and
// the actor. (The hub's own message bus and each speaker's text-to-speech are
// outside the platform's control.) These are the routes under
// /v1/hubs/{hub_id}/intercom on ControlPlane (Announce, CancelAnnouncement,
// GetAnnouncement, ListAnnouncements, ListIntercomSpeakers,
// UpdateIntercomSpeaker, GetIntercomSettings, SetIntercomSettings). The names
// say Announce and Intercom, never a bare Intercom for the HTTP calls: the
// HiveMind mesh already has an intercom frame, which is a different thing.
//
// Scopes: intercom:read for the reads, intercom:send for Announce and
// CancelAnnouncement, intercom:urgent IN ADDITION to intercom:send for an
// urgent announcement (Priority "urgent", and so every Repeat), and
// hubs:write (owner, manager or administrator of the hub) for settings and
// speakers. intercom:urgent is implied by nothing and implies nothing; a token
// without it is refused before anything is spent (ErrUrgentScopeRequired).
//
// A refusal is an *APIError; errors.Is reaches ErrIntercomDisabled (and its
// kinds ErrIntercomUnavailable and ErrIntercomPlatformDisabled),
// ErrIntercomNotConfigured, ErrUrgentScopeRequired (a kind of ErrAuth),
// ErrUrgentBusy (a kind of ErrRateLimited), ErrSkillNotInstalled,
// ErrNotASpeaker, ErrReservedName, ErrHubLocked, ErrImpersonationNotAllowed,
// ErrTooManyTargets, ErrManagedClient, ErrHubUnreachable, ErrNotCancellable,
// ErrRateLimited, ErrPlan and ErrAuth; the IntercomCode constants name each
// problem code.
//
// Idempotency: set a fresh unique IdempotencyKey (a UUID) for each
// announcement you mean to send. The same key replays the stored answer
// (Idempotency-Replayed: true) and never sends again, whatever its status; a
// replayed "failed" announcement stays "failed". To send the same text again
// on purpose, use a new key.
//
// Every status is open: a value the SDK does not know is kept as sent.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The scopes of the intercom routes. IntercomUrgentScope is needed IN
// ADDITION to IntercomSendScope for an urgent announcement; it is not implied
// by anything and implies nothing. Changing settings or a speaker needs
// hubs:write.
const (
	IntercomReadScope   = "intercom:read"
	IntercomSendScope   = "intercom:send"
	IntercomUrgentScope = "intercom:urgent"
)

// The problem codes the intercom routes (and the connection routes beside
// them) answer with, as APIError.Code carries them. errors.Is on the matching
// Err sentinel is the usual way to branch; any code not listed arrives as a
// plain *APIError with Code set.
const (
	// IntercomCodeDisabled (403 or 422): the hub's switch is off (ErrIntercomDisabled).
	IntercomCodeDisabled = "intercom_disabled"
	// IntercomCodeUnavailable (422): never available on a kid or public hub (ErrIntercomUnavailable).
	IntercomCodeUnavailable = "intercom_unavailable"
	// IntercomCodeDisabledPlatform (503): switched off for the whole platform (ErrIntercomPlatformDisabled).
	IntercomCodeDisabledPlatform = "intercom_disabled_platform"
	// IntercomCodeNotConfigured (503): the operator has not configured intercom (ErrIntercomNotConfigured).
	IntercomCodeNotConfigured = "intercom_not_configured"
	// IntercomCodeUrgentScopeRequired (403): intercom:urgent is needed as well (ErrUrgentScopeRequired).
	IntercomCodeUrgentScopeRequired = "urgent_scope_required"
	// IntercomCodeUrgentBusy (429): the urgent pool is full (ErrUrgentBusy).
	IntercomCodeUrgentBusy = "intercom_urgent_busy"
	// IntercomCodeSkillNotInstalled (409): no Intercom skill on the hub (ErrSkillNotInstalled).
	IntercomCodeSkillNotInstalled = "skill_not_installed"
	// IntercomCodeNotASpeaker (422): a connection that cannot play speech (ErrNotASpeaker).
	IntercomCodeNotASpeaker = "not_a_speaker"
	// IntercomCodeReservedName (422): a name the platform reserves (ErrReservedName).
	IntercomCodeReservedName = "reserved_name"
	// IntercomCodeHubLocked (403): the hub is locked (ErrHubLocked).
	IntercomCodeHubLocked = "hub_locked"
	// IntercomCodeImpersonationNotAllowed (403): an impersonated session cannot announce (ErrImpersonationNotAllowed).
	IntercomCodeImpersonationNotAllowed = "impersonation_not_allowed"
	// IntercomCodeTooManyTargets (422): more than 50 speakers resolved (ErrTooManyTargets).
	IntercomCodeTooManyTargets = "too_many_targets"
	// IntercomCodeManagedClient (403): the platform-managed Intercom client (ErrManagedClient).
	IntercomCodeManagedClient = "managed_client"
	// IntercomCodeNotCancellable (409): only a "sending" announcement can be cancelled (ErrNotCancellable).
	IntercomCodeNotCancellable = "not_cancellable"
	// IntercomCodeHubUnreachable (502): the hub did not take the announcement (ErrHubUnreachable).
	IntercomCodeHubUnreachable = "hub_unreachable"
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
	// MaxIntercomTargetNames is the most names in one speakers, rooms or groups
	// list, and MaxIntercomTargetNameLength the longest name in one.
	MaxIntercomTargetNames      = 50
	MaxIntercomTargetNameLength = 64
	// MaxIdempotencyKeyLength is the longest Idempotency-Key the API takes.
	MaxIdempotencyKeyLength = 128
	// MaxIntercomTimezoneLength is the longest IANA timezone name the API takes.
	MaxIntercomTimezoneLength = 64
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
// one is still repeating; "cancelled" after CancelAnnouncement. Open: a value
// the API adds later is kept as sent.
type AnnouncementStatus string

const (
	AnnouncementCompleted AnnouncementStatus = "completed"
	AnnouncementPartial   AnnouncementStatus = "partial"
	AnnouncementFailed    AnnouncementStatus = "failed"
	AnnouncementSending   AnnouncementStatus = "sending"
	AnnouncementCancelled AnnouncementStatus = "cancelled"
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

// IntercomUnavailableReason says why intercom cannot run on a hub: "platform"
// (the platform's own switch is off; it wins over every other reason), "plan",
// "kid_hub" or "public_hub". Open.
type IntercomUnavailableReason string

const (
	IntercomUnavailablePlatform IntercomUnavailableReason = "platform"
	IntercomUnavailablePlan     IntercomUnavailableReason = "plan"
	IntercomUnavailableKidHub   IntercomUnavailableReason = "kid_hub"
	IntercomUnavailablePublic   IntercomUnavailableReason = "public_hub"
)

// IntercomSettings is a hub's intercom switch.
type IntercomSettings struct {
	// Enabled is true only when the hub's switch is on AND UnavailableReason is "".
	Enabled     bool `json:"enabled"`
	PlanAllowed bool `json:"plan_allowed"`
	// UnavailableReason is "" when intercom can run. Until the platform
	// operator turns intercom on it is "platform" for every hub.
	UnavailableReason IntercomUnavailableReason `json:"unavailable_reason"`
}

// IntercomQuietHours are HH:MM quiet hours in an IANA timezone. Quiet hours
// that are switched on (IntercomDNDUpdate.Enabled true) need a real Timezone
// and a Start that differs from the End: the API refuses a window it would
// have to guess the zone of (422), so the SDK refuses it first. A Timezone
// that is given is checked even when DND is off.
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

// IntercomSpeaker is a connection of the hub that may be spoken to.
//
// Kind is the connection kind ("voice_satellite", "web_chat", "developer",
// "embedded", "home_assistant"; any other reads "voice_satellite", the API's
// own rule). CanPlay is the EFFECTIVE answer to "can this connection play
// speech at all": true by default for a voice_satellite and web_chat, false
// for home_assistant, developer and embedded unless its owner declared
// spec.intercom.can_play: true on the connection. One that cannot is listed
// (grey it out) but cannot be given Receive true (ErrNotASpeaker, 422), is
// never part of all, a room or a group, and naming it in an announcement is
// refused. CanPlay is declared on the connection (PATCH /v1/clients/{id}),
// never on the speaker. A missing can_play reads true (the API's default);
// only an explicit false is false.
//
// PolicySynced is set on the answer to UpdateIntercomSpeaker: false while the
// hub may hold an out-of-date consent map (the push failed and is retried in
// the background), so show "Changes may take a moment to reach the home". nil
// when the API did not say (a list item: the list has its own).
type IntercomSpeaker struct {
	ClientID     string         `json:"client_id"`
	Slug         string         `json:"slug"`
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	CanPlay      bool           `json:"can_play"`
	Online       bool           `json:"online"`
	LastSeenAt   *string        `json:"last_seen_at"`
	Intercom     IntercomPolicy `json:"intercom"`
	PolicySynced *bool          `json:"policy_synced,omitempty"`
}

// UnmarshalJSON reads a speaker with the API's defaults: Kind is
// "voice_satellite" unless the API names another, CanPlay is true unless it
// sent an explicit false, and PolicySynced stays nil unless it sent a boolean.
func (s *IntercomSpeaker) UnmarshalJSON(data []byte) error {
	type plain IntercomSpeaker // no methods: no recursion
	aux := struct {
		*plain
		Kind         json.RawMessage `json:"kind"`
		CanPlay      json.RawMessage `json:"can_play"`
		PolicySynced json.RawMessage `json:"policy_synced"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	s.Kind = "voice_satellite"
	var kind string
	if json.Unmarshal(aux.Kind, &kind) == nil && kind != "" {
		s.Kind = kind
	}
	s.CanPlay = strings.TrimSpace(string(aux.CanPlay)) != "false"
	s.PolicySynced = nil
	switch strings.TrimSpace(string(aux.PolicySynced)) {
	case "true":
		synced := true
		s.PolicySynced = &synced
	case "false":
		synced := false
		s.PolicySynced = &synced
	}
	return nil
}

// IntercomSpeakerList is every connection of the hub, as speakers (the
// platform's own Intercom client is not listed).
//
// PolicySynced is false while the hub may hold an out-of-date consent map (a
// push failed and is retried in the background): a speaker whose Receive was
// just turned off can still be spoken to by a routine until it clears. Show a
// quiet "Changes may take a moment to reach the home". true when nothing is
// pending, and while intercom is off; nil when the API did not say.
// UnavailableReason is the same value as IntercomSettings.UnavailableReason (a
// list read while intercom is off platform-wide says "platform").
type IntercomSpeakerList struct {
	Items             []IntercomSpeaker
	PolicySynced      *bool
	UnavailableReason IntercomUnavailableReason
}

// IntercomDelivery is what happened for one speaker.
//
// DeliveryDelivered means the hub handed the speech to that speaker's
// connection; there is NO playback receipt, so it does not prove the sound
// came out or that anybody heard it. Only DeliveryAcked (an urgent
// announcement somebody answered) is a person's answer, and DeliveryFailed is
// not proof that nothing was spoken. Detail is a fixed code (see
// KnownDeliveryDetail) or "" when the API sent none; a code this SDK does not
// know is kept as sent.
type IntercomDelivery struct {
	ClientID string         `json:"client_id"`
	Slug     string         `json:"slug"`
	Name     string         `json:"name"`
	Status   DeliveryStatus `json:"status"`
	At       *string        `json:"at"`
	Detail   string         `json:"detail"`
}

var knownDeliveryDetails = map[string]bool{
	"disabled": true, "sender_not_allowed": true, "rate_limited": true, "invalid_text": true,
	"text_too_long": true, "invalid_priority": true, "too_many_targets": true, "no_targets": true,
	"internal_error": true, "duplicate_id": true, "busy": true, "not_running": true, "invalid_id": true,
	"policy_changed": true, "dnd_unreadable": true, "cancelled": true, "intercom_disabled": true,
	"intercom_disabled_platform": true, "no_result": true, "unknown_status": true,
	"no_matching_speakers": true, "hub_no_response": true, "hub_unreachable": true, "hub_not_ready": true,
	"hub_transport_error": true, "skill_not_installed": true, "stopped": true,
}

// KnownDeliveryDetail reports whether detail is one of the fixed codes an
// IntercomDelivery.Detail can carry (never free text). Treat a code that is
// not known as opaque: it is kept as sent.
func KnownDeliveryDetail(detail string) bool { return knownDeliveryDetails[detail] }

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
	// HubConfirmed is set only on the answer to CancelAnnouncement: false means
	// the hub could not confirm the cancel (the record is "cancelled" anyway),
	// so speech already queued on the hub may still play. nil on every other
	// answer, and when the API sent something that is not a boolean.
	HubConfirmed *bool `json:"hub_confirmed,omitempty"`
	// Replayed is true when this answer is the STORED announcement for an
	// IdempotencyKey the hub had seen before (the API's response header
	// Idempotency-Replayed: true): nothing new was sent, whatever its status.
	// It is false on a first answer and on every read, and is kept after Wait.
	// Do not clear the text a person typed, or say "sent", on a replayed answer
	// without saying nothing new was sent. AckedBy is the display name of a
	// client of the hub, or "unknown".
	Replayed bool `json:"-"`
}

// UnmarshalJSON reads an announcement and keeps HubConfirmed only when the API
// sent a real boolean: any other value leaves it nil rather than failing the
// whole answer.
func (a *Announcement) UnmarshalJSON(data []byte) error {
	type plain Announcement // no methods: no recursion
	aux := struct {
		*plain
		HubConfirmed json.RawMessage `json:"hub_confirmed"`
	}{plain: (*plain)(a)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	a.HubConfirmed = nil
	switch strings.TrimSpace(string(aux.HubConfirmed)) {
	case "true":
		confirmed := true
		a.HubConfirmed = &confirmed
	case "false":
		confirmed := false
		a.HubConfirmed = &confirmed
	}
	return nil
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
	// Text is 1-280 characters, trimmed, with no "<" or ">", no control,
	// bidirectional or other invisible format characters (the zero-width
	// joiner and non-joiner excepted), no private-use characters and no line
	// or paragraph separators (see AnnouncementText). It is spoken aloud in a
	// home.
	Text    string
	Targets IntercomTargets
	// Priority is "normal" when empty.
	Priority IntercomPriority
	// Repeat is for urgent announcements only.
	Repeat *IntercomRepeat
	// Lang is a language tag such as "en-US" or "fr"; "" leaves it to the hub.
	Lang string
	// IdempotencyKey is sent as Idempotency-Key when set (at most 128
	// printable ASCII characters); it is never generated. Use a fresh unique
	// key (a UUID) per announcement you mean to send: the same key replays the
	// stored result for 24 hours and never sends again (Announcement.Replayed),
	// a replayed "failed" announcement stays "failed", and to send again on
	// purpose you need a new key.
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

// String redacts the text: it prints how many characters it has, never the
// characters, so a %v, %+v or %s in a log line does not carry what is spoken
// aloud in a home. Every other field is shown.
func (o AnnounceOptions) String() string {
	return fmt.Sprintf(
		"AnnounceOptions{Text:<%d chars> Targets:%+v Priority:%q Repeat:%s Lang:%q IdempotencyKey:%q Wait:%t WaitTimeout:%s PollInterval:%s}",
		utf8.RuneCountInString(o.Text), o.Targets, o.Priority, redactedRepeat(o.Repeat), o.Lang, o.IdempotencyKey,
		o.Wait, o.WaitTimeout, o.PollInterval,
	)
}

// GoString keeps %#v from printing the text, as String does.
func (o AnnounceOptions) GoString() string { return "thalovant." + o.String() }

// Format redacts the text under every verb, including %d, %x and %q, which
// would otherwise print the struct's fields themselves.
func (o AnnounceOptions) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = fmt.Fprint(f, o.GoString())
		return
	}
	_, _ = fmt.Fprint(f, o.String())
}

// LogValue keeps log/slog handlers, including the JSON one, from printing the text.
func (o AnnounceOptions) LogValue() slog.Value { return slog.StringValue(o.String()) }

func redactedRepeat(r *IntercomRepeat) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Times:%d IntervalSeconds:%d}", r.Times, r.IntervalSeconds)
}

// IntercomDNDUpdate is a do-not-disturb change. The API replaces the whole
// do-not-disturb setting with it and stores a missing enabled as false, so
// Enabled is required (nil is refused) rather than left to default to off:
// an update that only carries QuietHours must still say whether DND is on.
// Enabled true needs QuietHours with a Timezone that is a valid IANA name (at
// most 64 characters, not padded) and a Start that differs from the End; the
// SDK refuses anything else before a request is made.
type IntercomDNDUpdate struct {
	Enabled    *bool
	QuietHours *IntercomQuietHours
}

// IntercomSpeakerUpdate is a change to a speaker's policy: only what is set is
// sent, and only these four fields exist (send only what changed). At least
// one field is required.
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

// AnnouncementText returns the text as it is sent, or an error before any
// request: trimmed, 1-280 characters, and the API's own rule (one rule
// everywhere): no "<" or ">" (a speaker's text-to-speech may read them as
// markup), no control characters (a newline or a tab included), no
// bidirectional overrides or isolates or other invisible format characters
// (the zero-width joiner and non-joiner, which scripts and emoji need, are
// allowed), no invalid UTF-8 (the API refuses lone surrogates), no private-use
// characters and no line or paragraph separators. The text is refused rather
// than rewritten, and an error never echoes it.
//
// Unlike the API, and unlike the Python and Node SDKs, this SDK does not
// NFC-normalise the text first (the Go standard library has no normaliser and
// golang.org/x/text is not a dependency): the 280 characters are counted as
// sent, so a text written in decomposed form can be refused here that the API
// would accept once composed; the API normalises what it receives.
func AnnouncementText(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", intercomErr("an announcement's text must be valid UTF-8 (no lone surrogates)")
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", intercomErr("an announcement's text must not be empty")
	}
	if utf8.RuneCountInString(trimmed) > MaxAnnouncementText {
		return "", intercomErr("an announcement's text is at most %d characters", MaxAnnouncementText)
	}
	for _, r := range trimmed {
		if r == '<' || r == '>' {
			return "", intercomErr("an announcement's text must not contain markup characters (< or >)")
		}
		if r != 0x200c && r != 0x200d && unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Co, unicode.Zl, unicode.Zp) {
			return "", intercomErr("an announcement's text must not contain control, bidirectional, format or private-use characters")
		}
	}
	return trimmed, nil
}

// intercomLang is the language tag the API takes: "en", "en-US", "zho-Hant".
var intercomLang = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)

// intercomZoneShape is the shape of an IANA timezone name, used only where
// this machine has no timezone database.
var intercomZoneShape = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)

// loadLocation is time.LoadLocation, replaceable in tests.
var loadLocation = time.LoadLocation

// validIntercomTimezone checks an IANA timezone name the API will accept: at
// most 64 characters, not padded, and one the timezone database knows. Where
// this machine has no timezone database at all (a minimal container, Windows
// without Go's time/tzdata) every name would look unknown, so only the shape
// is checked and the API decides; that is probed with a well-known zone.
func validIntercomTimezone(name, what string) error {
	bad := intercomErr("%s must be an IANA timezone name", what)
	if name == "" || len(name) > MaxIntercomTimezoneLength || name != strings.TrimSpace(name) || name == "Local" {
		return bad
	}
	if _, err := loadLocation(name); err == nil {
		return nil
	}
	if _, err := loadLocation("America/New_York"); err != nil && intercomZoneShape.MatchString(name) {
		return nil // no timezone database here: the shape is all that can be checked
	}
	return bad
}

// validIdempotencyKey checks an Idempotency-Key: 1-128 printable ASCII characters.
func validIdempotencyKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return intercomErr("IdempotencyKey must not be blank")
	}
	if len(key) > MaxIdempotencyKeyLength {
		return intercomErr("IdempotencyKey is at most %d characters", MaxIdempotencyKeyLength)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < ' ' || key[i] > '~' {
			return intercomErr("IdempotencyKey must be printable ASCII")
		}
	}
	return nil
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
		if utf8.RuneCountInString(item) > MaxIntercomTargetNameLength {
			return nil, intercomErr("a target in %s is at most %d characters", kind, MaxIntercomTargetNameLength)
		}
	}
	if len(items) > MaxIntercomTargetNames {
		// 50 names of any kind; "at most 50 speakers" is the API's rule for explicit speakers.
		return nil, intercomErr("an announcement names at most %d %s", MaxIntercomTargetNames, kind)
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
		if !intercomLang.MatchString(opts.Lang) {
			return nil, intercomErr(`lang must be a language tag such as "en-US" or "fr"`)
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
		hours := update.DND.QuietHours
		if hours != nil {
			if !intercomClock.MatchString(hours.Start) || !intercomClock.MatchString(hours.End) {
				return nil, intercomErr("quiet_hours start and end must be HH:MM")
			}
			if hours.Timezone != nil {
				if err := validIntercomTimezone(*hours.Timezone, "quiet_hours timezone"); err != nil {
					return nil, err
				}
			}
			dnd["quiet_hours"] = map[string]any{"start": hours.Start, "end": hours.End, "timezone": hours.Timezone}
		}
		if *update.DND.Enabled {
			// The API refuses an enabled window with no zone (UTC would put a
			// Toronto bedroom's night in the afternoon) or with start == end.
			if hours == nil || hours.Timezone == nil {
				return nil, intercomErr("dnd quiet_hours with a timezone is required when dnd is enabled")
			}
			if hours.Start == hours.End {
				return nil, intercomErr("dnd quiet_hours start and end must differ when dnd is enabled")
			}
		}
		body["dnd"] = dnd
	}
	if len(body) == 0 {
		return nil, intercomErr("pass at least one of receive, room, groups or dnd")
	}
	return body, nil
}

// validIntercomID refuses, before any request, an id (hub, client or
// announcement) that cannot be one path segment: an empty or blank one, "."
// or ".." (dot segments, which a URL resolver collapses into another path),
// and any id holding "/", "?", "#", a backslash or a control character.
func validIntercomID(value, name string) error {
	if strings.TrimSpace(value) == "" {
		return intercomErr("%s is required", name)
	}
	if value == "." || value == ".." || strings.ContainsFunc(value, func(r rune) bool {
		return r == '/' || r == '?' || r == '#' || r == '\\' || unicode.IsControl(r)
	}) {
		return intercomErr("%s is not a valid id: it is a dot segment or holds '/', '?', '#', a backslash or a control character", name)
	}
	return nil
}

// intercomPath is the escaped path under a hub's intercom routes; an invalid
// id is refused before anything is sent.
func intercomPath(hubID string, rest ...string) (string, error) {
	if err := validIntercomID(hubID, "a hub id"); err != nil {
		return "", err
	}
	return strings.Join(append([]string{"/v1/hubs/" + url.PathEscape(hubID) + "/intercom"}, rest...), "/"), nil
}

func intercomSegment(value, name string) (string, error) {
	if err := validIntercomID(value, name); err != nil {
		return "", err
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
// (owner, manager or administrator of the hub), not intercom:send, and a
// session that is not an impersonation (ErrImpersonationNotAllowed). A plan
// without intercom is ErrPlan; a kid or public hub is ErrIntercomUnavailable
// (422 intercom_unavailable, APIError.Reason); a locked hub is ErrHubLocked.
// While the platform switch is off even enabled=false is
// ErrIntercomPlatformDisabled (503 intercom_disabled_platform: nothing was
// changed), and a platform that is not configured is ErrIntercomNotConfigured.
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

// ListIntercomSpeakers lists every connection of the hub as a speaker, with
// presence, policy, Kind and CanPlay. Requires intercom:read.
//
// The answer's PolicySynced is false while the hub may hold an out-of-date
// consent map, and UnavailableReason is the hub's ("platform" when intercom is
// off platform-wide: reads still work). A connection that cannot play speech
// (CanPlay false) is listed so it can be greyed out.
func (c *ControlPlane) ListIntercomSpeakers(ctx context.Context, hubID string) (IntercomSpeakerList, error) {
	path, err := intercomPath(hubID, "speakers")
	if err != nil {
		return IntercomSpeakerList{}, err
	}
	var envelope struct {
		Items             *[]IntercomSpeaker        `json:"items"`
		PolicySynced      json.RawMessage           `json:"policy_synced"`
		UnavailableReason IntercomUnavailableReason `json:"unavailable_reason"`
	}
	if err := c.controlInto(ctx, http.MethodGet, path, nil, nil, &envelope); err != nil {
		return IntercomSpeakerList{}, err
	}
	if envelope.Items == nil {
		return IntercomSpeakerList{}, &APIError{Detail: "unexpected intercom speaker listing shape"}
	}
	list := IntercomSpeakerList{Items: *envelope.Items, UnavailableReason: envelope.UnavailableReason}
	switch strings.TrimSpace(string(envelope.PolicySynced)) {
	case "true":
		synced := true
		list.PolicySynced = &synced
	case "false":
		synced := false
		list.PolicySynced = &synced
	}
	return list, nil
}

// UpdateIntercomSpeaker changes a speaker's policy: only what update sets is
// sent. A speaker is spoken to only when Receive is true. Only the four fields
// of IntercomSpeakerUpdate exist; DND enabled needs quiet hours with a valid
// IANA timezone and a Start that differs from the End, checked before anything
// is sent. Receive true on a connection that cannot play speech is
// ErrNotASpeaker (422 not_a_speaker, APIError.SpeakerKind). The answer's
// PolicySynced is false while the hub may hold an out-of-date consent map
// ("changes may take a moment to reach the home"). Requires hubs:write (owner,
// manager or administrator; a plain member and an impersonated session are
// refused).
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

// Announce says opts.Text aloud on a hub's speakers. The API keeps only a
// keyed hash of the text and its length. Not a certified safety system.
//
// A normal announcement waits up to ten seconds for the hub and returns its
// result. An urgent one with Repeat returns at once as "sending"; set Wait to
// poll GetAnnouncement until it completes (or WaitTimeout passes, which
// returns the latest answer, still "sending").
//
// An urgent announcement (and so every Repeat) needs the scope intercom:urgent
// IN ADDITION to intercom:send; without it Announce returns
// ErrUrgentScopeRequired (a kind of ErrAuth) before anything is spent. At most
// two urgent announcements repeat on one hub at a time (the platform has a pool
// too): the next is ErrUrgentBusy (429, APIError.UrgentScope, RetryAfter) with
// nothing stored; cancel one that is running, or wait. An urgent announcement
// on a hub whose runtime has no active Intercom skill is ErrSkillNotInstalled.
// All, a room or a group that resolves to more than 50 speakers is
// ErrTooManyTargets (APIError.Matched), and a named connection that cannot
// play speech is ErrNotASpeaker (APIError.NotSpeakers).
//
// Idempotency. Set IdempotencyKey to a fresh unique key (a UUID) for each
// announcement you mean to send, and reuse it only to repeat the same attempt
// after a network drop or timeout. The key is scoped to the hub and lasts 24
// hours: the same key replays the stored announcement (the response header
// Idempotency-Replayed: true) and never sends again, whatever its status; a
// replayed "failed" announcement stays "failed". The answer says so:
// Announcement.Replayed is true (do not tell the person it was sent anew). A
// key is at most 128 printable ASCII characters, and the same key with a
// different request is a 409. To send the same text again on purpose, use a
// new key. After ErrHubUnreachable (502 hub_unreachable)
// read the announcement first (APIError.AnnouncementID, then GetAnnouncement)
// before sending again: the hub may have spoken it. A constant key such as
// "dinner" would silence every later announcement for 24 hours.
//
// Timeout. The API waits up to ten seconds for the hub. This SDK sets no
// request timeout of its own (the HTTPClient's, if any, applies): a context
// or HTTPClient.Timeout shorter than twenty seconds can report a spoken
// announcement as failed, so give Announce at least 20 s. If it returns an
// error with no announcement, the outcome is unknown: call again with the same
// key to find out.
//
// Wait needs intercom:read as well as intercom:send: the polls are
// GetAnnouncement. A poll can fail after the announcement was accepted (a
// token with only intercom:send is refused at the first poll; a 429; the
// network). Announce then returns the last accepted announcement, whose ID
// lets the caller poll it later, together with the poll's error: a non-nil
// error with a non-empty Announcement.ID means the announcement exists; calling
// Announce again with the same IdempotencyKey replays it instead of speaking it
// again, and one without a key may speak it twice. WaitTimeout and
// PollInterval are checked before anything is sent, and WaitTimeout bounds
// each poll, so a stalled read cannot carry the wait past it.
//
// Requires intercom:send and the owner, a manager or an administrator acting as
// themselves (an impersonated session is ErrImpersonationNotAllowed, a locked
// hub ErrHubLocked). Intercom off for the hub (or for the platform, or on a kid
// or public hub) is ErrIntercomDisabled and its kinds, a platform that is not
// configured ErrIntercomNotConfigured, an unreachable hub (no answer in 5 s)
// ErrHubUnreachable (the text may or may not have been spoken; the same
// IdempotencyKey replays the stored failed record and does not retry), a rate
// limit ErrRateLimited (RetryAfter says how long), a plan without intercom
// ErrPlan (HTTP 402, or 403 plan_limit). DeliveryDelivered means the hub handed
// the speech to the speaker's connection: there is no playback receipt, and
// DeliveryFailed is not proof that nothing was spoken.
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
		if err := validIdempotencyKey(opts.IdempotencyKey); err != nil {
			return Announcement{}, err
		}
		headers = map[string]string{"Idempotency-Key": opts.IdempotencyKey}
	}
	var announcement Announcement
	responseHeader, err := c.controlIntoHeader(ctx, http.MethodPost, path, body, headers, &announcement)
	if err != nil {
		return Announcement{}, err
	}
	if announcement.ID == "" || announcement.Status == "" {
		return Announcement{}, &APIError{Detail: "unexpected announcement shape"}
	}
	announcement.Replayed = strings.EqualFold(strings.TrimSpace(responseHeader.Get("Idempotency-Replayed")), "true")
	replayed := announcement.Replayed
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
		latest.Replayed = replayed
		announcement = latest
	}
	return announcement, nil
}

// controlIntoHeader is controlInto, and the response's headers beside the body
// (for Idempotency-Replayed).
func (c *ControlPlane) controlIntoHeader(ctx context.Context, method, path string, payload map[string]any, headers map[string]string, out any) (http.Header, error) {
	status, raw, header, err := c.send(ctx, method, path, payload, headers, true)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, apiErrorFromResponse(status, raw, header)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, &APIError{StatusCode: status, Detail: "invalid JSON response"}
	}
	return header, nil
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

// CancelAnnouncement stops an urgent announcement that is still repeating
// (POST .../announcements/{id}/cancel) and returns it with status "cancelled".
// Only while it is "sending": one that already completed, failed or was
// cancelled is ErrNotCancellable (409 not_cancellable). The API asks the hub
// to stop repeating; a hub that cannot confirm still leaves the record
// "cancelled" and the answer has HubConfirmed false: then speech already
// queued on the hub may still play, and the hub's own stop paths (the hub
// switch, the platform kill switch, somebody acknowledging) remain. Requires
// intercom:send (owner or admin of the hub); a hub-scoped token is honoured.
// Stopping speech is never refused: it works while the platform switch is off,
// while the hub's switch is off, on a plan that lost intercom, and in an
// impersonated session (the answer then has HubConfirmed false when no hub was
// called). ErrNotCancellable carries APIError.AnnouncementStatus, the current
// status: refresh the result instead of showing an error. Not a certified
// safety system.
func (c *ControlPlane) CancelAnnouncement(ctx context.Context, hubID, announcementID string) (Announcement, error) {
	segment, err := intercomSegment(announcementID, "an announcement id")
	if err != nil {
		return Announcement{}, err
	}
	path, err := intercomPath(hubID, "announcements", segment, "cancel")
	if err != nil {
		return Announcement{}, err
	}
	var announcement Announcement
	if err := c.controlInto(ctx, http.MethodPost, path, nil, nil, &announcement); err != nil {
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
