// Package usage defines usage recording entities and the repository port.
// It ports the usageHistory accounting from src/lib/db/repos/usageRepo.js.
package usage

import (
	"context"
	"encoding/json"
	"time"
)

// UsageRecord is a single row to persist in usageHistory.
type UsageRecord struct {
	Timestamp        time.Time       `json:"timestamp"`
	Provider         string          `json:"provider"`
	Model            string          `json:"model"`
	ConnectionID     string          `json:"connectionId,omitempty"`
	APIKey           string          `json:"apiKey,omitempty"`
	Endpoint         string          `json:"endpoint,omitempty"`
	PromptTokens     int             `json:"promptTokens"`
	CompletionTokens int             `json:"completionTokens"`
	Cost             float64         `json:"cost"`
	Status           string          `json:"status"`
	StreamMs         *int            `json:"streamMs,omitempty"`
	TPS              *float64        `json:"tps,omitempty"`
	Meta             json.RawMessage `json:"meta,omitempty"`
	Tokens           json.RawMessage `json:"tokens,omitempty"`
}

// Query filters usage history.
type Query struct {
	Provider  string
	Model     string
	StartDate time.Time
	EndDate   time.Time
	Limit     int
	Offset    int
}

// Aggregates holds rollup statistics over a date range.
type Aggregates struct {
	TotalRequests         int
	TotalPromptTokens     int
	TotalCompletionTokens int
	TotalCost             float64
	ByProvider            map[string]Counter
	ByModel               map[string]Counter
	ByAccount             map[string]Counter
	ByAPIKey              map[string]Counter
	ByEndpoint            map[string]Counter
}

// Counter is an aggregate bucket used inside Aggregates.
type Counter struct {
	Requests         int
	PromptTokens     int
	CompletionTokens int
	Cost             float64
	TPSCount         int
	TPSSum           float64
	AvgTPS           *float64
}

// Repo is the domain port for persistence of usage records.
type Repo interface {
	Save(ctx context.Context, r UsageRecord) error
	// SaveDedup inserts the record but skips the insert (and returns inserted=false)
	// when an identical row already exists in usageHistory, mirroring the JS
	// saveRequestUsage dedup (decolua/9router #2509 / 0d216689). On a duplicate it
	// still backfills the endpoint onto the existing row when the existing row has
	// none and the incoming record does. inserted=true means a new row was written
	// (and the caller should publish a stats update).
	SaveDedup(ctx context.Context, r UsageRecord) (inserted bool, err error)
	Query(ctx context.Context, q Query) ([]UsageRecord, error)
	Aggregates(ctx context.Context, period string) (Aggregates, error)
}
