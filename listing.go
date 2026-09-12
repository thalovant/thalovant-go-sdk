package thalovant

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
)

// ListingLanguage contains optional canonical thalovant-languages listing rules.
type ListingLanguage struct {
	TrailingWords         []string          `json:"trailing_words,omitempty"`
	QuestionOpeners       []string          `json:"question_openers,omitempty"`
	QuestionWordsAnywhere []string          `json:"question_words_anywhere,omitempty"`
	QuestionPatterns      []string          `json:"question_patterns,omitempty"`
	WrittenForms          map[string]string `json:"written_forms,omitempty"`
	SlotExamples          map[string]string `json:"slot_examples,omitempty"`
}

// ListingData is a complete language data tree, rather than an overlay.
type ListingData struct {
	SentenceEnds string                     `json:"sentence_ends"`
	Languages    map[string]ListingLanguage `json:"languages"`
}

// ListingRules snapshots language rules; its methods may be used concurrently.
// Construct with nil for bare rendering without language data.
type ListingRules struct {
	available bool
	data      ListingData
	tags      []string
	patterns  map[string][]*regexp2.Regexp
	written   map[string][]writtenRule
}
type writtenRule struct {
	pattern     *regexp2.Regexp
	replacement string
}

//go:embed data/listing.json
var listingJSON []byte

//go:embed data/unicode-upper.json
var unicodeUpperJSON []byte
var upperExpansions = func() map[string]string {
	var data struct {
		Expansions map[string]string `json:"expansions"`
	}
	if err := json.Unmarshal(unicodeUpperJSON, &data); err != nil {
		panic(err)
	}
	return data.Expansions
}()
var defaultListing = func() *ListingRules {
	var data ListingData
	if err := json.Unmarshal(listingJSON, &data); err != nil {
		panic(err)
	}
	rules, err := NewListingRules(&data)
	if err != nil {
		panic(err)
	}
	return rules
}()

// Python's Unicode word boundary excludes combining marks; regexp2's native
// boundary follows .NET. Translate boundaries without changing escaped literals.
func listingPattern(expression string) string {
	const boundary = `(?:(?<![\p{L}\p{N}_])(?=[\p{L}\p{N}_])|(?<=[\p{L}\p{N}_])(?![\p{L}\p{N}_]))`
	var out strings.Builder
	inClass := false
	for i := 0; i < len(expression); i++ {
		char := expression[i]
		if char == '\\' && i+1 < len(expression) {
			i++
			next := expression[i]
			if next == 'b' && !inClass {
				out.WriteString(boundary)
			} else {
				out.WriteByte(char)
				out.WriteByte(next)
			}
		} else {
			if char == '[' {
				inClass = true
			}
			if char == ']' {
				inClass = false
			}
			out.WriteByte(char)
		}
	}
	return out.String()
}
func compileListingPattern(expression string, ignoreCase bool) (*regexp2.Regexp, error) {
	options := []regexp2.CompileOption{regexp2.OptionMaxBacktrackingStackSize(65536)}
	if ignoreCase {
		options = append(options, regexp2.IgnoreCase)
	}
	pattern, err := regexp2.Compile(listingPattern(expression), options...)
	if err != nil {
		return nil, err
	}
	pattern.MatchTimeout = 100 * time.Millisecond
	return pattern, nil
}

// NewListingRules validates patterns before publishing an immutable snapshot.
// Invalid patterns return an error. Runtime backtracking is bounded by a 100ms
// per-pattern deadline and a 65536-entry stack; Asks reports matching failures.
func NewListingRules(data *ListingData) (*ListingRules, error) {
	rules := &ListingRules{available: data != nil, patterns: map[string][]*regexp2.Regexp{}, written: map[string][]writtenRule{}}
	if data == nil {
		return rules, nil
	}
	serialized, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(serialized, &rules.data); err != nil {
		return nil, err
	}
	for tag, language := range rules.data.Languages {
		rules.tags = append(rules.tags, tag)
		for _, expression := range language.QuestionPatterns {
			pattern, err := compileListingPattern(expression, true)
			if err != nil {
				return nil, fmt.Errorf("listing %s question pattern: %w", tag, err)
			}
			rules.patterns[tag] = append(rules.patterns[tag], pattern)
		}
		// Sorting makes map-backed written forms deterministic across processes.
		keys := make([]string, 0, len(language.WrittenForms))
		for key := range language.WrittenForms {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, word := range keys {
			pattern, err := compileListingPattern(`\b`+regexp.QuoteMeta(word)+`\b`, false)
			if err != nil {
				return nil, err
			}
			rules.written[tag] = append(rules.written[tag], writtenRule{pattern, language.WrittenForms[word]})
		}
	}
	sort.Strings(rules.tags)
	return rules, nil
}

// Available reports whether this snapshot contains language data.
func (r *ListingRules) Available() bool { return r.available }
func (r *ListingRules) tag(lang string) string {
	if lang == "" {
		return ""
	}
	tag, ok := ClosestLanguage(lang, r.tags)
	if !ok {
		return ""
	}
	return tag
}

// LanguageData returns a copy of the closest locale's rules.
func (r *ListingRules) LanguageData(lang string) ListingLanguage {
	data, _ := json.Marshal(r.data.Languages[r.tag(lang)])
	var copy ListingLanguage
	_ = json.Unmarshal(data, &copy)
	return copy
}
func (r *ListingRules) wordSet(lang string, selector func(ListingLanguage) []string) map[string]bool {
	tags := r.tags
	if lang != "" {
		tags = []string{r.tag(lang)}
	}
	words := map[string]bool{}
	for _, tag := range tags {
		for _, word := range selector(r.data.Languages[tag]) {
			words[strings.ToLower(word)] = true
		}
	}
	return words
}

// Dangling reports a locale's trailing prefix waiting for an entity.
func (r *ListingRules) Dangling(text, lang string) bool {
	words := strings.Fields(strings.TrimRight(text, r.data.SentenceEnds+" "))
	if len(words) == 0 {
		return false
	}
	return r.wordSet(lang, func(d ListingLanguage) []string { return d.TrailingWords })[strings.ToLower(words[len(words)-1])]
}

// Asks recognizes questions and reports bounded regex failures to the caller.
func (r *ListingRules) Asks(text, lang string) (bool, error) {
	for _, pattern := range r.patterns[r.tag(lang)] {
		matched, err := pattern.MatchString(text)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	var words []string
	for _, word := range strings.Fields(text) {
		if word = strings.ToLower(strings.Trim(word, ",;:!?.’'\"()")); word != "" {
			words = append(words, word)
		}
	}
	if len(words) == 0 {
		return false, nil
	}
	if r.wordSet(lang, func(d ListingLanguage) []string { return d.QuestionOpeners })[words[0]] {
		return true, nil
	}
	anywhere := r.wordSet(lang, func(d ListingLanguage) []string { return d.QuestionWordsAnywhere })
	for _, word := range words {
		if anywhere[word] {
			return true, nil
		}
	}
	return false, nil
}

// AsSentence capitalizes and punctuates a phrase using known locale rules.
// Unknown rules, dangling prefixes and regex failures leave a bare line.
func (r *ListingRules) AsSentence(text, lang string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	_, size := utf8.DecodeRuneInString(text)
	first := text[:size]
	upper := strings.ToUpper(first)
	if expanded := upperExpansions[first]; expanded != "" {
		upper = expanded
	}
	text = upper + text[size:]
	last, _ := utf8.DecodeLastRuneInString(text)
	if strings.ContainsRune(r.data.SentenceEnds, last) || r.Dangling(text, lang) {
		return text
	}
	tag := r.tag(lang)
	data := r.data.Languages[tag]
	if lang == "" || len(data.QuestionOpeners)+len(data.QuestionWordsAnywhere)+len(data.QuestionPatterns) == 0 {
		return text
	}
	for _, rule := range r.written[tag] {
		next, err := rule.pattern.ReplaceFunc(text, func(regexp2.Match) string { return rule.replacement }, 0, -1)
		if err != nil {
			return text
		}
		text = next
	}
	asks, err := r.Asks(text, lang)
	if err != nil {
		return text
	}
	if asks {
		return text + "?"
	}
	return text + "."
}

// AsSentence uses the bundled canonical locale data.
func AsSentence(text, lang string) string { return defaultListing.AsSentence(text, lang) }

// Speakable renders a pattern using locale slot examples and caller overrides.
func (r *ListingRules) Speakable(pattern string, slots map[string]string, lang string) string {
	merged := map[string]string{}
	for k, v := range r.data.Languages[r.tag(lang)].SlotExamples {
		merged[k] = v
	}
	for k, v := range slots {
		merged[k] = v
	}
	return Speakable(pattern, merged)
}

// SpeakableWithLanguage uses bundled locale examples without changing the
// original two-argument Speakable function's calling convention.
func SpeakableWithLanguage(pattern string, slots map[string]string, lang string) string {
	return defaultListing.Speakable(pattern, slots, lang)
}

// Rank keeps whole phrases ahead of prefixes and slots, preferring fuller
// wording up to eight words and shorter strings after that.
func (r *ListingRules) Rank(phrases []string, lang string) []string {
	type ranked struct {
		text string
		key  [4]int
	}
	rows := make([]ranked, len(phrases))
	for i, text := range phrases {
		key := [4]int{0, 0, -min(len(strings.Fields(text)), 8), utf8.RuneCountInString(text)}
		if r.Dangling(text, lang) {
			key[0] = 1
		}
		if strings.Contains(text, "{") {
			key[1] = 1
		}
		rows[i] = ranked{text, key}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		for i := range rows[a].key {
			if rows[a].key[i] != rows[b].key[i] {
				return rows[a].key[i] < rows[b].key[i]
			}
		}
		return false
	})
	result := make([]string, len(rows))
	for i, row := range rows {
		result[i] = row.text
	}
	return result
}
