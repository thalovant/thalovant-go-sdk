package thalovant

import "testing"

// The tag to retry a listing with, when the hub had nothing under the one
// asked for. These answers are CLDR's, not this SDK's, and must match the
// Python reference: a managed port that disagreed about which language to
// retry would list a different hub.
func TestUsualForm(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"en-CA", "en-us", true},
		{"en-AT", "en-us", true},
		{"fr-BE", "fr-fr", true},
		{"pt-AO", "pt-br", true},
		{"pt-PT", "pt-br", true},
		{"de-AT", "de-de", true},
		// Already the usual form: false rather than the same tag, so a hub
		// that answered is never asked twice.
		{"en-US", "", false},
		{"en-us", "", false},
		{"fr-FR", "", false},
		// maximizeLanguage does not fail on a language it has never heard of:
		// it walks down to "und" and takes the root locale's region, so "zzz"
		// would come back "zzz-us" without the likely-table check.
		{"zzz", "", false},
		{"", "", false},
		{"xx-YY", "", false},
	} {
		got, ok := UsualForm(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("UsualForm(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
