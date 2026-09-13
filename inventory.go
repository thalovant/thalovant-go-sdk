package thalovant

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const InventoryCacheVersion = 1
const InventoryCacheTTL = time.Hour
const HubSource = "hub"

type Intent struct {
	// Languages preserves phrase-map order when no requested language is supplied.
	Languages []string            `json:"languages"`
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	SkillID   string              `json:"skill_id"`
	Engine    string              `json:"engine"`
	Phrases   map[string][]string `json:"phrases"`
}

func (i Intent) Examples(language string, limit int) []string {
	tags := i.phraseTags()
	var pool []string
	if language != "" {
		if tag, ok := ClosestLanguage(language, tags); ok {
			pool = i.Phrases[tag]
		}
	} else if len(tags) > 0 {
		pool = i.Phrases[tags[0]]
	}
	if limit <= 0 {
		return append([]string{}, pool...)
	}
	ranked := DefaultListing().Rank(pool, language)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

func (i Intent) phraseTags() []string {
	seen := map[string]bool{}
	tags := []string{}
	for _, tag := range i.Languages {
		if _, ok := i.Phrases[tag]; ok && !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	rest := []string{}
	for tag := range i.Phrases {
		if !seen[tag] {
			rest = append(rest, tag)
		}
	}
	sort.Strings(rest)
	return append(tags, rest...)
}
func (i *Intent) UnmarshalJSON(raw []byte) error {
	type wire Intent
	var value wire
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var fields struct {
		Phrases json.RawMessage `json:"phrases"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if len(fields.Phrases) > 0 && string(fields.Phrases) != "null" {
		decoder := json.NewDecoder(bytes.NewReader(fields.Phrases))
		if _, err := decoder.Token(); err != nil {
			return err
		}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			tag, ok := key.(string)
			if !ok {
				return errors.New("invalid phrase language")
			}
			value.Languages = append(value.Languages, tag)
			var skip json.RawMessage
			if err := decoder.Decode(&skip); err != nil {
				return err
			}
		}
	}
	*i = Intent(value)
	return nil
}
func (i Intent) MarshalJSON() ([]byte, error) {
	var phrases bytes.Buffer
	phrases.WriteByte('{')
	for n, tag := range i.phraseTags() {
		if n > 0 {
			phrases.WriteByte(',')
		}
		key, _ := json.Marshal(tag)
		phrases.Write(key)
		phrases.WriteByte(':')
		texts := i.Phrases[tag]
		if texts == nil {
			texts = []string{}
		}
		raw, err := json.Marshal(texts)
		if err != nil {
			return nil, err
		}
		phrases.Write(raw)
	}
	phrases.WriteByte('}')
	return json.Marshal(struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		SkillID   string          `json:"skill_id"`
		Engine    string          `json:"engine"`
		Phrases   json.RawMessage `json:"phrases"`
		Languages []string        `json:"languages"`
	}{i.ID, i.Name, i.SkillID, i.Engine, phrases.Bytes(), i.phraseTags()})
}

type Skill struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Locales []string `json:"locales"`
	Intents []Intent `json:"intents"`
}

func (s Skill) MarshalJSON() ([]byte, error) {
	type wire Skill
	value := wire(s)
	if value.Locales == nil {
		value.Locales = []string{}
	}
	if value.Intents == nil {
		value.Intents = []Intent{}
	}
	return json.Marshal(value)
}
func (s Skill) DeclaresLocales() bool { return len(s.Locales) > 0 }
func (s Skill) Speaks(language string) *bool {
	if len(s.Locales) == 0 {
		return nil
	}
	_, found := ClosestLanguage(language, s.Locales)
	return &found
}

type Inventory struct {
	CacheVersion int      `json:"cache_version"`
	HubID        string   `json:"hub_id"`
	HubName      string   `json:"hub_name"`
	Source       string   `json:"source"`
	GeneratedAt  string   `json:"generated_at"`
	Notes        []string `json:"notes"`
	Skills       []Skill  `json:"skills"`
}

func (i Inventory) Live() bool { return i.Source == HubSource || i.Source == "ovos-runtime" }
func (i Inventory) Intents() []Intent {
	out := []Intent{}
	for _, s := range i.Skills {
		out = append(out, s.Intents...)
	}
	return out
}
func (i Inventory) HasPhrases() bool {
	for _, v := range i.Intents() {
		if len(v.Phrases) > 0 {
			return true
		}
	}
	return false
}
func (i Inventory) MarshalJSON() ([]byte, error) {
	type wire Inventory
	value := wire(i)
	value.CacheVersion = InventoryCacheVersion
	if value.Notes == nil {
		value.Notes = []string{}
	}
	if value.Skills == nil {
		value.Skills = []Skill{}
	}
	return json.Marshal(value)
}
func InventoryFromJSON(raw []byte) (Inventory, error) {
	if err := validateInventoryJSON(raw); err != nil {
		return Inventory{}, err
	}
	var value Inventory
	err := json.Unmarshal(raw, &value)
	if err != nil {
		return value, err
	}
	if value.CacheVersion != InventoryCacheVersion {
		return Inventory{}, errors.New("not a current inventory cache")
	}
	return value, nil
}
func LanguagesPresent(i Inventory) []string {
	seen := map[string]bool{}
	for _, s := range i.Skills {
		for _, lang := range s.Locales {
			seen[lang] = true
		}
		for _, intent := range s.Intents {
			for lang := range intent.Phrases {
				seen[lang] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for lang := range seen {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}
func FriendlyTitle(id string) string {
	name := id
	for _, prefix := range []string{"thalovant-skill-", "ovos-skill-", "skill-"} {
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
			break
		}
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(name))
	if name == "" {
		return id
	}
	beginning := true
	return strings.Map(func(r rune) rune {
		if !unicode.IsLetter(r) {
			beginning = true
			return r
		}
		if beginning {
			beginning = false
			return unicode.ToTitle(r)
		}
		return unicode.ToLower(r)
	}, name)
}
func inventoryTokens(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '_' })
}
func Humanize(name string) string { return strings.Join(inventoryTokens(name), " ") }
func CommonAffix(names []string) (kind, token string) {
	if len(names) < 2 {
		return "", ""
	}
	parts := make([][]string, len(names))
	for i, name := range names {
		parts[i] = inventoryTokens(name)
		if len(parts[i]) < 2 {
			return "", ""
		}
	}
	suffix, prefix := true, true
	for _, part := range parts {
		suffix = suffix && part[len(part)-1] == parts[0][len(parts[0])-1]
		prefix = prefix && part[0] == parts[0][0]
	}
	if suffix {
		return "suffix", parts[0][len(parts[0])-1]
	}
	if prefix {
		return "prefix", parts[0][0]
	}
	return "", ""
}
func StripAffix(name, kind, token string) string {
	if kind == "" {
		return name
	}
	parts := inventoryTokens(name)
	if len(parts) == 0 {
		return name
	}
	if kind == "suffix" && parts[len(parts)-1] == token {
		parts = parts[:len(parts)-1]
	} else if kind == "prefix" && parts[0] == token {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return name
	}
	return strings.Join(parts, " ")
}

var naturalChunks = regexp.MustCompile(`[0-9]+|[^0-9]+`)

// CompareNames gives a deterministic natural order without integer overflow.
func CompareNames(left, right string) int {
	a, b := naturalChunks.FindAllString(strings.ToLower(left), -1), naturalChunks.FindAllString(strings.ToLower(right), -1)
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		nx, ny := x[0] >= '0' && x[0] <= '9', y[0] >= '0' && y[0] <= '9'
		if nx && ny {
			x = strings.TrimLeft(x, "0")
			y = strings.TrimLeft(y, "0")
			if len(x) != len(y) {
				return len(x) - len(y)
			}
		} else if nx != ny {
			if nx {
				return -1
			}
			return 1
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

type InventoryCache struct {
	Directory string
	TTL       time.Duration
}

func NewInventoryCache(directory string) *InventoryCache {
	if directory == "" {
		base, err := os.UserCacheDir()
		if err == nil {
			directory = filepath.Join(base, "thalovant")
		}
	}
	return &InventoryCache{Directory: directory, TTL: InventoryCacheTTL}
}
func IdentityHost(identityPath string) string {
	raw, err := os.ReadFile(identityPath)
	if err != nil {
		return ""
	}
	var value struct {
		Master string `json:"default_master"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return HubHostname(value.Master)
}
func InventoryCacheKey(mode, identityPath string) string {
	host := IdentityHost(identityPath)
	if host == "" {
		host = "local"
	}
	hostname := host
	host = regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(host, "-")
	if len(host) > 40 {
		host = host[:40]
	}
	sum := sha256.Sum256([]byte(mode + "|" + identityPath + "|" + hostname))
	return mode + "-" + host + "-" + hex.EncodeToString(sum[:])[:8]
}

var inventoryKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,160}$`)

func (c *InventoryCache) Path(key string) (string, error) {
	if c.Directory == "" || !inventoryKeyPattern.MatchString(key) {
		return "", errors.New("invalid inventory cache path")
	}
	return filepath.Join(c.Directory, "intents-"+key+".json"), nil
}
func (c *InventoryCache) Load(key string) *Inventory {
	path, err := c.Path(key)
	if err != nil {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || c.TTL < 0 || time.Since(info.ModTime()) > c.TTL {
		return nil
	}
	const limit = 8 * 1024 * 1024
	if info.Size() > limit {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(raw) > limit {
		return nil
	}
	value, err := InventoryFromJSON(raw)
	if err != nil {
		return nil
	}
	return &value
}
func (c *InventoryCache) Store(key string, inventory Inventory) {
	path, err := c.Path(key)
	if err != nil {
		return
	}
	if os.MkdirAll(c.Directory, 0700) != nil {
		return
	}
	scratch, err := os.CreateTemp(c.Directory, ".intents-*.partial")
	if err != nil {
		return
	}
	defer os.Remove(scratch.Name())
	err = json.NewEncoder(scratch).Encode(inventory)
	if err == nil {
		err = scratch.Sync()
	}
	closed := scratch.Close()
	if err == nil && closed == nil {
		_ = os.Rename(scratch.Name(), path)
	}
}

// Validate required cache fields before decoding into zero-valued Go structs.
func validateInventoryJSON(raw []byte) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	fail := errors.New("invalid inventory cache shape")
	record := func(value any, names ...string) (map[string]any, bool) {
		m, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		for _, name := range names {
			if _, ok := m[name].(string); !ok {
				return nil, false
			}
		}
		return m, true
	}
	stringsList := func(value any) bool {
		list, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range list {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	}
	top, ok := record(value, "hub_id", "hub_name", "source", "generated_at")
	if !ok || !stringsList(top["notes"]) {
		return fail
	}
	skills, ok := top["skills"].([]any)
	if !ok {
		return fail
	}
	for _, rawSkill := range skills {
		skill, ok := record(rawSkill, "id", "title")
		if !ok || !stringsList(skill["locales"]) {
			return fail
		}
		intents, ok := skill["intents"].([]any)
		if !ok {
			return fail
		}
		for _, rawIntent := range intents {
			intent, ok := record(rawIntent, "id", "name", "skill_id", "engine")
			if !ok {
				return fail
			}
			phrases, ok := intent["phrases"].(map[string]any)
			if !ok {
				return fail
			}
			for _, texts := range phrases {
				if !stringsList(texts) {
					return fail
				}
			}
			if languages, exists := intent["languages"]; exists && !stringsList(languages) {
				return fail
			}
		}
	}
	return nil
}
