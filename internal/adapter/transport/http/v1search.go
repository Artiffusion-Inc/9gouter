package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	searchprov "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/search"
)

// searchMaxBodyBytes caps the JSON request body read for /v1/search. Search
// queries are small; the cap is a guard.
const searchMaxBodyBytes int64 = 4 << 20

// searchRequestBody is the /v1/search request body. The legacy JS handler reads
// `provider || model` (for webSearch the provider IS the model — the UI sends
// `model`). Only the fields the usecase consumes are parsed; unknown fields are
// ignored. domain_filter / content_options / provider_options are accepted but
// not forwarded in this MVP slice.
type searchRequestBody struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Query       string `json:"query"`
	MaxResults  int    `json:"max_results"`
	SearchType   string `json:"search_type"`
	Country     string `json:"country"`
	Language    string `json:"language"`
	TimeRange   string `json:"time_range"`
	Offset      int    `json:"offset"`
}

// handleSearch implements POST /v1/search — the web-search endpoint. It ports
// src/sse/handlers/search.js (handleSearch) + open-sse/handlers/search/index.js
// (handleSearchCore): parse the JSON body, validate the API key, resolve the
// provider from body.provider || body.model (alias resolution → canonical id),
// then dispatch to the searchproxy usecase. The usecase routes dedicated search
// APIs vs chat-based search and normalizes into the unified response shape.
//
// Combo expansion and account-fallback rotation are separate slices.
func (h *v1Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// API-key gate (same as /v1/chat).
	apiKey := extractAPIKey(r)
	requireKey, err := h.requireAPIKey(ctx)
	if err != nil {
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

	var body searchRequestBody
	if err := json.NewDecoder(io.LimitReader(r.Body, searchMaxBodyBytes)).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	body.Query = strings.TrimSpace(body.Query)
	if body.Query == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required field: query")
		return
	}

	// Resolve provider: body.provider || body.model (UI sends `model` since for
	// webSearch the provider IS the model). Alias → canonical id.
	providerInput := strings.TrimSpace(body.Provider)
	if providerInput == "" {
		providerInput = strings.TrimSpace(body.Model)
	}
	if providerInput == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required field: provider or model")
		return
	}
	providerID := resolveSearchProvider(providerInput)
	if providerID == "" {
		h.writeError(w, http.StatusBadRequest, "Could not resolve search provider from: "+providerInput)
		return
	}

	creds, err := h.resolveCredentials(ctx, providerID, "")
	if err != nil {
		h.writeError(w, http.StatusNotFound, "No active credentials for provider: "+providerID)
		return
	}

	if h.deps.Search == nil {
		h.writeError(w, http.StatusNotImplemented, "Search pipeline not wired")
		return
	}

	res, err := h.deps.Search.Handle(ctx, SearchRequest{
		Ctx:         ctx,
		ProviderID:  providerID,
		Query:       body.Query,
		Model:       strings.TrimSpace(body.Model),
		MaxResults:  body.MaxResults,
		SearchType:  body.SearchType,
		Country:     body.Country,
		Language:    body.Language,
		TimeRange:   body.TimeRange,
		Offset:      body.Offset,
		Credentials: creds,
		UserAgent:   r.UserAgent(),
	})
	if err != nil && res.Err == nil {
		res.Err = err
	}
	h.writeSearchResult(w, res)
}

// writeSearchResult writes the unified search response to the client with CORS,
// mirroring the JS successResult writer.
func (h *v1Handler) writeSearchResult(w http.ResponseWriter, res SearchResult) {
	if res.Err != nil {
		status := res.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		h.writeError(w, status, res.Err.Error())
		return
	}
	if res.ContentType != "" {
		w.Header().Set("Content-Type", res.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if res.StatusCode == 0 {
		res.StatusCode = http.StatusOK
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(res.Body)
}

// resolveSearchProvider resolves a provider input (alias or canonical id) to
// the canonical search provider id, mirroring the JS resolveProviderId. Returns
// "" when the input matches no known search provider.
func resolveSearchProvider(input string) string {
	if _, ok := searchprov.Lookup(input); ok {
		return input
	}
	if id, ok := searchprov.LookupAlias(input); ok {
		return id
	}
	return ""
}