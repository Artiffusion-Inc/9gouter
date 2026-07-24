package http

// v1models_compatible_test.go pins 88a8c72d — fetchCompatibleModelIds +
// buildModelsList wiring for openai-compatible-* / anthropic-compatible-* /
// custom-embedding-* provider nodes. The live catalog is fetched from each
// active compatible connection's baseUrl, surfaced under the node's prefix,
// with the x-9r-internal-models-fetch recursion guard. Real sqlite
// ConnectionRepo + NodeRepo + httptest.Server upstreams — no mock.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// compatibleData builds a connection data blob for an openai-compatible node:
// apiKey at the top level + providerSpecificData{baseUrl, prefix}.
func compatibleData(apiKey, prefix, baseURL string) string {
	b, _ := json.Marshal(map[string]any{
		"apiKey": apiKey,
		"providerSpecificData": map[string]any{
			"baseUrl": baseURL,
			"prefix":  prefix,
		},
	})
	return string(b)
}

// modelsServer replays a fixed /models JSON body and records the auth header +
// the x-9r-internal-models-fetch header.
func modelsServer(t *testing.T, body string) (*httptest.Server, *string, *string) {
	t.Helper()
	var gotAuth, gotInternal string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotInternal = r.Header.Get(internalModelsFetchHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotAuth, &gotInternal
}

// TestFetchCompatibleModelIds_OpenAIStyle verifies the openai-compatible path:
// Bearer auth, x-9r-internal-models-fetch set, ids parsed from {data:[...]}
// and deduped.
func TestFetchCompatibleModelIds_OpenAIStyle(t *testing.T) {
	srv, gotAuth, gotInternal := modelsServer(t, `{"data":[{"id":"gpt-foo"},{"id":"gpt-bar"},{"id":"gpt-foo"},{"id":""},{"name":"named"}]}`)
	conn := settings.ProviderConnection{
		Provider: "9gouter-openai-compatible-chat-abc",
		Data:     json.RawMessage(compatibleData("sk-1", "ollama", srv.URL)),
	}
	client := &http.Client{}
	ids := fetchCompatibleModelIds(context.Background(), client, conn)
	got := make([]string, 0, len(ids))
	for _, m := range ids {
		got = append(got, m.ID)
	}
	want := []string{"gpt-foo", "gpt-bar", "named"}
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v (dedup + blank drop + name fallback)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if *gotAuth != "Bearer sk-1" {
		t.Errorf("Authorization = %q, want Bearer sk-1", *gotAuth)
	}
	if *gotInternal != "1" {
		t.Errorf("x-9r-internal-models-fetch = %q, want 1", *gotInternal)
	}
	for _, m := range ids {
		if m.Kind != "llm" {
			t.Errorf("kind(%q) = %q, want llm", m.ID, m.Kind)
		}
	}
}

// TestFetchCompatibleModelIds_BareArray verifies a bare-array /models body
// is parsed.
func TestFetchCompatibleModelIds_BareArray(t *testing.T) {
	srv, _, _ := modelsServer(t, `[{"id":"m1"},{"id":"m2"}]`)
	conn := settings.ProviderConnection{
		Provider: "9gouter-openai-compatible-chat-x",
		Data:     json.RawMessage(compatibleData("sk", "p", srv.URL)),
	}
	ids := fetchCompatibleModelIds(context.Background(), &http.Client{}, conn)
	if len(ids) != 2 || ids[0].ID != "m1" || ids[1].ID != "m2" {
		t.Fatalf("ids = %+v, want [m1 m2]", ids)
	}
}

// TestFetchCompatibleModelIds_AnthropicHeaders verifies the anthropic-compatible
// path: x-api-key + anthropic-version + Bearer headers, GET <baseUrl>/models.
// (The JS /messages→/models rewrite branch is dead code — url always ends in
// /models — so the real path is baseUrl without /messages.)
func TestFetchCompatibleModelIds_AnthropicHeaders(t *testing.T) {
	var gotPath, gotXAPIKey, gotAnthVer, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotXAPIKey = r.Header.Get("x-api-key")
		gotAnthVer = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-x"}]}`)
	}))
	t.Cleanup(srv.Close)

	data, _ := json.Marshal(map[string]any{
		"apiKey": "ant-key",
		"providerSpecificData": map[string]any{
			"baseUrl": srv.URL,
			"prefix":  "anth",
		},
	})
	conn := settings.ProviderConnection{
		Provider: "9gouter-anthropic-compatible-y",
		Data:     json.RawMessage(data),
	}
	ids := fetchCompatibleModelIds(context.Background(), &http.Client{}, conn)
	if len(ids) != 1 || ids[0].ID != "claude-x" {
		t.Fatalf("ids = %+v, want [claude-x]", ids)
	}
	if gotPath != "/models" {
		t.Errorf("upstream path = %q, want /models", gotPath)
	}
	if gotXAPIKey != "ant-key" {
		t.Errorf("x-api-key = %q, want ant-key", gotXAPIKey)
	}
	if gotAnthVer != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", gotAnthVer)
	}
	if gotAuth != "Bearer ant-key" {
		t.Errorf("Authorization = %q, want Bearer ant-key", gotAuth)
	}
}

// TestFetchCompatibleModelIds_FailsOpen verifies non-2xx + missing baseUrl
// yield no ids (no error surfaced) — the catalog simply omits the provider.
func TestFetchCompatibleModelIds_FailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	conn := settings.ProviderConnection{
		Provider: "9gouter-openai-compatible-chat-z",
		Data:     json.RawMessage(compatibleData("sk", "p", srv.URL)),
	}
	if ids := fetchCompatibleModelIds(context.Background(), &http.Client{}, conn); len(ids) != 0 {
		t.Errorf("non-2xx → ids = %v, want empty (fail open)", ids)
	}
	// Missing baseUrl → empty.
	conn.Data = json.RawMessage(`{"apiKey":"sk"}`)
	if ids := fetchCompatibleModelIds(context.Background(), &http.Client{}, conn); len(ids) != 0 {
		t.Errorf("missing baseUrl → ids = %v, want empty", ids)
	}
}

// TestInferKindFromModelID covers the kind inference for non-LLM ids.
func TestInferKindFromModelID(t *testing.T) {
	cases := map[string]string{
		"gpt-4o":           "llm",
		"text-embedding-3": "embedding",
		"tts-1":            "tts",
		"whisper-audio":    "tts",
		"dall-e-3":         "image",
		"flux-pro":         "image",
		"sd-xl":            "image",
	}
	for id, want := range cases {
		if got := inferKindFromModelID(id); got != want {
			t.Errorf("inferKindFromModelID(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestBuildModelsList_CompatibleProvider verifies the end-to-end wiring: an
// active openai-compatible connection whose baseUrl serves /models surfaces
// those ids under the node prefix in /v1/models. Real sqlite repos.
func TestBuildModelsList_CompatibleProvider(t *testing.T) {
	h, db := newModelsHandler(t)

	srv, gotAuth, gotInternal := modelsServer(t, `{"data":[{"id":"qwen-max"},{"id":"qwen-coder"}]}`)
	t.Cleanup(srv.Close)

	// Create the compatible node + an active connection referencing it.
	nodeRepo := repo.NewNodeRepo(db)
	if err := nodeRepo.Create(context.Background(), settings.ProviderNode{
		ID:   "9gouter-openai-compatible-chat-abc",
		Type: "openai-compatible",
		Name: "MyNode",
		Data: json.RawMessage(`{"prefix":"mynode","baseUrl":"` + srv.URL + `","apiType":"chat"}`),
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	connData := compatibleData("sk-live", "mynode", srv.URL)
	mustCreateConnectionWithID(t, db, "comp-conn", "9gouter-openai-compatible-chat-abc", connData)

	got := h.buildModelsList(context.Background(), []string{"llm"}, false)
	if !hasID(got, "mynode/qwen-max") {
		t.Fatalf("mynode/qwen-max missing from /v1/models: %v", ids(got))
	}
	if !hasID(got, "mynode/qwen-coder") {
		t.Fatalf("mynode/qwen-coder missing: %v", ids(got))
	}
	// Auth + recursion-guard header reached the upstream.
	if *gotAuth != "Bearer sk-live" {
		t.Errorf("upstream Authorization = %q, want Bearer sk-live", *gotAuth)
	}
	if *gotInternal != "1" {
		t.Errorf("upstream x-9r-internal-models-fetch = %q, want 1", *gotInternal)
	}

	// The entry carries the node prefix as owned_by.
	for _, m := range got {
		if m.ID == "mynode/qwen-max" && m.OwnedBy != "mynode" {
			t.Errorf("owned_by = %q, want mynode", m.OwnedBy)
		}
	}
}

// TestBuildModelsList_CompatibleSkipDynamicFetch verifies the recursion guard:
// when skipDynamicFetch is true (the request carried x-9r-internal-models-fetch),
// no live /models fetch is issued.
func TestBuildModelsList_CompatibleSkipDynamicFetch(t *testing.T) {
	h, db := newModelsHandler(t)

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"data":[{"id":"should-not-appear"}]}`)
	}))
	t.Cleanup(srv.Close)

	nodeRepo := repo.NewNodeRepo(db)
	_ = nodeRepo.Create(context.Background(), settings.ProviderNode{
		ID:   "9gouter-openai-compatible-chat-skip",
		Type: "openai-compatible",
		Data: json.RawMessage(`{"prefix":"skp"}`),
	})
	mustCreateConnectionWithID(t, db, "skp-conn", "9gouter-openai-compatible-chat-skip",
		compatibleData("sk", "skp", srv.URL))

	got := h.buildModelsList(context.Background(), []string{"llm"}, true)
	if called {
		t.Error("upstream /models was called despite skipDynamicFetch=true")
	}
	if hasID(got, "skp/should-not-appear") {
		t.Errorf("dynamic model surfaced under recursion guard: %v", ids(got))
	}
}

// TestBuildModelsList_CompatibleKindFilter verifies non-LLM ids (e.g. an
// embedding model) are only surfaced when the kind filter asks for them.
func TestBuildModelsList_CompatibleKindFilter(t *testing.T) {
	h, db := newModelsHandler(t)

	srv, _, _ := modelsServer(t, `{"data":[{"id":"llm-1"},{"id":"text-embedding-3"}]}`)
	t.Cleanup(srv.Close)

	nodeRepo := repo.NewNodeRepo(db)
	_ = nodeRepo.Create(context.Background(), settings.ProviderNode{
		ID:   "9gouter-openai-compatible-chat-kf",
		Type: "openai-compatible",
		Data: json.RawMessage(`{"prefix":"kf"}`),
	})
	mustCreateConnectionWithID(t, db, "kf-conn", "9gouter-openai-compatible-chat-kf",
		compatibleData("sk", "kf", srv.URL))

	// LLM-only filter → embedding model excluded.
	llm := h.buildModelsList(context.Background(), []string{"llm"}, false)
	if !hasID(llm, "kf/llm-1") {
		t.Errorf("kf/llm-1 missing on llm filter: %v", ids(llm))
	}
	if hasID(llm, "kf/text-embedding-3") {
		t.Errorf("embedding model surfaced on llm filter: %v", ids(llm))
	}
	// "all" (nil) → both present.
	all := h.buildModelsList(context.Background(), nil, false)
	if !hasID(all, "kf/llm-1") || !hasID(all, "kf/text-embedding-3") {
		t.Errorf("both kinds expected on nil filter: %v", ids(all))
	}
}

// TestBuildModelsList_CompatibleNodePrefixFallback verifies the prefix is
// resolved from the node when the connection's psd.prefix is absent.
func TestBuildModelsList_CompatibleNodePrefixFallback(t *testing.T) {
	h, db := newModelsHandler(t)

	srv, _, _ := modelsServer(t, `{"data":[{"id":"m1"}]}`)
	t.Cleanup(srv.Close)

	// Node defines prefix "fallback"; connection data carries only baseUrl (no prefix).
	nodeRepo := repo.NewNodeRepo(db)
	_ = nodeRepo.Create(context.Background(), settings.ProviderNode{
		ID:   "9gouter-openai-compatible-chat-fb",
		Type: "openai-compatible",
		Data: json.RawMessage(`{"prefix":"fallback"}`),
	})
	data, _ := json.Marshal(map[string]any{
		"apiKey":               "sk",
		"providerSpecificData": map[string]any{"baseUrl": srv.URL},
	})
	mustCreateConnectionWithID(t, db, "fb-conn", "9gouter-openai-compatible-chat-fb", string(data))

	got := h.buildModelsList(context.Background(), []string{"llm"}, false)
	if !hasID(got, "fallback/m1") {
		t.Errorf("prefix fallback from node failed: %v", ids(got))
	}
}
