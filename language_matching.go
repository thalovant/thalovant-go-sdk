package thalovant

// OVOS-INTENT-2 matching policy with langcodes 3.5.1 CLDR data.
// Tuple distance adapted from langcodes (MIT); see LICENSE-langcodes.
import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
)

//go:embed data/language-matching.json
var matchingJSON []byte

type languageMatchingData struct {
	Likely         map[string]string         `json:"likely"`
	Languages      map[string]string         `json:"languages"`
	Scripts        map[string]string         `json:"scripts"`
	Territories    map[string]string         `json:"territories"`
	DefaultScripts map[string]string         `json:"default_scripts"`
	Macrolanguages map[string]string         `json:"macrolanguages"`
	Distances      map[string]map[string]int `json:"distances"`
	Regions        map[string][]string       `json:"regions"`
}

var matching = func() languageMatchingData {
	var value languageMatchingData
	if err := json.Unmarshal(matchingJSON, &value); err != nil {
		panic(err)
	}
	return value
}()

type languageTag struct{ language, script, region string }

var scriptTag = regexp.MustCompile(`^[a-z]{4}$`)
var regionTag = regexp.MustCompile(`^(?:[a-z]{2}|[0-9]{3})$`)

func parseLanguage(value string, aliases bool) languageTag {
	value = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "_", "-"))
	if aliases && matching.Languages[value] != "" {
		value = strings.ToLower(matching.Languages[value])
	}
	tokens := strings.Split(value, "-")
	primary := tokens[0]
	if primary == "" {
		primary = "und"
	}
	base := languageTag{language: primary}
	if aliases && matching.Languages[primary] != "" {
		base = parseLanguage(matching.Languages[primary], false)
	}
	onlyScript := true
	for _, token := range tokens[1:] {
		if !scriptTag.MatchString(token) {
			onlyScript = false
		}
		if len(token) == 1 {
			break
		}
		if scriptTag.MatchString(token) {
			base.script = matching.Scripts[token]
			if base.script == "" {
				base.script = strings.ToUpper(token[:1]) + token[1:]
			}
		} else if regionTag.MatchString(token) {
			base.region = matching.Territories[token]
			if base.region == "" {
				base.region = strings.ToUpper(token)
			}
		}
	}
	if base.script == matching.DefaultScripts[base.language] {
		base.script = ""
	}
	if base.language == "pt" && base.script == "" && base.region == "" && onlyScript {
		base.region = "PT"
	}
	return base
}
func maximizeLanguage(value languageTag) languageTag {
	if value.language == "und" && value.script == "" && value.region == "" {
		return languageTag{"und", "Zzzz", "ZZ"}
	}
	if macro := matching.Macrolanguages[value.language]; macro != "" {
		value.language = macro
	}
	join := func(parts ...string) string {
		var found []string
		for _, p := range parts {
			if p != "" {
				found = append(found, p)
			}
		}
		return strings.Join(found, "-")
	}
	probes := []string{join(value.language, value.script, value.region), join(value.language, value.region), join(value.language, value.script), value.language}
	if value.script != "" {
		probes = append(probes, "und-"+value.script)
	}
	probes = append(probes, "und")
	for _, probe := range probes {
		if likely := matching.Likely[probe]; likely != "" {
			parts := strings.Split(likely, "-")
			if value.language == "und" {
				value.language = parts[0]
			}
			if value.script == "" {
				value.script = parts[1]
			}
			if value.region == "" {
				value.region = parts[2]
			}
			return value
		}
	}
	panic("invalid embedded likely-subtag data")
}
func languageDistance(wanted, candidate string) int {
	a, b := maximizeLanguage(parseLanguage(wanted, true)), maximizeLanguage(parseLanguage(candidate, true))
	lookup := func(from, to string, fallback int) int {
		if value, ok := matching.Distances[from][to]; ok {
			return value
		}
		return fallback
	}
	result := 0
	if a.language != b.language {
		result += lookup(a.language, b.language, 80)
	}
	pairA, pairB := a.language+"_"+a.script, b.language+"_"+b.script
	if a.script != b.script {
		result += lookup(pairA, pairB, 50)
	}
	if a.region == b.region {
		return result
	}
	regionDistance := 4
	inside := func(group, region string) bool {
		for _, r := range matching.Regions[group] {
			if r == region {
				return true
			}
		}
		return false
	}
	if pairA == pairB {
		switch {
		case a.language == "ar":
			if inside("MAGHREB", a.region) != inside("MAGHREB", b.region) {
				regionDistance = 5
			}
		case a.language == "en":
			if (a.region == "GB" && !inside("US", b.region)) || (!inside("US", a.region) && b.region == "GB") {
				regionDistance = 3
			} else if inside("US", a.region) != inside("US", b.region) {
				regionDistance = 5
			}
		case inside("LATIN_AMERICA", a.region) && b.region == "419":
			regionDistance = 1
		case a.language == "es" || a.language == "pt":
			if inside("AMERICAS", a.region) != inside("AMERICAS", b.region) {
				regionDistance = 5
			}
		case pairA == "zh_Hant":
			if inside("CNSAR", a.region) != inside("CNSAR", b.region) {
				regionDistance = 5
			}
		}
	}
	return result + regionDistance
}

// ClosestLanguage returns the nearest OVOS-compatible registration (maximum
// distance ten). Equal distances preserve the caller's registration order.
// UsualForm is the form a language is usually written in, when that differs
// from tag: "en-CA" and "en-AT" both to "en-us", "fr-BE" to "fr-fr", "pt-AO"
// to "pt-br", from CLDR's likely subtags. The second return is false when
// there is nothing different to try, so a caller can tell "already the usual
// form" from "no idea".
//
// Listing and asking do not agree about languages, and this closes the gap. A
// hub matches an utterance to the closest language it knows, so a phone set to
// "en-CA" is understood by skills registered under "en-US"; its manifest is
// keyed by exact tag, so the same hub lists nothing for "en-CA".
//
// Lower case, because that is how skills register and how the manifest is
// keyed: an exact lookup with BCP47's "en-US" finds nothing.
func UsualForm(tag string) (string, bool) {
	if strings.TrimSpace(tag) == "" {
		return "", false
	}
	base := parseLanguage(tag, true).language
	// "und" is the tag for "no idea", and parseLanguage produces it for
	// anything it cannot read. CLDR's guess for an unknown language is
	// English, so without this a blank tag lists a hub in a language nobody
	// asked for.
	if base == "" || base == "und" {
		return "", false
	}
	// maximizeLanguage does not fail on a language it has never heard of: it
	// walks its probes down to "und" and takes the root locale's region, so
	// "zzz" comes back "zzz-us". Round-tripping the tag does not catch that,
	// because the unknown language is carried through unchanged. A direct
	// entry in the likely table is what says CLDR has heard of this language.
	if matching.Likely[base] == "" {
		return "", false
	}
	likely := maximizeLanguage(languageTag{language: base})
	usual := likely.language
	if likely.region != "" {
		usual = likely.language + "-" + likely.region
	}
	usual = strings.ToLower(usual)
	// Byte comparison, NOT sameLanguage. They are not the same test, and the difference is the whole point: the canonical spelling is en-US, the manifest is keyed en-us, and sameLanguage calls those equal -- so the retry that exists for exactly this case suppressed itself.
	if usual == strings.TrimSpace(tag) {
		return "", false
	}
	return usual, true
}

func ClosestLanguage(target string, available []string) (string, bool) {
	best, minimum := "", int(^uint(0)>>1)
	for _, candidate := range available {
		if score := languageDistance(target, candidate); score < minimum {
			best, minimum = candidate, score
		}
	}
	return best, minimum <= 10
}
