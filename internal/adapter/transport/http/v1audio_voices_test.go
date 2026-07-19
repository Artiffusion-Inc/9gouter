package http

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/transport/http/api"
)

// TestV1AudioVoices_FullMux verifies the /v1/audio/voices route is registered
// on the v1 mux and self-dispatches through RegisterMediaProviders to produce
// the OpenAI-style voice list for edge-tts.
func TestV1AudioVoices_FullMux(t *testing.T) {
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
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	// Register the internal voice-list handlers on the same mux so the
	// self-dispatch inside HandleV1AudioVoices resolves them.
	api.RegisterMediaProviders(mux, api.Deps{})

	req := httptest.NewRequest(http.MethodGet, "/v1/audio/voices?provider=edge-tts", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["object"] != "list" {
		t.Errorf("object = %v, want list", out["object"])
	}
	data, _ := out["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("expected non-empty voice list, got 0; body=%s", rec.Body.String())
	}
	v, _ := data[0].(map[string]any)
	if model, _ := v["model"].(string); model == "" || model == "edge-tts/" {
		t.Errorf("model = %q, want edge-tts/<voiceId>", model)
	}
}

// TestV1AudioVoices_UnknownProvider verifies the 400 path through the full mux.
func TestV1AudioVoices_UnknownProvider(t *testing.T) {
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	deps := V1Deps{
		APIKeysRepo: repo.NewAPIKeyRepo(db), SettingsRepo: repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db), ComboRepo: repo.NewComboRepo(db),
		AliasRepo: repo.NewAliasRepo(db), NodeRepo: repo.NewNodeRepo(db),
		ProxyPoolRepo: repo.NewProxyPoolRepo(db),
		Config: config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	api.RegisterMediaProviders(mux, api.Deps{})

	req := httptest.NewRequest(http.MethodGet, "/v1/audio/voices?provider=nope", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}