package managedashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// fakeProxyPoolRepo implements ProxyPoolService.Repo.
type fakeProxyPoolRepo struct {
	pools      []settings.ProxyPool
	byID       map[string]*settings.ProxyPool
	listErr    error
	getErr     error
	createErr  error
	updateErr  error
	deletePool *settings.ProxyPool
	deleteErr  error
	createdID  string
	updatedID  string
}

func (r *fakeProxyPoolRepo) List(ctx context.Context, filter repo.ProxyPoolFilter) ([]settings.ProxyPool, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	if filter.IsActive == nil {
		return r.pools, nil
	}
	var out []settings.ProxyPool
	for _, p := range r.pools {
		if p.IsActive != *filter.IsActive {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (r *fakeProxyPoolRepo) GetByID(ctx context.Context, id string) (*settings.ProxyPool, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if p, ok := r.byID[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, nil
}

func (r *fakeProxyPoolRepo) Create(ctx context.Context, p settings.ProxyPool) error {
	r.createdID = p.ID
	if r.createErr != nil {
		return r.createErr
	}
	return nil
}

func (r *fakeProxyPoolRepo) Update(ctx context.Context, p settings.ProxyPool) error {
	r.updatedID = p.ID
	if r.updateErr != nil {
		return r.updateErr
	}
	if r.byID != nil {
		cp := p
		r.byID[p.ID] = &cp
	}
	return nil
}

func (r *fakeProxyPoolRepo) Delete(ctx context.Context, id string) (*settings.ProxyPool, error) {
	if r.deleteErr != nil {
		return nil, r.deleteErr
	}
	if r.deletePool != nil {
		cp := *r.deletePool
		return &cp, nil
	}
	return nil, nil
}

// fakeConnListRepo implements ProxyPoolService.ConnectionRepo (List only).
type fakeConnListRepo struct {
	conns []settings.ProviderConnection
	err   error
}

func (r *fakeConnListRepo) List(ctx context.Context, filter repo.ConnectionFilter) ([]settings.ProviderConnection, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.conns, nil
}

func poolData(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestProxyPoolService_List_NoUsage(t *testing.T) {
	t.Parallel()
	crepo := &fakeProxyPoolRepo{pools: []settings.ProxyPool{{ID: "p1", Data: poolData(t, map[string]any{})}}}
	s := &ProxyPoolService{Repo: crepo, ConnectionRepo: &fakeConnListRepo{}}

	out, err := s.List(context.Background(), nil, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 || out[0].ID != "p1" {
		t.Fatalf("got %+v", out)
	}
}

func TestProxyPoolService_List_RepoError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list fail")
	s := &ProxyPoolService{Repo: &fakeProxyPoolRepo{listErr: sentinel}, ConnectionRepo: &fakeConnListRepo{}}
	if _, err := s.List(context.Background(), nil, false); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestProxyPoolService_List_WithUsage_Enriches(t *testing.T) {
	t.Parallel()
	crepo := &fakeProxyPoolRepo{
		pools: []settings.ProxyPool{{ID: "pp1", Data: poolData(t, map[string]any{"name": "Pool1"})}},
	}
	connRepo := &fakeConnListRepo{
		conns: []settings.ProviderConnection{
			{ID: "c1", Data: poolData(t, map[string]any{"proxyPoolId": "pp1"})},
			{ID: "c2", Data: poolData(t, map[string]any{"proxyPoolId": "pp1"})},
			{ID: "c3", Data: poolData(t, map[string]any{"proxyPoolId": "pp2"})},
			{ID: "c4", Data: poolData(t, map[string]any{})},
		},
	}
	s := &ProxyPoolService{Repo: crepo, ConnectionRepo: connRepo}

	out, err := s.List(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d pools, want 1", len(out))
	}
	var data map[string]any
	_ = json.Unmarshal(out[0].Data, &data)
	if got, _ := data["boundConnectionCount"].(float64); got != 2 {
		t.Errorf("boundConnectionCount = %v, want 2", data["boundConnectionCount"])
	}
}

func TestProxyPoolService_List_WithUsage_ZeroCountSkipsEnrich(t *testing.T) {
	t.Parallel()
	crepo := &fakeProxyPoolRepo{
		pools: []settings.ProxyPool{{ID: "pp1", Data: poolData(t, map[string]any{"name": "Pool1"})}},
	}
	connRepo := &fakeConnListRepo{conns: []settings.ProviderConnection{}}
	s := &ProxyPoolService{Repo: crepo, ConnectionRepo: connRepo}

	out, err := s.List(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var data map[string]any
	_ = json.Unmarshal(out[0].Data, &data)
	if _, ok := data["boundConnectionCount"]; ok {
		t.Errorf("boundConnectionCount should be absent when count=0")
	}
}

func TestProxyPoolService_List_WithUsage_ConnectionListError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("conn list fail")
	crepo := &fakeProxyPoolRepo{pools: []settings.ProxyPool{{ID: "pp1"}}}
	connRepo := &fakeConnListRepo{err: sentinel}
	s := &ProxyPoolService{Repo: crepo, ConnectionRepo: connRepo}
	if _, err := s.List(context.Background(), nil, true); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestProxyPoolService_Get(t *testing.T) {
	t.Parallel()
	pool := &settings.ProxyPool{ID: "pp1", Data: poolData(t, map[string]any{})}
	s := &ProxyPoolService{Repo: &fakeProxyPoolRepo{byID: map[string]*settings.ProxyPool{"pp1": pool}}}
	got, err := s.Get(context.Background(), "pp1")
	if err != nil || got == nil || got.ID != "pp1" {
		t.Fatalf("Get: err=%v got=%+v", err, got)
	}
}

func TestProxyPoolService_Get_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("get fail")
	s := &ProxyPoolService{Repo: &fakeProxyPoolRepo{getErr: sentinel}}
	if _, err := s.Get(context.Background(), "pp1"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestProxyPoolService_Create(t *testing.T) {
	t.Parallel()
	pool := &settings.ProxyPool{ID: "new", Data: poolData(t, map[string]any{})}
	repo := &fakeProxyPoolRepo{
		byID: map[string]*settings.ProxyPool{"new": pool},
	}
	s := &ProxyPoolService{Repo: repo}
	got, err := s.Create(context.Background(), settings.ProxyPool{ID: "new"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got == nil || got.ID != "new" {
		t.Fatalf("Create returned %+v", got)
	}
	if repo.createdID != "new" {
		t.Errorf("Create not delegated: %q", repo.createdID)
	}
}

func TestProxyPoolService_Create_CreateError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("create fail")
	s := &ProxyPoolService{Repo: &fakeProxyPoolRepo{createErr: sentinel}}
	if _, err := s.Create(context.Background(), settings.ProxyPool{ID: "x"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestProxyPoolService_Update(t *testing.T) {
	t.Parallel()
	pool := &settings.ProxyPool{ID: "u1"}
	repo := &fakeProxyPoolRepo{
		byID: map[string]*settings.ProxyPool{"u1": pool},
	}
	s := &ProxyPoolService{Repo: repo}
	got, err := s.Update(context.Background(), settings.ProxyPool{ID: "u1"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got == nil || got.ID != "u1" {
		t.Fatalf("Update returned %+v", got)
	}
	if repo.updatedID != "u1" {
		t.Errorf("Update not delegated: %q", repo.updatedID)
	}
}

func TestProxyPoolService_Update_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("update fail")
	s := &ProxyPoolService{Repo: &fakeProxyPoolRepo{updateErr: sentinel}}
	if _, err := s.Update(context.Background(), settings.ProxyPool{ID: "u1"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestProxyPoolService_Delete_Success(t *testing.T) {
	t.Parallel()
	deleted := &settings.ProxyPool{ID: "pp1"}
	repo := &fakeProxyPoolRepo{
		deletePool: deleted,
	}
	connRepo := &fakeConnListRepo{conns: []settings.ProviderConnection{
		{ID: "c1", Data: poolData(t, map[string]any{"proxyPoolId": "other"})},
	}}
	s := &ProxyPoolService{Repo: repo, ConnectionRepo: connRepo}
	if err := s.Delete(context.Background(), "pp1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestProxyPoolService_Delete_InUse(t *testing.T) {
	t.Parallel()
	deleted := &settings.ProxyPool{ID: "pp1"}
	repo := &fakeProxyPoolRepo{deletePool: deleted}
	connRepo := &fakeConnListRepo{conns: []settings.ProviderConnection{
		{ID: "c1", Data: poolData(t, map[string]any{"proxyPoolId": "pp1"})},
	}}
	s := &ProxyPoolService{Repo: repo, ConnectionRepo: connRepo}
	if err := s.Delete(context.Background(), "pp1"); !errors.Is(err, ErrProxyPoolInUse) {
		t.Fatalf("err = %v, want ErrProxyPoolInUse", err)
	}
}

func TestProxyPoolService_Delete_NotFound(t *testing.T) {
	t.Parallel()
	repo := &fakeProxyPoolRepo{deletePool: nil}
	s := &ProxyPoolService{Repo: repo, ConnectionRepo: &fakeConnListRepo{}}
	if err := s.Delete(context.Background(), "ghost"); !errors.Is(err, ErrProxyPoolNotFound) {
		t.Fatalf("err = %v, want ErrProxyPoolNotFound", err)
	}
}

func TestProxyPoolService_Delete_SqlNoRows_NotFound(t *testing.T) {
	t.Parallel()
	repo := &fakeProxyPoolRepo{deleteErr: sql.ErrNoRows}
	s := &ProxyPoolService{Repo: repo, ConnectionRepo: &fakeConnListRepo{}}
	if err := s.Delete(context.Background(), "ghost"); !errors.Is(err, ErrProxyPoolNotFound) {
		t.Fatalf("err = %v, want ErrProxyPoolNotFound", err)
	}
}

func TestProxyPoolService_Delete_RepoError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("delete fail")
	repo := &fakeProxyPoolRepo{deleteErr: sentinel}
	s := &ProxyPoolService{Repo: repo, ConnectionRepo: &fakeConnListRepo{}}
	if err := s.Delete(context.Background(), "pp1"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestProxyPoolService_Delete_ConnectionListError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("conn list fail")
	repo := &fakeProxyPoolRepo{deletePool: &settings.ProxyPool{ID: "pp1"}}
	connRepo := &fakeConnListRepo{err: sentinel}
	s := &ProxyPoolService{Repo: repo, ConnectionRepo: connRepo}
	if err := s.Delete(context.Background(), "pp1"); err == nil {
		t.Fatal("expected error when connection list fails")
	}
}