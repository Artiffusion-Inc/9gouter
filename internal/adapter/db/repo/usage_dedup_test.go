package repo

import (
	"context"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
)

// usage_dedup_test.go ports the regression coverage for the saveRequestUsage
// dedup half of decolua/9router #2509 (0d216689): SaveDedup skips an identical
// row (and returns inserted=false) and backfills a missing endpoint. Tests
// drive the real UsageRepo over sqlite (no mock).

func dedupRec(ts time.Time, endpoint string, prompt, completion int) usage.UsageRecord {
	return usage.UsageRecord{
		Timestamp:        ts,
		Provider:         "openai",
		Model:            "gpt-4o",
		ConnectionID:     "conn-1",
		APIKey:           "key-1",
		Endpoint:         endpoint,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		Status:           "ok",
	}
}

func TestSaveDedup_FirstInsertReturnsTrue(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	inserted, err := r.SaveDedup(ctx, dedupRec(ts, "/v1/chat/completions", 10, 20))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !inserted {
		t.Fatal("first save must be inserted=true")
	}
}

func TestSaveDedup_IdenticalRowSkipped(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	rec := dedupRec(ts, "/v1/chat/completions", 10, 20)
	if _, err := r.SaveDedup(ctx, rec); err != nil {
		t.Fatalf("save first: %v", err)
	}
	// Identical row → skipped.
	inserted, err := r.SaveDedup(ctx, rec)
	if err != nil {
		t.Fatalf("save dup: %v", err)
	}
	if inserted {
		t.Fatal("duplicate save must be inserted=false")
	}
	// Only one history row.
	rows, err := r.Query(ctx, usage.Query{Limit: 100})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("history len = %d, want 1 (dup not inserted)", len(rows))
	}
}

func TestSaveDedup_LifetimeCounterNotInflatedOnDup(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	rec := dedupRec(ts, "/v1/chat/completions", 10, 20)
	if _, err := r.SaveDedup(ctx, rec); err != nil {
		t.Fatalf("save first: %v", err)
	}
	if _, err := r.SaveDedup(ctx, rec); err != nil {
		t.Fatalf("save dup: %v", err)
	}
	agg, err := r.Aggregates(ctx, "all")
	if err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	if agg.TotalRequests != 1 {
		t.Errorf("totalRequests = %d, want 1 (lifetime not inflated by dup)", agg.TotalRequests)
	}
}

func TestSaveDedup_DifferentPromptInserts(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	if _, err := r.SaveDedup(ctx, dedupRec(ts, "/v1/chat/completions", 10, 20)); err != nil {
		t.Fatalf("save first: %v", err)
	}
	// Different promptTokens → distinct row, inserted.
	inserted, err := r.SaveDedup(ctx, dedupRec(ts, "/v1/chat/completions", 11, 20))
	if err != nil {
		t.Fatalf("save diff: %v", err)
	}
	if !inserted {
		t.Error("distinct prompt must be inserted=true")
	}
	rows, _ := r.Query(ctx, usage.Query{Limit: 100})
	if len(rows) != 2 {
		t.Errorf("history len = %d, want 2", len(rows))
	}
}

func TestSaveDedup_DifferentModelInserts(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	if _, err := r.SaveDedup(ctx, dedupRec(ts, "/v1/chat/completions", 10, 20)); err != nil {
		t.Fatalf("save first: %v", err)
	}
	rec := dedupRec(ts, "/v1/chat/completions", 10, 20)
	rec.Model = "gpt-4o-mini"
	inserted, err := r.SaveDedup(ctx, rec)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !inserted {
		t.Error("distinct model must be inserted=true")
	}
}

func TestSaveDedup_DifferentConnectionInserts(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	if _, err := r.SaveDedup(ctx, dedupRec(ts, "/v1/chat/completions", 10, 20)); err != nil {
		t.Fatalf("save first: %v", err)
	}
	rec := dedupRec(ts, "/v1/chat/completions", 10, 20)
	rec.ConnectionID = "conn-2"
	inserted, err := r.SaveDedup(ctx, rec)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !inserted {
		t.Error("distinct connection must be inserted=true")
	}
}

func TestSaveDedup_DifferentTimestampInserts(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts1 := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	if _, err := r.SaveDedup(ctx, dedupRec(ts1, "/v1/chat/completions", 10, 20)); err != nil {
		t.Fatalf("save first: %v", err)
	}
	ts2 := ts1.Add(time.Minute)
	inserted, err := r.SaveDedup(ctx, dedupRec(ts2, "/v1/chat/completions", 10, 20))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !inserted {
		t.Error("distinct timestamp must be inserted=true")
	}
}

func TestSaveDedup_EndpointBackfillOnDup(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	// First write: no endpoint.
	if _, err := r.SaveDedup(ctx, dedupRec(ts, "", 10, 20)); err != nil {
		t.Fatalf("save first: %v", err)
	}
	// Duplicate write with an endpoint → backfill onto existing row, still inserted=false.
	inserted, err := r.SaveDedup(ctx, dedupRec(ts, "/v1/chat/completions", 10, 20))
	if err != nil {
		t.Fatalf("save dup: %v", err)
	}
	if inserted {
		t.Error("endpoint-backfill dup must still be inserted=false")
	}
	rows, _ := r.Query(ctx, usage.Query{Limit: 100})
	if len(rows) != 1 {
		t.Fatalf("history len = %d, want 1", len(rows))
	}
	if rows[0].Endpoint != "/v1/chat/completions" {
		t.Errorf("endpoint backfill = %q, want /v1/chat/completions", rows[0].Endpoint)
	}
}

func TestSaveDedup_EndpointNotOverwrittenWhenExistingHasOne(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	// Existing row already has an endpoint.
	if _, err := r.SaveDedup(ctx, dedupRec(ts, "/v1/chat/completions", 10, 20)); err != nil {
		t.Fatalf("save first: %v", err)
	}
	// Dup without an endpoint → must NOT wipe the existing endpoint.
	if _, err := r.SaveDedup(ctx, dedupRec(ts, "", 10, 20)); err != nil {
		t.Fatalf("save dup: %v", err)
	}
	rows, _ := r.Query(ctx, usage.Query{Limit: 100})
	if rows[0].Endpoint != "/v1/chat/completions" {
		t.Errorf("endpoint = %q, want preserved /v1/chat/completions", rows[0].Endpoint)
	}
}

func TestSave_StillInsertsEveryCall(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	ts := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	rec := dedupRec(ts, "/v1/chat/completions", 10, 20)
	if err := r.Save(ctx, rec); err != nil {
		t.Fatalf("save first: %v", err)
	}
	// Save (non-dedup) inserts every time — backwards compatible.
	if err := r.Save(ctx, rec); err != nil {
		t.Fatalf("save second: %v", err)
	}
	rows, _ := r.Query(ctx, usage.Query{Limit: 100})
	if len(rows) != 2 {
		t.Errorf("Save (non-dedup) history len = %d, want 2", len(rows))
	}
}
