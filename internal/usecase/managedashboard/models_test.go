package managedashboard

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
)

// fakeAliasRepo implements ModelService.AliasRepo.
type fakeAliasRepo struct {
	aliases      map[string]string
	aliasSetErr  error
	setCalls     [][2]string // (alias, model)
	deleteCalls  []string
	custom       []repo.CustomModel
	addedCalls   []repo.CustomModel
	addOk        bool
	addErr       error
	deletedCalls []struct{ ProviderAlias, ID, Type string }
}

func (r *fakeAliasRepo) GetAliases(ctx context.Context) (map[string]string, error) {
	cp := map[string]string{}
	for k, v := range r.aliases {
		cp[k] = v
	}
	return cp, nil
}

func (r *fakeAliasRepo) SetAlias(ctx context.Context, alias, model string) error {
	r.setCalls = append(r.setCalls, [2]string{alias, model})
	if r.aliasSetErr != nil {
		return r.aliasSetErr
	}
	if r.aliases == nil {
		r.aliases = map[string]string{}
	}
	r.aliases[alias] = model
	return nil
}

func (r *fakeAliasRepo) DeleteAlias(ctx context.Context, alias string) error {
	r.deleteCalls = append(r.deleteCalls, alias)
	delete(r.aliases, alias)
	return nil
}

func (r *fakeAliasRepo) GetCustomModels(ctx context.Context) ([]repo.CustomModel, error) {
	return r.custom, nil
}

func (r *fakeAliasRepo) AddCustomModel(ctx context.Context, cm repo.CustomModel) (bool, error) {
	r.addedCalls = append(r.addedCalls, cm)
	if r.addErr != nil {
		return false, r.addErr
	}
	return r.addOk, nil
}

func (r *fakeAliasRepo) DeleteCustomModel(ctx context.Context, providerAlias, id, typ string) error {
	r.deletedCalls = append(r.deletedCalls, struct{ ProviderAlias, ID, Type string }{providerAlias, id, typ})
	return nil
}

// fakeDisabledRepo implements ModelService.DisabledRepo.
type fakeDisabledRepo struct {
	all         map[string][]string
	byProvider  map[string][]string
	disableErr  error
	enabledErr  error
	disableCall struct {
		provider string
		ids      []string
	}
	enableCall struct {
		provider string
		ids      []string
	}
}

func (r *fakeDisabledRepo) GetAll(ctx context.Context) (map[string][]string, error) {
	cp := map[string][]string{}
	for k, v := range r.all {
		cp[k] = append([]string(nil), v...)
	}
	return cp, nil
}

func (r *fakeDisabledRepo) GetByProvider(ctx context.Context, providerAlias string) ([]string, error) {
	return r.byProvider[providerAlias], nil
}

func (r *fakeDisabledRepo) Disable(ctx context.Context, providerAlias string, ids []string) error {
	r.disableCall.provider = providerAlias
	r.disableCall.ids = append([]string(nil), ids...)
	return r.disableErr
}

func (r *fakeDisabledRepo) Enable(ctx context.Context, providerAlias string, ids []string) error {
	r.enableCall.provider = providerAlias
	r.enableCall.ids = append([]string(nil), ids...)
	return r.enabledErr
}

func TestModelService_Aliases(t *testing.T) {
	t.Parallel()
	repo := &fakeAliasRepo{aliases: map[string]string{"a": "model-a", "b": "model-b"}}
	s := &ModelService{AliasRepo: repo}

	got, err := s.Aliases(context.Background())
	if err != nil {
		t.Fatalf("Aliases: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]string{"a": "model-a", "b": "model-b"}) {
		t.Fatalf("got %v", got)
	}
}

func TestModelService_SetAlias(t *testing.T) {
	t.Parallel()
	repo := &fakeAliasRepo{}
	s := &ModelService{AliasRepo: repo}

	// Note: SetAlias(model, alias) flips the argument order before delegating.
	if err := s.SetAlias(context.Background(), "model-x", "alias-y"); err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	if len(repo.setCalls) != 1 {
		t.Fatalf("setCalls = %v", repo.setCalls)
	}
	if repo.setCalls[0] != [2]string{"alias-y", "model-x"} {
		t.Errorf("args = %v, want (alias-y, model-x)", repo.setCalls[0])
	}
}

func TestModelService_SetAlias_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("set failed")
	s := &ModelService{AliasRepo: &fakeAliasRepo{aliasSetErr: sentinel}}
	if err := s.SetAlias(context.Background(), "m", "a"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestModelService_DeleteAlias(t *testing.T) {
	t.Parallel()
	repo := &fakeAliasRepo{aliases: map[string]string{"a": "m"}}
	s := &ModelService{AliasRepo: repo}
	if err := s.DeleteAlias(context.Background(), "a"); err != nil {
		t.Fatalf("DeleteAlias: %v", err)
	}
	if len(repo.deleteCalls) != 1 || repo.deleteCalls[0] != "a" {
		t.Errorf("deleteCalls = %v", repo.deleteCalls)
	}
}

func TestModelService_CustomModels(t *testing.T) {
	t.Parallel()
	want := []repo.CustomModel{{ProviderAlias: "p", ID: "id1", Type: "llm", Name: "id1"}}
	s := &ModelService{AliasRepo: &fakeAliasRepo{custom: want}}
	got, err := s.CustomModels(context.Background())
	if err != nil {
		t.Fatalf("CustomModels: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestModelService_AddCustom_Defaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
	 testName                         string
	 providerAlias, id, typ, nameVal string
	 wantTyp, wantName                string
	}{
		{"all empty", "p", "m1", "", "", "llm", "m1"},
		{"explicit", "p", "m2", "embedding", "My Model", "embedding", "My Model"},
		{"only typ", "p", "m3", "embedding", "", "embedding", "m3"},
		{"only name", "p", "m4", "", "My Model", "llm", "My Model"},
	}
	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			repo := &fakeAliasRepo{addOk: true}
			s := &ModelService{AliasRepo: repo}
			ok, err := s.AddCustom(context.Background(), tc.providerAlias, tc.id, tc.typ, tc.nameVal)
			if err != nil {
				t.Fatalf("AddCustom: %v", err)
			}
			if !ok {
				t.Fatal("expected ok=true")
			}
			if len(repo.addedCalls) != 1 {
				t.Fatalf("addedCalls = %v", repo.addedCalls)
			}
			got := repo.addedCalls[0]
			if got.ProviderAlias != tc.providerAlias || got.ID != tc.id {
				t.Errorf("passthrough = %+v", got)
			}
			if got.Type != tc.wantTyp {
				t.Errorf("Type = %q, want %q", got.Type, tc.wantTyp)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
		})
	}
}

func TestModelService_AddCustom_PropagatesError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("add failed")
	s := &ModelService{AliasRepo: &fakeAliasRepo{addErr: sentinel}}
	if _, err := s.AddCustom(context.Background(), "p", "id", "llm", "n"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestModelService_DeleteCustom_DefaultTyp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		typ  string
		want string
	}{
		{"empty defaults to llm", "", "llm"},
		{"explicit preserved", "embedding", "embedding"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeAliasRepo{}
			s := &ModelService{AliasRepo: repo}
			if err := s.DeleteCustom(context.Background(), "p", "m", tc.typ); err != nil {
				t.Fatalf("DeleteCustom: %v", err)
			}
			if len(repo.deletedCalls) != 1 {
				t.Fatalf("deletedCalls = %v", repo.deletedCalls)
			}
			if repo.deletedCalls[0].Type != tc.want {
				t.Errorf("Type = %q, want %q", repo.deletedCalls[0].Type, tc.want)
			}
		})
	}
}

func TestModelService_Disabled(t *testing.T) {
	t.Parallel()
	repo := &fakeDisabledRepo{
		all:        map[string][]string{"p1": {"a", "b"}},
		byProvider: map[string][]string{"p1": {"a", "b"}},
	}
	s := &ModelService{DisabledRepo: repo}

	all, err := s.Disabled(context.Background())
	if err != nil {
		t.Fatalf("Disabled: %v", err)
	}
	if !reflect.DeepEqual(all, map[string][]string{"p1": {"a", "b"}}) {
		t.Fatalf("got %v", all)
	}

	one, err := s.DisabledByProvider(context.Background(), "p1")
	if err != nil {
		t.Fatalf("DisabledByProvider: %v", err)
	}
	if !reflect.DeepEqual(one, []string{"a", "b"}) {
		t.Fatalf("got %v", one)
	}
}

func TestModelService_DisableEnable(t *testing.T) {
	t.Parallel()
	repo := &fakeDisabledRepo{}
	s := &ModelService{DisabledRepo: repo}

	if err := s.Disable(context.Background(), "p1", []string{"x", "y"}); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if repo.disableCall.provider != "p1" || !reflect.DeepEqual(repo.disableCall.ids, []string{"x", "y"}) {
		t.Errorf("disable call = %+v", repo.disableCall)
	}

	if err := s.Enable(context.Background(), "p1", []string{"x"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if repo.enableCall.provider != "p1" || !reflect.DeepEqual(repo.enableCall.ids, []string{"x"}) {
		t.Errorf("enable call = %+v", repo.enableCall)
	}
}

func TestModelService_Disable_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("disable failed")
	s := &ModelService{DisabledRepo: &fakeDisabledRepo{disableErr: sentinel}}
	if err := s.Disable(context.Background(), "p", []string{"x"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestModelService_Enable_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("enable failed")
	s := &ModelService{DisabledRepo: &fakeDisabledRepo{enabledErr: sentinel}}
	if err := s.Enable(context.Background(), "p", []string{"x"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}