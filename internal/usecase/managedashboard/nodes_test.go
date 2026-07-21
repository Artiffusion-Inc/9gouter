package managedashboard

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// --- fakes for NodeService ---

type fakeNodeRepo struct {
	nodes     []settings.ProviderNode
	byID      map[string]*settings.ProviderNode
	listErr   error
	getErr    error
	createErr error
	updateErr error
	deleteErr error
	created   settings.ProviderNode
	updated   settings.ProviderNode
	deletedID string
}

func (r *fakeNodeRepo) List(ctx context.Context, filter repo.NodeFilter) ([]settings.ProviderNode, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.nodes, nil
}

func (r *fakeNodeRepo) GetByID(ctx context.Context, id string) (*settings.ProviderNode, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if n, ok := r.byID[id]; ok {
		cp := *n
		return &cp, nil
	}
	return nil, nil
}

func (r *fakeNodeRepo) Create(ctx context.Context, n settings.ProviderNode) error {
	r.created = n
	if r.createErr != nil {
		return r.createErr
	}
	return nil
}

func (r *fakeNodeRepo) Update(ctx context.Context, n settings.ProviderNode) error {
	r.updated = n
	if r.updateErr != nil {
		return r.updateErr
	}
	return nil
}

func (r *fakeNodeRepo) Delete(ctx context.Context, id string) error {
	r.deletedID = id
	return r.deleteErr
}

// fakeConnRepoForNodes implements NodeService.ConnRepo.
type fakeConnRepoForNodes struct {
	conns        []settings.ProviderConnection
	listErr      error
	updatedConns []settings.ProviderConnection
	updateErr    error
	deletedBy    string
	deletedCount int64
	deleteErr    error
}

func (r *fakeConnRepoForNodes) List(ctx context.Context, filter repo.ConnectionFilter) ([]settings.ProviderConnection, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	if filter.Provider == "" {
		return r.conns, nil
	}
	var out []settings.ProviderConnection
	for _, c := range r.conns {
		if c.Provider == filter.Provider {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *fakeConnRepoForNodes) Update(ctx context.Context, c settings.ProviderConnection) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	r.updatedConns = append(r.updatedConns, c)
	return nil
}

func (r *fakeConnRepoForNodes) DeleteByProvider(ctx context.Context, provider string) (int64, error) {
	r.deletedBy = provider
	if r.deleteErr != nil {
		return 0, r.deleteErr
	}
	return r.deletedCount, nil
}

func nodeData(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- pure helper tests ---

func TestStringsOr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b, want string
	}{
		{"", "fallback", "fallback"},
		{"   ", "fallback", "fallback"},
		{"x", "fallback", "x"},
		{" x ", "fallback", " x "},
	}
	for _, tc := range tests {
		if got := stringsOr(tc.a, tc.b); got != tc.want {
			t.Errorf("stringsOr(%q,%q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMustJSON(t *testing.T) {
	t.Parallel()
	got := mustJSON(map[string]any{"a": 1})
	if got != `{"a":1}` {
		t.Errorf("mustJSON = %q", got)
	}
}

func TestJSONBytes(t *testing.T) {
	t.Parallel()
	got := jsonBytes(map[string]any{"a": 1})
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("jsonBytes invalid: %v", err)
	}
	if m["a"].(float64) != 1 {
		t.Errorf("jsonBytes decoded wrong: %v", m)
	}
}

func TestSanitizeAnthropicBaseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"", "https://api.anthropic.com/v1"},
		{"   ", "https://api.anthropic.com/v1"},
		{"https://custom/v1/messages", "https://custom/v1"},
		{"https://custom/v1/messages/", "https://custom/v1"}, // trims trailing slash too
		{"https://custom/v1/", "https://custom/v1"},
		{"https://custom/v1", "https://custom/v1"},
		{"  https://custom/v1/messages  ", "https://custom/v1"},
	}
	for _, tc := range tests {
		if got := sanitizeAnthropicBaseURL(tc.in); got != tc.want {
			t.Errorf("sanitizeAnthropicBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeEmbeddingBaseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"", "https://api.openai.com/v1"},
		{"https://custom/v1/embeddings", "https://custom/v1"},
		{"https://custom/v1/embeddings/", "https://custom/v1"},
		{"https://custom/v1/", "https://custom/v1"},
		{"https://custom/v1", "https://custom/v1"},
	}
	for _, tc := range tests {
		if got := sanitizeEmbeddingBaseURL(tc.in); got != tc.want {
			t.Errorf("sanitizeEmbeddingBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsPublicURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{"https://api.openai.com/v1", true},
		{"https://example.com", true},
		{"http://localhost:8080", false},
		{"http://127.0.0.1:8080", false},
		{"http://[::1]:8080", false},
		{"http://10.0.0.1", false},
		{"http://172.16.0.1", false},
		{"http://172.31.255.255", false},
		{"http://172.32.0.1", true}, // outside private range
		{"http://192.168.1.1", false},
		{"http://169.254.1.1", false}, // link-local
		{"http://0.0.0.0", false},
		{"", false},
		{":::not-a-url", false},
		{"http://fc00:1::1", false},
		{"http://fd00:1::1", false},
	}
	for _, tc := range tests {
		if got := isPublicURL(tc.in); got != tc.want {
			t.Errorf("isPublicURL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNetworkError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"connection refused", errors.New("dial tcp: connection refused"), "Connection refused - provider node offline or unreachable"},
		{"no such host", errors.New("lookup foo: no such host"), "DNS lookup failed - invalid domain or network issue"},
		{"DNS", errors.New("DNS problem"), "DNS lookup failed - invalid domain or network issue"},
		{"timeout", errors.New("request timeout"), "Request timeout (>10s) - provider node not responding"},
		{"exceeded", errors.New("deadline exceeded"), "Request timeout (>10s) - provider node not responding"},
		{"certificate", errors.New("certificate verification failed"), "SSL certificate verification failed"},
		{"unknown", errors.New("something else"), "Network connection failed - check URL and network connectivity"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := networkError(tc.err); got != tc.want {
				t.Errorf("networkError = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChatErrorMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		want   string
	}{
		{401, "API key unauthorized"},
		{403, "API key unauthorized"},
		{400, "Invalid model or bad request"},
		{404, "Chat endpoint not found"},
		{500, "Server error - try again later"},
		{502, "Server error - try again later"},
		{418, "Chat request failed (418)"},
	}
	for _, tc := range tests {
		if got := chatErrorMessage(tc.status); got != tc.want {
			t.Errorf("chatErrorMessage(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestModelsErrorMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		want   string
	}{
		{401, "API key unauthorized"},
		{403, "API key unauthorized"},
		{404, "/models endpoint not found - try chat validation with model ID"},
		{500, "Server error - try again later"},
		{418, "Unexpected response (418)"},
	}
	for _, tc := range tests {
		if got := modelsErrorMessage(tc.status); got != tc.want {
			t.Errorf("modelsErrorMessage(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestGenerateShortID(t *testing.T) {
	t.Parallel()
	id1 := generateShortID()
	id2 := generateShortID()
	if id1 == "" {
		t.Error("generateShortID returned empty")
	}
	// Should be a numeric string.
	for _, r := range id1 {
		if r < '0' || r > '9' {
			t.Errorf("generateShortID = %q, expected digits only", id1)
			break
		}
	}
	_ = id2
}

// --- NodeService.Create tests ---

func TestNodeService_Create_ValidationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  NodeCreateRequest
		want string
	}{
		{"empty name", NodeCreateRequest{Name: "", Prefix: "p", Type: "openai-compatible", APIType: "chat"}, "name is required"},
		{"empty prefix", NodeCreateRequest{Name: "n", Prefix: "", Type: "openai-compatible", APIType: "chat"}, "prefix is required"},
		{"invalid type", NodeCreateRequest{Name: "n", Prefix: "p", Type: "bogus"}, "invalid provider node type"},
		{"openai invalid apitype", NodeCreateRequest{Name: "n", Prefix: "p", Type: "openai-compatible", APIType: "bogus"}, "invalid OpenAI compatible API type"},
		{"openai empty apitype", NodeCreateRequest{Name: "n", Prefix: "p", Type: "openai-compatible", APIType: ""}, "invalid OpenAI compatible API type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &NodeService{Repo: &fakeNodeRepo{}, ConnRepo: &fakeConnRepoForNodes{}}
			_, err := s.Create(context.Background(), tc.req)
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestNodeService_Create_OpenAICompatible_Chat(t *testing.T) {
	t.Parallel()
	repo := &fakeNodeRepo{}
	s := &NodeService{Repo: repo, ConnRepo: &fakeConnRepoForNodes{}}

	got, err := s.Create(context.Background(), NodeCreateRequest{
		Name: "MyNode", Prefix: "openai_", Type: "openai-compatible", APIType: "chat",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Name != "MyNode" || got.Type != "openai-compatible" {
		t.Errorf("node = %+v", got)
	}
	if !contains(got.ID, "9gouter-openai-compatible-chat-") {
		t.Errorf("ID = %q, want prefix 9gouter-openai-compatible-chat-", got.ID)
	}
	var data map[string]any
	_ = json.Unmarshal(got.Data, &data)
	if data["prefix"] != "openai_" {
		t.Errorf("data.prefix = %v", data["prefix"])
	}
	if data["apiType"] != "chat" {
		t.Errorf("data.apiType = %v", data["apiType"])
	}
	if data["baseUrl"] != "https://api.openai.com/v1" {
		t.Errorf("data.baseUrl = %v (default should apply)", data["baseUrl"])
	}
	if !contains(string(repo.created.ID), "9gouter-openai-compatible-chat-") {
		t.Errorf("Create not delegated to repo")
	}
}

func TestNodeService_Create_OpenAICompatible_Responses_BaseURLOverride(t *testing.T) {
	t.Parallel()
	s := &NodeService{Repo: &fakeNodeRepo{}, ConnRepo: &fakeConnRepoForNodes{}}
	got, err := s.Create(context.Background(), NodeCreateRequest{
		Name: "N", Prefix: "p", Type: "openai-compatible", APIType: "responses",
		BaseURL: "  https://custom.example/v1  ",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var data map[string]any
	_ = json.Unmarshal(got.Data, &data)
	if data["baseUrl"] != "https://custom.example/v1" {
		t.Errorf("baseUrl = %v, want trimmed custom", data["baseUrl"])
	}
	if data["apiType"] != "responses" {
		t.Errorf("apiType = %v", data["apiType"])
	}
}

func TestNodeService_Create_CustomEmbedding(t *testing.T) {
	t.Parallel()
	s := &NodeService{Repo: &fakeNodeRepo{}, ConnRepo: &fakeConnRepoForNodes{}}
	got, err := s.Create(context.Background(), NodeCreateRequest{
		Name: "Embed", Prefix: "emb_", Type: "custom-embedding", BaseURL: "https://custom/v1/embeddings/",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !contains(got.ID, "9gouter-custom-embedding-") {
		t.Errorf("ID = %q", got.ID)
	}
	var data map[string]any
	_ = json.Unmarshal(got.Data, &data)
	if data["baseUrl"] != "https://custom/v1" {
		t.Errorf("baseUrl = %v, want https://custom/v1 (sanitized)", data["baseUrl"])
	}
}

func TestNodeService_Create_AnthropicCompatible(t *testing.T) {
	t.Parallel()
	s := &NodeService{Repo: &fakeNodeRepo{}, ConnRepo: &fakeConnRepoForNodes{}}
	got, err := s.Create(context.Background(), NodeCreateRequest{
		Name: "Anthropic", Prefix: "ant_", Type: "anthropic-compatible", BaseURL: "https://custom/v1/messages",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !contains(got.ID, "9gouter-anthropic-compatible-") {
		t.Errorf("ID = %q", got.ID)
	}
	var data map[string]any
	_ = json.Unmarshal(got.Data, &data)
	if data["baseUrl"] != "https://custom/v1" {
		t.Errorf("baseUrl = %v, want https://custom/v1 (sanitized)", data["baseUrl"])
	}
}

func TestNodeService_Create_DefaultType(t *testing.T) {
	t.Parallel()
	s := &NodeService{Repo: &fakeNodeRepo{}, ConnRepo: &fakeConnRepoForNodes{}}
	got, err := s.Create(context.Background(), NodeCreateRequest{
		Name: "N", Prefix: "p", APIType: "chat",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Type != "openai-compatible" {
		t.Errorf("Type = %q, want openai-compatible default", got.Type)
	}
}

func TestNodeService_Create_RepoError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("create fail")
	s := &NodeService{Repo: &fakeNodeRepo{createErr: sentinel}, ConnRepo: &fakeConnRepoForNodes{}}
	if _, err := s.Create(context.Background(), NodeCreateRequest{Name: "n", Prefix: "p", Type: "openai-compatible", APIType: "chat"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

// --- NodeService.Update tests ---

func TestNodeService_Update_NodeNotFound(t *testing.T) {
	t.Parallel()
	s := &NodeService{Repo: &fakeNodeRepo{}, ConnRepo: &fakeConnRepoForNodes{}}
	_, err := s.Update(context.Background(), "ghost", NodeUpdateRequest{Name: "n", Prefix: "p", BaseURL: "https://x"})
	if err == nil || !contains(err.Error(), "provider node not found") {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestNodeService_Update_GetError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("get fail")
	s := &NodeService{Repo: &fakeNodeRepo{getErr: sentinel}, ConnRepo: &fakeConnRepoForNodes{}}
	if _, err := s.Update(context.Background(), "x", NodeUpdateRequest{Name: "n", Prefix: "p", BaseURL: "https://x"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestNodeService_Update_ValidationErrors(t *testing.T) {
	t.Parallel()
	repo := &fakeNodeRepo{byID: map[string]*settings.ProviderNode{
		"n1": {ID: "n1", Type: "openai-compatible", Data: nodeData(t, map[string]any{})},
	}}
	tests := []struct {
		name string
		req  NodeUpdateRequest
		want string
	}{
		{"empty name", NodeUpdateRequest{Name: "", Prefix: "p", BaseURL: "https://x"}, "name is required"},
		{"empty prefix", NodeUpdateRequest{Name: "n", Prefix: "", BaseURL: "https://x"}, "prefix is required"},
		{"empty baseurl", NodeUpdateRequest{Name: "n", Prefix: "p", BaseURL: ""}, "base URL is required"},
		{"invalid apitype", NodeUpdateRequest{Name: "n", Prefix: "p", BaseURL: "https://x", APIType: "bogus"}, "invalid OpenAI compatible API type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &NodeService{Repo: repo, ConnRepo: &fakeConnRepoForNodes{}}
			_, err := s.Update(context.Background(), "n1", tc.req)
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestNodeService_Update_OpenAI_PropagatesToConnections(t *testing.T) {
	t.Parallel()
	existing := &settings.ProviderNode{ID: "n1", Type: "openai-compatible", Name: "Old", Data: nodeData(t, map[string]any{"apiType": "chat", "baseUrl": "https://old"})}
	nrepo := &fakeNodeRepo{byID: map[string]*settings.ProviderNode{"n1": existing}}
	crepo := &fakeConnRepoForNodes{
		conns: []settings.ProviderConnection{
			{ID: "c1", Provider: "n1", Data: nodeData(t, map[string]any{
				"providerSpecificData": map[string]any{"baseUrl": "https://old"},
			})},
			{ID: "c2", Provider: "n1", Data: nodeData(t, map[string]any{})}, // no PSD yet
		},
	}
	s := &NodeService{Repo: nrepo, ConnRepo: crepo}

	got, err := s.Update(context.Background(), "n1", NodeUpdateRequest{
		Name: "NewName", Prefix: "newp_", BaseURL: "https://new/v1", APIType: "responses",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "NewName" {
		t.Errorf("Name = %q, want NewName", got.Name)
	}
	var data map[string]any
	_ = json.Unmarshal(got.Data, &data)
	if data["apiType"] != "responses" {
		t.Errorf("apiType = %v, want responses", data["apiType"])
	}
	if data["baseUrl"] != "https://new/v1" {
		t.Errorf("baseUrl = %v, want https://new/v1", data["baseUrl"])
	}
	if len(crepo.updatedConns) != 2 {
		t.Fatalf("expected 2 connection updates, got %d", len(crepo.updatedConns))
	}
	for _, uc := range crepo.updatedConns {
		var cdata map[string]any
		_ = json.Unmarshal(uc.Data, &cdata)
		psd, _ := cdata["providerSpecificData"].(map[string]any)
		if psd["prefix"] != "newp_" {
			t.Errorf("psd.prefix = %v, want newp_", psd["prefix"])
		}
		if psd["baseUrl"] != "https://new/v1" {
			t.Errorf("psd.baseUrl = %v", psd["baseUrl"])
		}
		if psd["apiType"] != "responses" {
			t.Errorf("psd.apiType = %v, want responses", psd["apiType"])
		}
		if psd["nodeName"] != "NewName" {
			t.Errorf("psd.nodeName = %v, want NewName", psd["nodeName"])
		}
	}
}

func TestNodeService_Update_Anthropic_RemovesApiType(t *testing.T) {
	t.Parallel()
	existing := &settings.ProviderNode{ID: "n1", Type: "anthropic-compatible", Name: "Old", Data: nodeData(t, map[string]any{"baseUrl": "https://old"})}
	nrepo := &fakeNodeRepo{byID: map[string]*settings.ProviderNode{"n1": existing}}
	crepo := &fakeConnRepoForNodes{
		conns: []settings.ProviderConnection{
			{ID: "c1", Provider: "n1", Data: nodeData(t, map[string]any{
				"providerSpecificData": map[string]any{"apiType": "chat"},
			})},
		},
	}
	s := &NodeService{Repo: nrepo, ConnRepo: crepo}

	if _, err := s.Update(context.Background(), "n1", NodeUpdateRequest{
		Name: "N", Prefix: "p", BaseURL: "https://new/v1/messages",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var cdata map[string]any
	_ = json.Unmarshal(crepo.updatedConns[0].Data, &cdata)
	psd, _ := cdata["providerSpecificData"].(map[string]any)
	if _, ok := psd["apiType"]; ok {
		t.Errorf("apiType should be deleted for anthropic nodes, got %v", psd["apiType"])
	}
	if psd["baseUrl"] != "https://new/v1" {
		t.Errorf("baseUrl = %v, want sanitized https://new/v1", psd["baseUrl"])
	}
}

func TestNodeService_Update_DefaultTypeBranch(t *testing.T) {
	t.Parallel()
	// An unknown node type hits the default branch which sets baseUrl = baseURL as-is.
	existing := &settings.ProviderNode{ID: "n1", Type: "custom", Name: "Old", Data: nodeData(t, map[string]any{})}
	nrepo := &fakeNodeRepo{byID: map[string]*settings.ProviderNode{"n1": existing}}
	crepo := &fakeConnRepoForNodes{}
	s := &NodeService{Repo: nrepo, ConnRepo: crepo}

	got, err := s.Update(context.Background(), "n1", NodeUpdateRequest{Name: "N", Prefix: "p", BaseURL: "https://custom"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	var data map[string]any
	_ = json.Unmarshal(got.Data, &data)
	if data["baseUrl"] != "https://custom" {
		t.Errorf("default branch baseUrl = %v, want https://custom", data["baseUrl"])
	}
}

func TestNodeService_Update_RepoUpdateError(t *testing.T) {
	t.Parallel()
	nrepo := &fakeNodeRepo{
		byID:      map[string]*settings.ProviderNode{"n1": {ID: "n1", Type: "openai-compatible", Data: nodeData(t, map[string]any{})}},
		updateErr: errors.New("update fail"),
	}
	s := &NodeService{Repo: nrepo, ConnRepo: &fakeConnRepoForNodes{}}
	if _, err := s.Update(context.Background(), "n1", NodeUpdateRequest{Name: "n", Prefix: "p", BaseURL: "https://x", APIType: "chat"}); err == nil {
		t.Fatal("expected update error")
	}
}

func TestNodeService_Update_ConnectionListError(t *testing.T) {
	t.Parallel()
	nrepo := &fakeNodeRepo{
		byID: map[string]*settings.ProviderNode{"n1": {ID: "n1", Type: "openai-compatible", Data: nodeData(t, map[string]any{})}},
	}
	crepo := &fakeConnRepoForNodes{listErr: errors.New("conn list fail")}
	s := &NodeService{Repo: nrepo, ConnRepo: crepo}
	if _, err := s.Update(context.Background(), "n1", NodeUpdateRequest{Name: "n", Prefix: "p", BaseURL: "https://x", APIType: "chat"}); err == nil {
		t.Fatal("expected error on connection list failure")
	}
}

func TestNodeService_Update_ConnectionUpdateError(t *testing.T) {
	t.Parallel()
	nrepo := &fakeNodeRepo{
		byID: map[string]*settings.ProviderNode{"n1": {ID: "n1", Type: "openai-compatible", Data: nodeData(t, map[string]any{})}},
	}
	crepo := &fakeConnRepoForNodes{
		conns:     []settings.ProviderConnection{{ID: "c1", Provider: "n1", Data: nodeData(t, map[string]any{})}},
		updateErr: errors.New("update fail"),
	}
	s := &NodeService{Repo: nrepo, ConnRepo: crepo}
	if _, err := s.Update(context.Background(), "n1", NodeUpdateRequest{Name: "n", Prefix: "p", BaseURL: "https://x", APIType: "chat"}); err == nil {
		t.Fatal("expected error on connection update failure")
	}
}

// --- NodeService.Delete tests ---

func TestNodeService_Delete_Success(t *testing.T) {
	t.Parallel()
	nrepo := &fakeNodeRepo{
		byID: map[string]*settings.ProviderNode{"n1": {ID: "n1", Data: nodeData(t, map[string]any{})}},
	}
	crepo := &fakeConnRepoForNodes{deletedCount: 3}
	s := &NodeService{Repo: nrepo, ConnRepo: crepo}
	if err := s.Delete(context.Background(), "n1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if nrepo.deletedID != "n1" {
		t.Errorf("Repo.Delete not called with n1, got %q", nrepo.deletedID)
	}
	if crepo.deletedBy != "n1" {
		t.Errorf("DeleteByProvider called with %q, want n1", crepo.deletedBy)
	}
}

func TestNodeService_Delete_NodeNotFound(t *testing.T) {
	t.Parallel()
	s := &NodeService{Repo: &fakeNodeRepo{}, ConnRepo: &fakeConnRepoForNodes{}}
	if err := s.Delete(context.Background(), "ghost"); err == nil || !contains(err.Error(), "provider node not found") {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestNodeService_Delete_GetError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("get fail")
	s := &NodeService{Repo: &fakeNodeRepo{getErr: sentinel}, ConnRepo: &fakeConnRepoForNodes{}}
	if err := s.Delete(context.Background(), "n1"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestNodeService_Delete_DeleteByProviderError(t *testing.T) {
	t.Parallel()
	nrepo := &fakeNodeRepo{byID: map[string]*settings.ProviderNode{"n1": {ID: "n1"}}}
	crepo := &fakeConnRepoForNodes{deleteErr: errors.New("delete conn fail")}
	s := &NodeService{Repo: nrepo, ConnRepo: crepo}
	if err := s.Delete(context.Background(), "n1"); err == nil || !contains(err.Error(), "delete conn fail") {
		t.Fatalf("err = %v", err)
	}
}

func TestNodeService_Delete_RepoDeleteError(t *testing.T) {
	t.Parallel()
	nrepo := &fakeNodeRepo{
		byID:     map[string]*settings.ProviderNode{"n1": {ID: "n1"}},
		deleteErr: errors.New("node delete fail"),
	}
	crepo := &fakeConnRepoForNodes{deletedCount: 0}
	s := &NodeService{Repo: nrepo, ConnRepo: crepo}
	if err := s.Delete(context.Background(), "n1"); err == nil || !contains(err.Error(), "node delete fail") {
		t.Fatalf("err = %v", err)
	}
}

// --- NodeService.List test ---

func TestNodeService_List(t *testing.T) {
	t.Parallel()
	nrepo := &fakeNodeRepo{
		nodes: []settings.ProviderNode{{ID: "n1"}, {ID: "n2"}},
	}
	s := &NodeService{Repo: nrepo, ConnRepo: &fakeConnRepoForNodes{}}
	got, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestNodeService_List_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list fail")
	s := &NodeService{Repo: &fakeNodeRepo{listErr: sentinel}, ConnRepo: &fakeConnRepoForNodes{}}
	if _, err := s.List(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

// --- NodeService.Validate tests (pure-logic branch) ---

func TestNodeService_Validate_MissingFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		req     ValidateRequest
		wantErr string
	}{
		{"empty baseurl", ValidateRequest{APIKey: "k"}, "Base URL and API key required"},
		{"empty apikey", ValidateRequest{BaseURL: "https://x"}, "Base URL and API key required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &NodeService{}
			res, err := s.Validate(context.Background(), tc.req, false)
			if err != nil {
				t.Fatalf("Validate err: %v", err)
			}
			if res.Valid {
				t.Error("expected Valid=false")
			}
			if !contains(res.Error, tc.wantErr) {
				t.Errorf("Error = %q, want substring %q", res.Error, tc.wantErr)
			}
		})
	}
}

func TestNodeService_Validate_NonLocalPrivateURL(t *testing.T) {
	t.Parallel()
	s := &NodeService{}
	res, err := s.Validate(context.Background(), ValidateRequest{
		BaseURL: "http://127.0.0.1:8080", APIKey: "k", Type: "openai",
	}, false)
	if err != nil {
		t.Fatalf("Validate err: %v", err)
	}
	if res.Valid {
		t.Error("expected private URL rejected for non-local caller")
	}
	if res.Error != "URL not allowed" {
		t.Errorf("Error = %q, want URL not allowed", res.Error)
	}
}

func TestNodeService_Validate_LocalAllowsPrivateURL(t *testing.T) {
	// isLocal=true bypasses the public-URL check; the validator proceeds to
	// the network call. To stay deterministic and network-free we point at a
	// closed port and assert the failure mode is the networkError fallback,
	// not the URL-rejected one.
	t.Parallel()
	s := &NodeService{}
	res, err := s.Validate(context.Background(), ValidateRequest{
		BaseURL: "http://127.0.0.1:1", APIKey: "k", Type: "openai",
	}, true)
	if err != nil {
		t.Fatalf("Validate err: %v", err)
	}
	if res.Valid {
		t.Error("expected non-2xx response (no server on port 1)")
	}
	if res.Error == "URL not allowed" {
		t.Error("local caller must not hit URL not allowed branch")
	}
}

func TestNodeService_Validate_LocalAllowsPrivateURL_AnthropicPath(t *testing.T) {
	t.Parallel()
	s := &NodeService{}
	res, err := s.Validate(context.Background(), ValidateRequest{
		BaseURL: "http://127.0.0.1:1", APIKey: "k", Type: "anthropic-compatible",
	}, true)
	if err != nil {
		t.Fatalf("Validate err: %v", err)
	}
	if res.Valid {
		t.Error("expected failure (no server on port 1)")
	}
}

func TestNodeService_Validate_LocalAllowsPrivateURL_EmbeddingPath(t *testing.T) {
	t.Parallel()
	s := &NodeService{}
	// Embedding path requires model id; without one it returns a validation error before hitting network.
	res, err := s.Validate(context.Background(), ValidateRequest{
		BaseURL: "http://127.0.0.1:1", APIKey: "k", Type: "custom-embedding",
	}, true)
	if err != nil {
		t.Fatalf("Validate err: %v", err)
	}
	if res.Valid {
		t.Error("expected Valid=false without model id")
	}
	if !contains(res.Error, "Model ID required") {
		t.Errorf("Error = %q, want substring 'Model ID required'", res.Error)
	}
}

// contains is a tiny helper for substring assertions.
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}