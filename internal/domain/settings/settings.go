// Package settings defines the configuration entities stored in SQLite.
// Shapes are ported 1:1 from src/lib/db/schema.js TABLES.
package settings

import (
	"encoding/json"
	"time"
)

// Settings holds the single-row application settings object. The JS backend
// stores this as a JSON blob in the settings.data column.
type Settings struct {
	ID   int             `json:"id"`
	Data json.RawMessage `json:"data"`
}

// Combo is a model alias group.
type Combo struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind,omitempty"`
	Models    json.RawMessage `json:"models"` // stored as JSON array
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// ProxyPool is an upstream proxy pool definition.
type ProxyPool struct {
	ID         string          `json:"id"`
	IsActive   bool            `json:"isActive"`
	TestStatus string          `json:"testStatus,omitempty"`
	Data       json.RawMessage `json:"data"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// APIKey is a client API key used to authenticate proxy requests.
type APIKey struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	Name      string    `json:"name,omitempty"`
	MachineID string    `json:"machineId,omitempty"`
	IsActive  bool      `json:"isActive"`
	CreatedAt time.Time `json:"createdAt"`
}

// ProviderConnection is a single upstream account (OAuth, API key, access
// token) along with provider-specific metadata stored in Data.
type ProviderConnection struct {
	ID         string          `json:"id"`
	Provider   string          `json:"provider"`
	AuthType   string          `json:"authType"`
	Name       string          `json:"name,omitempty"`
	Email      string          `json:"email,omitempty"`
	Priority   int             `json:"priority,omitempty"`
	IsActive   bool            `json:"isActive"`
	Data       json.RawMessage `json:"data"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// ProviderNode is a provider/node definition (base URLs, aliases, prefixes).
type ProviderNode struct {
	ID        string          `json:"id"`
	Type      string          `json:"type,omitempty"`
	Name      string          `json:"name,omitempty"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}
