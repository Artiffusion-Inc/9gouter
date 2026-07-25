package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// mediaproviders_voices.go ports the legacy JS dashboard voice-listing routes
// (src/app/api/media-providers/tts/{elevenlabs,deepgram,inworld,minimax}/voices/
// route.js). Each route picks the first active connection for the provider,
// reads its apiKey, calls the provider's voice-list API through the proxy stack
// (connectionProxyFetch — same path as the live usage fetchers), normalizes the
// result into the edge-tts grouped shape {languages, byLang}, and — when a lang
// query filter is present — returns just {voices:[...]} for that group.
//
// All four share the byLang structure consumed by TtsExampleCard.js
// (voiceSource "api-language"): byLang[code] = {code, name, voices:[{id,name,
// gender,lang}]}. lang-name resolution mirrors JS Intl.DisplayNames(["en"]).

// langName returns the English display name for a BCP-47 code, falling back to
// the code itself on any parse error (matching JS `try { langNames.of(code) }
// catch { return code }`).
func langName(code string) string {
	tag, err := language.Parse(code)
	if err != nil {
		return code
	}
	name := display.English.Tags().Name(tag)
	if name == "" {
		return code
	}
	return name
}

// connAPIKey reads data.apiKey from the connection blob.
func connAPIKey(conn *settings.ProviderConnection) string {
	var d map[string]any
	if err := json.Unmarshal(conn.Data, &d); err != nil {
		return ""
	}
	if s, ok := d["apiKey"].(string); ok {
		return s
	}
	return ""
}

// fetchGroupedVoices resolves the first active connection for provider, reads
// its apiKey, and dispatches to the per-provider fetcher. On any failure it
// returns the JSON error shape the frontend's modal-error path renders.
func (h *mediaHandler) fetchGroupedVoices(w http.ResponseWriter, r *http.Request, provider string) {
	if h.deps.Connections == nil {
		writeError(w, http.StatusBadRequest, "No "+provider+" connection found")
		return
	}
	active := true
	conns, err := h.deps.Connections.List(r.Context(), repo.ConnectionFilter{Provider: provider, IsActive: &active})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch connection")
		return
	}
	if len(conns) == 0 {
		writeError(w, http.StatusBadRequest, "No "+provider+" connection found")
		return
	}
	conn := &conns[0]
	apiKey := connAPIKey(conn)
	if apiKey == "" {
		writeError(w, http.StatusBadRequest, "No "+provider+" connection found")
		return
	}

	langFilter := strings.TrimSpace(r.URL.Query().Get("lang"))
	voiceType := strings.TrimSpace(r.URL.Query().Get("voice_type"))
	if voiceType == "" {
		voiceType = "all"
	}

	var grouped *voiceGroups
	switch provider {
	case "elevenlabs":
		grouped, err = fetchElevenLabsVoices(r.Context(), h.deps.ProxyPools, h.deps.ProxyOpts, conn, apiKey)
	case "deepgram":
		grouped, err = fetchDeepgramVoices(r.Context(), h.deps.ProxyPools, h.deps.ProxyOpts, conn, apiKey)
	case "inworld":
		grouped, err = fetchInworldVoices(r.Context(), h.deps.ProxyPools, h.deps.ProxyOpts, conn, apiKey)
	case "minimax", "minimax-cn":
		grouped, err = fetchMiniMaxVoices(r.Context(), h.deps.ProxyPools, h.deps.ProxyOpts, conn, provider, apiKey, voiceType)
	default:
		writeError(w, http.StatusBadRequest, "Unsupported voice provider: "+provider)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if grouped == nil || len(grouped.byLang) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"languages": []any{}, "byLang": map[string]any{}})
		return
	}

	if langFilter != "" {
		voices, ok := grouped.byLang[langFilter]
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"voices": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"voices": voices["voices"]})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"languages": grouped.languages(),
		"byLang":    grouped.byLang,
	})
}

// voiceGroups is the normalized {byLang, languages} shape shared by all four
// providers. byLang maps a language code to {code, name, voices:[...]}; the
// order of byLang is not significant — languages() returns the sorted slice the
// frontend renders.
type voiceGroups struct {
	byLang map[string]map[string]any
}

func (g *voiceGroups) languages() []map[string]any {
	out := make([]map[string]any, 0, len(g.byLang))
	for _, l := range g.byLang {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	return out
}

// addVoice appends a voice to a language group, creating the group when needed
// and skipping duplicates by id (matching the JS `find(v => v.id === ...)` guard).
func (g *voiceGroups) addVoice(code string, voice map[string]any) {
	grp, ok := g.byLang[code]
	if !ok {
		grp = map[string]any{"code": code, "name": langName(code), "voices": []any{}}
		g.byLang[code] = grp
	}
	voices := grp["voices"].([]any)
	id, _ := voice["id"].(string)
	for _, e := range voices {
		if vm, ok := e.(map[string]any); ok {
			if eid, _ := vm["id"].(string); eid == id {
				return
			}
		}
	}
	grp["voices"] = append(voices, voice)
}

func newVoiceGroups() *voiceGroups {
	return &voiceGroups{byLang: map[string]map[string]any{}}
}

// doVoicesRequest runs req through the proxy stack (connectionProxyFetch) and
// returns the drained body + status. It mirrors doJSON in the quotafetch
// package but lives here to reuse connectionProxyFetch.
func doVoicesRequest(ctx context.Context, pools *repo.ProxyPoolRepo, opts proxy.Options, conn *settings.ProviderConnection, req *http.Request) ([]byte, int, error) {
	resp, err := connectionProxyFetch(ctx, pools, opts, conn, req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	return readBodySafe(resp.Body), resp.StatusCode, nil
}
