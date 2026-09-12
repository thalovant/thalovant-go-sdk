package thalovant

import (
	"math"
	"strconv"
	"strings"
)

type ClientContextOptions struct {
	UserID       string
	UserName     string
	AuthToken    string
	AuthProvider string
	AuthClaims   map[string]any
	Roles        []string
	Platform     string
	Source       string
	Destination  string
	Channel      string
	DeviceID     string
	Locale       string
	Metadata     map[string]any
	SessionID    string
}

func BuildClientContext(base Context, opts ClientContextOptions) Context {
	ctx := MergeContext(base, nil)
	if opts.UserID != "" || opts.UserName != "" || len(opts.Roles) > 0 {
		user := mapValue(ctx["user"])
		if opts.UserID != "" {
			user["id"] = opts.UserID
			if _, ok := ctx["user_id"]; !ok {
				ctx["user_id"] = opts.UserID
			}
		}
		if opts.UserName != "" {
			user["name"] = opts.UserName
			if _, ok := ctx["user_name"]; !ok {
				ctx["user_name"] = opts.UserName
			}
		}
		if len(opts.Roles) > 0 {
			user["roles"] = append([]string(nil), opts.Roles...)
			if _, ok := ctx["roles"]; !ok {
				ctx["roles"] = append([]string(nil), opts.Roles...)
			}
		}
		ctx["user"] = user
	}
	if opts.AuthToken != "" || opts.AuthProvider != "" || len(opts.AuthClaims) > 0 {
		auth := mapValue(ctx["auth"])
		if opts.AuthToken != "" {
			auth["token"] = opts.AuthToken
			if _, ok := ctx["auth_token"]; !ok {
				ctx["auth_token"] = opts.AuthToken
			}
		}
		if opts.AuthProvider != "" {
			auth["provider"] = opts.AuthProvider
		}
		if len(opts.AuthClaims) > 0 {
			auth["claims"] = cloneMap(opts.AuthClaims)
		}
		ctx["auth"] = auth
	}
	setDefault(ctx, "platform", opts.Platform)
	setDefault(ctx, "source", opts.Source)
	setDefault(ctx, "destination", opts.Destination)
	setDefault(ctx, "channel", opts.Channel)
	setDefault(ctx, "locale", opts.Locale)
	if opts.DeviceID != "" {
		device := mapValue(ctx["device"])
		device["id"] = opts.DeviceID
		if opts.Platform != "" {
			device["platform"] = opts.Platform
		}
		ctx["device"] = device
	}
	if len(opts.Metadata) > 0 {
		metadata := mapValue(ctx["metadata"])
		for key, value := range opts.Metadata {
			metadata[key] = value
		}
		ctx["metadata"] = metadata
	}
	if opts.SessionID != "" {
		session := sessionFromContext(ctx)
		session["session_id"] = opts.SessionID
		if _, ok := ctx["session_id"]; !ok {
			ctx["session_id"] = opts.SessionID
		}
		ctx["session"] = session
	}
	return ctx
}

func setDefault(ctx Context, key string, value string) {
	if value == "" {
		return
	}
	if _, ok := ctx[key]; !ok {
		ctx[key] = value
	}
}

func cloneMap(values map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range values {
		out[key] = value
	}
	return out
}

// RequestContextOptions carries per-request hints read by OVOS.
type RequestContextOptions struct {
	STTLang  string
	Pipeline []string
	Location map[string]any
}

// RequestContext copies the context and its session before applying nonempty hints.
func RequestContext(base Context, opts RequestContextOptions) Context {
	result := MergeContext(base, nil)
	var stages []string
	for _, stage := range opts.Pipeline {
		if stage = strings.TrimSpace(stage); stage != "" {
			stages = append(stages, stage)
		}
	}
	if len(stages) > 0 {
		session := sessionFromContext(result)
		session["pipeline"] = stages
		result["session"] = session
	}
	if lang := strings.TrimSpace(opts.STTLang); lang != "" {
		result["stt_lang"] = lang
	}
	if len(opts.Location) > 0 {
		result["location"] = cloneMap(opts.Location)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// LocationOptions accepts numeric or string coordinates. A city is required.
type LocationOptions struct {
	City, Region, Country, Timezone string
	Latitude, Longitude             any
}

func BuildLocation(opts LocationOptions) map[string]any {
	city := strings.TrimSpace(opts.City)
	if city == "" {
		return nil
	}
	result := map[string]any{"city": city}
	if v := strings.TrimSpace(opts.Region); v != "" {
		result["region"] = v
	}
	if v := strings.TrimSpace(opts.Country); v != "" {
		result["country_code"] = strings.ToUpper(v)
	}
	if v := strings.TrimSpace(opts.Timezone); v != "" {
		result["timezone"] = map[string]any{"code": v}
	}
	coordinate := func(value any) float64 {
		switch v := value.(type) {
		case float64:
			return v
		case float32:
			return float64(v)
		case int:
			return float64(v)
		case string:
			n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err == nil {
				return n
			}
		}
		return math.NaN()
	}
	lat, lon := coordinate(opts.Latitude), coordinate(opts.Longitude)
	if (lat != 0 || lon != 0) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 {
		result["coordinate"] = map[string]any{"latitude": lat, "longitude": lon}
	}
	return result
}
