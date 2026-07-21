package managedashboard

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// fakeComboRepo is an in-memory ComboService.Repo implementation.
type fakeComboRepo struct {
	listErr error
	byID    map[string]*settings.Combo
	byName  map[string]*settings.Combo
	// call counters
	creates  int
	updates  int
	deletes  int
	deleteID string
}

func (r *fakeComboRepo) List(ctx context.Context) ([]settings.Combo, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]settings.Combo, 0, len(r.byID))
	for _, c := range r.byID {
		out = append(out, *c)
	}
	return out, nil
}

func (r *fakeComboRepo) GetByID(ctx context.Context, id string) (*settings.Combo, error) {
	if c, ok := r.byID[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}

func (r *fakeComboRepo) GetByName(ctx context.Context, name string) (*settings.Combo, error) {
	if c, ok := r.byName[name]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}

func (r *fakeComboRepo) Create(ctx context.Context, c settings.Combo) error {
	r.creates++
	if r.byID == nil {
		r.byID = map[string]*settings.Combo{}
	}
	if r.byName == nil {
		r.byName = map[string]*settings.Combo{}
	}
	cp := c
	r.byID[c.ID] = &cp
	r.byName[c.Name] = &cp
	return nil
}

func (r *fakeComboRepo) Update(ctx context.Context, c settings.Combo) error {
	r.updates++
	if _, ok := r.byID[c.ID]; !ok {
		return errors.New("not found")
	}
	cp := c
	r.byID[c.ID] = &cp
	r.byName[c.Name] = &cp
	return nil
}

func (r *fakeComboRepo) Delete(ctx context.Context, id string) error {
	r.deletes++
	r.deleteID = id
	if c, ok := r.byID[id]; ok {
		delete(r.byID, id)
		delete(r.byName, c.Name)
	}
	return nil
}

func TestComboService_List(t *testing.T) {
	t.Parallel()
	repo := &fakeComboRepo{byID: map[string]*settings.Combo{
		"a": {ID: "a", Name: "Alpha"},
		"b": {ID: "b", Name: "Beta"},
	}}
	s := &ComboService{Repo: repo}

	got, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d combos, want 2", len(got))
	}
}

func TestComboService_List_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	s := &ComboService{Repo: &fakeComboRepo{listErr: sentinel}}
	if _, err := s.List(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestComboService_Get_GetByName(t *testing.T) {
	t.Parallel()
	repo := &fakeComboRepo{byID: map[string]*settings.Combo{
		"a": {ID: "a", Name: "Alpha"},
	}, byName: map[string]*settings.Combo{
		"Alpha": {ID: "a", Name: "Alpha"},
	}}
	s := &ComboService{Repo: repo}

	got, err := s.Get(context.Background(), "a")
	if err != nil || got == nil || got.ID != "a" {
		t.Fatalf("Get: err=%v got=%+v", err, got)
	}
	got, err = s.Get(context.Background(), "missing")
	if err != nil || got != nil {
		t.Fatalf("Get missing: err=%v got=%+v", err, got)
	}

	byName, err := s.GetByName(context.Background(), "Alpha")
	if err != nil || byName == nil || byName.ID != "a" {
		t.Fatalf("GetByName: err=%v got=%+v", err, byName)
	}
}

func TestComboService_Create(t *testing.T) {
	t.Parallel()
	repo := &fakeComboRepo{byID: map[string]*settings.Combo{}, byName: map[string]*settings.Combo{}}
	s := &ComboService{Repo: repo}

	in := settings.Combo{ID: "c1", Name: "Combo1"}
	got, err := s.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got == nil || got.ID != "c1" {
		t.Fatalf("Create returned %+v", got)
	}
	if repo.creates != 1 {
		t.Errorf("creates = %d, want 1", repo.creates)
	}
}

func TestComboService_Create_RepoError(t *testing.T) {
	t.Parallel()
	repo := &fakeComboRepo{
		byID: map[string]*settings.Combo{},
		byName: map[string]*settings.Combo{},
	}
	// Override Create to fail by wrapping.
	s := &ComboService{Repo: &failingComboCreate{repo}}
	if _, err := s.Create(context.Background(), settings.Combo{ID: "x"}); err == nil {
		t.Fatal("expected error from failing create")
	}
}

type failingComboCreate struct{ inner *fakeComboRepo }

func (f *failingComboCreate) List(ctx context.Context) ([]settings.Combo, error) {
	return f.inner.List(ctx)
}
func (f *failingComboCreate) GetByID(ctx context.Context, id string) (*settings.Combo, error) {
	return f.inner.GetByID(ctx, id)
}
func (f *failingComboCreate) GetByName(ctx context.Context, name string) (*settings.Combo, error) {
	return f.inner.GetByName(ctx, name)
}
func (f *failingComboCreate) Create(ctx context.Context, c settings.Combo) error {
	return errors.New("create failed")
}
func (f *failingComboCreate) Update(ctx context.Context, c settings.Combo) error {
	return f.inner.Update(ctx, c)
}
func (f *failingComboCreate) Delete(ctx context.Context, id string) error {
	return f.inner.Delete(ctx, id)
}

func TestComboService_Update(t *testing.T) {
	t.Parallel()
	repo := &fakeComboRepo{
		byID:   map[string]*settings.Combo{"u1": {ID: "u1", Name: "Old"}},
		byName: map[string]*settings.Combo{"Old": {ID: "u1", Name: "Old"}},
	}
	s := &ComboService{Repo: repo}

	got, err := s.Update(context.Background(), settings.Combo{ID: "u1", Name: "New"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got == nil || got.Name != "New" {
		t.Fatalf("Update returned %+v", got)
	}
	if repo.updates != 1 {
		t.Errorf("updates = %d, want 1", repo.updates)
	}
}

func TestComboService_Update_NotFound(t *testing.T) {
	t.Parallel()
	repo := &fakeComboRepo{byID: map[string]*settings.Combo{}, byName: map[string]*settings.Combo{}}
	s := &ComboService{Repo: repo}
	if _, err := s.Update(context.Background(), settings.Combo{ID: "ghost"}); err == nil {
		t.Fatal("expected error updating missing combo")
	}
}

func TestComboService_Delete(t *testing.T) {
	t.Parallel()
	repo := &fakeComboRepo{
		byID:   map[string]*settings.Combo{"d1": {ID: "d1", Name: "Del"}},
		byName: map[string]*settings.Combo{"Del": {ID: "d1", Name: "Del"}},
	}
	s := &ComboService{Repo: repo}
	if err := s.Delete(context.Background(), "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if repo.deletes != 1 || repo.deleteID != "d1" {
		t.Errorf("delete state = %d/%q, want 1/d1", repo.deletes, repo.deleteID)
	}
	if _, ok := repo.byID["d1"]; ok {
		t.Error("combo not removed from byID")
	}
}

func TestFakeComboRepo_DedupSanity(t *testing.T) {
	// Sanity check the fake's map semantics so the assertions above are meaningful.
	t.Parallel()
	r := &fakeComboRepo{
		byID:   map[string]*settings.Combo{"x": {ID: "x", Name: "X"}},
		byName: map[string]*settings.Combo{"X": {ID: "x", Name: "X"}},
	}
	got, _ := r.List(context.Background())
	if !reflect.DeepEqual([]string{"x"}, func() []string {
		var ids []string
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		return ids
	}()) {
		t.Fatalf("unexpected list: %+v", got)
	}
}