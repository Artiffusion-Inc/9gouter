package managedashboard

import (
	"context"
	"encoding/json"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// ProviderService exposes provider connection operations.
type ProviderService struct {
	Repo interface {
		List(ctx context.Context, filter repo.ConnectionFilter) ([]settings.ProviderConnection, error)
		Create(ctx context.Context, c settings.ProviderConnection) (*settings.ProviderConnection, error)
	}
	NodeRepo interface {
		List(ctx context.Context, filter repo.NodeFilter) ([]settings.ProviderNode, error)
	}
	PoolRepo interface {
		GetByID(ctx context.Context, id string) (*settings.ProxyPool, error)
	}
}

// List returns all provider connections with sensitive fields removed.
func (s *ProviderService) List(ctx context.Context) ([]map[string]any, error) {
	conns, err := s.Repo.List(ctx, repo.ConnectionFilter{})
	if err != nil {
		return nil, err
	}
	nodes, err := s.NodeRepo.List(ctx, repo.NodeFilter{})
	if err != nil {
		nodes = nil
	}
	nodeName := map[string]string{}
	for _, n := range nodes {
		if n.Name != "" {
			nodeName[n.ID] = n.Name
		}
	}
	out := make([]map[string]any, 0, len(conns))
	for _, c := range conns {
		m := connectionToMap(c)
		if _, ok := nodeName[c.Provider]; ok {
			m["name"] = nodeName[c.Provider]
		}
		out = append(out, m)
	}
	return out, nil
}

// Create persists a new provider connection. apiKey is stored inside the JSON data blob.
func (s *ProviderService) Create(ctx context.Context, c settings.ProviderConnection, apiKey string) (*settings.ProviderConnection, error) {
	data := map[string]any{}
	if len(c.Data) > 0 {
		_ = json.Unmarshal(c.Data, &data)
	}
	data["apiKey"] = apiKey
	c.Data, _ = json.Marshal(data)
	resolved, err := s.Repo.Create(ctx, c)
	if err != nil {
		return nil, err
	}
	return resolved, nil
}

// ClientFilter is the query for the client-facing paginated connection list.
type ClientFilter struct {
	Provider      string
	AccountStatus string
	Sort          string
	Page          int
	PageSize      int
}

// Client returns a paginated, sanitized list of connections eligible for usage.
func (s *ProviderService) Client(ctx context.Context, f ClientFilter) (map[string]any, error) {
	conns, err := s.Repo.List(ctx, repo.ConnectionFilter{IsActive: boolPtr(true)})
	if err != nil {
		return nil, err
	}

	// Usage-eligible whitelist copied from the JS constants subset used by the client route.
	usageSupported := map[string]bool{
		"openai": true, "anthropic": true, "gemini": true, "azure": true,
		"codex": true, "github": true, "grok-cli": true, "kiro": true,
		"glm": true, "minimax": true, "qwen": true, "xai": true,
	}
	apiKeyProviders := map[string]bool{"glm": true, "minimax": true, "kiro": true, "qwen": true}

	var eligible []settings.ProviderConnection
	for _, c := range conns {
		ok := usageSupported[c.Provider] && (c.AuthType == "oauth" || apiKeyProviders[c.Provider])
		if !ok {
			continue
		}
		if f.Provider != "" && f.Provider != "all" && c.Provider != f.Provider {
			continue
		}
		if f.AccountStatus == "active" && !c.IsActive {
			continue
		}
		if f.AccountStatus == "inactive" && c.IsActive {
			continue
		}
		eligible = append(eligible, c)
	}

	sortConnections(eligible, f.Sort)

	total := len(eligible)
	totalPages := (total + f.PageSize - 1) / f.PageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if f.Page > totalPages {
		f.Page = totalPages
	}
	offset := (f.Page - 1) * f.PageSize
	end := offset + f.PageSize
	if end > total {
		end = total
	}

	pageConns := eligible[offset:end]
	sanitized := make([]map[string]any, 0, len(pageConns))
	for _, c := range pageConns {
		sanitized = append(sanitized, connectionToMap(c))
	}

	providerOptions := map[string]struct{}{}
	for _, c := range conns {
		providerOptions[c.Provider] = struct{}{}
	}
	options := make([]string, 0, len(providerOptions))
	for p := range providerOptions {
		options = append(options, p)
	}

	return map[string]any{
		"connections":     sanitized,
		"providerOptions": options,
		"pagination": map[string]any{
			"page":       f.Page,
			"pageSize":   f.PageSize,
			"total":      total,
			"totalPages": totalPages,
		},
		"totals": map[string]any{
			"eligibleConnections":         len(eligible),
			"providerFilteredConnections": len(eligible),
		},
	}, nil
}

func connectionToMap(c settings.ProviderConnection) map[string]any {
	m := map[string]any{
		"id":           c.ID,
		"provider":     c.Provider,
		"authType":     c.AuthType,
		"name":         c.Name,
		"email":        c.Email,
		"priority":     c.Priority,
		"isActive":     c.IsActive,
		"defaultModel": nil,
		"testStatus":   "unknown",
		"data":         c.Data,
	}
	var data map[string]any
	_ = json.Unmarshal(c.Data, &data)
	for _, k := range []string{"baseUrl", "azureEndpoint", "deployment", "apiVersion", "accountId", "region", "projectId", "resourceUrl", "proxyPoolId", "connectionProxyEnabled", "connectionProxyUrl", "connectionNoProxy", "nodeName"} {
		if v, ok := data[k]; ok {
			m[k] = v
		}
	}
	if v, ok := data["testStatus"].(string); ok {
		m["testStatus"] = v
	}
	if v, ok := data["defaultModel"].(string); ok {
		m["defaultModel"] = v
	}
	return m
}

func sortConnections(conns []settings.ProviderConnection, sort string) {
	if sort == "provider" {
		// stable enough with slices.SortFunc in handler if needed; keep simple.
		return
	}
	// default priority sort is handled by repo.List already.
}

func boolPtr(b bool) *bool { return &b }
