package managedashboard

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// fakeConnectionRepoLite implements the ProviderService.Repo interface.
type fakeConnectionRepoLite struct {
	conns    []settings.ProviderConnection
	listErr  error
	createID string
	createFn func(settings.ProviderConnection) (*settings.ProviderConnection, error)
}

func (r *fakeConnectionRepoLite) List(ctx context.Context, filter repo.ConnectionFilter) ([]settings.ProviderConnection, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	if filter.IsActive == nil && filter.Provider == "" {
		return r.conns, nil
	}
	var out []settings.ProviderConnection
	for _, c := range r.conns {
		if filter.Provider != "" && c.Provider != filter.Provider {
			continue
		}
		if filter.IsActive != nil && c.IsActive != *filter.IsActive {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (r *fakeConnectionRepoLite) Create(ctx context.Context, c settings.ProviderConnection) (*settings.ProviderConnection, error) {
	r.createID = c.ID
	if r.createFn != nil {
		return r.createFn(c)
	}
	r.conns = append(r.conns, c)
	return &c, nil
}

// fakeNodeRepoLite implements ProviderService.NodeRepo.
type fakeNodeRepoLite struct {
	nodes   []settings.ProviderNode
	listErr error
}

func (r *fakeNodeRepoLite) List(ctx context.Context, filter repo.NodeFilter) ([]settings.ProviderNode, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.nodes, nil
}

func connJSON(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestProviderService_List_EnrichesNodeName(t *testing.T) {
	t.Parallel()
	crepo := &fakeConnectionRepoLite{
		conns: []settings.ProviderConnection{
			{ID: "c1", Provider: "n1", AuthType: "oauth", Data: connJSON(t, map[string]any{"baseUrl": "https://x"})},
			{ID: "c2", Provider: "n2", AuthType: "oauth", Data: connJSON(t, map[string]any{})},
		},
	}
	nrepo := &fakeNodeRepoLite{
		nodes: []settings.ProviderNode{
			{ID: "n1", Name: "MyNode1"},
		},
	}
	s := &ProviderService{Repo: crepo, NodeRepo: nrepo}

	out, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if name, _ := out[0]["name"].(string); name != "MyNode1" {
		t.Errorf("c1 name = %v, want MyNode1", out[0]["name"])
	}
	// c2 has no matching node → falls back to the connectionToMap default (Name == "").
	if _, hasName := out[1]["name"]; hasName && out[1]["name"] != "" {
		t.Errorf("c2 name = %v, want empty when no matching node", out[1]["name"])
	}
}

func TestProviderService_List_SanitizesFields(t *testing.T) {
	t.Parallel()
	crepo := &fakeConnectionRepoLite{
		conns: []settings.ProviderConnection{
			{ID: "c1", Provider: "openai", AuthType: "oauth", Name: "Bob", Email: "b@x.com", Priority: 5, IsActive: true,
				Data: connJSON(t, map[string]any{"baseUrl": "https://api", "apiKey": "sk-secret", "testStatus": "ok", "defaultModel": "gpt-4", "nodeName": "node-x"})},
		},
	}
	s := &ProviderService{Repo: crepo, NodeRepo: &fakeNodeRepoLite{}}

	out, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	m := out[0]
	// Whitelisted fields pulled from data.
	if v, _ := m["baseUrl"].(string); v != "https://api" {
		t.Errorf("baseUrl = %v", m["baseUrl"])
	}
	if v, _ := m["testStatus"].(string); v != "ok" {
		t.Errorf("testStatus = %v", m["testStatus"])
	}
	if v, _ := m["defaultModel"].(string); v != "gpt-4" {
		t.Errorf("defaultModel = %v", m["defaultModel"])
	}
	// Top-level fields.
	if m["id"] != "c1" || m["provider"] != "openai" || m["authType"] != "oauth" {
		t.Errorf("identity fields wrong: %+v", m)
	}
	if m["priority"] != 5 || m["isActive"] != true {
		t.Errorf("priority/isActive wrong: %+v", m)
	}
	// Sensitive fields are NOT hoisted — apiKey lives only inside data blob.
	var data map[string]any
	_ = json.Unmarshal(crepo.conns[0].Data, &data)
	if _, ok := data["apiKey"]; !ok {
		t.Error("apiKey should remain inside the raw data blob")
	}
	// 'apiKey' is not whitelisted at the top level so it should not appear there.
	if _, ok := m["apiKey"]; ok {
		t.Error("apiKey must not be hoisted to top-level map")
	}
}

func TestProviderService_List_RepoError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list failed")
	s := &ProviderService{Repo: &fakeConnectionRepoLite{listErr: sentinel}}
	if _, err := s.List(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestProviderService_List_NodeRepoError_SwallowsAndContinues(t *testing.T) {
	t.Parallel()
	// A node-repo failure should not break List — nodes become nil and the loop is skipped.
	crepo := &fakeConnectionRepoLite{
		conns: []settings.ProviderConnection{{ID: "c1", Provider: "p", AuthType: "oauth", Data: connJSON(t, map[string]any{})}},
	}
	nrepo := &fakeNodeRepoLite{listErr: errors.New("node boom")}
	s := &ProviderService{Repo: crepo, NodeRepo: nrepo}

	out, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d, want 1", len(out))
	}
}

func TestProviderService_Create_MergesAPIKeyIntoData(t *testing.T) {
	t.Parallel()
	crepo := &fakeConnectionRepoLite{}
	s := &ProviderService{Repo: crepo}

	in := settings.ProviderConnection{ID: "new", Provider: "openai", AuthType: "oauth", Data: connJSON(t, map[string]any{"baseUrl": "https://x"})}
	got, err := s.Create(context.Background(), in, "sk-secret")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got == nil || got.ID != "new" {
		t.Fatalf("Create returned %+v", got)
	}
	var data map[string]any
	if err := json.Unmarshal(got.Data, &data); err != nil {
		t.Fatal(err)
	}
	if v, _ := data["apiKey"].(string); v != "sk-secret" {
		t.Errorf("apiKey = %v, want sk-secret", data["apiKey"])
	}
	if v, _ := data["baseUrl"].(string); v != "https://x" {
		t.Errorf("baseUrl lost: %v", data["baseUrl"])
	}
	if crepo.createID != "new" {
		t.Errorf("Create not delegated: createID=%q", crepo.createID)
	}
}

func TestProviderService_Create_EmptyDataStillGetsAPIKey(t *testing.T) {
	t.Parallel()
	crepo := &fakeConnectionRepoLite{}
	s := &ProviderService{Repo: crepo}

	got, err := s.Create(context.Background(), settings.ProviderConnection{ID: "x", Provider: "p", AuthType: "api-key"}, "k")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var data map[string]any
	_ = json.Unmarshal(got.Data, &data)
	if v, _ := data["apiKey"].(string); v != "k" {
		t.Errorf("apiKey = %v, want k", data["apiKey"])
	}
}

func TestProviderService_Create_RepoError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("create failed")
	s := &ProviderService{Repo: &fakeConnectionRepoLite{createFn: func(settings.ProviderConnection) (*settings.ProviderConnection, error) { return nil, sentinel }}}
	if _, err := s.Create(context.Background(), settings.ProviderConnection{ID: "x"}, "k"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestProviderService_Client_Filtering(t *testing.T) {
	t.Parallel()
	// Build conns covering: oauth eligible, api-key eligible, unsupported provider, active/inactive.
	crepo := &fakeConnectionRepoLite{
		conns: []settings.ProviderConnection{
			{ID: "a", Provider: "openai", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},
			{ID: "b", Provider: "glm", AuthType: "api-key", IsActive: true, Data: connJSON(t, map[string]any{})},
			{ID: "c", Provider: "qwen", AuthType: "oauth", IsActive: false, Data: connJSON(t, map[string]any{})},       // inactive → not active status
			{ID: "d", Provider: "unknown", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},     // unsupported
			{ID: "e", Provider: "anthropic", AuthType: "api-key", IsActive: true, Data: connJSON(t, map[string]any{})}, // not in apiKeyProviders
		},
	}
	s := &ProviderService{Repo: crepo, NodeRepo: &fakeNodeRepoLite{}}

	out, err := s.Client(context.Background(), ClientFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	conns, _ := out["connections"].([]map[string]any)
	if len(conns) != 2 {
		t.Fatalf("got %d eligible, want 2 (openai oauth + glm api-key)", len(conns))
	}
	pagination, _ := out["pagination"].(map[string]any)
	if pagination["total"].(int) != 2 {
		t.Errorf("total = %v, want 2", pagination["total"])
	}
	if pagination["totalPages"].(int) != 1 {
		t.Errorf("totalPages = %v, want 1", pagination["totalPages"])
	}
	if pagination["page"].(int) != 1 {
		t.Errorf("page = %v, want 1", pagination["page"])
	}
}

func TestProviderService_Client_StatusFilter(t *testing.T) {
	t.Parallel()
	crepo := &fakeConnectionRepoLite{
		conns: []settings.ProviderConnection{
			{ID: "a", Provider: "openai", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},
			{ID: "b", Provider: "openai", AuthType: "oauth", IsActive: false, Data: connJSON(t, map[string]any{})},
		},
	}
	s := &ProviderService{Repo: crepo, NodeRepo: &fakeNodeRepoLite{}}

	// The Client filter queries repo with IsActive=true; only active conns are ever returned.
	out, err := s.Client(context.Background(), ClientFilter{AccountStatus: "active", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	conns, _ := out["connections"].([]map[string]any)
	if len(conns) != 1 || conns[0]["id"] != "a" {
		t.Fatalf("active filter: got %+v", conns)
	}
}

func TestProviderService_Client_ProviderFilter(t *testing.T) {
	t.Parallel()
	crepo := &fakeConnectionRepoLite{
		conns: []settings.ProviderConnection{
			{ID: "a", Provider: "openai", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},
			{ID: "b", Provider: "anthropic", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},
		},
	}
	s := &ProviderService{Repo: crepo, NodeRepo: &fakeNodeRepoLite{}}

	out, err := s.Client(context.Background(), ClientFilter{Provider: "anthropic", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	conns, _ := out["connections"].([]map[string]any)
	if len(conns) != 1 || conns[0]["id"] != "b" {
		t.Fatalf("provider filter: got %+v", conns)
	}
}

func TestProviderService_Client_ProviderFilterAll(t *testing.T) {
	t.Parallel()
	crepo := &fakeConnectionRepoLite{
		conns: []settings.ProviderConnection{
			{ID: "a", Provider: "openai", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},
			{ID: "b", Provider: "anthropic", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},
		},
	}
	s := &ProviderService{Repo: crepo, NodeRepo: &fakeNodeRepoLite{}}

	out, err := s.Client(context.Background(), ClientFilter{Provider: "all", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	conns, _ := out["connections"].([]map[string]any)
	if len(conns) != 2 {
		t.Fatalf("provider=all: got %d, want 2", len(conns))
	}
}

func TestProviderService_Client_Pagination(t *testing.T) {
	t.Parallel()
	var conns []settings.ProviderConnection
	for i := 0; i < 5; i++ {
		conns = append(conns, settings.ProviderConnection{
			ID:       string(rune('a' + i)),
			Provider: "openai",
			AuthType: "oauth",
			IsActive: true,
			Data:     connJSON(t, map[string]any{}),
		})
	}
	crepo := &fakeConnectionRepoLite{conns: conns}
	s := &ProviderService{Repo: crepo, NodeRepo: &fakeNodeRepoLite{}}

	out, err := s.Client(context.Background(), ClientFilter{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	pagination, _ := out["pagination"].(map[string]any)
	if pagination["total"].(int) != 5 {
		t.Errorf("total = %v, want 5", pagination["total"])
	}
	if pagination["totalPages"].(int) != 3 {
		t.Errorf("totalPages = %v, want 3", pagination["totalPages"])
	}
	if pagination["page"].(int) != 2 {
		t.Errorf("page = %v, want 2", pagination["page"])
	}
	pageConns, _ := out["connections"].([]map[string]any)
	if len(pageConns) != 2 {
		t.Errorf("page slice = %d, want 2", len(pageConns))
	}
}

func TestProviderService_Client_Pagination_PageOverflow(t *testing.T) {
	t.Parallel()
	crepo := &fakeConnectionRepoLite{
		conns: []settings.ProviderConnection{
			{ID: "a", Provider: "openai", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},
		},
	}
	s := &ProviderService{Repo: crepo, NodeRepo: &fakeNodeRepoLite{}}

	out, err := s.Client(context.Background(), ClientFilter{Page: 99, PageSize: 10})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	pagination, _ := out["pagination"].(map[string]any)
	if pagination["page"].(int) != 1 {
		t.Errorf("page overflow should clamp to totalPages=1, got %v", pagination["page"])
	}
}

func TestProviderService_Client_EmptyResults(t *testing.T) {
	t.Parallel()
	crepo := &fakeConnectionRepoLite{}
	s := &ProviderService{Repo: crepo, NodeRepo: &fakeNodeRepoLite{}}

	out, err := s.Client(context.Background(), ClientFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	pagination, _ := out["pagination"].(map[string]any)
	if pagination["total"].(int) != 0 {
		t.Errorf("total = %v, want 0", pagination["total"])
	}
	if pagination["totalPages"].(int) != 1 {
		t.Errorf("totalPages = %v, want 1 for empty result", pagination["totalPages"])
	}
	conns, _ := out["connections"].([]map[string]any)
	if len(conns) != 0 {
		t.Errorf("connections = %d, want 0", len(conns))
	}
}

func TestProviderService_Client_RepoError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list fail")
	s := &ProviderService{Repo: &fakeConnectionRepoLite{listErr: sentinel}, NodeRepo: &fakeNodeRepoLite{}}
	if _, err := s.Client(context.Background(), ClientFilter{Page: 1, PageSize: 10}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestProviderService_Client_ProviderOptionsFromAllConns(t *testing.T) {
	t.Parallel()
	// providerOptions should be built from ALL conns returned by repo.List,
	// including ones that get filtered out by eligibility.
	crepo := &fakeConnectionRepoLite{
		conns: []settings.ProviderConnection{
			{ID: "a", Provider: "openai", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},
			{ID: "b", Provider: "unknown", AuthType: "oauth", IsActive: true, Data: connJSON(t, map[string]any{})},
		},
	}
	s := &ProviderService{Repo: crepo, NodeRepo: &fakeNodeRepoLite{}}

	out, err := s.Client(context.Background(), ClientFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	options, _ := out["providerOptions"].([]string)
	if len(options) != 2 {
		t.Errorf("providerOptions = %v, want 2 entries", options)
	}
}

func TestConnectionToMap_Whitelist(t *testing.T) {
	t.Parallel()
	c := settings.ProviderConnection{
		ID: "c", Provider: "p", AuthType: "oauth", Name: "n", Email: "e", Priority: 3, IsActive: true,
		Data: connJSON(t, map[string]any{
			"baseUrl":                "https://x",
			"azureEndpoint":          "https://azure",
			"deployment":             "dep",
			"apiVersion":             "2024",
			"accountId":              "acc",
			"region":                 "us",
			"projectId":              "p1",
			"resourceUrl":            "https://r",
			"proxyPoolId":            "pp1",
			"connectionProxyEnabled": true,
			"connectionProxyUrl":     "https://proxy",
			"connectionNoProxy":      "127.0.0.1",
			"nodeName":               "node",
			"ignoredField":           "x",
			"testStatus":             "ok",
			"defaultModel":           "gpt-4",
		}),
	}
	m := connectionToMap(c)
	whitelist := []string{
		"baseUrl", "azureEndpoint", "deployment", "apiVersion", "accountId",
		"region", "projectId", "resourceUrl", "proxyPoolId", "connectionProxyEnabled",
		"connectionProxyUrl", "connectionNoProxy", "nodeName", "testStatus", "defaultModel",
	}
	for _, k := range whitelist {
		if _, ok := m[k]; !ok {
			t.Errorf("whitelisted key %q missing from map", k)
		}
	}
	if _, ok := m["ignoredField"]; ok {
		t.Error("non-whitelisted field should not be hoisted")
	}
	if m["testStatus"] != "ok" {
		t.Errorf("testStatus = %v, want ok", m["testStatus"])
	}
	if m["defaultModel"] != "gpt-4" {
		t.Errorf("defaultModel = %v, want gpt-4", m["defaultModel"])
	}
}

func TestConnectionToMap_Defaults(t *testing.T) {
	t.Parallel()
	c := settings.ProviderConnection{ID: "c", Provider: "p", Data: connJSON(t, map[string]any{})}
	m := connectionToMap(c)
	if m["defaultModel"] != nil {
		t.Errorf("defaultModel default = %v, want nil", m["defaultModel"])
	}
	if m["testStatus"] != "unknown" {
		t.Errorf("testStatus default = %v, want unknown", m["testStatus"])
	}
}

func TestSortConnections_NoOpForProviderSort(t *testing.T) {
	t.Parallel()
	// sortConnections("provider") returns early without mutating the slice.
	in := []settings.ProviderConnection{
		{ID: "a", Priority: 5},
		{ID: "b", Priority: 1},
	}
	sortConnections(in, "provider")
	if in[0].ID != "a" || in[1].ID != "b" {
		t.Errorf("sortConnections mutated slice: %+v", in)
	}
}

func TestBoolPtr(t *testing.T) {
	t.Parallel()
	b := boolPtr(true)
	if b == nil || !*b {
		t.Error("boolPtr(true) should return pointer to true")
	}
}
