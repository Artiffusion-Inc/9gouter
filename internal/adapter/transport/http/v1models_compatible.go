package http

// v1models_compatible.go ports the JS fetchCompatibleModelIds
// (src/app/api/v1/models/route.js) — upstream 88a8c72d. For
// openai-compatible-* / anthropic-compatible-* providers (the dynamic
// "provider node" connections), the static catalog has no model list, so
// /v1/models fetches the live catalog from the connection's baseUrl
// (<baseUrl>/models) and surfaces the ids under the node's prefix.
//
// A request that already carries x-9r-internal-models-fetch (sent by our own
// fetchCompatibleModelIds below) is a cross-instance recursive /models call —
// buildModelsList skips the dynamic fetch in that case to break the loop
// (mirrors the JS skipDynamicFetch gate).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// internalModelsFetchHeader is sent by fetchCompatibleModelIds to detect
// cross-instance /models fetches between 9router instances connected to each
// other and break the recursive loop (mirrors the JS
// INTERNAL_MODELS_FETCH_HEADER).
const internalModelsFetchHeader = "x-9r-internal-models-fetch"

// compatibleFetchTimeout bounds each compatible /models fetch. Matches the JS
// 5000ms AbortController budget.
const compatibleFetchTimeout = 5 * time.Second

// openaiCompatiblePrefix / anthropicCompatiblePrefix are the provider-id
// prefixes that mark a dynamic compatible node connection (the node id starts
// with one of these). Mirrors the JS OPENAI_COMPATIBLE_PREFIX /
// ANTHROPIC_COMPATIBLE_PREFIX.
const (
	openaiCompatiblePrefix      = "openai-compatible-"
	anthropicCompatiblePrefix   = "anthropic-compatible-"
	customEmbeddingPrefix       = "custom-embedding-"
	compatibleNodeIDPrefix      = "9gouter-openai-compatible-"
	anthropicNodeIDPrefix       = "9gouter-anthropic-compatible-"
	customEmbeddingNodeIDPrefix = "9gouter-custom-embedding-"
)

// isCompatibleProviderNode reports whether providerID refers to a dynamic
// compatible provider-node connection (the kind whose model list is fetched
// live rather than read from the static catalog). Mirrors the JS
// isOpenAICompatibleProvider || isAnthropicCompatibleProvider check.
//
// In the Go build, node ids are generated as "9gouter-openai-compatible-…",
// "9gouter-anthropic-compatible-…" and "9gouter-custom-embedding-…"
// (internal/usecase/managedashboard/nodes.go), so we key off those. The bare
// openai-compatible-/anthropic-compatible- prefixes are also accepted for
// parity with imported JS-era connection rows.
func isCompatibleProviderNode(providerID string) bool {
	switch {
	case strings.HasPrefix(providerID, compatibleNodeIDPrefix),
		strings.HasPrefix(providerID, anthropicNodeIDPrefix),
		strings.HasPrefix(providerID, openaiCompatiblePrefix),
		strings.HasPrefix(providerID, anthropicCompatiblePrefix):
		return true
	}
	return false
}

// isAnthropicCompatibleNode distinguishes anthropic-compatible nodes (which
// need x-api-key + anthropic-version + a /messages → /models URL rewrite)
// from openai-compatible ones (Bearer auth, plain /models).
func isAnthropicCompatibleNode(providerID string) bool {
	return strings.HasPrefix(providerID, anthropicNodeIDPrefix) ||
		strings.HasPrefix(providerID, anthropicCompatiblePrefix)
}

// isCustomEmbeddingNode reports whether providerID is a custom-embedding node
// (embedding catalog fetched live). Embedding kinds are non-LLM, so they only
// surface when the kind filter includes "embedding".
func isCustomEmbeddingNode(providerID string) bool {
	return strings.HasPrefix(providerID, customEmbeddingNodeIDPrefix) ||
		strings.HasPrefix(providerID, customEmbeddingPrefix)
}

// compatibleModelID is one fetched live catalog entry: the raw upstream id
// plus the service kind inferred from the id (so non-LLM ids like embedding/
// tts models are filtered correctly by the kind filter).
type compatibleModelID struct {
	ID   string
	Kind string
}

// fetchCompatibleModelIds fetches the live model catalog from a compatible
// provider-node connection's baseUrl and returns the deduped ids. Mirrors the
// JS fetchCompatibleModelIds:
//
//   - baseUrl = connection.providerSpecificData.baseUrl (trimmed, trailing
//     slash stripped); empty → no fetch.
//   - openai-compatible: GET <baseUrl>/models with Authorization: Bearer
//     <apiKey>.
//   - anthropic-compatible: rewrite a trailing /messages[/models] to /models,
//     send x-api-key + anthropic-version + Bearer.
//   - custom-embedding: GET <baseUrl>/models with Authorization: Bearer.
//   - 5s timeout; any non-2xx / network error → empty list (fail open).
//   - parse OpenAI-style {data|models|results|bare-array}; dedupe by id.
//
// The request carries x-9r-internal-models-fetch: 1 so a downstream 9router
// instance receiving this request skips its own dynamic fetch (the recursion
// guard in buildModelsList).
func fetchCompatibleModelIds(ctx context.Context, client *http.Client, conn settings.ProviderConnection) []compatibleModelID {
	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)
	if data == nil {
		return nil
	}
	apiKey, _ := data["apiKey"].(string)
	baseURL := ""
	if psd, ok := data["providerSpecificData"].(map[string]any); ok {
		if v, ok := psd["baseUrl"].(string); ok {
			baseURL = strings.TrimSpace(v)
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil
	}

	url := baseURL + "/models"
	hdr := http.Header{"Content-Type": []string{"application/json"}}

	switch {
	case isAnthropicCompatibleNode(conn.Provider):
		// Anthropic-compatible nodes authenticate with x-api-key +
		// anthropic-version (and Bearer, for parity with the JS route). The
		// upstream is queried at <baseUrl>/models — the JS /messages→/models
		// rewrite branch is dead code (url always ends in /models), so it is
		// not reproduced here.
		hdr.Set("x-api-key", apiKey)
		hdr.Set("anthropic-version", "2023-06-01")
		hdr.Set("Authorization", "Bearer "+apiKey)
	case isCustomEmbeddingNode(conn.Provider):
		hdr.Set("Authorization", "Bearer "+apiKey)
	default: // openai-compatible
		hdr.Set("Authorization", "Bearer "+apiKey)
	}
	hdr.Set(internalModelsFetchHeader, "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header = hdr

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return nil
	}

	ids := parseOpenAIStyleModelIDs(body)
	// Dedupe preserving order, infer service kind for non-LLM ids.
	seen := map[string]bool{}
	out := make([]compatibleModelID, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, compatibleModelID{ID: id, Kind: inferKindFromModelID(id)})
	}
	return out
}

// parseOpenAIStyleModelIDs extracts the model ids from an OpenAI-style /models
// response: a bare array, or {data|models|results: [...]}. Each element's id
// is read from id | name | model (JS parity). Empty/blank ids are dropped.
func parseOpenAIStyleModelIDs(body []byte) []string {
	// Try bare array first.
	var arr []map[string]any
	if json.Unmarshal(body, &arr) == nil {
		return extractModelIDs(arr)
	}
	var wrap struct {
		Data    []map[string]any `json:"data"`
		Models  []map[string]any `json:"models"`
		Results []map[string]any `json:"results"`
	}
	if json.Unmarshal(body, &wrap) == nil {
		if len(wrap.Data) > 0 {
			return extractModelIDs(wrap.Data)
		}
		if len(wrap.Models) > 0 {
			return extractModelIDs(wrap.Models)
		}
		return extractModelIDs(wrap.Results)
	}
	return nil
}

// extractModelIDs reads id | name | model from each element, dropping blanks.
func extractModelIDs(elems []map[string]any) []string {
	out := make([]string, 0, len(elems))
	for _, m := range elems {
		for _, k := range []string{"id", "name", "model"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				out = append(out, v)
				break
			}
		}
	}
	return out
}

// inferKindFromModelID mirrors the JS inferKindFromUnknownModelId: embed →
// embedding, tts/speech/audio/voice → tts, image/imagen/dall-e/flux/sdxl/sd-/
// stable-diffusion → image, else llm.
func inferKindFromModelID(id string) string {
	lower := strings.ToLower(id)
	if strings.Contains(lower, "embed") {
		return "embedding"
	}
	if strings.Contains(lower, "tts") || strings.Contains(lower, "speech") ||
		strings.Contains(lower, "audio") || strings.Contains(lower, "voice") {
		return "tts"
	}
	if strings.Contains(lower, "image") || strings.Contains(lower, "imagen") ||
		strings.Contains(lower, "dall-e") || strings.Contains(lower, "dalle") ||
		strings.Contains(lower, "flux") || strings.Contains(lower, "sdxl") ||
		strings.HasPrefix(lower, "sd-") || strings.Contains(lower, "stable-diffusion") {
		return "image"
	}
	return "llm"
}

// compatibleClient returns the http.Client used for compatible /models
// fetches. A caller may inject one (tests); nil yields a default 5s client.
func compatibleClient(inject *http.Client) *http.Client {
	if inject != nil {
		return inject
	}
	return &http.Client{Timeout: compatibleFetchTimeout}
}

// fmtCompatibleEntry renders the prefixed catalog id "<prefix>/<modelId>" the
// way the static catalog does (JS outputAlias convention).
func fmtCompatibleEntry(prefix, modelID string) string {
	return fmt.Sprintf("%s/%s", strings.TrimRight(prefix, "/"), modelID)
}

// appendCompatibleModels fetches the live catalog for every active compatible
// provider-node connection and appends the ids (prefixed "<prefix>/<id>")
// under the node's prefix. It is the Go analogue of the JS
// `isCompatibleProvider && rawModelIds.length === 0 && !skipDynamicFetch`
// branch in buildModelsList. Each connection's prefix is read from its
// providerSpecificData.prefix (copied from the node at create time, JS
// parity); when absent it falls back to the node's prefix via NodeRepo, and
// finally to the connection's own id (so the models are still addressable).
//
// Failures are per-connection and fail open: a dead/unreachable baseUrl yields
// no entries but does not abort the whole list.
func (h *v1Handler) appendCompatibleModels(ctx context.Context, kindFilter []string, activeConns []settings.ProviderConnection, out *[]oaiModel, seen map[string]bool) {
	// Collect compatible connections (first active per node, JS parity —
	// activeConnectionByProvider keeps the first per provider id).
	byNode := map[string]settings.ProviderConnection{}
	for _, c := range activeConns {
		if !isCompatibleProviderNode(c.Provider) && !isCustomEmbeddingNode(c.Provider) {
			continue
		}
		if _, has := byNode[c.Provider]; !has {
			byNode[c.Provider] = c
		}
	}
	if len(byNode) == 0 {
		return
	}

	// nodePrefix fallback: providerID → node prefix, for connections whose
	// psd.prefix was not stored.
	nodePrefixes := map[string]string{}
	if h.deps.NodeRepo != nil {
		nodes, err := h.deps.NodeRepo.List(ctx, repo.NodeFilter{})
		if err == nil {
			for _, n := range nodes {
				nodePrefixes[n.ID] = nodePrefix(n)
			}
		}
	}

	client := compatibleClient(nil)
	for providerID, conn := range byNode {
		prefix := compatiblePrefix(conn, nodePrefixes)
		if prefix == "" {
			prefix = providerID
		}
		models := fetchCompatibleModelIds(ctx, client, conn)
		for _, m := range models {
			mk := modelKind(m.Kind)
			if kindFilter != nil && !containsStr(kindFilter, mk) {
				continue
			}
			id := fmtCompatibleEntry(prefix, m.ID)
			if seen[id] {
				continue
			}
			seen[id] = true
			entry := oaiModel{ID: id, Object: "model", OwnedBy: prefix}
			if mk != "llm" {
				entry.Kind = mk
			}
			entry.Capabilities = capsForModel(providerID, m.ID, mk)
			*out = append(*out, entry)
		}
	}
}

// compatiblePrefix reads the node prefix from the connection's
// providerSpecificData.prefix (JS parity), falling back to the node's prefix
// via the nodePrefixes map.
func compatiblePrefix(conn settings.ProviderConnection, nodePrefixes map[string]string) string {
	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)
	if psd, ok := data["providerSpecificData"].(map[string]any); ok {
		if v, ok := psd["prefix"].(string); ok && v != "" {
			return v
		}
	}
	if p, ok := nodePrefixes[conn.Provider]; ok && p != "" {
		return p
	}
	return ""
}
