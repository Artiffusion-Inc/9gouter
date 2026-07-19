package http

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// handleResponsesCompact implements POST /v1/responses/compact. It is a thin
// wrapper over the chat pipeline: the JS route (src/app/api/v1/responses/
// compact/route.js on legacy/js-backend) sets body._compact = true and then
// delegates to the same handleChat used by /v1/responses. The compact flag is
// a downstream compression hint consumed inside the chat pipeline; the wire
// shape is identical to POST /v1/responses.
//
// We rewrite the request URL path to /v1/responses (so source-format detection
// sees the OpenAI Responses endpoint) and inject _compact=true into the JSON
// body, then re-dispatch through handleChat. The body is parsed once and
// re-marshaled so the injected field lands cleanly even if the client already
// set it.
func (h *v1Handler) handleResponsesCompact(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	var reqMap map[string]any
	if err := json.Unmarshal(body, &reqMap); err != nil {
		// Not JSON — let handleChat produce the canonical 400.
		h.dispatchResponsesCompact(w, r, body)
		return
	}
	if reqMap == nil {
		reqMap = map[string]any{}
	}
	reqMap["_compact"] = true
	newBody, err := json.Marshal(reqMap)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to encode request body")
		return
	}
	h.dispatchResponsesCompact(w, r, newBody)
}

// dispatchResponsesCompact re-dispatches the (possibly rewritten) body through
// handleChat with the URL path rewritten to /v1/responses so source-format
// detection treats it as an OpenAI Responses request.
func (h *v1Handler) dispatchResponsesCompact(w http.ResponseWriter, r *http.Request, body []byte) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/v1/responses"
	r2.URL.RawPath = ""
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.Header.Set("Content-Type", "application/json")
	h.handleChat(w, r2)
}