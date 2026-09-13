package thalovant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type managedFixture struct {
	closeError    error
	phase         TransportConnectionPhase
	closed, asked int
	ask           func() (Reply, error)
	events        eventStream[Event]
}

func (f *managedFixture) ConnectionInfo() TransportConnectionInfo {
	return TransportConnectionInfo{Phase: f.phase}
}
func (f *managedFixture) Close(context.Context) error { f.closed++; return f.closeError }
func (f *managedFixture) SubscribeEvents(capacity int) *Subscription[Event] {
	return f.events.subscribe(capacity)
}
func (f *managedFixture) AskWithOptions(context.Context, string, AskOptions) (Reply, error) {
	f.asked++
	if f.ask != nil {
		return f.ask()
	}
	return Reply{Text: "ok"}, nil
}
func (f *managedFixture) Emit(context.Context, string, Data, Context) error { return nil }
func TestManagedSessionNeverReplaysAndRestoresStream(t *testing.T) {
	first, second := &managedFixture{phase: ConnectionReady}, &managedFixture{phase: ConnectionReady}
	first.ask = func() (Reply, error) { return Reply{}, ErrConnection }
	attempts := 0
	session, err := NewHubSession(func(context.Context) (HubSessionClient, error) {
		attempts++
		if attempts == 1 {
			return first, nil
		}
		return second, nil
	}, DefaultHubSessionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	sub := session.SubscribeEvents(4)
	defer sub.Close()
	ctx := context.Background()
	if _, err = session.Ask(ctx, "action", AskOptions{}); !errors.Is(err, ErrConnection) {
		t.Fatal(err)
	}
	if attempts != 1 || first.asked != 1 || first.closed != 1 || session.Held() {
		t.Fatal("ambiguous action replayed or client leaked")
	}
	if _, err = session.Ask(ctx, "status", AskOptions{}); err != nil {
		t.Fatal(err)
	}
	second.events.publish(Event{Name: "event"})
	select {
	case event := <-sub.C:
		if event.Name != "event" {
			t.Fatal(event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscription not restored")
	}
	if err = session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = session.Ask(ctx, "again", AskOptions{}); err == nil {
		t.Fatal("closed session reopened")
	}
	if _, open := <-sub.C; open || sub.Err() == nil {
		t.Fatal("closed subscription did not report termination")
	}
}
func TestManagedSessionCloseWaitsForAdmittedCall(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	client := &managedFixture{phase: ConnectionReady}
	client.ask = func() (Reply, error) {
		close(entered)
		<-release
		if client.closed != 0 {
			t.Error("closed active client")
		}
		return Reply{Text: "ok"}, nil
	}
	session, _ := NewHubSession(func(context.Context) (HubSessionClient, error) { return client, nil }, DefaultHubSessionPolicy())
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := session.Ask(ctx, "hi", AskOptions{}); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	go func() {
		defer wg.Done()
		if err := session.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	close(release)
	wg.Wait()
	if client.closed != 1 {
		t.Fatal(client.closed)
	}
}
func TestManagedSessionRetryPolicy(t *testing.T) {
	p := DefaultHubSessionPolicy()
	wait := p.Retry
	var ladder []time.Duration
	for i := 0; i < 6; i++ {
		ladder = append(ladder, wait)
		wait = p.NextWait(wait)
	}
	if !reflect.DeepEqual(ladder, []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 120 * time.Second, 120 * time.Second}) {
		t.Fatal(ladder)
	}
	attempts := 0
	session, _ := NewHubSession(func(context.Context) (HubSessionClient, error) { attempts++; return nil, ErrConnection }, p)
	now := time.Unix(100, 0)
	session.clock = func() time.Time { return now }
	_, _ = session.Ask(context.Background(), "hi", AskOptions{})
	if session.RetryAt() != now.Add(10*time.Second) || session.RetryWait() != 20*time.Second {
		t.Fatal("incorrect backoff")
	}
	if session.Warm(context.Background()) {
		t.Fatal("warm ignored retry window")
	}
	_, _ = session.Ask(context.Background(), "again", AskOptions{})
	if attempts != 2 {
		t.Fatal("foreground obeyed unattended backoff")
	}
}
func sampleInventory() Inventory {
	return Inventory{HubID: "hub-1", HubName: "Example", Source: "hub", GeneratedAt: "2026-09-13", Notes: []string{"from the hub"}, Skills: []Skill{{ID: "weather", Title: "Weather", Locales: []string{"en-US", "fr-FR"}, Intents: []Intent{{ID: "a", Name: "current.weather", SkillID: "weather", Engine: "padatious", Phrases: map[string][]string{"en-US": {"weather in", "what is the weather"}, "fr-FR": {"quel temps fait-il"}}}}}}}
}
func TestPresentableInventoryAndSafeCache(t *testing.T) {
	value := sampleInventory()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	again, err := InventoryFromJSON(raw)
	if err != nil || !again.Live() || !again.HasPhrases() {
		t.Fatal(err, again)
	}
	if got := again.Intents()[0].Examples("en-GB", 1); !reflect.DeepEqual(got, []string{"what is the weather"}) {
		t.Fatal(got)
	}
	if got := again.Skills[0].Speaks("fr-CA"); got == nil || !*got {
		t.Fatal(got)
	}
	if (Skill{}).Speaks("fr") != nil {
		t.Fatal("unknown locale collapsed")
	}
	directory := t.TempDir()
	cache := NewInventoryCache(directory)
	outside := filepath.Join(directory, "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "intents-safe.partial")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); cache.Store("safe", value) }()
	}
	wg.Wait()
	if cache.Load("safe") == nil {
		t.Fatal("concurrent cache corrupted")
	}
	contents, _ := os.ReadFile(outside)
	if string(contents) != "keep" {
		t.Fatal("followed scratch symlink")
	}
	for _, key := range []string{"x/../../outside", "x\\outside"} {
		cache.Store(key, value)
		if cache.Load(key) != nil {
			t.Fatal("accepted unsafe key")
		}
	}
	path, _ := cache.Path("safe")
	old := time.Now().Add(-2 * time.Hour)
	if os.Chtimes(path, old, old) != nil {
		t.Fatal("mtime")
	}
	if cache.Load("safe") != nil {
		t.Fatal("expired cache accepted")
	}
}
func TestInventoryDisplayHelpers(t *testing.T) {
	if FriendlyTitle("thalovant-skill-custos-query.thalovant") != "Custos Query" {
		t.Fatal("title")
	}
	kind, token := CommonAffix([]string{"current.weather", "high_low.weather"})
	if kind != "suffix" || token != "weather" || StripAffix("high_low.weather", kind, token) != "high low" {
		t.Fatal(kind, token)
	}
	names := []string{"intent10", "intent2", "Intent1"}
	sort.Slice(names, func(i, j int) bool { return CompareNames(names[i], names[j]) < 0 })
	if !reflect.DeepEqual(names, []string{"Intent1", "intent2", "intent10"}) {
		t.Fatal(names)
	}
}

func TestInventorySharedReference(t *testing.T) {
	raw, err := os.ReadFile("testdata/inventory-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		CacheKey  string `json:"cache_key"`
		Inventory json.RawMessage
		Examples  []struct {
			Language string
			Limit    int
			Expected []string
		}
		Speaks []struct {
			Language string
			Expected bool
		}
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if InventoryCacheKey("hub", "") != data.CacheKey {
		t.Fatal("default cache key drift")
	}
	inventory, err := InventoryFromJSON(data.Inventory)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range data.Examples {
		got := inventory.Intents()[0].Examples(row.Language, row.Limit)
		if !reflect.DeepEqual(got, row.Expected) {
			t.Fatalf("%+v: %v", row, got)
		}
	}
	for _, row := range data.Speaks {
		if got := inventory.Skills[0].Speaks(row.Language); got == nil || *got != row.Expected {
			t.Fatalf("%+v: %v", row, got)
		}
	}
	if inventory.Skills[1].Speaks("en") != nil {
		t.Fatal("unknown locales were inferred")
	}
}

func TestOriginCancellationDoesNotFallbackOrStartCooldown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origin := OriginPreference{Address: "10.0.0.2", HandshakeTimeout: 1500 * time.Millisecond, Cooldown: 5 * time.Minute}
	attempts := 0
	build := func(context.Context, OriginAttempt) (HubSessionClient, error) {
		attempts++
		cancel()
		return nil, errors.New("interrupted dial")
	}
	_, err := origin.Connect(ctx, build, OriginAttempt{Host: "hub.example", ConnectTimeout: 12 * time.Second})
	if !errors.Is(err, context.Canceled) || attempts != 1 || origin.CoolingDown() {
		t.Fatalf("cancellation: %v attempts=%d cooldown=%v", err, attempts, origin.CoolingDown())
	}
	_, err = origin.Connect(ctx, build, OriginAttempt{Host: "hub.example"})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatal("pre-cancelled call invoked factory")
	}
}

func TestInventoryKeysHashFullNormalizedHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	first, second := strings.Repeat("a", 40)+"one.example", strings.Repeat("a", 40)+"two.example"
	raw, _ := json.Marshal(map[string]string{"default_master": first})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	key := InventoryCacheKey("hub", path)
	if IdentityHost(path) != first {
		t.Fatal("scheme-less host missing")
	}
	raw, _ = json.Marshal(map[string]string{"default_master": second})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if InventoryCacheKey("hub", path) == key {
		t.Fatal("truncated hosts shared a key")
	}
}

func TestManagedSessionRetainsFailedCleanupBeforeReplacement(t *testing.T) {
	original, cleanup := errors.New("response lost"), errors.New("disconnect unconfirmed")
	first := &managedFixture{phase: ConnectionReady, closeError: cleanup, ask: func() (Reply, error) { return Reply{}, original }}
	builds := 0
	session, err := NewHubSession(func(context.Context) (HubSessionClient, error) {
		builds++
		if builds == 1 {
			return first, nil
		}
		return &managedFixture{phase: ConnectionReady}, nil
	}, DefaultHubSessionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = session.Ask(ctx, "action", AskOptions{})
	if !errors.Is(err, original) || !errors.Is(err, cleanup) {
		t.Fatal(err)
	}
	_, err = session.Ask(ctx, "status", AskOptions{})
	if !errors.Is(err, cleanup) || builds != 1 {
		t.Fatal("replaced transport before cleanup", err, builds)
	}
	first.closeError = nil
	if _, err = session.Ask(ctx, "status", AskOptions{}); err != nil || builds != 2 {
		t.Fatal(err, builds)
	}
	if err = session.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
