package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// kilocodeResolver fetches the live model catalog from the Kilo Code gateway
// /api/gateway/models endpoint and applies the upstream "openrouter-free"
// filter (the same filter the legacy JS dashboard used for the combo picker —
// see src/app/api/providers/suggested-models/filters.js). Mirrors the JS
// providerModelsFetcher flow: fetch the URL, filter, cache.
//
// The Kilo Code gateway is a passthrough over OpenRouter; the gateway models
// endpoint is unauthenticated (no accessToken / apiKey required) — the auth
// lives on the chat path (api.kilo.ai/api/openrouter/chat/completions), not on
// the catalog read. So this resolver fetches regardless of the connection's
// credential state; an empty-credential connection still gets a live catalog.
// On any failure (network, non-200, malformed body) it returns nil so the
// caller falls back to the 8-model static catalog in registry.go.
type kilocodeResolver struct {
	cache   *Cache
	client  *http.Client
	baseURL string
}

const (
	kilocodeGatewayURL   = "https://api.kilo.ai/api/gateway/models"
	kilocodeFetchTimeout = 20 * time.Second
	kilocodeCacheTTL     = 10 * time.Minute
	// openrouterFreeMinContext mirrors the JS openrouter-free filter: only
	// models with context_length >= 200000 are eligible (the gateway exposes
	// the full OpenRouter free-tier catalog; the filter narrows it to the
	// long-context subset the combo picker surfaces).
	openrouterFreeMinContext = 200000
)

// NewKilocodeResolver builds a Kilo Code live resolver. cache nil → default
// 10 min TTL (the JS providerModelsFetcher cached 10 min).
func NewKilocodeResolver(cache *Cache) LiveModelResolver {
	if cache == nil {
		cache = NewCache(kilocodeCacheTTL)
	}
	return &kilocodeResolver{
		cache:   cache,
		client:  &http.Client{Timeout: kilocodeFetchTimeout},
		baseURL: kilocodeGatewayURL,
	}
}

func (r *kilocodeResolver) ProviderID() string { return "kilocode" }

func (r *kilocodeResolver) Resolve(ctx context.Context, creds provider.Credentials, opts ResolveOpts) (*Result, error) {
	log := loggerOr(opts.Logger)
	// The gateway catalog is unauthenticated; the connection id is the only
	// stable per-connection dimension (same catalog for every kilocode
	// connection, but keying by connection keeps entries independent).
	key := kilocodeCacheKey(creds)
	if cached, ok := r.cache.Get(key); ok {
		return cached, nil
	}
	raw, err := r.fetch(ctx)
	if err != nil {
		log.Warn("KILOCODE_MODELS: fetch failed", "error", err)
		return nil, nil
	}
	models := filterKilocodeFreeModels(raw)
	if len(models) == 0 {
		return nil, nil
	}
	res := &Result{Models: models, RawModels: raw}
	r.cache.Set(key, res)
	return res, nil
}

func (r *kilocodeResolver) fetch(ctx context.Context) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kilocode /gateway/models %d", resp.StatusCode)
	}
	// The OpenRouter-shaped response is {data: [...]} (or {models: [...]});
	// the JS route did `json.data ?? json.models ?? json`.
	var wrap struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		// Fall back to a bare array.
		var arr []map[string]any
		if err2 := json.Unmarshal(body, &arr); err2 == nil {
			return arr, nil
		}
		return nil, err
	}
	if len(wrap.Data) > 0 {
		return wrap.Data, nil
	}
	return wrap.Models, nil
}

// filterKilocodeFreeModels applies the openrouter-free filter from the legacy
// JS filters.js: pricing.prompt == "0" && pricing.completion == "0" &&
// context_length >= 200000, projected to {id, name, contextLength}, sorted by
// contextLength descending. Returns 9gouter ResolvedModel entries. The JS
// filter compared against the STRING "0" (OpenRouter serializes pricing as
// strings), so we accept both string "0" and numeric 0.
func filterKilocodeFreeModels(raw []map[string]any) []ResolvedModel {
	var out []ResolvedModel
	seen := map[string]bool{}
	for _, m := range raw {
		if !isFreeModel(m) {
			continue
		}
		ctxLen := asPositiveInt(m["context_length"], m["contextLength"], m["context_window"])
		if ctxLen < openrouterFreeMinContext {
			continue
		}
		id := firstNonEmpty(asString(m["id"]), asString(m["model"]), asString(m["name"]))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := firstNonEmpty(asString(m["name"]), id)
		rm := ResolvedModel{ID: id, Name: name, Kind: "llm", UpstreamModelID: id, ContextLength: ctxLen}
		out = append(out, rm)
	}
	// Sort by contextLength descending (the JS .sort(b.contextLength - a.contextLength)).
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ContextLength > out[j].ContextLength
	})
	return out
}

// isFreeModel reports whether the OpenRouter pricing object marks the model as
// free (prompt == 0 && completion == 0). OpenRouter serializes pricing fields
// as strings ("0"), but tolerate numeric 0 too.
func isFreeModel(m map[string]any) bool {
	pricing, ok := m["pricing"].(map[string]any)
	if !ok {
		return false
	}
	return isZeroPrice(pricing["prompt"]) && isZeroPrice(pricing["completion"])
}

func isZeroPrice(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) == "0"
	case float64:
		return t == 0
	case int:
		return t == 0
	case int64:
		return t == 0
	case nil:
		return false
	default:
		return false
	}
}

func kilocodeCacheKey(creds provider.Credentials) string {
	seed := ""
	if v, ok := creds.ProviderSpecificData["_connectionId"].(string); ok {
		seed = v
	}
	if seed == "" {
		seed = "shared"
	}
	return stableHash("kilocode:" + seed)
}

// setTransport lets tests redirect the resolver's HTTP client at an httptest
// server (liveSwap transport) without exposing the base URL as a constructor
// knob. Same pattern as the other live resolvers (see live_test.go).
func (r *kilocodeResolver) setTransport(t http.RoundTripper) { r.client.Transport = t }
