package thalovant

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
