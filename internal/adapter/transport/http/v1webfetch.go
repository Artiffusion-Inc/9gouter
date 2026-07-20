package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/webfetch"
)

// handleWebFetch implements POST /v1/web/fetch. It ports the legacy JS
// src/sse/handlers/fetch.js + open-sse/handlers/fetch/index.js: the provider IS
// the model (body.provider || body.model), so ProviderID is taken directly
// from the request body rather than via resolveModel. The target URL is
// validated and SSRF-guarded at this boundary (rejecting non-http(s) schemes
// and private/loopback/link-local hosts) before dispatch to the usecase.
//
// Account fallback, on-401 token refresh, and combo expansion are NOT in this
// slice — they are separate slices mirroring the embeddings port scope.
func (h *v1Handler) handleWebFetch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	var reqMap map[string]json.RawMessage
	if err := json.Unmarshal(body, &reqMap); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	// API-key gate (same as /v1/chat and /v1/embeddings).
	apiKey := extractAPIKey(r)
	requireKey, err := h.requireAPIKey(ctx)
	if err != nil {
		h.logger.Warn("api-key check failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "Auth check failed")
		return
	}
	if requireKey || !isLocalRequest(r) {
		if apiKey == "" {
			h.writeError(w, http.StatusUnauthorized, "Missing API key")
			return
		}
		valid, err := h.deps.APIKeysRepo.Validate(ctx, apiKey)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, "Auth check failed")
			return
		}
		if !valid {
			h.writeError(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
	}

	// Provider IS the model: accept body.provider or body.model.
	providerID := firstStringField(reqMap, "provider", "model")
	if providerID == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required field: provider (or model)")
		return
	}

	targetURL := firstStringField(reqMap, "url")
	if targetURL == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required field: url")
		return
	}
	if err := assertPublicURL(targetURL); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	params := webfetch.Params{
		URL:           targetURL,
		Format:        firstStringField(reqMap, "format"),
		MaxCharacters: firstIntField(reqMap, "max_characters"),
	}

	creds, err := h.resolveCredentials(ctx, providerID, providerID)
	if err != nil {
		h.writeError(w, http.StatusNotFound, "No active credentials for provider: "+providerID)
		return
	}

	if h.deps.WebFetch == nil {
		h.writeError(w, http.StatusNotImplemented, "Web fetch pipeline not wired")
		return
	}

	connectionID := ""
	if m := creds.ProviderSpecificData; m != nil {
		if v, ok := m["_connectionId"].(string); ok {
			connectionID = v
		}
	}

	res, err := h.deps.WebFetch.Handle(ctx, WebFetchRequest{
		Ctx:          ctx,
		ProviderID:   providerID,
		Credentials:  creds,
		APIKey:       apiKey,
		ConnectionID: connectionID,
		Endpoint:     r.URL.Path,
		UserAgent:    r.UserAgent(),
		Params:       params,
	})
	if err != nil && res.Err == nil {
		res.Err = err
	}
	if res.Err != nil {
		status := res.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		h.writeError(w, status, res.Err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(res.Body)
}

// firstStringField returns the first non-empty string value among the given
// keys in a JSON object map (in priority order). Used to read body.provider ||
// body.model, where the UI sends `model` since provider IS the model.
func firstStringField(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if raw, ok := m[k]; ok && len(raw) > 0 {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

// firstIntField returns the integer value of the first present key, or 0.
func firstIntField(m map[string]json.RawMessage, keys ...string) int {
	for _, k := range keys {
		if raw, ok := m[k]; ok && len(raw) > 0 {
			var n int
			if err := json.Unmarshal(raw, &n); err == nil {
				return n
			}
			// JSON numbers may decode as float64.
			var f float64
			if err := json.Unmarshal(raw, &f); err == nil {
				return int(f)
			}
		}
	}
	return 0
}

// assertPublicURL validates that the target URL is an http(s) URL pointing at a
// public host (not loopback / private / link-local / metadata endpoints). This
// is the SSRF guard for /v1/web/fetch, mirroring the JS assertPublicUrl in
// shared/utils/ssrfGuard.js. It runs at the transport boundary so the usecase
// never sees an unsafe target.
func assertPublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errInvalidURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errInvalidURL
	}
	host := u.Hostname()
	if host == "" {
		return errInvalidURL
	}
	// Reject obvious internal/metadata hostnames. A full IP-range check
	// (RFC1918 / loopback / link-local / 169.254.169.254 metadata) is the JS
	// ssrfGuard's job; here we cover the named-host + simple-IP cases so the
	// common SSRF vectors are closed. Deeper IP validation is a follow-up.
	host = strings.TrimSpace(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errSSRFBlocked
	}
	if isPrivateOrLoopbackIP(host) {
		return errSSRFBlocked
	}
	return nil
}

var (
	errInvalidURL  = &fetchErr{msg: "Invalid URL format"}
	errSSRFBlocked = &fetchErr{msg: "Blocked URL: internal/private/metadata targets are not allowed"}
)

type fetchErr struct{ msg string }

func (e *fetchErr) Error() string { return e.msg }

// isPrivateOrLoopbackIP reports whether host is a literal IPv4/IPv6 address in
// the loopback, private (RFC1918), link-local, or cloud-metadata ranges. Non-IP
// hostnames return false (DNS resolution at the upstream is the final guard).
func isPrivateOrLoopbackIP(host string) bool {
	// Strip zone/brackets for IPv6 literal forms.
	host = strings.Trim(host, "[]")
	// Crude prefix checks covering the common ranges; the upstream fetch still
	// resolves DNS, this is a belt-and-braces guard against literal-IP SSRF.
	switch {
	case host == "127.0.0.1" || strings.HasPrefix(host, "127."):
	case strings.HasPrefix(host, "10."):
	case strings.HasPrefix(host, "192.168."):
	case strings.HasPrefix(host, "172.") && is172Private(host):
	case strings.HasPrefix(host, "169.254."):
	case host == "::1" || strings.HasPrefix(host, "fe80:") || strings.HasPrefix(host, "fc") || strings.HasPrefix(host, "fd"):
	default:
		return false
	}
	return true
}

func is172Private(host string) bool {
	// 172.16.0.0/12 → second octet 16..31.
	parts := strings.SplitN(host, ".", 4)
	if len(parts) < 2 {
		return false
	}
	o := 0
	for _, ch := range parts[1] {
		if ch < '0' || ch > '9' {
			return false
		}
		o = o*10 + int(ch-'0')
	}
	return o >= 16 && o <= 31
}
