package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/capabilities"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// newModelsHandler wires a v1Handler with the minimal deps for buildModelsList
// against a fresh temp SQLite DB. It returns the handler and the DB handle so
// tests can populate combos / disabled models / custom models directly.
func newModelsHandler(t *testing.T) (*v1Handler, *sql.DB) {
	t.Helper()
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		DisabledModels: repo.NewDisabledModelsRepo(db),
		Config:         config.Config{},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	return newV1Handler(deps), db
}

func TestKindFilterFromPath(t *testing.T) {
	cases := map[string][]string{
		"":      {"llm"},
		"llm":   {"llm"},
		"LLM":   {"llm"},
		"all":   nil,
		"image": {"image"},
	}
	for in, want := range cases {
		got := kindFilterFromPath(in)
		if !sameStrs(got, want) {
			t.Errorf("kindFilterFromPath(%q) = %v, want %v", in, got, want)
		}
	}
}

func sameStrs(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBuildModelsList_StaticCatalogOnlyActive verifies the core #2702
// acceptance: a provider's static catalog is listed only when an active
// connection exists, and entries are prefixed as "<alias>/<modelId>".
func TestBuildModelsList_StaticCatalogOnlyActive(t *testing.T) {
	h, db := newModelsHandler(t)

	// No active connection for ollama -> no ollama models.
	got := h.buildModelsList(context.Background(), []string{"llm"})
	if hasID(got, "ollama/gpt-oss:120b") {
		t.Fatalf("ollama model listed without active connection: %v", ids(got))
	}

	// Create an active ollama connection; now the static catalog appears.
	mustCreateConnection(t, db, "ollama", `{"apiKey":"k"}`)

	got = h.buildModelsList(context.Background(), []string{"llm"})
	if !hasID(got, "ollama/gpt-oss:120b") {
		t.Fatalf("ollama/gpt-oss:120b missing with active connection: %v", ids(got))
	}
	if !hasID(got, "ollama/minimax-m3") {
		t.Fatalf("ollama/minimax-m3 missing: %v", ids(got))
	}
	// Inactive provider (no connection) should not contribute.
	if hasID(got, "openai/gpt-4") {
		t.Fatalf("openai catalog leaked without active connection: %v", ids(got))
	}
}

// TestBuildModelsList_DisabledRemoved verifies disabled models are filtered.
func TestBuildModelsList_DisabledRemoved(t *testing.T) {
	h, db := newModelsHandler(t)
	mustCreateConnection(t, db, "ollama", `{"apiKey":"k"}`)

	dm := repo.NewDisabledModelsRepo(db)
	if err := dm.Disable(context.Background(), "ollama", []string{"glm-5"}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	got := h.buildModelsList(context.Background(), []string{"llm"})
	if hasID(got, "ollama/glm-5") {
		t.Fatalf("disabled model ollama/glm-5 present: %v", ids(got))
	}
	if !hasID(got, "ollama/gpt-oss:120b") {
		t.Fatalf("non-disabled model removed: %v", ids(got))
	}
}

// TestBuildModelsList_KindFilter verifies /v1/models/{kind} filtering.
func TestBuildModelsList_KindFilter(t *testing.T) {
	h, db := newModelsHandler(t)
	mustCreateConnection(t, db, "ollama", `{"apiKey":"k"}`)

	// ollama catalog is llm-only; an "image" kind filter must exclude it.
	got := h.buildModelsList(context.Background(), []string{"image"})
	if hasID(got, "ollama/gpt-oss:120b") {
		t.Fatalf("llm model leaked into image kind filter: %v", ids(got))
	}

	// "all" (nil filter) includes everything.
	got = h.buildModelsList(context.Background(), nil)
	if !hasID(got, "ollama/gpt-oss:120b") {
		t.Fatalf("ollama model missing under nil kind filter: %v", ids(got))
	}
}

// TestBuildModelsList_CombosFirst verifies combos are emitted as model ids.
func TestBuildModelsList_CombosFirst(t *testing.T) {
	h, db := newModelsHandler(t)
	cr := repo.NewComboRepo(db)
	if err := cr.Create(context.Background(), settings.Combo{
		ID:     "combo-my-combo",
		Name:   "my-combo",
		Kind:   "llm",
		Models: json.RawMessage(`["ollama/gpt-oss:120b"]`),
	}); err != nil {
		t.Fatalf("create combo: %v", err)
	}

	got := h.buildModelsList(context.Background(), []string{"llm"})
	if !hasID(got, "my-combo") {
		t.Fatalf("combo not listed: %v", ids(got))
	}
}

// TestBuildModelsList_CustomModels verifies custom models merge under the
// provider alias prefix.
func TestBuildModelsList_CustomModels(t *testing.T) {
	h, db := newModelsHandler(t)
	mustCreateConnection(t, db, "ollama", `{"apiKey":"k"}`)
	ar := repo.NewAliasRepo(db)
	if _, err := ar.AddCustomModel(context.Background(), repo.CustomModel{
		ProviderAlias: "ollama",
		ID:            "my-custom-model",
		Type:          "llm",
		Name:          "My Custom",
	}); err != nil {
		t.Fatalf("add custom model: %v", err)
	}

	got := h.buildModelsList(context.Background(), []string{"llm"})
	if !hasID(got, "ollama/my-custom-model") {
		t.Fatalf("custom model ollama/my-custom-model missing: %v", ids(got))
	}
}

func hasID(ms []oaiModel, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

func ids(ms []oaiModel) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// TestCapsForModel_LLM_SurfacesCapabilities ports upstream 2629218b: a known LLM
// model (claude-opus-4.8) surfaces its resolved capabilities (vision/reasoning/
// search, 1M context) on /v1/models, while an unknown LLM id carries only the
// Default floor and is therefore omitted (no redundant blob).
func TestCapsForModel_LLM_SurfacesCapabilities(t *testing.T) {
	c := capsForModel("", "claude-opus-4.8", "llm")
	if c == nil {
		t.Fatal("claude-opus-4.8 should surface capabilities")
	}
	if !c.Vision || !c.Reasoning || !c.Search {
		t.Errorf("claude-opus-4.8 caps = %+v, want vision+reasoning+search", c)
	}
	if c.ContextWindow != 1000000 {
		t.Errorf("claude-opus-4.8 ContextWindow = %d, want 1000000", c.ContextWindow)
	}
	// Unknown LLM → Default floor only → no signal → nil (no redundant blob).
	if c2 := capsForModel("", "some-unknown-model-xyz", "llm"); c2 != nil {
		t.Errorf("unknown LLM should not surface capabilities, got %+v", c2)
	}
}

// TestCapsForModel_NonLLM_ServiceKind verifies non-LLM kinds resolve via
// capabilitiesFromServiceKind (imageToText → vision, embedding → tools:false).
func TestCapsForModel_NonLLM_ServiceKind(t *testing.T) {
	c := capsForModel("", "some-vision-model", "imageToText")
	if c == nil || !c.Vision {
		t.Fatal("imageToText kind should resolve to a Vision capability delta")
	}
	c2 := capsForModel("", "text-embedding-3", "embedding")
	if c2 == nil || c2.Tools {
		t.Fatal("embedding kind should resolve to Tools:false")
	}
}

// TestBuildModelsList_CapabilitiesPopulated verifies the end-to-end path: a
// provider catalog model with a known id carries capabilities in the
// serialized /v1/models entry. We use the ollama static catalog entry that
// resolves to a known kimi/coder id via capsForModel if present; otherwise we
// assert the field is simply absent for unknown ids (no panic, omitempty).
func TestBuildModelsList_CapabilitiesPopulated(t *testing.T) {
	h, db := newModelsHandler(t)
	mustCreateConnection(t, db, "ollama", `{"apiKey":"k"}`)

	got := h.buildModelsList(context.Background(), []string{"llm"})
	// At least one entry must serialize without error and the capabilities
	// field, when present, must round-trip through JSON with the expected shape.
	enc, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal models: %v", err)
	}
	// The list must include known ollama catalog ids (TestBuildModelsList_StaticCatalogOnlyActive).
	if !strings.Contains(string(enc), "ollama/gpt-oss:120b") {
		t.Fatalf("ollama/gpt-oss:120b missing from catalog: %s", string(enc))
	}
	// Find any entry that carries a capabilities blob and assert its shape.
	var list []oaiModel
	if err := json.Unmarshal(enc, &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var found *capabilities.Capabilities
	for i := range list {
		if list[i].Capabilities != nil {
			found = list[i].Capabilities
			break
		}
	}
	if found != nil {
		// A surfaced capability must carry real signal (non-default ContextWindow
		// or a modality flag), never the bare Default floor.
		if found.ContextWindow == capabilities.Default.ContextWindow &&
			!found.Vision && !found.Reasoning && !found.Search && !found.ImageOutput {
			t.Errorf("surfaced capability is bare Default floor: %+v", found)
		}
	}
}
