package api

import (
	"net/http"
	"strings"
)

// RegisterV1Dashboard mounts the dashboard-side /api/v1 routes.
//
// The real client-facing API lives under /v1/* (registered by
// httptransport.RegisterV1). The legacy JS build exposed a parallel
// /api/v1/* surface the static frontend called, which re-dispatched the
// same handlers. In the Go build we preserve that surface as a thin alias:
// for every /v1/* endpoint that is actually implemented, /api/v1/* is a
// passthrough that rewrites the path and delegates to deps.V1Dispatch.
// Endpoints with no /v1/* implementation yet (modality pipelines: audio,
// image, video, search, web/fetch, embeddings, models/info, api/chat,
// responses/compact) remain explicit not-available stubs so the gap is
// visible. See docs/goals/go-rewrite/notes/T033-api-v1-stub-audit.md.
func RegisterV1Dashboard(mux *http.ServeMux, deps Deps) {
	h := &v1DashboardHandler{deps: deps}

	mux.HandleFunc("GET /api/v1", h.root)

	// Implemented in /v1/* — passthrough.
	mux.HandleFunc("POST /api/v1/chat/completions", h.passthrough("/v1/chat/completions"))
	mux.HandleFunc("POST /api/v1/messages", h.passthrough("/v1/messages"))
	mux.HandleFunc("POST /api/v1/messages/count_tokens", h.passthrough("/v1/messages/count_tokens"))
	mux.HandleFunc("POST /api/v1/responses", h.passthrough("/v1/responses"))
	mux.HandleFunc("GET /api/v1/models", h.passthrough("/v1/models"))
	mux.HandleFunc("GET /api/v1/models/{kind}", h.passthrough("/v1/models/{kind}"))
	mux.HandleFunc("GET /api/v1/models/info", h.passthrough("/v1/models/info"))

	// Not yet implemented in /v1/* — explicit not-available stubs.
	mux.HandleFunc("POST /api/v1/api/chat", h.passthrough("/v1/api/chat"))
	mux.HandleFunc("POST /api/v1/responses/compact", h.passthrough("/v1/responses/compact"))
	mux.HandleFunc("POST /api/v1/audio/speech", h.passthrough("/v1/audio/speech"))
	mux.HandleFunc("POST /api/v1/audio/transcriptions", h.passthrough("/v1/audio/transcriptions"))
	mux.HandleFunc("GET /api/v1/audio/voices", h.passthrough("/v1/audio/voices"))
	mux.HandleFunc("POST /api/v1/embeddings", h.passthrough("/v1/embeddings"))
	mux.HandleFunc("POST /api/v1/images/generations", h.passthrough("/v1/images/generations"))
	mux.HandleFunc("POST /api/v1/search", h.notAvailable)
	mux.HandleFunc("POST /api/v1/videos/generations", h.passthrough("/v1/videos/generations"))
	mux.HandleFunc("POST /api/v1/videos/edits", h.passthrough("/v1/videos/edits"))
	mux.HandleFunc("POST /api/v1/videos/extensions", h.passthrough("/v1/videos/extensions"))
	mux.HandleFunc("GET /api/v1/videos/{id}", h.passthrough("/v1/videos/{id}"))
	mux.HandleFunc("POST /api/v1/web/fetch", h.passthrough("/v1/web/fetch"))
}

type v1DashboardHandler struct {
	deps Deps
}

func (h *v1DashboardHandler) root(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": "v1"})
}

// passthrough returns a handler that rewrites the request URL to the given
// /v1/* path (preserving the {kind} path value when present) and delegates
// to deps.V1Dispatch. If V1Dispatch is unset, it degrades to notAvailable
// so the surface stays honest during partial wiring / tests.
func (h *v1DashboardHandler) passthrough(v1Path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.deps.V1Dispatch == nil {
			h.notAvailable(w, r)
			return
		}
		// Substitute any Go ServeMux {name} placeholder in the target path
		// (e.g. {kind}, {id}) with the value captured on the incoming request.
		target := v1Path
		target = substitutePathValues(target, r)
		r2 := r.Clone(r.Context())
		r2.URL.Path = target
		r2.URL.RawPath = ""
		h.deps.V1Dispatch(w, r2)
	}
}

func (h *v1DashboardHandler) notAvailable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": false,
		"message": "Dashboard /api/v1 endpoint not yet available in Go build; use /v1 directly",
	})
}

// substitutePathValues replaces every "{name}" placeholder in path with the
// corresponding r.PathValue("name"). This generalizes the {kind} substitution
// so any path-value placeholder (e.g. {id} for video polling) is forwarded.
func substitutePathValues(path string, r *http.Request) string {
	if !strings.Contains(path, "{") {
		return path
	}
	out := path
	for {
		start := strings.Index(out, "{")
		if start < 0 {
			break
		}
		end := strings.Index(out[start:], "}")
		if end < 0 {
			break
		}
		end += start
		name := out[start+1 : end]
		out = out[:start] + r.PathValue(name) + out[end+1:]
	}
	return out
}
