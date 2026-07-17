package managedashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// Sentinel errors returned by ProxyPoolService.
var (
	ErrProxyPoolNotFound = errors.New("proxy pool not found")
	ErrProxyPoolInUse    = errors.New("proxy pool is currently in use")
)

// ProxyPoolService exposes proxy pool management operations.
type ProxyPoolService struct {
	Repo interface {
		List(ctx context.Context, filter repo.ProxyPoolFilter) ([]settings.ProxyPool, error)
		GetByID(ctx context.Context, id string) (*settings.ProxyPool, error)
		Create(ctx context.Context, p settings.ProxyPool) error
		Update(ctx context.Context, p settings.ProxyPool) error
		Delete(ctx context.Context, id string) (*settings.ProxyPool, error)
	}
	ConnectionRepo interface {
		List(ctx context.Context, filter repo.ConnectionFilter) ([]settings.ProviderConnection, error)
	}
}

// List returns proxy pools, optionally enriched with bound connection counts.
func (s *ProxyPoolService) List(ctx context.Context, isActive *bool, includeUsage bool) ([]settings.ProxyPool, error) {
	pools, err := s.Repo.List(ctx, repo.ProxyPoolFilter{IsActive: isActive})
	if err != nil {
		return nil, err
	}
	if !includeUsage {
		return pools, nil
	}
	conns, err := s.ConnectionRepo.List(ctx, repo.ConnectionFilter{})
	if err != nil {
		return nil, err
	}
	usage := map[string]int{}
	for _, c := range conns {
		var data map[string]any
		_ = json.Unmarshal(c.Data, &data)
		if v, ok := data["proxyPoolId"].(string); ok && v != "" {
			usage[v]++
		}
	}
	for i := range pools {
		count := usage[pools[i].ID]
		if count == 0 {
			continue
		}
		m := map[string]any{}
		_ = json.Unmarshal(pools[i].Data, &m)
		m["boundConnectionCount"] = count
		pools[i].Data, _ = json.Marshal(m)
	}
	return pools, nil
}

// Get returns a single proxy pool.
func (s *ProxyPoolService) Get(ctx context.Context, id string) (*settings.ProxyPool, error) {
	return s.Repo.GetByID(ctx, id)
}

// Create persists a new proxy pool.
func (s *ProxyPoolService) Create(ctx context.Context, p settings.ProxyPool) (*settings.ProxyPool, error) {
	if err := s.Repo.Create(ctx, p); err != nil {
		return nil, err
	}
	return s.Repo.GetByID(ctx, p.ID)
}

// Update persists proxy pool changes.
func (s *ProxyPoolService) Update(ctx context.Context, p settings.ProxyPool) (*settings.ProxyPool, error) {
	if err := s.Repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return s.Repo.GetByID(ctx, p.ID)
}

// Delete removes a proxy pool unless it is bound to active connections.
func (s *ProxyPoolService) Delete(ctx context.Context, id string) error {
	pool, err := s.Repo.Delete(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrProxyPoolNotFound
		}
		return err
	}
	if pool == nil {
		return ErrProxyPoolNotFound
	}
	conns, err := s.ConnectionRepo.List(ctx, repo.ConnectionFilter{})
	if err != nil {
		return fmt.Errorf("count connections: %w", err)
	}
	for _, c := range conns {
		var data map[string]any
		_ = json.Unmarshal(c.Data, &data)
		if v, ok := data["proxyPoolId"].(string); ok && v == id {
			return ErrProxyPoolInUse
		}
	}
	return nil
}
