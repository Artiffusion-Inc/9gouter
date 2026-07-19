package http

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// handleApiChat implements POST /v1/api/chat. It ports src/app/api/v1/api/chat/route.js:
// the request is dispatched to the normal chat pipeline (handleChat), but the
// OpenAI SSE response body is transformed on the fly into Ollama NDJSON via
// ollamaNDJSONConverter. Content-Type becomes application/x-ndjson.
//
// The model name for NDJSON output is read from the request body (fallback
// "llama3.2") BEFORE dispatch — matching the JS clone-body-then-handleChat flow.
// Streaming stays true to the upstream (NDJSON lines stream as chunks arrive).
// Non-streaming chat responses are not transformed — the JS path always sets
// stream-on and the transform assumes SSE; if the upstream returns a single
// JSON body (non-stream), we pass it through as-is (the JS code returns the
// raw response body in that edge case).
func (h *v1Handler) handleApiChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Read the model name for NDJSON output before dispatch (do not consume the
	// body — we re-create the request with a fresh reader below).
	modelName := "llama3.2"
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err == nil {
		if m, ok := probe["model"]; ok && len(m) > 0 {
			var s string
			if err := json.Unmarshal(m, &s); err == nil && s != "" {
				modelName = s
			}
		}
	}

	// Pipe: the chat handler writes SSE into prWriter (an http.ResponseWriter
	// fronting io.PipeWriter); a goroutine reads the SSE stream, converts it to
	// NDJSON, and writes to the real client ResponseWriter.
	pr, pw := io.Pipe()
	defer pw.Close()
	capture := &pipeResponseWriter{header: http.Header{}, pw: pw}

	// Real response headers for the NDJSON stream.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	convErr := make(chan error, 1)
	go func() {
		conv := newOllamaNDJSONConverter(modelName)
		err := conv.Convert(w, pr)
		pr.Close()
		convErr <- err
	}()

	// Dispatch to the chat pipeline with a reconstructed request whose body is
	// the original bytes. handleChat reads r.Body and resolves the model from
	// it independently.
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.URL.Path = "/v1/chat/completions"
	r2.RequestURI = "POST /v1/chat/completions HTTP/1.1"

	h.handleChat(capture, r2)
	pw.Close() // signal EOF to the converter goroutine

	<-convErr
}

// pipeResponseWriter is an http.ResponseWriter fronting an io.PipeWriter. It
// captures everything the chat handler writes (SSE frames via WriteRaw / Write)
// and forwards the bytes into the pipe so the NDJSON converter can read them.
// Header() returns a throwaway map so handleChat's SSE header-setting does not
// touch the real response (we set our own NDJSON headers on the real writer).
type pipeResponseWriter struct {
	header     http.Header
	pw         *io.PipeWriter
	statusCode int
}

func (p *pipeResponseWriter) Header() http.Header {
	if p.header == nil {
		p.header = http.Header{}
	}
	return p.header
}

func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	return p.pw.Write(b)
}

func (p *pipeResponseWriter) WriteHeader(statusCode int) {
	p.statusCode = statusCode
}

// wroteResponse reports whether the response was already started (used by
// handleChat's error path). For the pipe writer we always return false so the
// error path writes the error SSE into the pipe (which the converter will pass
// through as non-data lines — ignored — so errors are silent in NDJSON, as in
// JS). This matches the JS behavior where an error SSE event does not match
// the "data:" prefix and is dropped by the transform.

// Compile-time interface check.
var _ http.ResponseWriter = (*pipeResponseWriter)(nil)