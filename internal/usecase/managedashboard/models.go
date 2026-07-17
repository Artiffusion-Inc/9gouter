package managedashboard

import (
	"context"
	"encoding/json"

	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
)

// ModelService exposes alias, custom model, and disabled model operations.
type ModelService struct {
	AliasRepo    interface {
		GetAliases(ctx context.Context) (map[string]string, error)
		SetAlias(ctx context.Context, alias, model string) error
		DeleteAlias(ctx context.Context, alias string) error
		GetCustomModels(ctx context.Context) ([]repo.CustomModel, error)
		AddCustomModel(ctx context.Context, cm repo.CustomModel) (bool, error)
		DeleteCustomModel(ctx context.Context, providerAlias, id, typ string) error
	}
	DisabledRepo interface {
		GetAll(ctx context.Context) (map[string][]string, error)
		GetByProvider(ctx context.Context, providerAlias string) ([]string, error)
		Disable(ctx context.Context, providerAlias string, ids []string) error
		Enable(ctx context.Context, providerAlias string, ids []string) error
	}
}

// Aliases returns all model aliases.
func (s *ModelService) Aliases(ctx context.Context) (map[string]string, error) {
	return s.AliasRepo.GetAliases(ctx)
}

// SetAlias maps an alias to a model id.
func (s *ModelService) SetAlias(ctx context.Context, model, alias string) error {
	return s.AliasRepo.SetAlias(ctx, alias, model)
}

// DeleteAlias removes a model alias.
func (s *ModelService) DeleteAlias(ctx context.Context, alias string) error {
	return s.AliasRepo.DeleteAlias(ctx, alias)
}

// CustomModels lists user-defined custom models.
func (s *ModelService) CustomModels(ctx context.Context) ([]repo.CustomModel, error) {
	return s.AliasRepo.GetCustomModels(ctx)
}

// AddCustom registers a custom model with defaults.
func (s *ModelService) AddCustom(ctx context.Context, providerAlias, id, typ, name string) (bool, error) {
	if typ == "" {
		typ = "llm"
	}
	if name == "" {
		name = id
	}
	return s.AliasRepo.AddCustomModel(ctx, repo.CustomModel{
		ProviderAlias: providerAlias,
		ID:            id,
		Type:          typ,
		Name:          name,
	})
}

// DeleteCustom removes a custom model.
func (s *ModelService) DeleteCustom(ctx context.Context, providerAlias, id, typ string) error {
	if typ == "" {
		typ = "llm"
	}
	return s.AliasRepo.DeleteCustomModel(ctx, providerAlias, id, typ)
}

// Disabled returns all disabled models grouped by provider alias.
func (s *ModelService) Disabled(ctx context.Context) (map[string][]string, error) {
	return s.DisabledRepo.GetAll(ctx)
}

// DisabledByProvider returns disabled ids for one provider.
func (s *ModelService) DisabledByProvider(ctx context.Context, providerAlias string) ([]string, error) {
	return s.DisabledRepo.GetByProvider(ctx, providerAlias)
}

// Disable marks models as disabled for a provider.
func (s *ModelService) Disable(ctx context.Context, providerAlias string, ids []string) error {
	return s.DisabledRepo.Disable(ctx, providerAlias, ids)
}

// Enable removes models from the disabled list.
func (s *ModelService) Enable(ctx context.Context, providerAlias string, ids []string) error {
	return s.DisabledRepo.Enable(ctx, providerAlias, ids)
}

var _ = json.Marshal
