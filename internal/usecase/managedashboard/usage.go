package managedashboard

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
)

// UsageService exposes read-only usage analytics operations.
type UsageService struct {
	Repo interface {
		Aggregates(ctx context.Context, period string) (usage.Aggregates, error)
		Query(ctx context.Context, q usage.Query) ([]usage.UsageRecord, error)
		RecentLogs(ctx context.Context, limit int) ([]string, error)
	}
	DetailRepo interface {
		Query(ctx context.Context, f repo.RequestDetailFilter) (repo.RequestDetailPage, error)
	}
}

// Stats returns usage aggregates for a period.
func (s *UsageService) Stats(ctx context.Context, period string) (usage.Aggregates, error) {
	return s.Repo.Aggregates(ctx, period)
}

// Chart returns per-bucket usage totals as an array of records
// [{label, tokens, cost}, ...], mirroring the legacy JS getChartData() so the
// dashboard UsageChart component (which calls data.some(d => d.tokens>0 || d.cost>0))
// receives the array it expects. Returning {labels,requests,costs,tokens} column
// arrays here broke the UI: data.some crashed with "t.some is not a function",
// taking down the dashboard render tree (including the Import Backup button).
//
// Valid periods mirror legacy: today (24 hourly buckets), 24h (24 hourly
// buckets rolling), 7d / 30d / 60d (N daily buckets from usageHistory).
func (s *UsageService) Chart(ctx context.Context, period string) ([]map[string]any, error) {
	switch period {
	case "today", "24h":
		return s.chartHourly(ctx, period)
	default:
		// 7d / 30d / 60d / unknown → daily buckets.
		bucketCount := 7
		switch period {
		case "30d":
			bucketCount = 30
		case "60d":
			bucketCount = 60
		}
		return s.chartDaily(ctx, bucketCount)
	}
}

// chartHourly builds `count` hourly buckets. "today" starts at local midnight;
// "24h" rolls back count hours from now. Each bucket aggregates
// prompt+completion tokens and cost over the hour window.
func (s *UsageService) chartHourly(ctx context.Context, period string) ([]map[string]any, error) {
	const bucketCount = 24
	now := time.Now()

	var startTime time.Time
	if period == "today" {
		startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		startTime = startOfDay
	} else {
		startTime = now.Add(-time.Duration(bucketCount) * time.Hour)
	}
	endTime := startTime.Add(time.Duration(bucketCount) * time.Hour)

	records, err := s.Repo.Query(ctx, usage.Query{
		StartDate: startTime,
		Limit:     100000,
	})
	if err != nil {
		return nil, err
	}

	buckets := make([]map[string]any, bucketCount)
	for i := 0; i < bucketCount; i++ {
		t := startTime.Add(time.Duration(i) * time.Hour)
		label := t.Format("15:04")
		buckets[i] = map[string]any{"label": label, "tokens": 0, "cost": 0.0}
	}
	for _, rec := range records {
		ts := rec.Timestamp.In(now.Location())
		if ts.Before(startTime) {
			continue
		}
		if period == "today" && !ts.Before(endTime) {
			continue
		}
		if period == "24h" && ts.After(now) {
			continue
		}
		idx := int(ts.Sub(startTime) / time.Hour)
		if idx < 0 || idx >= bucketCount {
			continue
		}
		tok, _ := buckets[idx]["tokens"].(int)
		cost, _ := buckets[idx]["cost"].(float64)
		buckets[idx]["tokens"] = tok + rec.PromptTokens + rec.CompletionTokens
		buckets[idx]["cost"] = cost + rec.Cost
	}
	return buckets, nil
}

// chartDaily builds N daily buckets ending today, aggregating tokens and
// cost from usage records. Mirrors legacy getChartData's 7d/30d/60d branch.
func (s *UsageService) chartDaily(ctx context.Context, bucketCount int) ([]map[string]any, error) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	startDate := today.AddDate(0, 0, -(bucketCount - 1))

	records, err := s.Repo.Query(ctx, usage.Query{
		StartDate: startDate,
		Limit:     100000,
	})
	if err != nil {
		return nil, err
	}

	byDay := map[string]usage.Counter{}
	for _, rec := range records {
		key := rec.Timestamp.UTC().Format("2006-01-02")
		c := byDay[key]
		c.Requests++
		c.PromptTokens += rec.PromptTokens
		c.CompletionTokens += rec.CompletionTokens
		c.Cost += rec.Cost
		byDay[key] = c
	}

	out := make([]map[string]any, 0, bucketCount)
	for i := 0; i < bucketCount; i++ {
		d := startDate.AddDate(0, 0, i)
		key := d.UTC().Format("2006-01-02")
		label := d.Format("Jan 2")
		c := byDay[key]
		out = append(out, map[string]any{
			"label":  label,
			"tokens": c.PromptTokens + c.CompletionTokens,
			"cost":   c.Cost,
		})
	}
	return out, nil
}


// RecentLogs returns recent formatted log lines.
func (s *UsageService) RecentLogs(ctx context.Context, limit int) ([]string, error) {
	return s.Repo.RecentLogs(ctx, limit)
}

// RequestDetails returns a paginated list of request details.
func (s *UsageService) RequestDetails(ctx context.Context, f repo.RequestDetailFilter) (repo.RequestDetailPage, error) {
	return s.DetailRepo.Query(ctx, f)
}

// DistinctProviders returns unique provider names seen in request details.
func (s *UsageService) DistinctProviders(ctx context.Context) ([]map[string]string, error) {
	page, err := s.DetailRepo.Query(ctx, repo.RequestDetailFilter{Page: 1, PageSize: 10000})
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, d := range page.Details {
		if d.Provider != "" {
			seen[d.Provider] = struct{}{}
		}
	}
	out := make([]map[string]string, 0, len(seen))
	for p := range seen {
		out = append(out, map[string]string{"id": p, "name": p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"] < out[j]["id"] })
	return out, nil
}

var _ = fmt.Sprintf
