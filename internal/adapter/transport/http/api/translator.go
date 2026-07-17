package api

import (
	"net/http"
	"os"
	"path/filepath"
)

const translatorLogsDir = "logs/translator"

// RegisterTranslator mounts translator debug/log routes.
func RegisterTranslator(mux *http.ServeMux, deps Deps) {
	h := &translatorHandler{deps: deps}
	mux.HandleFunc("GET /api/translator/console-logs", h.consoleLogs)
	mux.HandleFunc("GET /api/translator/console-logs/stream", h.consoleLogsStream)
	mux.HandleFunc("GET /api/translator/load", h.load)
	mux.HandleFunc("POST /api/translator/save", h.save)
	mux.HandleFunc("POST /api/translator/send", h.send)
	mux.HandleFunc("POST /api/translator/translate", h.translate)
}

type translatorHandler struct {
	deps Deps
}

func (h *translatorHandler) consoleLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"logs": []any{}})
}

func (h *translatorHandler) consoleLogsStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("event: ping\ndata: ok\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *translatorHandler) load(w http.ResponseWriter, r *http.Request) {
	file := r.URL.Query().Get("file")
	allowed := map[string]bool{
		"1_req_client.json": true, "2_req_source.json": true, "3_req_openai.json": true,
		"4_req_target.json": true, "5_res_provider.txt": true, "6_res_openai.txt": true,
		"7_res_client.txt": true, "7_res_client.json": true,
	}
	if !allowed[file] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Invalid file name"})
		return
	}
	p := filepath.Join(translatorLogsDir, file)
	content, err := os.ReadFile(p)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "File not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "content": string(content)})
}

func (h *translatorHandler) save(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File    string `json:"file"`
		Content string `json:"content"`
	}
	if err := parseJSON(r, &body); err != nil || body.File == "" {
		writeError(w, http.StatusBadRequest, "file and content required")
		return
	}
	_ = os.MkdirAll(translatorLogsDir, 0o755)
	p := filepath.Join(translatorLogsDir, body.File)
	if err := os.WriteFile(p, []byte(body.Content), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save file")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *translatorHandler) send(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Translator send stubbed in Go build"})
}

func (h *translatorHandler) translate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Translator translate stubbed in Go build"})
}
