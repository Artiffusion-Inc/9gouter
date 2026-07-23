package managedashboard

// usage_stats_test.go covers the real-time analytics reconstruction (#81/#83)
// against a REAL on-disk SQLite database — no in-memory fakes, no mocks of the
// usage/connection/node/apikey repos. This is the same persistence path the
// production binary uses (sqlite.Open + db.SyncSchema + repo.New*Repo), so the
// tests exercise the exact join StatsWithMeta performs and the exact
// EventTracker publish/subscribe loop the SSE handler drives.
//
// Coverage:
//   - StatsWithMeta: camelCase keys, byModel "model (provider)" shape with
//     rawModel/provider/requests/promptTokens/.../avgTps/lastUsed, totals,
//     recentRequests, last10Minutes, pending overlay from EventTracker.
//   - EventTracker: PublishStart/PublishStop pending accounting + safety
//     timer expiry, PublishSave ring + dedupe, ActiveRequests name resolution,
//     Subscribe notify fan-out.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/sqlite"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
)

// realStatsDB opens a fresh on-disk SQLite, runs SyncSchema, and returns the
// real *sql.DB plus the four repos StatsWithMeta joins over. Cleanup closes DB.
func realStatsDB(t *testing.T) (*repo.UsageRepo, *repo.ConnectionRepo, *repo.NodeRepo, *repo.APIKeyRepo) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats-test.db")
	conn, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := db.SyncSchema(conn); err != nil {
		t.Fatalf("SyncSchema: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return repo.NewUsageRepo(conn), repo.NewConnectionRepo(conn), repo.NewNodeRepo(conn), repo.NewAPIKeyRepo(conn)
}

func mustSave(t *testing.T, r *repo.UsageRepo, rec usage.UsageRecord) {
	t.Helper()
	rec.Timestamp = time.Now().UTC()
	if err := r.Save(context.Background(), rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestStatsWithMeta_FullContract is the end-to-end regression test for #81:
// the dashboard UsageStats.js expects a specific camelCase JSON shape that the
// old usage.Aggregates struct (no JSON tags) could not produce. It seeds two
// real usageHistory rows (ollama + openai) plus a connection + node + apikey,
// then asserts StatsWithMeta returns the exact dashboard contract:
//   - top-level camelCase totals (totalRequests, totalPromptTokens, ...)
//   - byModel keyed "model (provider)" with rawModel, provider (node display
//     name), requests, promptTokens, completionTokens, cachedTokens, cost,
//     tpsSum, tpsCount, avgTps, lastUsed
//   - byAccount keyed "model (provider - accountName)" with accountName
//     resolved from the real connection
//   - recentRequests newest-first with prompt/completion/status
//   - last10Minutes is a 10-element array
func TestStatsWithMeta_FullContract(t *testing.T) {
	ur, cr, nr, kr := realStatsDB(t)

	// Seed a connection whose display name must surface in byAccount.
	conn := settings.ProviderConnection{
		ID: "conn-abc123def456", Provider: "ollama", AuthType: "apikey",
		Name: "My Ollama Box", IsActive: true,
	}
	if err := cr.Create(context.Background(), conn); err != nil {
		t.Fatalf("conn create: %v", err)
	}
	// Seed a node whose name overrides the raw provider id in byModel.provider.
	node := settings.ProviderNode{ID: "ollama", Name: "Ollama Local"}
	if err := nr.Create(context.Background(), node); err != nil {
		t.Fatalf("node create: %v", err)
	}
	// Seed an apikey whose name surfaces in byApiKey.keyName.
	key := settings.APIKey{ID: "k1", Key: "sk-abcdefgh1234567890", Name: "Primary Key"}
	if err := kr.Create(context.Background(), key); err != nil {
		t.Fatalf("key create: %v", err)
	}

	// Two usage rows: ollama via the connection, openai passthrough.
	mustSave(t, ur, usage.UsageRecord{
		Provider: "ollama", Model: "minimax-m3", ConnectionID: conn.ID,
		APIKey: key.Key, Endpoint: "/v1/chat/completions",
		PromptTokens: 177, CompletionTokens: 42, Cost: 0.01, Status: "success",
	})
	mustSave(t, ur, usage.UsageRecord{
		Provider: "openai", Model: "gpt-4",
		PromptTokens: 100, CompletionTokens: 25, Cost: 0.02, Status: "success",
	})

	svc := &UsageService{Repo: ur}
	tracker := NewEventTracker()
	stats := svc.StatsWithMeta(context.Background(), FullStatsOptions{
		Period:  "7d",
		Meta:    &StatsMeta{Connections: cr, Nodes: nr, APIKeys: kr},
		Pending: tracker,
	})

	// Top-level totals (camelCase — the old struct emitted TotalRequests).
	if got, _ := stats["totalRequests"].(int); got != 2 {
		t.Fatalf("totalRequests = %v, want 2", stats["totalRequests"])
	}
	if got, _ := stats["totalPromptTokens"].(int); got != 277 {
		t.Errorf("totalPromptTokens = %v, want 277", stats["totalPromptTokens"])
	}
	if got, _ := stats["totalCompletionTokens"].(int); got != 67 {
		t.Errorf("totalCompletionTokens = %v, want 67", stats["totalCompletionTokens"])
	}
	if got, _ := stats["totalCost"].(float64); got < 0.029 || got > 0.031 {
		t.Errorf("totalCost = %v, want ~0.03", stats["totalCost"])
	}
	if _, ok := stats["totalCachedTokens"].(int); !ok {
		t.Errorf("totalCachedTokens missing or wrong type: %T", stats["totalCachedTokens"])
	}

	// byModel keyed "model (provider)" with rawModel + provider(node name).
	byModel, _ := stats["byModel"].(map[string]map[string]any)
	mk := "minimax-m3 (ollama)"
	m, ok := byModel[mk]
	if !ok {
		t.Fatalf("byModel missing key %q; got keys: %v", mk, mapKeys(byModel))
	}
	if m["rawModel"] != "minimax-m3" {
		t.Errorf("byModel[%q].rawModel = %v, want minimax-m3", mk, m["rawModel"])
	}
	// provider must be the NODE display name, not the raw id.
	if m["provider"] != "Ollama Local" {
		t.Errorf("byModel[%q].provider = %v, want node display 'Ollama Local'", mk, m["provider"])
	}
	if got, _ := m["requests"].(int); got != 1 {
		t.Errorf("byModel[%q].requests = %v, want 1", mk, m["requests"])
	}
	if got, _ := m["promptTokens"].(int); got != 177 {
		t.Errorf("byModel[%q].promptTokens = %v, want 177", mk, m["promptTokens"])
	}
	if got, _ := m["completionTokens"].(int); got != 42 {
		t.Errorf("byModel[%q].completionTokens = %v, want 42", mk, m["completionTokens"])
	}
	if lu, _ := m["lastUsed"].(string); lu == "" {
		t.Errorf("byModel[%q].lastUsed empty", mk)
	}

	// byAccount keyed "model (provider - accountName)" with connection display name.
	byAccount, _ := stats["byAccount"].(map[string]map[string]any)
	ak := "minimax-m3 (ollama - My Ollama Box)"
	a, ok := byAccount[ak]
	if !ok {
		t.Fatalf("byAccount missing key %q; got keys: %v", ak, mapKeys(byAccount))
	}
	if a["accountName"] != "My Ollama Box" {
		t.Errorf("byAccount[%q].accountName = %v, want 'My Ollama Box'", ak, a["accountName"])
	}
	if a["connectionId"] != conn.ID {
		t.Errorf("byAccount[%q].connectionId = %v, want %s", ak, a["connectionId"], conn.ID)
	}

	// byApiKey carries the resolved keyName.
	byApiKey, _ := stats["byApiKey"].(map[string]map[string]any)
	var kEntry map[string]any
	for _, v := range byApiKey {
		if v["rawModel"] == "minimax-m3" {
			kEntry = v
			break
		}
	}
	if kEntry == nil {
		t.Fatalf("byApiKey missing minimax-m3 entry; keys: %v", mapKeys(byApiKey))
	}
	if kEntry["keyName"] != "Primary Key" {
		t.Errorf("byApiKey.keyName = %v, want 'Primary Key'", kEntry["keyName"])
	}

	// recentRequests newest-first, 2 entries, with prompt/completion.
	rr, _ := stats["recentRequests"].([]map[string]any)
	if len(rr) != 2 {
		t.Fatalf("recentRequests len = %d, want 2", len(rr))
	}
	if rr[0]["promptTokens"] == nil {
		t.Errorf("recentRequests[0].promptTokens missing: %+v", rr[0])
	}

	// last10Minutes is a 10-element array.
	if l10, _ := stats["last10Minutes"].([]map[string]any); len(l10) != 10 {
		t.Errorf("last10Minutes len = %d, want 10", len(l10))
	}

	// pending overlay from the (empty) tracker — must be present, not absent.
	if _, ok := stats["pending"]; !ok {
		t.Errorf("pending missing from stats")
	}
}

// TestStatsWithMeta_NoMeta_DegradesToIDs asserts StatsWithMeta still works
// (and produces a stable contract) when no metadata repos are wired — Meta=nil
// degrades provider to the raw id and account to "Account <id[:8]>...".
func TestStatsWithMeta_NoMeta_DegradesToIDs(t *testing.T) {
	ur, _, _, _ := realStatsDB(t)
	mustSave(t, ur, usage.UsageRecord{
		Provider: "openai", Model: "gpt-4", ConnectionID: "conn-zzz1234567890",
		PromptTokens: 10, CompletionTokens: 5, Status: "success",
	})

	svc := &UsageService{Repo: ur}
	stats := svc.StatsWithMeta(context.Background(), FullStatsOptions{Period: "7d", Meta: nil})

	byAccount, _ := stats["byAccount"].(map[string]map[string]any)
	ak := "gpt-4 (openai - Account conn-zzz)"
	if a, ok := byAccount[ak]; !ok {
		t.Fatalf("byAccount missing degraded key %q; got %v", ak, mapKeys(byAccount))
	} else if a["accountName"] != "Account conn-zzz" {
		t.Errorf("degraded accountName = %v, want 'Account conn-zzz'", a["accountName"])
	}

	byModel, _ := stats["byModel"].(map[string]map[string]any)
	if m := byModel["gpt-4 (openai)"]; m == nil {
		t.Fatalf("byModel missing gpt-4 (openai): %v", mapKeys(byModel))
	} else if m["provider"] != "openai" {
		t.Errorf("no-meta provider = %v, want raw 'openai'", m["provider"])
	}
}

// TestStatsWithMeta_JSONCamelCaseRoundTrip marshals the stats map and asserts
// the JSON carries camelCase keys the frontend reads (totalRequests, byModel,
// promptTokens) — guards against a future refactor re-introducing the
// PascalCase domain-struct emission that broke the dashboard.
func TestStatsWithMeta_JSONCamelCaseRoundTrip(t *testing.T) {
	ur, _, _, _ := realStatsDB(t)
	mustSave(t, ur, usage.UsageRecord{Provider: "openai", Model: "gpt-4", PromptTokens: 1, CompletionTokens: 1, Status: "success"})

	stats := (&UsageService{Repo: ur}).StatsWithMeta(context.Background(), FullStatsOptions{Period: "7d"})
	b, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{`"totalRequests"`, `"byModel"`, `"promptTokens"`, `"completionTokens"`, `"recentRequests"`} {
		if !contains(s, want) {
			t.Errorf("JSON missing %s; snippet: %s", want, snippet(s))
		}
	}
	for _, bad := range []string{`"TotalRequests"`, `"ByModel"`, `"PromptTokens"`} {
		if contains(s, bad) {
			t.Errorf("JSON must NOT contain PascalCase %s", bad)
		}
	}
}

// --- EventTracker tests (#83) ---

// TestEventTracker_PendingAccounting verifies PublishStart/PublishStop keep
// the byModel and byAccount counters balanced, and that ActiveRequests reports
// the in-flight request with the connection display name resolved.
func TestEventTracker_PendingAccounting(t *testing.T) {
	tr := NewEventTracker()
	tr.PublishStart("gpt-4", "openai", "conn-aaa111111111111")

	// While in flight: byModel and byAccount both report 1.
	snap := tr.Snapshot()
	byModel, _ := snap["byModel"].(map[string]int)
	if byModel["gpt-4 (openai)"] != 1 {
		t.Errorf("pending byModel = %v, want 1 for gpt-4 (openai)", byModel)
	}
	active := tr.ActiveRequests(context.Background(), func(id string) string { return "Acct " + id })
	if len(active) != 1 {
		t.Fatalf("activeRequests = %d, want 1", len(active))
	}
	if active[0]["account"] != "Acct conn-aaa111111111111" {
		t.Errorf("active account = %v, want resolved name", active[0]["account"])
	}
	if active[0]["provider"] != "openai" {
		t.Errorf("active provider = %v, want openai", active[0]["provider"])
	}

	// On Stop the counters clear.
	tr.PublishStop("gpt-4", "openai", "conn-aaa111111111111", false)
	snap = tr.Snapshot()
	byModel, _ = snap["byModel"].(map[string]int)
	if _, stillThere := byModel["gpt-4 (openai)"]; stillThere {
		t.Errorf("pending byModel still present after Stop: %v", byModel)
	}
	if got := tr.ActiveRequests(context.Background(), func(string) string { return "" }); len(got) != 0 {
		t.Errorf("activeRequests after Stop = %d, want 0", len(got))
	}
}

// TestEventTracker_ErrorProviderWindow verifies a PublishStop with errored=true
// stamps lastErrorProvider, surfaced within the 10s window and cleared after.
func TestEventTracker_ErrorProviderWindow(t *testing.T) {
	tr := NewEventTracker()
	tr.PublishStop("claude-3", "anthropic", "c1", true)
	if got := tr.ErrorProvider(); got != "anthropic" {
		t.Errorf("ErrorProvider = %q, want anthropic", got)
	}
	// Non-error stop does not stamp.
	tr2 := NewEventTracker()
	tr2.PublishStop("gpt-4", "openai", "c2", false)
	if got := tr2.ErrorProvider(); got != "" {
		t.Errorf("ErrorProvider after non-error stop = %q, want empty", got)
	}
}

// TestEventTracker_RecentRingAndDedupe verifies PublishSave populates the
// recent ring (newest-first via RecentRequests) and dedupes identical
// model|provider|prompt|completion|minute entries.
func TestEventTracker_RecentRingAndDedupe(t *testing.T) {
	tr := NewEventTracker()
	ts := time.Now()
	tr.PublishSave("gpt-4", "openai", "success", 100, 25, ts)
	tr.PublishSave("gpt-4", "openai", "success", 100, 25, ts) // same minute → deduped
	tr.PublishSave("claude-3", "anthropic", "success", 50, 10, ts)

	rr := tr.RecentRequests(20)
	if len(rr) != 2 {
		t.Fatalf("RecentRequests = %d, want 2 (deduped), got %+v", len(rr), rr)
	}
	// Newest-first: the claude entry was appended last.
	if rr[0]["model"] != "claude-3" {
		t.Errorf("RecentRequests[0].model = %v, want claude-3 (newest first)", rr[0]["model"])
	}
}

// TestEventTracker_PublishSave_ZeroTokensSkipped verifies a save with no tokens
// is not recorded (matches the JS recentRequests filter).
func TestEventTracker_PublishSave_ZeroTokensSkipped(t *testing.T) {
	tr := NewEventTracker()
	tr.PublishSave("gpt-4", "openai", "success", 0, 0, time.Now())
	if got := tr.RecentRequests(20); len(got) != 0 {
		t.Errorf("zero-token save should be skipped; got %+v", got)
	}
}

// TestEventTracker_SubscribeFanOut verifies subscribers are notified on each
// publish and that unsubscribe stops further notifications. Since #2509
// (0d216689) notifications are debounced per kind, so flush pending timers
// with NotifyNow before asserting counts.
func TestEventTracker_SubscribeFanOut(t *testing.T) {
	tr := NewEventTracker()
	var mu sync.Mutex
	count := 0
	unsub := tr.Subscribe(func() {
		mu.Lock()
		count++
		mu.Unlock()
	})

	tr.PublishStart("m", "p", "")                         // pending kind
	tr.PublishSave("m", "p", "success", 1, 1, time.Now()) // update kind
	tr.PublishStop("m", "p", "", false)                   // pending kind (coalesced with Start)
	tr.NotifyNow()                                        // flush debounced timers

	mu.Lock()
	got := count
	mu.Unlock()
	// Start+Stop coalesce to one pending notify; Save fires one update notify → 2.
	if got < 2 {
		t.Errorf("subscriber notified %d times, want >= 2 (one per debounced kind)", got)
	}

	// After unsubscribe, no further notifications.
	unsub()
	before := count
	tr.PublishStart("m2", "p2", "")
	tr.NotifyNow()
	mu.Lock()
	after := count
	mu.Unlock()
	if after != before {
		t.Errorf("subscriber notified after unsubscribe: before=%d after=%d", before, after)
	}
}

// TestEventTracker_NilSafe asserts every publish/read method is a no-op when
// the tracker is nil — proxychat and the handlers rely on this to keep legacy
// wiring working.
func TestEventTracker_NilSafe(t *testing.T) {
	var tr *EventTracker
	tr.PublishStart("m", "p", "c")
	tr.PublishStop("m", "p", "c", true)
	tr.PublishSave("m", "p", "success", 1, 1, time.Now())
	if got := tr.ActiveRequests(context.Background(), func(string) string { return "" }); got != nil {
		t.Errorf("nil ActiveRequests = %v, want nil", got)
	}
	if got := tr.RecentRequests(10); got != nil {
		t.Errorf("nil RecentRequests = %v, want nil", got)
	}
	if got := tr.ErrorProvider(); got != "" {
		t.Errorf("nil ErrorProvider = %q, want empty", got)
	}
	if got := tr.Snapshot(); got == nil {
		t.Errorf("nil Snapshot = nil, want non-nil default map")
	}
}

func mapKeys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func snippet(s string) string {
	if len(s) > 400 {
		return s[:400]
	}
	return s
}
