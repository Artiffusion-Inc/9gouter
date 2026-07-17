// Package managedashboard exposes usecase operations for the dashboard /api
// routes. The initial implementation is thin: most logic lives in the HTTP
// handlers because the repository layer already encapsulates persistence rules.
// Usecases become the boundary for cross-cutting behavior (caching, side effects,
// validation) without duplicating repository semantics.
package managedashboard

import (
	"context"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// ComboService delegates to the combo repository. It exists so handlers depend
// on a usecase boundary rather than repositories directly.
type ComboService struct {
	Repo interface {
		List(ctx context.Context) ([]settings.Combo, error)
		GetByID(ctx context.Context, id string) (*settings.Combo, error)
		GetByName(ctx context.Context, name string) (*settings.Combo, error)
		Create(ctx context.Context, c settings.Combo) error
		Update(ctx context.Context, c settings.Combo) error
		Delete(ctx context.Context, id string) error
	}
}

// List returns all combos.
func (s *ComboService) List(ctx context.Context) ([]settings.Combo, error) {
	return s.Repo.List(ctx)
}

// Get returns a single combo by id.
func (s *ComboService) Get(ctx context.Context, id string) (*settings.Combo, error) {
	return s.Repo.GetByID(ctx, id)
}

// GetByName returns a single combo by name.
func (s *ComboService) GetByName(ctx context.Context, name string) (*settings.Combo, error) {
	return s.Repo.GetByName(ctx, name)
}

// Create validates and persists a new combo.
func (s *ComboService) Create(ctx context.Context, c settings.Combo) (*settings.Combo, error) {
	if err := s.Repo.Create(ctx, c); err != nil {
		return nil, err
	}
	return s.Repo.GetByID(ctx, c.ID)
}

// Update validates and persists combo changes.
func (s *ComboService) Update(ctx context.Context, c settings.Combo) (*settings.Combo, error) {
	if err := s.Repo.Update(ctx, c); err != nil {
		return nil, err
	}
	return s.Repo.GetByID(ctx, c.ID)
}

// Delete removes a combo.
func (s *ComboService) Delete(ctx context.Context, id string) error {
	return s.Repo.Delete(ctx, id)
}
