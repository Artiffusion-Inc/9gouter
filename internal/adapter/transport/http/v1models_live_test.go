package http

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/resolver"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// mockResolver is a live-model resolver for tests; it returns a fixed model
// list (or nil) and records the credentials it received.
type mockResolver struct {
	providerID string
	models     []resolver.ResolvedModel
	gotCreds   domainProv.Credentials
}

func (m *mockResolver) ProviderID() string { return m.providerID }
func (m *mockResolver) Resolve(ctx context.Context, creds domainProv.Credentials, opts resolver.ResolveOpts) (*resolver.Result, error) {
	m.gotCreds = creds
	if m.models == nil {
		return nil, nil
	}
	return &resolver.Result{Models: m.models}, nil
}

// TestBuildModelsList_LiveResolverOverridesStatic verifies that when a
// provider has both a static catalog and a live resolver returning models,
// /v1/models emits the LIVE catalog (not the static one) for that provider.
func TestBuildModelsList_LiveResolverOverridesStatic(t *testing.T) {
	// Register a mock resolver under "ollama" for this test, then restore.
	mock := &mockResolver{
		providerID: "ollama",
		models: []resolver.ResolvedModel{
			{ID: "live-model-1", Name: "Live Model 1", Kind: "llm"},
			{ID: "live-model-2", Name: "Live Model 2", Kind: "llm"},
		},
	}
	resolver.Register(mock)
	t.Cleanup(func() { resolver.Unregister("ollama") })

	h, db := newModelsHandler(t)
	mustCreateConnection(t, db, "ollama", `{"apiKey":"k","accessToken":"tok"}`)

	got := h.buildModelsList(context.Background(), []string{"llm"})

	// Live models should be present under the ollama alias.
	if !hasID(got, "ollama/live-model-1") {
		t.Fatalf("live model ollama/live-model-1 missing: %v", ids(got))
	}
	if !hasID(got, "ollama/live-model-2") {
		t.Fatalf("live model ollama/live-model-2 missing: %v", ids(got))
	}
	// Static ollama models should be suppressed for this provider (live takes
	// precedence for LLM entries).
	if hasID(got, "ollama/gpt-oss:120b") {
		t.Fatalf("static ollama model leaked when live catalog present: %v", ids(got))
	}
}

// TestBuildModelsList_LiveResolverFallsBackToStatic verifies that when a
// resolver returns nil (no live catalog), the static catalog is used.
func TestBuildModelsList_LiveResolverFallsBackToStatic(t *testing.T) {
	mock := &mockResolver{providerID: "ollama", models: nil} // nil = no live catalog
	resolver.Register(mock)
	t.Cleanup(func() { resolver.Unregister("ollama") })

	h, db := newModelsHandler(t)
	mustCreateConnection(t, db, "ollama", `{"apiKey":"k","accessToken":"tok"}`)

	got := h.buildModelsList(context.Background(), []string{"llm"})

	// Static ollama model should be present (live returned nil).
	if !hasID(got, "ollama/gpt-oss:120b") {
		t.Fatalf("static ollama model missing when live returned nil: %v", ids(got))
	}
	// Live model should NOT be present.
	if hasID(got, "ollama/live-model-1") {
		t.Fatalf("live model present when resolver returned nil: %v", ids(got))
	}
}

// TestResolveLiveCatalogs_NoAccessToken verifies that a connection without an
// accessToken still gets attempted (the resolver decides to skip); the
// mock records the call and returns its models regardless. The point: the
// handler must call the resolver for every active connection whose provider
// has a resolver, not gate on accessToken itself.
func TestResolveLiveCatalogs_CallsResolverPerConnection(t *testing.T) {
	mock := &mockResolver{
		providerID: "ollama",
		models:     []resolver.ResolvedModel{{ID: "m", Name: "M", Kind: "llm"}},
	}
	resolver.Register(mock)
	t.Cleanup(func() { resolver.Unregister("ollama") })

	h, db := newModelsHandler(t)
	mustCreateConnection(t, db, "ollama", `{"apiKey":"k","accessToken":"tok"}`)

	// Re-list connections the same way buildModelsList does.
	conns, err := h.deps.ConnectionRepo.List(context.Background(), repo.ConnectionFilter{IsActive: boolPtr(true)})
	if err != nil {
		t.Fatalf("list conns: %v", err)
	}
	out := h.resolveLiveCatalogs(context.Background(), conns)
	if out["ollama"] == nil || len(out["ollama"].Models) != 1 {
		t.Fatalf("resolveLiveCatalogs = %+v, want 1 ollama model", out)
	}
	// The resolver must have received the connection's accessToken.
	if mock.gotCreds.AccessToken != "tok" {
		t.Errorf("resolver got accessToken = %q, want tok", mock.gotCreds.AccessToken)
	}
}

// TestConnectionCredentials verifies the credential extraction from a
// connection record (apiKey, accessToken, providerSpecificData passthrough).
func TestConnectionCredentials(t *testing.T) {
	c := settings.ProviderConnection{
		ID:     "c1",
		Provider: "ollama",
		Data:    json.RawMessage(`{"apiKey":"k","accessToken":"at","providerSpecificData":{"profileArn":"arn:1"}}`),
	}
	creds := connectionCredentials(c)
	if creds.APIKey != "k" {
		t.Errorf("apiKey = %q", creds.APIKey)
	}
	if creds.AccessToken != "at" {
		t.Errorf("accessToken = %q", creds.AccessToken)
	}
	if creds.ProviderSpecificData["_connectionId"] != "c1" {
		t.Errorf("_connectionId = %v", creds.ProviderSpecificData["_connectionId"])
	}
	if creds.ProviderSpecificData["profileArn"] != "arn:1" {
		t.Errorf("profileArn passthrough = %v", creds.ProviderSpecificData["profileArn"])
	}
}