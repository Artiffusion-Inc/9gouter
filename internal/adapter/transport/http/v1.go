// Package http implements the /v1 route handlers for the Go rewrite.
// v1.go wires the /v1 chat/messages/responses POST endpoints and is kept
// decoupled from the proxychat usecase via dependency injection: it depends on
// a ChatHandler interface declared in this package, and wire.go supplies the
// proxychat adapter. This breaks the import cycle with internal/usecase/proxychat.
package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/provider"
	"github.com/Artiffusion-Inc/9router/internal/adapter/transport/proxy"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// ChatHandler is the boundary between the HTTP transport layer and the chat
// usecase. Implementations are provided by wire.go (proxychat adapter).
type ChatHandler interface {
	// Handle runs the chat usecase for a parsed HTTP request.
	// The SSE writer, response writer, and context are provided so the transport
	// layer can stream results to the client without leaking proxychat internals.
	Handle(ctx context.Context, req ChatRequest, w http.ResponseWriter, sse *Writer) (ChatResult, error)
}

// EmbeddingsHandler is the boundary between the HTTP transport layer and the
// embeddings usecase. Implementations are provided by wire.go (proxyembeddings
// adapter). Unlike chat, embeddings is non-streaming JSON in/out, so the handler
// owns writing the response body directly.
type EmbeddingsHandler interface {
	Handle(ctx context.Context, req EmbeddingsRequest) (EmbeddingsResult, error)
}

// EmbeddingsRequest carries the parsed HTTP request into the embeddings usecase.
type EmbeddingsRequest struct {
	Ctx          context.Context
	Body         json.RawMessage
	Endpoint     string
	Headers      http.Header
	ProviderID   string
	Model        string
	Credentials  domainProv.Credentials
	APIKey       string
	ConnectionID string
	UserAgent    string
}

// EmbeddingsResult carries the outcome back to the HTTP layer.
type EmbeddingsResult struct {
	StatusCode int
	Err        error
	Body       []byte
}

// ChatRequest carries the parsed HTTP request into the usecase.
type ChatRequest struct {
	Ctx         context.Context
	Body        json.RawMessage
	Endpoint    string
	Headers     http.Header
	ProviderID  string
	Model       string
	Credentials domainProv.Credentials
	Stream      bool
	APIKey      string
	ConnectionID string
	UserAgent   string
}

// ChatResult carries the outcome back to the HTTP layer.
type ChatResult struct {
	StatusCode int
	Streamed   bool
	Err        error
}

// APIKeyValidator validates extracted API keys.
type APIKeyValidator interface {
	Validate(ctx context.Context, key string) (bool, error)
}

// V1Deps holds the runtime dependencies required by the /v1 handlers.
// It is constructed by the app.Wire composition root and injected into
// RegisterV1 so the transport layer stays decoupled from DB/lifecycle wiring.
type V1Deps struct {
	APIKeysRepo     *repo.APIKeyRepo
	SettingsRepo    *repo.SettingsRepo
	ConnectionRepo  *repo.ConnectionRepo
	ComboRepo       *repo.ComboRepo
	AliasRepo       *repo.AliasRepo
	NodeRepo        *repo.NodeRepo
	ProxyPoolRepo   *repo.ProxyPoolRepo
	DisabledModels  *repo.DisabledModelsRepo
	ProxyOpts       proxy.Options
	Logger          *slog.Logger
	Config          config.Config

	// Chat is the injected chat usecase boundary.
	Chat ChatHandler

	// Embeddings is the injected embeddings usecase boundary (POST /v1/embeddings).
	Embeddings EmbeddingsHandler
}

// RegisterV1 mounts POST handlers for /v1/chat/completions, /v1/messages,
// and /v1/responses onto the provided ServeMux.
func RegisterV1(mux *http.ServeMux, deps V1Deps) {
	handler := newV1Handler(deps)
	mux.HandleFunc("POST /v1/chat/completions", handler.handleChat)
	mux.HandleFunc("POST /v1/messages", handler.handleChat)
	mux.HandleFunc("POST /v1/responses", handler.handleChat)
	// POST /v1/responses/compact — thin wrapper over the chat pipeline:
	// injects body._compact = true and rewrites the path to /v1/responses so
	// source-format detection treats it as an OpenAI Responses request. Ports
	// legacy JS src/app/api/v1/responses/compact/route.js.
	mux.HandleFunc("POST /v1/responses/compact", handler.handleResponsesCompact)
	// POST /v1/messages/count_tokens — Anthropic-compatible token-count
	// estimate. Local (chars/4) only, no upstream — mirrors legacy JS
	// src/app/api/v1/messages/count_tokens/route.js.
	mux.HandleFunc("POST /v1/messages/count_tokens", handler.handleCountTokens)
	// POST /v1/embeddings — OpenAI-compatible embeddings pipeline. Ports
	// open-sse/handlers/embeddings.js + embeddingsCore.js: per-provider adapter
	// builds the upstream URL/headers/body, fetch via the proxy stack,
	// normalize to OpenAI shape, record usage. Account fallback and on-401
	// token refresh are separate slices.
	mux.HandleFunc("POST /v1/embeddings", handler.handleEmbeddings)
	// GET /v1/models — OpenAI-compatible model catalog. Static MVP (issue
	// decolua/9router #2702): combos + per-provider static catalogs (only for
	// providers with an active connection) + custom models + aliases, minus
	// disabled, filtered by service kind. Live-model resolvers and
	// compatible-fetch are not yet ported — providers without a static catalog
	// contribute only their custom models until the resolver pipeline lands.
	mux.HandleFunc("GET /v1/models", handler.handleModels)
	mux.HandleFunc("GET /v1/models/{kind}", handler.handleModels)
	// GET /v1/models/info?id={alias}/{modelId}&kind={optional} — per-model
	// capability metadata. Ports src/app/api/v1/models/info/route.js:
	// lookup the model in the provider static catalog (or a virtual
	// search/fetch model when the provider has a search/fetch config) and
	// report {id, name, kind, owned_by, endpoint}. Go's static catalog does
	// not yet carry params/capabilities/options/dimensions/contextWindow, so
	// those extra JS fields are omitted until the catalog is enriched.
	mux.HandleFunc("GET /v1/models/info", handler.handleModelsInfo)
}

type v1Handler struct {
	deps   V1Deps
	logger *slog.Logger
}

func newV1Handler(deps V1Deps) *v1Handler {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &v1Handler{deps: deps, logger: log}
}

func (h *v1Handler) handleChat(w http.ResponseWriter, r *http.Request) {
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

	apiKey := extractAPIKey(r)

	// API-key enforcement mirrors dashboardGuard.js + auth.js.
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
			h.logger.Warn("api-key validate failed", "error", err)
			h.writeError(w, http.StatusInternalServerError, "Auth check failed")
			return
		}
		if !valid {
			h.writeError(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
	}

	modelStr := ""
	if m, ok := reqMap["model"]; ok && len(m) > 0 {
		var s string
		if err := json.Unmarshal(m, &s); err == nil {
			modelStr = s
		}
	}
	if modelStr == "" {
		h.writeError(w, http.StatusBadRequest, "Missing model")
		return
	}

	modelInfo, err := h.resolveModel(ctx, modelStr)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if modelInfo.Provider == "" {
		h.writeError(w, http.StatusBadRequest, "Invalid model format")
		return
	}

	creds, err := h.resolveCredentials(ctx, modelInfo.Provider, modelInfo.Model)
	if err != nil {
		h.writeError(w, http.StatusNotFound, fmt.Sprintf("No active credentials for provider: %s", modelInfo.Provider))
		return
	}

	stream := resolveStream(body, r.Header, modelInfo.Provider)
	sseWriter := New(w, ctx)

	req := ChatRequest{
		Ctx:          ctx,
		Body:         body,
		Endpoint:     r.URL.Path,
		Headers:      r.Header.Clone(),
		ProviderID:   modelInfo.Provider,
		Model:        modelInfo.Model,
		Credentials:  creds,
		Stream:       stream,
		APIKey:       apiKey,
		ConnectionID: func() string {
			if m := creds.ProviderSpecificData; m != nil {
				if v, ok := m["_connectionId"].(string); ok {
					return v
				}
			}
			return ""
		}(),
		UserAgent:    r.UserAgent(),
	}

	res, err := h.deps.Chat.Handle(ctx, req, w, sseWriter)
	if err != nil && res.Err == nil {
		res.Err = err
	}
	if res.Err != nil {
		if !wroteResponse(w) {
			status := res.StatusCode
			if status == 0 {
				status = http.StatusBadGateway
			}
			h.writeError(w, status, res.Err.Error())
		}
		return
	}

	if res.Streamed {
		// SSE headers and terminator already handled by stream pipe.
		return
	}

	// Non-streaming success: usecase already wrote the JSON body.
}

func (h *v1Handler) requireAPIKey(ctx context.Context) (bool, error) {
	settings, err := h.deps.SettingsRepo.Get(ctx)
	if err != nil {
		return false, err
	}
	var data map[string]any
	if err := json.Unmarshal(settings.Data, &data); err != nil {
		return false, err
	}
	v, ok := data["requireApiKey"].(bool)
	return ok && v, nil
}

func (h *v1Handler) resolveModel(ctx context.Context, modelStr string) (*modelInfo, error) {
	// Provider/model syntax.
	if strings.Contains(modelStr, "/") {
		parts := strings.SplitN(modelStr, "/", 2)
		providerAlias := parts[0]
		model := parts[1]
		resolved, err := provider.Lookup(providerAlias)
		if err == nil {
			return &modelInfo{Provider: resolved.ID(), Model: model}, nil
		}
		// Not a built-in provider id/alias — try user-defined provider nodes.
		if resolved, ok := h.matchNode(ctx, providerAlias, model); ok {
			return resolved, nil
		}
		return nil, fmt.Errorf("unknown provider %q", providerAlias)
	}

	// Model alias lookup.
	aliases, err := h.deps.AliasRepo.GetAliases(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve alias: %w", err)
	}
	if resolved, ok := aliases[modelStr]; ok {
		info, err := h.resolveModel(ctx, resolved)
		if err == nil {
			return info, nil
		}
	}

	// Combo lookup.
	combo, err := h.deps.ComboRepo.GetByName(ctx, modelStr)
	if err == nil && combo != nil {
		var models []string
		_ = json.Unmarshal(combo.Models, &models)
		if len(models) > 0 {
			// Fallback strategy: use first model.
			return h.resolveModel(ctx, models[0])
		}
	}

	// Final fallback: infer provider from model name prefix.
	return &modelInfo{Provider: inferProvider(modelStr), Model: modelStr}, nil
}

type modelInfo struct {
	Provider string
	Model    string
}

func (h *v1Handler) matchNode(ctx context.Context, alias, model string) (*modelInfo, bool) {
	nodes, err := h.deps.NodeRepo.List(ctx, repo.NodeFilter{Type: "openai-compatible"})
	if err == nil {
		for _, n := range nodes {
			if nodePrefix(n) == alias {
				return &modelInfo{Provider: n.ID, Model: model}, true
			}
		}
	}
	nodes, err = h.deps.NodeRepo.List(ctx, repo.NodeFilter{Type: "anthropic-compatible"})
	if err == nil {
		for _, n := range nodes {
			if nodePrefix(n) == alias {
				return &modelInfo{Provider: n.ID, Model: model}, true
			}
		}
	}
	nodes, err = h.deps.NodeRepo.List(ctx, repo.NodeFilter{Type: "custom-embedding"})
	if err == nil {
		for _, n := range nodes {
			if nodePrefix(n) == alias {
				return &modelInfo{Provider: n.ID, Model: model}, true
			}
		}
	}
	return nil, false
}

func nodePrefix(n settings.ProviderNode) string {
	var data map[string]any
	_ = json.Unmarshal(n.Data, &data)
	prefix, _ := data["prefix"].(string)
	if prefix != "" {
		return prefix
	}
	prefix, _ = data["Prefix"].(string)
	return prefix
}

func (h *v1Handler) resolveCredentials(ctx context.Context, providerID, model string) (domainProv.Credentials, error) {
	// No-auth providers use a virtual public connection.
	if isNoAuthProvider(providerID) {
		return domainProv.Credentials{
			APIKey:       "public",
			AccessToken:  "public",
				ProviderSpecificData: map[string]any{
				"connectionProxyEnabled": false,
			},
		}, nil
	}

	connections, err := h.deps.ConnectionRepo.List(ctx, repo.ConnectionFilter{Provider: providerID, IsActive: boolPtr(true)})
	if err != nil {
		return domainProv.Credentials{}, err
	}
	if len(connections) == 0 {
		return domainProv.Credentials{}, fmt.Errorf("no active credentials")
	}
	conn := connections[0]

	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)

	creds := domainProv.Credentials{
		ProviderSpecificData: map[string]any{"_connectionId": conn.ID},
	}
	if v, ok := data["apiKey"].(string); ok {
		creds.APIKey = v
	}
	if v, ok := data["accessToken"].(string); ok {
		creds.AccessToken = v
	}
	if v, ok := data["refreshToken"].(string); ok {
		if creds.ProviderSpecificData == nil {
			creds.ProviderSpecificData = map[string]any{}
		}
		creds.ProviderSpecificData["refreshToken"] = v
	}
	if v, ok := data["expiresAt"].(string); ok && v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			creds.ExpiresAt = &t
		}
	}
	if v, ok := data["providerSpecificData"].(map[string]any); ok {
		if creds.ProviderSpecificData == nil {
			creds.ProviderSpecificData = map[string]any{}
		}
		for k, val := range v {
			creds.ProviderSpecificData[k] = val
		}
	}

	// Resolve per-connection route affinity (decolua/9router #2703 Fix 1).
	// When a connection has a proxyPoolId, look up the pool and copy its
	// strictProxy + proxyUrl + noProxy into providerSpecificData so the
	// provider executor's ProxyAwareFetch sees the strict flag per-request.
	// strictProxy reaching proxyAwareFetch is the acceptance criterion; a
	// strict route must never fall back to the host's direct IP.
	h.resolveConnectionProxyConfig(ctx, creds.ProviderSpecificData)

	return creds, nil
}

// resolveConnectionProxyConfig merges the connection's assigned proxy pool
// into providerSpecificData. It copies the pool's strictProxy, proxyUrl, and
// noProxy when the connection references a pool via proxyPoolId, so the
// provider executor can build ProxyFetchOptions per-request. This mirrors
// the JS resolveConnectionProxyConfig that chatCore.js consumed.
//
// Connection-level fields (connectionProxyEnabled/Url/NoProxy, vercelRelayUrl)
// already live in providerSpecificData from the connection's stored data and
// are passed through unchanged; only the pool-derived strict flag and pool
// proxy URL/noProxy are filled here when a pool is assigned and the
// connection does not already carry an explicit per-connection proxy URL.
func (h *v1Handler) resolveConnectionProxyConfig(ctx context.Context, psd map[string]any) {
	if psd == nil {
		return
	}
	poolID, _ := psd["proxyPoolId"].(string)
	if poolID == "" {
		return
	}
	pool, err := h.deps.ProxyPoolRepo.GetByID(ctx, poolID)
	if err != nil || pool == nil || !pool.IsActive {
		// Pool missing or inactive: leave strictProxy unset so the
		// executor falls back to its default. Strict-mode fail-closed for a
		// missing strict pool is the executor's responsibility once
		// strictProxy=true is resolved; an inactive pool with no strict flag
		// is a config error the operator should fix, not a silent direct
		// fallback.
		return
	}
	var poolData map[string]any
	_ = json.Unmarshal(pool.Data, &poolData)
	if v, ok := poolData["strictProxy"].(bool); ok {
		psd["strictProxy"] = v
	}
	// Only inherit the pool's proxyUrl/noProxy when the connection does not
	// set its own per-connection proxy URL — the per-connection value wins.
	if _, hasConnURL := psd["connectionProxyUrl"].(string); !hasConnURL || psd["connectionProxyUrl"] == "" {
		if v, ok := poolData["proxyUrl"].(string); ok && v != "" {
			psd["connectionProxyUrl"] = v
			// A pool-assigned connection is implicitly proxy-enabled unless
			// the connection explicitly disabled it.
			if enabled, set := psd["connectionProxyEnabled"].(bool); !set || !enabled {
				if !set {
					psd["connectionProxyEnabled"] = true
				}
			}
		}
	}
	if _, hasConnNoProxy := psd["connectionNoProxy"].(string); !hasConnNoProxy || psd["connectionNoProxy"] == "" {
		if v, ok := poolData["noProxy"].(string); ok && v != "" {
			psd["connectionNoProxy"] = v
		}
	}
}

func (h *v1Handler) writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	body := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType(status),
			"code":    errorCode(status),
		},
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func extractAPIKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return v
	}
	if v := r.Header.Get("x-goog-api-key"); v != "" {
		return v
	}
	return r.URL.Query().Get("key")
}

func isLocalRequest(r *http.Request) bool {
	// Loopback without a proxy stamp is treated as local, matching custom-server.js.
	if r.Header.Get("X-9r-Via-Proxy") != "" {
		return false
	}
	ip := FromRequest(r)
	return isLoopback(ip)
}

func resolveStream(body []byte, headers http.Header, providerID string) bool {
	var req struct {
		Stream *bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)

	// Provider force-streaming list.
	forceStream := map[string]bool{}
	if forceStream[providerID] {
		return true
	}

	// Accept header can force JSON.
	accept := strings.ToLower(headers.Get("Accept"))
	prefersJSON := strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/event-stream")
	if prefersJSON {
		if req.Stream == nil || !*req.Stream {
			return false
		}
	}

	if req.Stream != nil {
		return *req.Stream
	}
	return true
}

func isNoAuthProvider(id string) bool {
	switch id {
	case "mimo-free", "opencode", "grok-web", "mmf":
		return true
	}
	return false
}

func inferProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude-"):
		return "anthropic"
	case strings.HasPrefix(m, "gemini-"):
		return "gemini"
	case strings.HasPrefix(m, "gpt-"):
		return "openai"
	case strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "openai"
	case strings.HasPrefix(m, "deepseek-"):
		return "openrouter"
	}
	return "openai"
}

func errorType(status int) string {
	switch {
	case status >= 500:
		return "server_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusPaymentRequired:
		return "billing_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "invalid_request_error"
	}
}

func errorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusPaymentRequired:
		return "payment_required"
	case http.StatusForbidden:
		return "insufficient_quota"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusNotAcceptable:
		return "model_not_supported"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusInternalServerError:
		return "internal_server_error"
	case http.StatusBadGateway:
		return "bad_gateway"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	case http.StatusGatewayTimeout:
		return "gateway_timeout"
	default:
		return ""
	}
}

func boolPtr(v bool) *bool { return &v }

func wroteResponse(w http.ResponseWriter) bool {
	if rec, ok := w.(*httptest.ResponseRecorder); ok {
		return rec.Code != 0
	}
	return false
}

// handleEmbeddings serves POST /v1/embeddings. It reuses the chat handler's
// auth gate, model resolution, and credential resolution, then delegates to
// the injected EmbeddingsHandler. Unlike chat, the response is a single JSON
// body the usecase has already normalized; the transport layer only writes it.
func (h *v1Handler) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
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
			h.logger.Warn("api-key validate failed", "error", err)
			h.writeError(w, http.StatusInternalServerError, "Auth check failed")
			return
		}
		if !valid {
			h.writeError(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
	}

	modelStr := ""
	if m, ok := reqMap["model"]; ok && len(m) > 0 {
		var s string
		if err := json.Unmarshal(m, &s); err == nil {
			modelStr = s
		}
	}
	if modelStr == "" {
		h.writeError(w, http.StatusBadRequest, "Missing model")
		return
	}

	modelInfo, err := h.resolveModel(ctx, modelStr)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if modelInfo.Provider == "" {
		h.writeError(w, http.StatusBadRequest, "Invalid model format")
		return
	}

	creds, err := h.resolveCredentials(ctx, modelInfo.Provider, modelInfo.Model)
	if err != nil {
		h.writeError(w, http.StatusNotFound, fmt.Sprintf("No active credentials for provider: %s", modelInfo.Provider))
		return
	}

	if h.deps.Embeddings == nil {
		h.writeError(w, http.StatusNotImplemented, "Embeddings pipeline not wired")
		return
	}

	connectionID := ""
	if m := creds.ProviderSpecificData; m != nil {
		if v, ok := m["_connectionId"].(string); ok {
			connectionID = v
		}
	}

	res, err := h.deps.Embeddings.Handle(ctx, EmbeddingsRequest{
		Ctx:          ctx,
		Body:         body,
		Endpoint:     r.URL.Path,
		Headers:      r.Header.Clone(),
		ProviderID:   modelInfo.Provider,
		Model:        modelInfo.Model,
		Credentials:  creds,
		APIKey:       apiKey,
		ConnectionID: connectionID,
		UserAgent:    r.UserAgent(),
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


// kindEndpoint maps a service kind to the /v1/* endpoint that serves it,
// mirroring the JS KIND_ENDPOINT table in src/app/api/v1/models/info/route.js.
var kindEndpoint = map[string]string{
	"llm":         "/v1/chat/completions",
	"image":       "/v1/images/generations",
	"tts":         "/v1/audio/speech",
	"stt":         "/v1/audio/transcriptions",
	"embedding":   "/v1/embeddings",
	"imageToText": "/v1/chat/completions",
	"webSearch":   "/v1/search",
	"webFetch":    "/v1/web/fetch",
}

// modelInfoResponse is the JSON shape returned by GET /v1/models/info.
// Mirrors JS buildInfo. Extra JS fields (params/capabilities/options/
// dimensions/contextWindow/voicesUrl/searchTypes) are omitted until the Go
// static catalog carries them; this is the honest subset available today.
type modelInfoResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	OwnedBy  string `json:"owned_by"`
	Endpoint string `json:"endpoint"`
}

// handleModelsInfo implements GET /v1/models/info?id={alias}/{modelId}. It
// looks up the model in the provider's static catalog (built by
// provider.AllCatalogs) and reports {id, name, kind, owned_by, endpoint}. The
// optional ?kind= query disambiguates duplicate ids across kinds (e.g. a gemini
// model that exists as both llm and tts). Returns 400 for a missing id, 404
// when no catalog entry matches.
func (h *v1Handler) handleModelsInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	id := r.URL.Query().Get("id")
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required query param: id (e.g. ?id=openai/dall-e-3)")
		return
	}
	requestedKind := r.URL.Query().Get("kind")
	info := lookupModelInfo(id, requestedKind)
	if info == nil {
		h.writeError(w, http.StatusNotFound, "Model not found: "+id)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(info)
}

// lookupModelInfo resolves "{alias}/{modelId}" to a modelInfoResponse using
// the provider static catalogs (provider.AllCatalogs). requestedKind, when
// non-empty, disambiguates a model id that exists under multiple kinds. The
// alias half may be either the provider alias or the canonical provider id;
// AllCatalogs entries are matched on both.
func lookupModelInfo(fullID, requestedKind string) *modelInfoResponse {
	slash := strings.Index(fullID, "/")
	if slash <= 0 {
		return nil
	}
	alias := fullID[:slash]
	modelID := fullID[slash+1:]
	for _, cat := range provider.AllCatalogs() {
		if cat.Alias != alias && cat.ID != alias {
			continue
		}
		for _, m := range cat.Models {
			if m.ID != modelID {
				continue
			}
			kind := m.Kind
			if kind == "" {
				kind = "llm"
			}
			if requestedKind != "" && kind != requestedKind {
				continue
			}
			return &modelInfoResponse{
				ID:       cat.Alias + "/" + m.ID,
				Name:     orDefault(m.Name, m.ID),
				Kind:     kind,
				OwnedBy:  cat.Alias,
				Endpoint: kindEndpoint[kind],
			}
		}
	}
	return nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
