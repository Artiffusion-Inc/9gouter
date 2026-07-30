package api

// grokbuildsettings.go ports src/app/api/cli-tools/grok-build-settings/route.js
// (decolua/9router) into the Go dashboard backend. It serves the three routes
// the frontend GrokBuildToolCard.js talks to:
//
//   GET    /api/cli-tools/grok-build-settings -> {installed, settings, has9Gouter, configPath} | {installed:false}
//   POST   /api/cli-tools/grok-build-settings <- {baseUrl, apiKey, model, contextWindow, subagentModels}
//        -> {success, message, configPath, modelSlot:"9gouter"}
//   DELETE /api/cli-tools/grok-build-settings -> {success, message} | {success:true,"No config file to reset"}
//
// The TOML editing lives in grokbuildconfig.go; this file is only the HTTP
// plumbing (install detection, read/write ~/.grok/config.toml, request/response
// shaping, context-window normalization via capabilities).

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/capabilities"
)

// grokHomeDirOverride, when non-empty, overrides the resolved ~/.grok path.
// Tests point it at a temp dir so the handlers never touch the real home.
var grokHomeDirOverride string

// grokCheckInstalledOverride, when non-nil, replaces grokCheckInstalled so tests
// can simulate a missing/installed grok binary without shelling out to `which`.
var grokCheckInstalledOverride func() bool

// grokHomeDir returns ~/.grok. The JS uses os.homedir(); we prefer os.UserHomeDir
// and fall back to $HOME (matches the headroom handler's HOME convention) so a
// process without a passwd entry still resolves.
func grokHomeDir() string {
	if grokHomeDirOverride != "" {
		return grokHomeDirOverride
	}
	if dir, err := os.UserHomeDir(); err == nil && dir != "" {
		return filepath.Join(dir, ".grok")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".grok")
	}
	return filepath.Join(".", ".grok")
}

func grokConfigPath() string { return filepath.Join(grokHomeDir(), "config.toml") }
func grokBinPath() string    { return filepath.Join(grokHomeDir(), "bin", "grok") }

// grokCheckInstalled mirrors checkGrokInstalled: `which grok` succeeds, or the
// grok binary or config file is present on disk.
func grokCheckInstalled() bool {
	if grokCheckInstalledOverride != nil {
		return grokCheckInstalledOverride()
	}
	which := "which"
	if runtime.GOOS == "windows" {
		which = "where"
	}
	if err := exec.Command(which, "grok").Run(); err == nil {
		return true
	}
	for _, candidate := range []string{grokBinPath(), grokConfigPath()} {
		if _, err := os.Stat(candidate); err == nil {
			return true
		}
	}
	return false
}

// grokReadConfigToml mirrors readConfigToml: ENOENT -> "", other errors
// propagate.
func grokReadConfigToml() (string, error) {
	b, err := os.ReadFile(grokConfigPath())
	if err == nil {
		return string(b), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return "", err
}

// grokNormalizeContextWindow mirrors normalizeContextWindow: an explicit
// positive finite number wins; otherwise fall back to the model's capability
// context window. The package-level toFiniteInt (codex_reset_credits.go) clamps
// negatives to 0 and returns 0 for non-numbers, so the `> 0` check is enough to
// tell "explicit positive" from "absent" — matching Number.isFinite && > 0.
func grokNormalizeContextWindow(value any, model string) int {
	if n := toFiniteInt(value); n > 0 {
		return n
	}
	provider, modelID := splitProviderModel(model)
	return capabilities.GetCapabilitiesForModel(provider, modelID).ContextWindow
}

// splitProviderModel mirrors the JS slash split: "provider/model-id" ->
// ("provider", "model-id"); bare "model-id" -> ("", "model-id").
func splitProviderModel(model string) (provider, modelID string) {
	if i := strings.Index(model, "/"); i > 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}

// grokNormalizeSubagentModels mirrors normalizeSubagentModels. nil input (the
// "subagentModels" key absent in the JSON body) returns nil, which signals
// applyGrokBuildConfig to leave existing overrides untouched; an empty map
// clears them. Each entry accepts either a bare model string or
// {model, contextWindow}; blank means inherit the main model.
func grokNormalizeSubagentModels(value any) map[string]*grokSubagentOverride {
	if value == nil {
		return nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return map[string]*grokSubagentOverride{}
	}
	out := map[string]*grokSubagentOverride{}
	for _, typ := range grokSubagentTypes {
		entry, present := m[typ]
		if !present {
			continue
		}
		var model string
		var cwRaw any
		switch e := entry.(type) {
		case string:
			model = strings.TrimSpace(e)
		case map[string]any:
			if s, ok := e["model"].(string); ok {
				model = strings.TrimSpace(s)
			}
			cwRaw = e["contextWindow"]
		}
		if model == "" {
			continue
		}
		out[typ] = &grokSubagentOverride{
			Model:         model,
			ContextWindow: grokNormalizeContextWindow(cwRaw, model),
		}
	}
	return out
}

func (h *cliToolsHandler) grokBuildSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.grokBuildSettingsGet(w, r)
	case http.MethodPost:
		h.grokBuildSettingsPost(w, r)
	case http.MethodDelete:
		h.grokBuildSettingsDelete(w, r)
	}
}

func (h *cliToolsHandler) grokBuildSettingsGet(w http.ResponseWriter, _ *http.Request) {
	if !grokCheckInstalled() {
		writeJSON(w, http.StatusOK, map[string]any{
			"installed": false,
			"settings":  nil,
			"message":   "Grok Build is not installed",
		})
		return
	}
	toml, err := grokReadConfigToml()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to check grok-build settings")
		return
	}
	settings := parseGrokBuildConfig(toml)
	writeJSON(w, http.StatusOK, map[string]any{
		"installed":  true,
		"settings":   settings,
		"has9Gouter": hasGrokBuildConfig(settings),
		"configPath": grokConfigPath(),
	})
}

func (h *cliToolsHandler) grokBuildSettingsPost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL        string          `json:"baseUrl"`
		APIKey         string          `json:"apiKey"`
		Model          string          `json:"model"`
		ContextWindow  json.RawMessage `json:"contextWindow"`
		SubagentModels json.RawMessage `json:"subagentModels"`
	}
	if err := parseJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	model := strings.TrimSpace(body.Model)
	if body.BaseURL == "" || model == "" {
		writeError(w, http.StatusBadRequest, "baseUrl and model are required")
		return
	}

	if err := os.MkdirAll(grokHomeDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update grok-build settings")
		return
	}

	normalizedBaseURL := body.BaseURL
	if !strings.HasSuffix(normalizedBaseURL, "/v1") {
		normalizedBaseURL += "/v1"
	}
	apiKey := body.APIKey
	if apiKey == "" {
		apiKey = "sk_9gouter"
	}

	var cwRaw any
	if len(body.ContextWindow) > 0 && string(body.ContextWindow) != "null" {
		_ = json.Unmarshal(body.ContextWindow, &cwRaw)
	}
	var subRaw any
	if len(body.SubagentModels) > 0 && string(body.SubagentModels) != "null" {
		_ = json.Unmarshal(body.SubagentModels, &subRaw)
	}

	toml, err := grokReadConfigToml()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update grok-build settings")
		return
	}
	next := applyGrokBuildConfig(
		toml,
		normalizedBaseURL,
		apiKey,
		model,
		grokNormalizeContextWindow(cwRaw, model),
		grokNormalizeSubagentModels(subRaw),
	)
	if err := os.WriteFile(grokConfigPath(), []byte(next), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update grok-build settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"message":    "Grok Build settings applied successfully!",
		"configPath": grokConfigPath(),
		"modelSlot":  grokMainModelSlot,
	})
}

func (h *cliToolsHandler) grokBuildSettingsDelete(w http.ResponseWriter, _ *http.Request) {
	toml, err := grokReadConfigToml()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to reset grok-build settings")
		return
	}
	if toml == "" {
		// readConfigToml returns "" both for ENOENT and an empty file; the JS
		// distinguishes ENOENT ("No config file to reset") from a present file.
		// Stat to recover that distinction.
		if _, statErr := os.Stat(grokConfigPath()); statErr != nil && errors.Is(statErr, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "No config file to reset"})
			return
		}
	}
	if err := os.WriteFile(grokConfigPath(), []byte(resetGrokBuildConfig(toml)), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to reset grok-build settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "9gouter model slots removed from Grok Build",
	})
}
