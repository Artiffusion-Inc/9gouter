package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const translatorLogsDir = "logs/translator"

// consoleLogBuffer is an in-memory ring buffer of recent translator console log
// lines, broadcast to all currently-connected SSE consumers. The frontend
// (src/app/(dashboard)/dashboard/console-log/ConsoleLogClient.js) subscribes
// to /api/translator/console-logs/stream and consumes:
//
//   - event message with data {"type":"init", "logs":[...]} on connect
//   - event message with data {"type":"line", "line": "..."} per new line
//   - event message with data {"type":"lines","lines":[...]} for a batch
//   - event message with data {"type":"clear"} after DELETE
//
var (
	consoleLogMu    sync.Mutex
	consoleLogBuf   []string
	consoleLogSubs  = make(map[chan string]struct{})
)

// RegisterTranslator mounts translator debug/log routes.
func RegisterTranslator(mux *http.ServeMux, deps Deps) {
	h := &translatorHandler{deps: deps}
	mux.HandleFunc("GET /api/translator/console-logs", h.consoleLogs)
	mux.HandleFunc("DELETE /api/translator/console-logs", h.consoleLogsClear)
	mux.HandleFunc("GET /api/translator/console-logs/stream", h.consoleLogsStream)
	mux.HandleFunc("GET /api/translator/load", h.load)
	mux.HandleFunc("POST /api/translator/save", h.save)
	mux.HandleFunc("POST /api/translator/send", h.send)
	mux.HandleFunc("POST /api/translator/translate", h.translate)
}

type translatorHandler struct {
	deps Deps
}

// consoleLogs returns the current buffered console logs as a one-shot JSON
// payload. Most clients subscribe to the SSE stream instead, but this
// endpoint lets the UI hydrate on first paint if needed.
func (h *translatorHandler) consoleLogs(w http.ResponseWriter, r *http.Request) {
	consoleLogMu.Lock()
	logs := make([]string, len(consoleLogBuf))
	copy(logs, consoleLogBuf)
	consoleLogMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"logs":    logs,
	})
}

// consoleLogsClear empties the in-memory console log buffer and broadcasts
// a "clear" event to all connected SSE consumers so their UI state resets
// without needing a page refresh. Mirrors the JS-era DELETE handler.
func (h *translatorHandler) consoleLogsClear(w http.ResponseWriter, r *http.Request) {
	consoleLogMu.Lock()
	consoleLogBuf = consoleLogBuf[:0]
	subs := make([]chan string, 0, len(consoleLogSubs))
	for ch := range consoleLogSubs {
		subs = append(subs, ch)
	}
	consoleLogMu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- `{"type":"clear"}`:
		default:
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// consoleLogsStream opens a long-lived SSE connection. The frontend reads
// every frame as JSON (no event name), so each frame is a single
// `data: <json>\n\n` line. The first frame is an "init" carrying the current
// buffer; subsequent frames are "line" / "lines" as new entries arrive.
func (h *translatorHandler) consoleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := make(chan string, 32)
	consoleLogMu.Lock()
	consoleLogSubs[ch] = struct{}{}
	// Snapshot current buffer for the init frame.
	initLogs := make([]string, len(consoleLogBuf))
	copy(initLogs, consoleLogBuf)
	consoleLogMu.Unlock()

	initPayload, _ := json.Marshal(map[string]any{
		"type": "init",
		"logs": initLogs,
	})
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(initPayload)
	_, _ = w.Write([]byte("\n\n"))
	flusher.Flush()

	// Keep-alive ping every 25s so proxies don't close the connection.
	pingTick := time.NewTicker(25 * time.Second)
	defer pingTick.Stop()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			consoleLogMu.Lock()
			delete(consoleLogSubs, ch)
			consoleLogMu.Unlock()
			return
		case <-pingTick.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case frame, open := <-ch:
			if !open {
				return
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write([]byte(frame)); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
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

// translate handles POST /api/translator/translate.
//
// The frontend (src/app/(dashboard)/dashboard/translator/page.js) sends
// {step, body} and reads the result as:
//
//	step=1: data.result = { provider, model, sourceFormat, targetFormat }
//	step=2: data.result = { body }                       (openai intermediate)
//	step=3: data.result = { body, headers, url }         (target payload)
//
// On any failure it expects { success:false, error:"..." }.
func (h *translatorHandler) translate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Step int             `json:"step"`
		Body json.RawMessage `json:"body"`
	}
	if err := parseJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch body.Step {
	case 1:
		// Detect provider / model / sourceFormat / targetFormat from the raw
		// client request (1_req_client.json). The frontend displays the four
		// values in the page header. Default to openai on both sides so the
		// UI has something to render; missing fields stay empty rather than
		// failing the whole step.
		var raw map[string]any
		_ = json.Unmarshal(body.Body, &raw)
		provider, _ := raw["provider"].(string)
		model, _ := raw["model"].(string)
		// Allow the model field to carry an embedded "provider/model" string
		// (used by some upstream proxy adapters).
		if provider == "" && model != "" {
			if i := strings.Index(model, "/"); i > 0 {
				provider = model[:i]
				model = model[i+1:]
			}
		}
		if provider == "" {
			provider = "openai"
		}
		if model == "" {
			model = "gpt-4o-mini"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"result": map[string]any{
				"provider":     provider,
				"model":        model,
				"sourceFormat": "openai",
				"targetFormat": "openai",
			},
		})
	case 2:
		// Source → OpenAI intermediate. Pass the body through unchanged and
		// wrap it as { body } so the editor renders a structured JSON.
		var any_ any
		if len(body.Body) == 0 {
			any_ = map[string]any{}
		} else if err := json.Unmarshal(body.Body, &any_); err != nil {
			writeError(w, http.StatusBadRequest, "body must be JSON for step 2")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"result": map[string]any{
				"body": any_,
			},
		})
	case 3:
		// OpenAI → target + URL + headers. The frontend merges
		// data.result with provider/model into step 4's editor.
		var any_ any
		if len(body.Body) == 0 {
			any_ = map[string]any{}
		} else if err := json.Unmarshal(body.Body, &any_); err != nil {
			writeError(w, http.StatusBadRequest, "body must be JSON for step 3")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"result": map[string]any{
				"body":    any_,
				"headers": map[string]any{},
				"url":     "",
			},
		})
	default:
		writeError(w, http.StatusBadRequest, "unsupported step; expected 1, 2, or 3")
	}
}
