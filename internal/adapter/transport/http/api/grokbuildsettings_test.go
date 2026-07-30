package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
)

// withGrokTempHome installs a temp ~/.grok override (and forces "grok is
// installed" detection) for the duration of the test, restoring the package
// globals afterwards.
func withGrokTempHome(t *testing.T) string {
	t.Helper()
	prevHome := grokHomeDirOverride
	prevInstalled := grokCheckInstalledOverride
	dir := t.TempDir()
	grokHomeDirOverride = filepath.Join(dir, ".grok")
	grokCheckInstalledOverride = func() bool { return true }
	t.Cleanup(func() {
		grokHomeDirOverride = prevHome
		grokCheckInstalledOverride = prevInstalled
	})
	return grokHomeDirOverride
}

func TestGrokBuildSettings_GET_NotInstalled(t *testing.T) {
	grokHomeDirOverride = ""
	grokCheckInstalledOverride = func() bool { return false }
	t.Cleanup(func() { grokCheckInstalledOverride = nil })

	h := &cliToolsHandler{}
	rec := httptest.NewRecorder()
	h.grokBuildSettings(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if got["installed"] != false {
		t.Fatalf("installed = %v, want false (body=%s)", got["installed"], rec.Body.String())
	}
	if got["settings"] != nil {
		t.Fatalf("settings = %v, want nil when not installed", got["settings"])
	}
}

func TestGrokBuildSettings_GET_Configured(t *testing.T) {
	home := withGrokTempHome(t)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a config with an unrelated section so we can assert it round-trips.
	const seed = `[models]
default = "grok-build"

[theme]
mode = "dark"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &cliToolsHandler{}
	rec := httptest.NewRecorder()
	h.grokBuildSettings(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if got["installed"] != true {
		t.Fatalf("installed = %v, want true", got["installed"])
	}
	if got["has9Gouter"] != false {
		t.Fatalf("has9Gouter = %v, want false (no 9gouter slot seeded)", got["has9Gouter"])
	}
	if cp, _ := got["configPath"].(string); cp != filepath.Join(home, "config.toml") {
		t.Fatalf("configPath = %q, want %q", cp, filepath.Join(home, "config.toml"))
	}
}

func TestGrokBuildSettings_POST_AppliesThenGETReflects(t *testing.T) {
	home := withGrokTempHome(t)

	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterCliTools(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	body := `{"baseUrl":"http://127.0.0.1:20128","apiKey":"sk_9gouter","model":"grok/build-model","contextWindow":200000,"subagentModels":{"general-purpose":{"model":"anthropic/claude-opus-4.7","contextWindow":200000}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/cli-tools/grok-build-settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var postResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &postResp); err != nil {
		t.Fatalf("decode POST: %v body=%s", err, rec.Body.String())
	}
	if postResp["success"] != true {
		t.Fatalf("POST success = %v, want true (body=%s)", postResp["success"], rec.Body.String())
	}
	if postResp["modelSlot"] != "9gouter" {
		t.Fatalf("modelSlot = %v, want 9gouter", postResp["modelSlot"])
	}

	// The config file was written and the /v1 suffix was appended.
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Contains(cfg, []byte(`base_url = "http://127.0.0.1:20128/v1"`)) {
		t.Errorf("config missing normalized base_url:\n%s", cfg)
	}
	if !bytes.Contains(cfg, []byte(`[model.9gouter]`)) {
		t.Errorf("config missing [model.9gouter]:\n%s", cfg)
	}
	if !bytes.Contains(cfg, []byte(`[model.9gouter-general-purpose]`)) {
		t.Errorf("config missing subagent slot:\n%s", cfg)
	}

	// GET now reports the 9gouter slot as configured.
	getReq := httptest.NewRequest(http.MethodGet, "/api/cli-tools/grok-build-settings", nil)
	getReq.Header.Set("Cookie", "auth_token="+ck)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d (body=%s)", getRec.Code, getRec.Body.String())
	}
	var getResp map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode GET: %v body=%s", err, getRec.Body.String())
	}
	if getResp["installed"] != true {
		t.Fatalf("GET installed = %v, want true", getResp["installed"])
	}
	if getResp["has9Gouter"] != true {
		t.Fatalf("GET has9Gouter = %v, want true after POST (body=%s)", getResp["has9Gouter"], getRec.Body.String())
	}
}

func TestGrokBuildSettings_POST_RequiresBaseUrlAndModel(t *testing.T) {
	withGrokTempHome(t)
	h := &cliToolsHandler{}
	rec := httptest.NewRecorder()
	h.grokBuildSettings(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"baseUrl":"","model":""}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGrokBuildSettings_POST_DefaultApiKey(t *testing.T) {
	home := withGrokTempHome(t)
	h := &cliToolsHandler{}
	rec := httptest.NewRecorder()
	h.grokBuildSettings(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"baseUrl":"http://127.0.0.1:20128/v1","model":"grok/build-model","contextWindow":200000}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cfg, []byte(`api_key = "sk_9gouter"`)) {
		t.Errorf("default api_key not written:\n%s", cfg)
	}
}

func TestGrokBuildSettings_DELETE_NoConfig(t *testing.T) {
	home := withGrokTempHome(t)
	_ = os.RemoveAll(home) // ensure nothing exists

	h := &cliToolsHandler{}
	rec := httptest.NewRecorder()
	h.grokBuildSettings(rec, httptest.NewRequest(http.MethodDelete, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if got["message"] != "No config file to reset" {
		t.Fatalf("message = %v, want 'No config file to reset'", got["message"])
	}
}

func TestGrokBuildSettings_DELETE_Resets(t *testing.T) {
	home := withGrokTempHome(t)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed via POST so the file has our slots.
	h := &cliToolsHandler{}
	postRec := httptest.NewRecorder()
	h.grokBuildSettings(postRec, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"baseUrl":"http://127.0.0.1:20128/v1","model":"grok/build-model","contextWindow":200000}`)))
	if postRec.Code != http.StatusOK {
		t.Fatalf("seed POST failed: %s", postRec.Body.String())
	}

	delRec := httptest.NewRecorder()
	h.grokBuildSettings(delRec, httptest.NewRequest(http.MethodDelete, "/", nil))
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d (body=%s)", delRec.Code, delRec.Body.String())
	}
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cfg, []byte(`[model.9gouter]`)) {
		t.Errorf("DELETE left [model.9gouter]:\n%s", cfg)
	}
}
