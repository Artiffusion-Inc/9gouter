package managedashboard

// usage_stats.go reconstructs the full legacy getUsageStats() JSON contract
// (src/lib/db/repos/usageRepo.js::getUsageStats) that the dashboard
// UsageStats.js / OverviewCards.js / UsageTable.js / ProviderTopology.js
// consume. The prior UsageService.Stats returned the domain usage.Aggregates
// struct, whose fields carry NO JSON tags → Go emitted PascalCase
// (TotalRequests, ByModel, Requests) while the frontend expects camelCase
// (totalRequests, byModel, requests) AND a different shape: byModel is an
// OBJECT keyed by "rawModel (provider)" whose values carry rawModel, provider
// (display name), requests, promptTokens, completionTokens, cachedTokens, cost,
// tpsSum, tpsCount, avgTps, lastUsed — plus top-level recentRequests,
// activeRequests, pending, last10Minutes, totalCachedTokens, errorProvider.
//
// Rather than mutate the domain type (which is the persistence contract), we
// build the dashboard contract here as map[string]any, mirroring the JS shape
// 1:1. Real-time fields (activeRequests, pending, errorProvider) are overlaid
// from the live PendingTracker when wired (#83); until then they are zeroed so
// the UI renders a stable "no active requests" state instead of crashing on
// undefined.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
)

// StatsMeta is the concrete metadata source passed from the api.Deps wiring.
// It holds the already-constructed *repo connection/node/apikey repos. Each
// lookup is best-effort — a repo failure degrades to ID-derived names, never
// aborts the stats response.
type StatsMeta struct {
	Connections *repo.ConnectionRepo
	Nodes       *repo.NodeRepo
	APIKeys     *repo.APIKeyRepo
}

// PendingTracker is the live in-memory pending/active-request surface (#83).
// nil means real-time fields are reported empty.
type PendingTracker interface {
	ActiveRequests(ctx context.Context, connName func(id string) string) []map[string]any
	Snapshot() map[string]any // pending {byModel,byAccount}
	ErrorProvider() string
}

// statsBucket is one accumulation bucket for the byModel/byAccount/byApiKey/
// byEndpoint/byProvider maps. It mirrors the JS counter shape exactly.
type statsBucket struct {
	Requests         int      `json:"requests"`
	PromptTokens     int      `json:"promptTokens"`
	CompletionTokens int      `json:"completionTokens"`
	CachedTokens     int      `json:"cachedTokens"`
	Cost             float64  `json:"cost"`
	TpsSum           float64  `json:"tpsSum"`
	TpsCount         int      `json:"tpsCount"`
	AvgTps           *float64 `json:"avgTps"`
}

// FullStatsOptions configures StatsWithMeta.
type FullStatsOptions struct {
	Period  string
	Meta    *StatsMeta
	Pending PendingTracker
}

// StatsWithMeta builds the full dashboard contract. It scans usageHistory for
// the period directly (the legacy 24h/today branch, generalised to every
// period) so per-bucket rawModel/provider/cachedTokens/tps survive — the
// usageDaily rollup does not carry per-bucket rawModel in the Go schema, so
// the daily path would lose the model display name. History scan is exact.
func (s *UsageService) StatsWithMeta(ctx context.Context, opts FullStatsOptions) map[string]any {
	period := opts.Period
	if period == "" {
		period = "7d"
	}

	stats := map[string]any{
		"totalRequests":         0,
		"totalPromptTokens":     0,
		"totalCompletionTokens": 0,
		"totalCachedTokens":     0,
		"totalCost":             0.0,
		"byProvider":            map[string]map[string]any{},
		"byModel":               map[string]map[string]any{},
		"byAccount":             map[string]map[string]any{},
		"byApiKey":              map[string]map[string]any{},
		"byEndpoint":            map[string]map[string]any{},
		"last10Minutes":         []map[string]any{},
		"activeRequests":        []map[string]any{},
		"recentRequests":        []map[string]any{},
		"errorProvider":         "",
	}

	now := time.Now()
	cutoff := periodCutoff(period, now)

	// Metadata joins (best-effort).
	connName := func(id string) string { return "Account " + shortID(id) }
	nodeName := map[string]string{}
	keyName := func(key string) string { return "Local (No API Key)" }
	apiKeyMap := map[string]settings.APIKey{}
	if opts.Meta != nil {
		if conns, err := opts.Meta.Connections.List(ctx, repo.ConnectionFilter{}); err == nil {
			for _, c := range conns {
				if c.Name != "" || c.Email != "" {
					connName = makeConnNameOverride(connName, c)
				}
			}
		}
		if nodes, err := opts.Meta.Nodes.List(ctx, repo.NodeFilter{}); err == nil {
			for _, n := range nodes {
				if n.Name != "" {
					nodeName[n.ID] = n.Name
				}
			}
		}
		if keys, err := opts.Meta.APIKeys.List(ctx); err == nil {
			for _, k := range keys {
				apiKeyMap[k.Key] = k
				if k.Key != "" {
					keyName = makeKeyNameOverride(keyName, k)
				}
			}
		}
	}
	_ = keyName // keyName used per-row below

	records, err := s.Repo.Query(ctx, usage.Query{StartDate: cutoff, Limit: 100000})
	if err != nil {
		return stats
	}

	// Sort newest-first for recentRequests + lastUsed overlay. Query returns
	// ASC by id; reverse a copy.
	recs := make([]usage.UsageRecord, len(records))
	copy(recs, records)
	sort.Slice(recs, func(i, j int) bool { return recs[i].Timestamp.After(recs[j].Timestamp) })

	totalReq := 0
	totalPrompt := 0
	totalCompletion := 0
	totalCached := 0
	totalCost := 0.0

	byProvider := map[string]*statsBucket{}
	byModel := map[string]*statsBucket{}
	byAccount := map[string]*statsBucket{}
	byApiKey := map[string]*statsBucket{}
	byEndpoint := map[string]*statsBucket{}
	modelMeta := map[string]map[string]any{} // key -> {rawModel, provider(display), connectionId?, accountName?, apiKeyMasked?, keyName?, endpoint?}
	lastUsed := map[string]string{}

	for _, rec := range recs {
		tokens := tokenBlobOf(rec.Tokens)
		prompt := firstNonZeroOf(rec.PromptTokens, tokens.Prompt, tokens.Input)
		completion := firstNonZeroOf(rec.CompletionTokens, tokens.Completion, tokens.Output)
		cached := firstNonZeroOf(tokens.Cached, tokens.CacheRead)
		cost := rec.Cost
		tps := validTps(rec.StreamMs, rec.TPS)

		totalReq++
		totalPrompt += prompt
		totalCompletion += completion
		totalCached += cached
		totalCost += cost

		providerDisplay := nodeName[rec.Provider]
		if providerDisplay == "" {
			providerDisplay = rec.Provider
		}

		// byProvider
		accBucket(byProvider, rec.Provider, prompt, completion, cached, cost, tps)

		// byModel — key "rawModel (provider)"
		modelKey := rec.Model
		if rec.Provider != "" {
			modelKey = rec.Model + " (" + rec.Provider + ")"
		}
		accBucket(byModel, modelKey, prompt, completion, cached, cost, tps)
		if _, ok := modelMeta[modelKey]; !ok {
			modelMeta[modelKey] = map[string]any{
				"rawModel": rec.Model,
				"provider": providerDisplay,
				"lastUsed": formatRFC3339(rec.Timestamp),
			}
		}
		updateLastUsed(lastUsed, modelKey, rec.Timestamp)

		// byAccount — key "rawModel (provider - accountName)"
		if rec.ConnectionID != "" {
			acctName := connName(rec.ConnectionID)
			acctKey := rec.Model + " (" + rec.Provider + " - " + acctName + ")"
			accBucket(byAccount, acctKey, prompt, completion, cached, cost, tps)
			if _, ok := modelMeta[acctKey]; !ok {
				modelMeta[acctKey] = map[string]any{
					"rawModel":     rec.Model,
					"provider":     providerDisplay,
					"connectionId": rec.ConnectionID,
					"accountName":  acctName,
					"lastUsed":     formatRFC3339(rec.Timestamp),
				}
			}
			updateLastUsed(lastUsed, acctKey, rec.Timestamp)
		}

		// byApiKey — key "apiKey|model|provider" (matches JS akModelKey).
		// Port upstream d8c2298d (security audit): the map key must be the
		// masked apiKey, not the raw value, otherwise the raw api key leaks as
		// a JSON object key in the byApiKey response bucket.
		apiKeyVal := rec.APIKey
		if apiKeyVal == "" {
			apiKeyVal = "local-no-key"
		}
		maskedKey := apiKeyVal
		if rec.APIKey != "" {
			maskedKey = maskApiKey(rec.APIKey)
		}
		akKey := maskedKey + "|" + rec.Model + "|" + nonEmpty(rec.Provider, "unknown")
		accBucket(byApiKey, akKey, prompt, completion, cached, cost, tps)
		if _, ok := modelMeta[akKey]; !ok {
			kInfo, hasKey := apiKeyMap[rec.APIKey]
			kName := "Local (No API Key)"
			masked := ""
			if hasKey {
				kName = kInfo.Name
				if kName == "" {
					kName = shortPrefix(rec.APIKey) + "..."
				}
				masked = maskApiKey(rec.APIKey)
			}
			modelMeta[akKey] = map[string]any{
				"rawModel":     rec.Model,
				"provider":     providerDisplay,
				"apiKeyMasked": masked,
				"keyName":      kName,
				"apiKeyKey":    masked,
				"lastUsed":     formatRFC3339(rec.Timestamp),
			}
		}
		updateLastUsed(lastUsed, akKey, rec.Timestamp)

		// byEndpoint — key "endpoint|model|provider"
		endpoint := nonEmpty(rec.Endpoint, "Unknown")
		epKey := endpoint + "|" + rec.Model + "|" + nonEmpty(rec.Provider, "unknown")
		accBucket(byEndpoint, epKey, prompt, completion, cached, cost, tps)
		if _, ok := modelMeta[epKey]; !ok {
			modelMeta[epKey] = map[string]any{
				"rawModel": rec.Model,
				"provider": providerDisplay,
				"endpoint": endpoint,
				"lastUsed": formatRFC3339(rec.Timestamp),
			}
		}
		updateLastUsed(lastUsed, epKey, rec.Timestamp)
	}

	stats["totalRequests"] = totalReq
	stats["totalPromptTokens"] = totalPrompt
	stats["totalCompletionTokens"] = totalCompletion
	stats["totalCachedTokens"] = totalCached
	stats["totalCost"] = totalCost

	stats["byProvider"] = finalizeBuckets(byProvider, nil, lastUsed)
	stats["byModel"] = finalizeBuckets(byModel, modelMeta, lastUsed)
	stats["byAccount"] = finalizeBuckets(byAccount, modelMeta, lastUsed)
	stats["byApiKey"] = finalizeBuckets(byApiKey, modelMeta, lastUsed)
	stats["byEndpoint"] = finalizeBuckets(byEndpoint, modelMeta, lastUsed)

	// recentRequests — newest 20 with non-zero tokens, deduped by minute bucket.
	stats["recentRequests"] = recentRequestsFrom(recs)

	// last10Minutes — 10 per-minute buckets ending at the current minute.
	stats["last10Minutes"] = last10MinutesFrom(records, now)

	// Real-time overlay (#83). When no tracker is wired, activeRequests stays
	// empty and pending is {byModel:{}, byAccount:{}}.
	pendingSnap := map[string]any{"byModel": map[string]int{}, "byAccount": map[string]map[string]int{}}
	if opts.Pending != nil {
		stats["activeRequests"] = opts.Pending.ActiveRequests(ctx, connName)
		stats["errorProvider"] = opts.Pending.ErrorProvider()
		pendingSnap = opts.Pending.Snapshot()
	}
	stats["pending"] = pendingSnap

	return stats
}

func accBucket(m map[string]*statsBucket, key string, prompt, completion, cached int, cost float64, tps *float64) {
	b := m[key]
	if b == nil {
		b = &statsBucket{}
		m[key] = b
	}
	b.Requests++
	b.PromptTokens += prompt
	b.CompletionTokens += completion
	b.CachedTokens += cached
	b.Cost += cost
	if tps != nil {
		b.TpsSum += *tps
		b.TpsCount++
	}
}

func finalizeBuckets(m map[string]*statsBucket, meta map[string]map[string]any, lastUsed map[string]string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for k, b := range m {
		entry := map[string]any{
			"requests":         b.Requests,
			"promptTokens":     b.PromptTokens,
			"completionTokens": b.CompletionTokens,
			"cachedTokens":     b.CachedTokens,
			"cost":             b.Cost,
			"tpsSum":           b.TpsSum,
			"tpsCount":         b.TpsCount,
			"avgTps":           avgTps(b.TpsSum, b.TpsCount),
		}
		if mm, ok := meta[k]; ok {
			for mk, mv := range mm {
				if _, present := entry[mk]; !present {
					entry[mk] = mv
				}
			}
		}
		if lu, ok := lastUsed[k]; ok && lu != "" {
			entry["lastUsed"] = lu
		}
		out[k] = entry
	}
	return out
}

func recentRequestsFrom(recs []usage.UsageRecord) []map[string]any {
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, rec := range recs {
		tokens := tokenBlobOf(rec.Tokens)
		prompt := firstNonZeroOf(rec.PromptTokens, tokens.Prompt, tokens.Input)
		completion := firstNonZeroOf(rec.CompletionTokens, tokens.Completion, tokens.Output)
		if prompt == 0 && completion == 0 {
			continue
		}
		ts := formatRFC3339(rec.Timestamp)
		minute := ""
		if len(ts) >= 16 {
			minute = ts[:16]
		}
		key := rec.Model + "|" + rec.Provider + "|" + itoa(prompt) + "|" + itoa(completion) + "|" + minute
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, map[string]any{
			"timestamp":        ts,
			"model":            rec.Model,
			"provider":         rec.Provider,
			"promptTokens":     prompt,
			"completionTokens": completion,
			"cachedTokens":     firstNonZeroOf(tokens.Cached, tokens.CacheRead),
			"status":           nonEmpty(rec.Status, "ok"),
		})
		if len(out) >= 20 {
			break
		}
	}
	return out
}

func last10MinutesFrom(records []usage.UsageRecord, now time.Time) []map[string]any {
	currentMinuteStart := now.UTC().Truncate(time.Minute)
	buckets := make([]map[string]any, 10)
	idx := map[int64]int{}
	for i := 0; i < 10; i++ {
		ts := currentMinuteStart.Add(time.Duration(-(9 - i)) * time.Minute)
		buckets[i] = map[string]any{"requests": 0, "promptTokens": 0, "completionTokens": 0, "cost": 0.0}
		idx[ts.UnixMilli()] = i
	}
	tenAgo := currentMinuteStart.Add(-9 * time.Minute)
	for _, rec := range records {
		t := rec.Timestamp.UTC()
		if t.Before(tenAgo) || t.After(now.Add(time.Minute)) {
			continue
		}
		ms := t.Truncate(time.Minute).UnixMilli()
		i, ok := idx[ms]
		if !ok {
			continue
		}
		tokens := tokenBlobOf(rec.Tokens)
		buckets[i]["requests"] = buckets[i]["requests"].(int) + 1
		buckets[i]["promptTokens"] = buckets[i]["promptTokens"].(int) + firstNonZeroOf(rec.PromptTokens, tokens.Prompt, tokens.Input)
		buckets[i]["completionTokens"] = buckets[i]["completionTokens"].(int) + firstNonZeroOf(rec.CompletionTokens, tokens.Completion, tokens.Output)
		buckets[i]["cost"] = buckets[i]["cost"].(float64) + rec.Cost
	}
	return buckets
}

func periodCutoff(period string, now time.Time) time.Time {
	switch period {
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	case "24h":
		return now.Add(-24 * time.Hour)
	case "7d":
		return now.AddDate(0, 0, -7)
	case "30d":
		return now.AddDate(0, 0, -30)
	case "60d":
		return now.AddDate(0, 0, -60)
	case "all":
		return time.Time{}
	}
	return now.AddDate(0, 0, -7)
}

func updateLastUsed(m map[string]string, key string, ts time.Time) {
	cur := m[key]
	newS := formatRFC3339(ts)
	if cur == "" || newS > cur {
		m[key] = newS
	}
}

type usageTokenBlob struct {
	Prompt     int `json:"prompt_tokens"`
	Input      int `json:"input_tokens"`
	Completion int `json:"completion_tokens"`
	Output     int `json:"output_tokens"`
	Cached     int `json:"cached_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
}

func tokenBlobOf(raw json.RawMessage) usageTokenBlob {
	if len(raw) == 0 {
		return usageTokenBlob{}
	}
	var tb usageTokenBlob
	_ = json.Unmarshal(raw, &tb)
	return tb
}

func validTps(streamMs *int, tps *float64) *float64 {
	if tps == nil {
		return nil
	}
	if streamMs != nil && *streamMs < 1000 {
		return nil // streamMs < 1s is TTFT — bogus TPS (mirrors JS guard).
	}
	v := *tps
	return &v
}

func avgTps(sum float64, count int) *float64 {
	if count <= 0 {
		return nil
	}
	v := round2(sum / float64(count))
	return &v
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

func makeConnNameOverride(prev func(string) string, c settings.ProviderConnection) func(string) string {
	name := c.Name
	if name == "" {
		name = c.Email
	}
	if name == "" {
		name = c.ID
	}
	captured := name
	return func(id string) string {
		if id == c.ID {
			return captured
		}
		return prev(id)
	}
}

func makeKeyNameOverride(prev func(string) string, k settings.APIKey) func(string) string {
	name := k.Name
	if name == "" {
		name = shortPrefix(k.Key) + "..."
	}
	captured := name
	cKey := k.Key
	return func(key string) string {
		if key == cKey {
			return captured
		}
		return prev(key)
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func shortPrefix(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

func maskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return string(key[0]) + "***"
	}
	return key[:8] + "***"
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func firstNonZeroOf(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func itoa(n int) string {
	// avoid strconv import churn in this file; small helper
	return intToStr(n)
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
