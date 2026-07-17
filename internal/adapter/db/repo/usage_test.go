package repo

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/usage"
)

func TestUsageRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()

	streamMs := 1234
	tps := 45.6
	rec := usage.UsageRecord{
		Timestamp:        time.Now().UTC(),
		Provider:         "openai",
		Model:            "gpt-4o",
		ConnectionID:     "conn-1",
		APIKey:           "key-1",
		Endpoint:         "/v1/chat/completions",
		PromptTokens:     10,
		CompletionTokens: 20,
		Cost:             0.0001,
		Status:           "ok",
		Tokens:           json.RawMessage(`{"prompt_tokens":10,"completion_tokens":20}`),
		StreamMs:         &streamMs,
		TPS:              &tps,
	}
	if err := r.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	q := usage.Query{Limit: 10}
	rows, err := r.Query(ctx, q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("query len = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Provider != rec.Provider || got.Model != rec.Model || got.PromptTokens != 10 || got.CompletionTokens != 20 {
		t.Fatalf("query mismatch: %+v", got)
	}
	if got.StreamMs == nil || *got.StreamMs != 1234 {
		t.Fatalf("streamMs mismatch")
	}
	if got.TPS == nil || *got.TPS != 45.6 {
		t.Fatalf("tps mismatch")
	}

	agg, err := r.Aggregates(ctx, "all")
	if err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	if agg.TotalRequests != 1 {
		t.Fatalf("total requests = %d, want 1", agg.TotalRequests)
	}
	if c, ok := agg.ByProvider["openai"]; !ok || c.Requests != 1 {
		t.Fatalf("byProvider mismatch: %+v", agg.ByProvider)
	}

	logs, err := r.RecentLogs(ctx, 10)
	if err != nil {
		t.Fatalf("recentLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("recentLogs len = %d, want 1", len(logs))
	}
}
