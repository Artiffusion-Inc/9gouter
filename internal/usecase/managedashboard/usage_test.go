package managedashboard

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
)

// fakeUsageRepo implements UsageService.Repo.
type fakeUsageRepo struct {
	aggregates usage.Aggregates
	aggErr     error
	queryRet   []usage.UsageRecord
	queryErr   error
	queryCalls []usage.Query
	logsRet    []string
	logsErr    error
	logsLimit  int
}

func (r *fakeUsageRepo) Aggregates(ctx context.Context, period string) (usage.Aggregates, error) {
	return r.aggregates, r.aggErr
}

func (r *fakeUsageRepo) Query(ctx context.Context, q usage.Query) ([]usage.UsageRecord, error) {
	r.queryCalls = append(r.queryCalls, q)
	if r.queryErr != nil {
		return nil, r.queryErr
	}
	return r.queryRet, nil
}

func (r *fakeUsageRepo) RecentLogs(ctx context.Context, limit int) ([]string, error) {
	r.logsLimit = limit
	if r.logsErr != nil {
		return nil, r.logsErr
	}
	return r.logsRet, nil
}

// fakeDetailRepo implements UsageService.DetailRepo.
type fakeDetailRepo struct {
	page repo.RequestDetailPage
	err  error
	calls []repo.RequestDetailFilter
}

func (r *fakeDetailRepo) Query(ctx context.Context, f repo.RequestDetailFilter) (repo.RequestDetailPage, error) {
	r.calls = append(r.calls, f)
	if r.err != nil {
		return repo.RequestDetailPage{}, r.err
	}
	return r.page, nil
}

func TestUsageService_Stats(t *testing.T) {
	t.Parallel()
	want := usage.Aggregates{TotalRequests: 5, TotalPromptTokens: 100}
	s := &UsageService{Repo: &fakeUsageRepo{aggregates: want}}
	got, err := s.Stats(context.Background(), "today")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestUsageService_Stats_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("agg fail")
	s := &UsageService{Repo: &fakeUsageRepo{aggErr: sentinel}}
	if _, err := s.Stats(context.Background(), "today"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestUsageService_RecentLogs(t *testing.T) {
	t.Parallel()
	want := []string{"line1", "line2"}
	repo := &fakeUsageRepo{logsRet: want}
	s := &UsageService{Repo: repo}
	got, err := s.RecentLogs(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentLogs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if repo.logsLimit != 10 {
		t.Errorf("limit = %d, want 10", repo.logsLimit)
	}
}

func TestUsageService_RecentLogs_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("logs fail")
	s := &UsageService{Repo: &fakeUsageRepo{logsErr: sentinel}}
	if _, err := s.RecentLogs(context.Background(), 5); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestUsageService_RequestDetails(t *testing.T) {
	t.Parallel()
	want := repo.RequestDetailPage{}
	drepo := &fakeDetailRepo{page: want}
	s := &UsageService{DetailRepo: drepo}

	got, err := s.RequestDetails(context.Background(), repo.RequestDetailFilter{Page: 2, PageSize: 20})
	if err != nil {
		t.Fatalf("RequestDetails: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if len(drepo.calls) != 1 || drepo.calls[0].Page != 2 || drepo.calls[0].PageSize != 20 {
		t.Errorf("filter call = %+v", drepo.calls)
	}
}

func TestUsageService_RequestDetails_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("detail fail")
	s := &UsageService{DetailRepo: &fakeDetailRepo{err: sentinel}}
	if _, err := s.RequestDetails(context.Background(), repo.RequestDetailFilter{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestUsageService_DistinctProviders(t *testing.T) {
	t.Parallel()
	drepo := &fakeDetailRepo{
		page: repo.RequestDetailPage{
			Details: []repo.RequestDetail{
				{ID: "1", Provider: "openai"},
				{ID: "2", Provider: "anthropic"},
				{ID: "3", Provider: "openai"},
				{ID: "4", Provider: ""}, // empty provider dropped
			},
		},
	}
	s := &UsageService{DetailRepo: drepo}

	got, err := s.DistinctProviders(context.Background())
	if err != nil {
		t.Fatalf("DistinctProviders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	// Sorted ascending by id.
	if got[0]["id"] != "anthropic" || got[1]["id"] != "openai" {
		t.Errorf("got = %+v, want sorted [anthropic, openai]", got)
	}
	for _, e := range got {
		if e["name"] != e["id"] {
			t.Errorf("name should mirror id: %+v", e)
		}
	}
	// Verify the filter used.
	if len(drepo.calls) != 1 || drepo.calls[0].Page != 1 || drepo.calls[0].PageSize != 10000 {
		t.Errorf("filter call = %+v", drepo.calls)
	}
}

func TestUsageService_DistinctProviders_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("detail fail")
	s := &UsageService{DetailRepo: &fakeDetailRepo{err: sentinel}}
	if _, err := s.DistinctProviders(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestUsageService_DistinctProviders_Empty(t *testing.T) {
	t.Parallel()
	s := &UsageService{DetailRepo: &fakeDetailRepo{}}
	got, err := s.DistinctProviders(context.Background())
	if err != nil {
		t.Fatalf("DistinctProviders: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}

// --- Chart tests ---

func TestUsageService_Chart_PeriodRouting(t *testing.T) {
	t.Parallel()
	// Test the period routing logic: today/24h → hourly, 7d/30d/60d → daily with right bucket count.
	// Use empty records to avoid bucket-boundary non-determinism.
	tests := []struct {
		period      string
		wantBuckets int
		wantHourly  bool
	}{
		{"today", 24, true},
		{"24h", 24, true},
		{"7d", 7, false},
		{"30d", 30, false},
		{"60d", 60, false},
		{"unknown", 7, false}, // default → 7 daily buckets
	}
	for _, tc := range tests {
		t.Run(tc.period, func(t *testing.T) {
			repo := &fakeUsageRepo{queryRet: nil}
			s := &UsageService{Repo: repo}
			out, err := s.Chart(context.Background(), tc.period)
			if err != nil {
				t.Fatalf("Chart: %v", err)
			}
			if len(out) != tc.wantBuckets {
				t.Fatalf("period %q: got %d buckets, want %d", tc.period, len(out), tc.wantBuckets)
			}
			if tc.wantHourly {
				// Hourly labels look like "HH:MM".
				if l, _ := out[0]["label"].(string); len(l) != 5 || l[2] != ':' {
					t.Errorf("period %q: first label = %q, want HH:MM", tc.period, l)
				}
			} else {
				// Daily labels look like "Jan 2".
				if l, _ := out[0]["label"].(string); len(l) < 4 {
					t.Errorf("period %q: first label = %q, want MMM D", tc.period, l)
				}
			}
		})
	}
}

func TestUsageService_Chart_QueryError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("query fail")
	s := &UsageService{Repo: &fakeUsageRepo{queryErr: sentinel}}
	if _, err := s.Chart(context.Background(), "today"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if _, err := s.Chart(context.Background(), "7d"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestUsageService_Chart_Hourly_AggregatesRecords(t *testing.T) {
	t.Parallel()
	// Build a record pinned to "now" so it lands in bucket[23] for "24h".
	now := time.Now()
	repo := &fakeUsageRepo{
		queryRet: []usage.UsageRecord{
			{Timestamp: now, PromptTokens: 100, CompletionTokens: 50, Cost: 1.5},
			{Timestamp: now, PromptTokens: 10, CompletionTokens: 5, Cost: 0.25},
		},
	}
	s := &UsageService{Repo: repo}

	out, err := s.Chart(context.Background(), "24h")
	if err != nil {
		t.Fatalf("Chart: %v", err)
	}
	if len(out) != 24 {
		t.Fatalf("got %d buckets, want 24", len(out))
	}
	totalTokens := 0
	totalCost := 0.0
	for _, b := range out {
		tok, _ := b["tokens"].(int)
		cost, _ := b["cost"].(float64)
		totalTokens += tok
		totalCost += cost
	}
	if totalTokens != 165 {
		t.Errorf("total tokens = %d, want 165", totalTokens)
	}
	if totalCost != 1.75 {
		t.Errorf("total cost = %f, want 1.75", totalCost)
	}
}

func TestUsageService_Chart_Today_AggregatesRecords(t *testing.T) {
	t.Parallel()
	// Use a timestamp at the start of "today" + 1h so it lands deterministically in bucket[1].
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	ts := start.Add(1 * time.Hour)
	repo := &fakeUsageRepo{
		queryRet: []usage.UsageRecord{
			{Timestamp: ts, PromptTokens: 30, CompletionTokens: 10, Cost: 0.5},
		},
	}
	s := &UsageService{Repo: repo}

	out, err := s.Chart(context.Background(), "today")
	if err != nil {
		t.Fatalf("Chart: %v", err)
	}
	idx := int(ts.Sub(start) / time.Hour)
	if idx < 0 || idx >= 24 {
		t.Fatalf("bucket index %d out of range", idx)
	}
	tok, _ := out[idx]["tokens"].(int)
	cost, _ := out[idx]["cost"].(float64)
	if tok != 40 {
		t.Errorf("bucket[%d].tokens = %d, want 40", idx, tok)
	}
	if cost != 0.5 {
		t.Errorf("bucket[%d].cost = %f, want 0.5", idx, cost)
	}
}

func TestUsageService_Chart_Hourly_RecordsBeforeStart_Skipped(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// A timestamp far in the past should be skipped (idx<0).
	repo := &fakeUsageRepo{
		queryRet: []usage.UsageRecord{
			{Timestamp: now.Add(-72 * time.Hour), PromptTokens: 999, CompletionTokens: 999, Cost: 99},
		},
	}
	s := &UsageService{Repo: repo}
	out, err := s.Chart(context.Background(), "24h")
	if err != nil {
		t.Fatalf("Chart: %v", err)
	}
	totalTokens := 0
	for _, b := range out {
		tok, _ := b["tokens"].(int)
		totalTokens += tok
	}
	if totalTokens != 0 {
		t.Errorf("old record should be skipped, got total tokens = %d", totalTokens)
	}
}

func TestUsageService_Chart_Daily_AggregatesRecords(t *testing.T) {
	t.Parallel()
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// A record from "today" should land in the last bucket of the 7-day chart.
	repo := &fakeUsageRepo{
		queryRet: []usage.UsageRecord{
			{Timestamp: today.Add(2 * time.Hour), PromptTokens: 200, CompletionTokens: 100, Cost: 2.5},
		},
	}
	s := &UsageService{Repo: repo}

	out, err := s.Chart(context.Background(), "7d")
	if err != nil {
		t.Fatalf("Chart: %v", err)
	}
	if len(out) != 7 {
		t.Fatalf("got %d buckets, want 7", len(out))
	}
	// Today's bucket should be the last one (index 6) and contain the record.
	lastTok, _ := out[6]["tokens"].(int)
	lastCost, _ := out[6]["cost"].(float64)
	if lastTok != 300 {
		t.Errorf("last bucket tokens = %d, want 300", lastTok)
	}
	if lastCost != 2.5 {
		t.Errorf("last bucket cost = %f, want 2.5", lastCost)
	}
	// Other buckets should be empty.
	otherTok, _ := out[0]["tokens"].(int)
	if otherTok != 0 {
		t.Errorf("bucket[0].tokens = %d, want 0", otherTok)
	}
}

func TestUsageService_Chart_Daily_EmptyRecords(t *testing.T) {
	t.Parallel()
	repo := &fakeUsageRepo{queryRet: nil}
	s := &UsageService{Repo: repo}
	out, err := s.Chart(context.Background(), "30d")
	if err != nil {
		t.Fatalf("Chart: %v", err)
	}
	if len(out) != 30 {
		t.Fatalf("got %d, want 30", len(out))
	}
	for i, b := range out {
		if b["tokens"] != 0 {
			t.Errorf("bucket[%d].tokens = %v, want 0", i, b["tokens"])
		}
		if b["cost"] != 0.0 {
			t.Errorf("bucket[%d].cost = %v, want 0", i, b["cost"])
		}
	}
}

// --- smoke test for the package: ensure the bucketMs const is referenced.
// usage.go:61 has an unused `bucketMs` const (per CLAUDE.md — do NOT remove).
// We don't need to reference it; just ensure the package compiles.

func TestSortStability_DistinctProviders(t *testing.T) {
	// Verify DistinctProviders sorts ascending and is stable across many providers.
	t.Parallel()
	providers := []string{"zeta", "alpha", "gamma", "beta"}
	var details []repo.RequestDetail
	for i, p := range providers {
		details = append(details, repo.RequestDetail{ID: string(rune('a' + i)), Provider: p})
	}
	s := &UsageService{DetailRepo: &fakeDetailRepo{page: repo.RequestDetailPage{Details: details}}}

	got, err := s.DistinctProviders(context.Background())
	if err != nil {
		t.Fatalf("DistinctProviders: %v", err)
	}
	ids := make([]string, len(got))
	for i, e := range got {
		ids[i] = e["id"]
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("ids not sorted: %v", ids)
	}
}