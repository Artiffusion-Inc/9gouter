package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
)

// UsageRepo persists usage rows, ported from usageRepo.js.
//
// Note: the JS module mixes in-memory pending-request tracking, cross-repo
// aggregation, event emission, and cost calculation. Those concerns are outside
// the repository port; this file implements only the SQLite-backed persistence
// (save, query, daily rollup, basic aggregates). The full stats usecase will be
// built later in internal/usecase.
type UsageRepo struct{ db *sql.DB }

func NewUsageRepo(db *sql.DB) *UsageRepo { return &UsageRepo{db: db} }

// Save writes a usageHistory row and updates the daily aggregate. It does not
// compute cost (the JS cost calculation depends on the pricing provider stack);
// callers must set UsageRecord.Cost.
func (r *UsageRepo) Save(ctx context.Context, rec usage.UsageRecord) error {
	if rec.Timestamp.IsZero() {
		rec.Timestamp = now()
	}
	prompt, completion := rec.PromptTokens, rec.CompletionTokens
	if rec.Tokens != nil {
		var t tokenBlob
		_ = json.Unmarshal(rec.Tokens, &t)
		if prompt == 0 {
			prompt = firstNonZero(t.Prompt, t.Input)
		}
		if completion == 0 {
			completion = firstNonZero(t.Completion, t.Output)
		}
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage save tx: %w", err)
	}
	defer tx.Rollback()

	streamMs := sql.NullInt64{Valid: rec.StreamMs != nil}
	if rec.StreamMs != nil {
		streamMs.Int64 = int64(*rec.StreamMs)
	}
	tps := sql.NullFloat64{Valid: rec.TPS != nil}
	if rec.TPS != nil {
		tps.Float64 = *rec.TPS
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO usageHistory(timestamp, provider, model, connectionId, apiKey, endpoint, promptTokens, completionTokens, cost, status, tokens, meta, streamMs, tps)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(rec.Timestamp), rec.Provider, rec.Model, rec.ConnectionID, rec.APIKey, rec.Endpoint,
		prompt, completion, rec.Cost, rec.Status, jsonText(rec.Tokens), jsonText(rec.Meta), streamMs, tps)
	if err != nil {
		return fmt.Errorf("usage save insert: %w", err)
	}

	dateKey := localDateKey(rec.Timestamp)
	day, err := r.getDayTx(ctx, tx, dateKey)
	if err != nil {
		return err
	}
	aggregateEntryToDay(day, rec)
	data, err := json.Marshal(day)
	if err != nil {
		return fmt.Errorf("usage save marshal day: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO usageDaily(dateKey, data) VALUES(?, ?) ON CONFLICT(dateKey) DO UPDATE SET data = excluded.data`,
		dateKey, string(data))
	if err != nil {
		return fmt.Errorf("usage save daily: %w", err)
	}

	cur := tx.QueryRowContext(ctx, `SELECT value FROM _meta WHERE key = 'totalRequestsLifetime'`)
	var curVal string
	if err := cur.Scan(&curVal); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("usage save lifetime read: %w", err)
	}
	next, _ := strconv.ParseInt(curVal, 10, 64)
	next++
	_, err = tx.ExecContext(ctx,
		`INSERT INTO _meta(key, value) VALUES('totalRequestsLifetime', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.FormatInt(next, 10))
	if err != nil {
		return fmt.Errorf("usage save lifetime: %w", err)
	}

	return tx.Commit()
}

func (r *UsageRepo) Query(ctx context.Context, q usage.Query) ([]usage.UsageRecord, error) {
	conds, params := []string{}, []any{}
	if q.Provider != "" {
		conds, params = append(conds, "provider = ?"), append(params, q.Provider)
	}
	if q.Model != "" {
		conds, params = append(conds, "model = ?"), append(params, q.Model)
	}
	if !q.StartDate.IsZero() {
		conds, params = append(conds, "timestamp >= ?"), append(params, formatTime(q.StartDate))
	}
	if !q.EndDate.IsZero() {
		conds, params = append(conds, "timestamp <= ?"), append(params, formatTime(q.EndDate))
	}
	sqlWhere := ""
	if len(conds) > 0 {
		sqlWhere = " WHERE " + joinAnd(conds)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	offset := q.Offset
	rows, err := r.db.QueryContext(ctx,
		`SELECT timestamp, provider, model, connectionId, apiKey, endpoint, promptTokens, completionTokens, cost, status, tokens, meta, streamMs, tps FROM usageHistory`+sqlWhere+` ORDER BY id ASC LIMIT ? OFFSET ?`,
		append(params, limit, offset)...)
	if err != nil {
		return nil, fmt.Errorf("usage query: %w", err)
	}
	defer rows.Close()

	var out []usage.UsageRecord
	for rows.Next() {
		rec, err := r.scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Aggregates returns rollup data for a period. For "24h"/"today" it scans
// usageHistory directly; otherwise it reads usageDaily. This is a simplified
// SQL-only aggregate compared to the JS getUsageStats, which also joins
// connection/node/apikey metadata.
func (r *UsageRepo) Aggregates(ctx context.Context, period string) (usage.Aggregates, error) {
	var agg usage.Aggregates
	agg.ByProvider = map[string]usage.Counter{}
	agg.ByModel = map[string]usage.Counter{}
	agg.ByAccount = map[string]usage.Counter{}
	agg.ByAPIKey = map[string]usage.Counter{}
	agg.ByEndpoint = map[string]usage.Counter{}

	if period == "24h" || period == "today" {
		return r.aggregatesFromHistory(ctx, period)
	}

	days := map[string]int{"7d": 7, "30d": 30, "60d": 60}
	maxDays := days[period]
	var rows *sql.Rows
	var err error
	if maxDays == 0 {
		rows, err = r.db.QueryContext(ctx, `SELECT dateKey, data FROM usageDaily`)
	} else {
		cutoff := localDateKey(time.Now().AddDate(0, 0, -maxDays+1))
		rows, err = r.db.QueryContext(ctx, `SELECT dateKey, data FROM usageDaily WHERE dateKey >= ?`, cutoff)
	}
	if err != nil {
		return agg, fmt.Errorf("usage aggregates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dateKey string
		var raw []byte
		if err := rows.Scan(&dateKey, &raw); err != nil {
			return agg, err
		}
		var day dayAggregate
		if err := json.Unmarshal(raw, &day); err != nil {
			continue
		}
		agg.TotalRequests += day.Requests
		for k, c := range day.ByProvider {
			addCounter(&agg.ByProvider, k, counterToUsageCounter(c))
		}
		for k, c := range day.ByModel {
			addCounter(&agg.ByModel, k, counterToUsageCounter(c))
		}
		for k, c := range day.ByAccount {
			addCounter(&agg.ByAccount, k, counterToUsageCounter(c))
		}
		for k, c := range day.ByAPIKey {
			addCounter(&agg.ByAPIKey, k, counterToUsageCounter(c))
		}
		for k, c := range day.ByEndpoint {
			addCounter(&agg.ByEndpoint, k, counterToUsageCounter(c))
		}
	}
	return agg, rows.Err()
}

func (r *UsageRepo) aggregatesFromHistory(ctx context.Context, period string) (usage.Aggregates, error) {
	var agg usage.Aggregates
	agg.ByProvider = map[string]usage.Counter{}
	agg.ByModel = map[string]usage.Counter{}
	agg.ByAccount = map[string]usage.Counter{}
	agg.ByAPIKey = map[string]usage.Counter{}
	agg.ByEndpoint = map[string]usage.Counter{}

	var cutoff time.Time
	if period == "today" {
		cutoff = time.Now().Truncate(24 * time.Hour)
	} else {
		cutoff = time.Now().Add(-24 * time.Hour)
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT timestamp, provider, model, connectionId, apiKey, endpoint, promptTokens, completionTokens, cost, streamMs, tps FROM usageHistory WHERE timestamp >= ?`,
		formatTime(cutoff))
	if err != nil {
		return agg, fmt.Errorf("usage aggregates history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ts string
		var rec usage.UsageRecord
		var prompt, completion int
		var streamMs sql.NullInt64
		var tps sql.NullFloat64
		if err := rows.Scan(&ts, &rec.Provider, &rec.Model, &rec.ConnectionID, &rec.APIKey, &rec.Endpoint, &prompt, &completion, &rec.Cost, &streamMs, &tps); err != nil {
			return agg, err
		}
		rec.Timestamp, _ = parseTime(ts)
		rec.PromptTokens = prompt
		rec.CompletionTokens = completion
		if streamMs.Valid {
			v := int(streamMs.Int64)
			rec.StreamMs = &v
		}
		if tps.Valid {
			v := tps.Float64
			rec.TPS = &v
		}
		agg.TotalRequests++
		addCounter(&agg.ByProvider, rec.Provider, usage.Counter{Requests: 1, PromptTokens: prompt, CompletionTokens: completion, Cost: rec.Cost})
		modelKey := rec.Provider + "|" + rec.Model
		addCounter(&agg.ByModel, modelKey, usage.Counter{Requests: 1, PromptTokens: prompt, CompletionTokens: completion, Cost: rec.Cost})
		if rec.ConnectionID != "" {
			addCounter(&agg.ByAccount, rec.ConnectionID, usage.Counter{Requests: 1, PromptTokens: prompt, CompletionTokens: completion, Cost: rec.Cost})
		}
		apiKeyKey := rec.APIKey
		if apiKeyKey == "" {
			apiKeyKey = "local-no-key"
		}
		addCounter(&agg.ByAPIKey, apiKeyKey, usage.Counter{Requests: 1, PromptTokens: prompt, CompletionTokens: completion, Cost: rec.Cost})
		epKey := rec.Endpoint
		if epKey == "" {
			epKey = "Unknown"
		}
		addCounter(&agg.ByEndpoint, epKey, usage.Counter{Requests: 1, PromptTokens: prompt, CompletionTokens: completion, Cost: rec.Cost})
	}
	return agg, rows.Err()
}

// RecentLogs returns formatted log lines like the JS getRecentLogs.
func (r *UsageRepo) RecentLogs(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT timestamp, provider, model, connectionId, promptTokens, completionTokens, status, tokens FROM usageHistory ORDER BY id DESC LIMIT ?`,
		limit)
	if err != nil {
		return nil, fmt.Errorf("usage recentLogs: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var ts, provider, model, status string
		var connID sql.NullString
		var prompt, completion int
		var tokens []byte
		if err := rows.Scan(&ts, &provider, &model, &connID, &prompt, &completion, &status, &tokens); err != nil {
			return nil, err
		}
		t, _ := parseTime(ts)
		if prompt == 0 {
			var tb tokenBlob
			_ = json.Unmarshal(tokens, &tb)
			prompt = firstNonZero(tb.Prompt, tb.Input)
		}
		if completion == 0 {
			var tb tokenBlob
			_ = json.Unmarshal(tokens, &tb)
			completion = firstNonZero(tb.Completion, tb.Output)
		}
		out = append(out, fmt.Sprintf("%s | %s | %s | %s | %d | %d | %s",
			formatLogDate(t), model, upper(provider), connID.String, prompt, completion, status))
	}
	return out, rows.Err()
}

func (r *UsageRepo) scanRow(rows *sql.Rows) (usage.UsageRecord, error) {
	var rec usage.UsageRecord
	var ts string
	var prompt, completion int
	var tokens, meta []byte
	var streamMs sql.NullInt64
	var tps sql.NullFloat64
	if err := rows.Scan(&ts, &rec.Provider, &rec.Model, &rec.ConnectionID, &rec.APIKey, &rec.Endpoint, &prompt, &completion, &rec.Cost, &rec.Status, &tokens, &meta, &streamMs, &tps); err != nil {
		return rec, fmt.Errorf("usage scan: %w", err)
	}
	rec.Timestamp, _ = parseTime(ts)
	rec.PromptTokens = prompt
	rec.CompletionTokens = completion
	rec.Tokens = json.RawMessage(tokens)
	rec.Meta = json.RawMessage(meta)
	if streamMs.Valid {
		v := int(streamMs.Int64)
		rec.StreamMs = &v
	}
	if tps.Valid {
		v := tps.Float64
		rec.TPS = &v
	}
	return rec, nil
}

func (r *UsageRepo) getDayTx(ctx context.Context, tx *sql.Tx, dateKey string) (*dayAggregate, error) {
	row := tx.QueryRowContext(ctx, `SELECT data FROM usageDaily WHERE dateKey = ?`, dateKey)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &dayAggregate{
				ByProvider: map[string]counter{},
				ByModel:    map[string]counter{},
				ByAccount:  map[string]counter{},
				ByAPIKey:   map[string]counter{},
				ByEndpoint: map[string]counter{},
			}, nil
		}
		return nil, fmt.Errorf("usage read day: %w", err)
	}
	var day dayAggregate
	if err := json.Unmarshal(raw, &day); err != nil {
		return nil, fmt.Errorf("usage unmarshal day: %w", err)
	}
	r.ensureDayMaps(&day)
	return &day, nil
}

func (r *UsageRepo) ensureDayMaps(day *dayAggregate) {
	if day.ByProvider == nil {
		day.ByProvider = map[string]counter{}
	}
	if day.ByModel == nil {
		day.ByModel = map[string]counter{}
	}
	if day.ByAccount == nil {
		day.ByAccount = map[string]counter{}
	}
	if day.ByAPIKey == nil {
		day.ByAPIKey = map[string]counter{}
	}
	if day.ByEndpoint == nil {
		day.ByEndpoint = map[string]counter{}
	}
}

type tokenBlob struct {
	Prompt     int `json:"prompt_tokens"`
	Input      int `json:"input_tokens"`
	Completion int `json:"completion_tokens"`
	Output     int `json:"output_tokens"`
}

type dayAggregate struct {
	Requests         int               `json:"requests"`
	PromptTokens     int               `json:"promptTokens"`
	CompletionTokens int               `json:"completionTokens"`
	CachedTokens     int               `json:"cachedTokens"`
	Cost             float64           `json:"cost"`
	StreamMsTotal    int               `json:"streamMsTotal"`
	TpsCount         int               `json:"tpsCount"`
	TpsSum           float64           `json:"tpsSum"`
	ByProvider       map[string]counter `json:"byProvider"`
	ByModel          map[string]counter `json:"byModel"`
	ByAccount        map[string]counter `json:"byAccount"`
	ByAPIKey         map[string]counter `json:"byApiKey"`
	ByEndpoint       map[string]counter `json:"byEndpoint"`
}

type counter struct {
	Requests         int     `json:"requests"`
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	CachedTokens     int     `json:"cachedTokens"`
	Cost             float64 `json:"cost"`
	TpsSum           float64 `json:"tpsSum"`
	TpsCount         int     `json:"tpsCount"`
}

func aggregateEntryToDay(day *dayAggregate, rec usage.UsageRecord) {
	var tb tokenBlob
	_ = json.Unmarshal(rec.Tokens, &tb)
	prompt := rec.PromptTokens
	completion := rec.CompletionTokens
	cached := tb.Input - prompt
	if cached < 0 {
		cached = 0
	}
	cost := rec.Cost

	day.Requests++
	day.PromptTokens += prompt
	day.CompletionTokens += completion
	day.CachedTokens += cached
	day.Cost += cost
	day.StreamMsTotal += valueOrZero(rec.StreamMs)
	if rec.TPS != nil {
		day.TpsCount++
		day.TpsSum += *rec.TPS
	}

	addCounterMap(&day.ByProvider, rec.Provider, counter{1, prompt, completion, cached, cost, valueOrZeroF(rec.TPS), countF(rec.TPS)})

	modelKey := rec.Model
	if rec.Provider != "" {
		modelKey = rec.Model + "|" + rec.Provider
	}
	addCounterMap(&day.ByModel, modelKey, counter{1, prompt, completion, cached, cost, valueOrZeroF(rec.TPS), countF(rec.TPS)})

	if rec.ConnectionID != "" {
		addCounterMap(&day.ByAccount, rec.ConnectionID, counter{1, prompt, completion, cached, cost, valueOrZeroF(rec.TPS), countF(rec.TPS)})
	}

	apiKeyKey := rec.APIKey
	if apiKeyKey == "" {
		apiKeyKey = "local-no-key"
	}
	addCounterMap(&day.ByAPIKey, apiKeyKey, counter{1, prompt, completion, cached, cost, valueOrZeroF(rec.TPS), countF(rec.TPS)})

	epKey := rec.Endpoint
	if epKey == "" {
		epKey = "Unknown"
	}
	addCounterMap(&day.ByEndpoint, epKey, counter{1, prompt, completion, cached, cost, valueOrZeroF(rec.TPS), countF(rec.TPS)})
}

func addCounter(m *map[string]usage.Counter, key string, add usage.Counter) {
	if *m == nil {
		*m = map[string]usage.Counter{}
	}
	c := (*m)[key]
	c.Requests += add.Requests
	c.PromptTokens += add.PromptTokens
	c.CompletionTokens += add.CompletionTokens
	c.Cost += add.Cost
	if add.TPSCount > 0 {
		c.TPSSum += add.TPSSum
		c.TPSCount += add.TPSCount
		a := c.TPSSum / float64(c.TPSCount)
		c.AvgTPS = &a
	}
	(*m)[key] = c
}

func addCounterMap(m *map[string]counter, key string, add counter) {
	if *m == nil {
		*m = map[string]counter{}
	}
	c := (*m)[key]
	c.Requests += add.Requests
	c.PromptTokens += add.PromptTokens
	c.CompletionTokens += add.CompletionTokens
	c.CachedTokens += add.CachedTokens
	c.Cost += add.Cost
	c.TpsSum += add.TpsSum
	c.TpsCount += add.TpsCount
	(*m)[key] = c
}

func counterToUsageCounter(c counter) usage.Counter {
	uc := usage.Counter{
		Requests:         c.Requests,
		PromptTokens:     c.PromptTokens,
		CompletionTokens: c.CompletionTokens,
		Cost:             c.Cost,
		TPSCount:         c.TpsCount,
		TPSSum:           c.TpsSum,
	}
	if c.TpsCount > 0 {
		a := c.TpsSum / float64(c.TpsCount)
		uc.AvgTPS = &a
	}
	return uc
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func valueOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func valueOrZeroF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func countF(p *float64) int {
	if p == nil {
		return 0
	}
	return 1
}

func localDateKey(t time.Time) string {
	y, m, d := t.Date()
	return fmt.Sprintf("%d-%02d-%02d", y, m, d)
}

func formatLogDate(t time.Time) string {
	return fmt.Sprintf("%02d-%02d-%04d %02d:%02d:%02d",
		t.Day(), t.Month(), t.Year(), t.Hour(), t.Minute(), t.Second())
}

func upper(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sortableDateKeys helps tests.
func sortDateKeys(keys []string) {
	sort.Strings(keys)
}
