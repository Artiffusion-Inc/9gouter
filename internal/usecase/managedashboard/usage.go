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

// Chart returns per-day usage totals suitable for a chart.
func (s *UsageService) Chart(ctx context.Context, period string) (map[string]any, error) {
	records, err := s.Repo.Query(ctx, usage.Query{
		StartDate: startForPeriod(period),
		Limit:     10000,
	})
	if err != nil {
		return nil, err
	}
	days := map[string]usage.Counter{}
	for _, rec := range records {
		key := rec.Timestamp.UTC().Format("2006-01-02")
		addCounter(&days, key, usage.Counter{
			Requests:         1,
			PromptTokens:     rec.PromptTokens,
			CompletionTokens: rec.CompletionTokens,
			Cost:             rec.Cost,
		})
	}
	labels := make([]string, 0, len(days))
	for k := range days {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	requests := make([]int, 0, len(labels))
	costs := make([]float64, 0, len(labels))
	tokens := make([]int, 0, len(labels))
	for _, label := range labels {
		c := days[label]
		requests = append(requests, c.Requests)
		costs = append(costs, c.Cost)
		tokens = append(tokens, c.PromptTokens+c.CompletionTokens)
	}
	return map[string]any{
		"labels":   labels,
		"requests": requests,
		"costs":    costs,
		"tokens":   tokens,
	}, nil
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

func startForPeriod(period string) time.Time {
	switch period {
	case "today", "24h":
		return time.Now().UTC().Add(-24 * time.Hour)
	case "7d":
		return time.Now().UTC().AddDate(0, 0, -7)
	case "30d":
		return time.Now().UTC().AddDate(0, 0, -30)
	case "60d":
		return time.Now().UTC().AddDate(0, 0, -60)
	default:
		return time.Time{}
	}
}

func addCounter(m *map[string]usage.Counter, key string, add usage.Counter) {
	c := (*m)[key]
	c.Requests += add.Requests
	c.PromptTokens += add.PromptTokens
	c.CompletionTokens += add.CompletionTokens
	c.Cost += add.Cost
	(*m)[key] = c
}

var _ = fmt.Sprintf
