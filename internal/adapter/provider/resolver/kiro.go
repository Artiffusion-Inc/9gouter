package resolver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
	"github.com/google/uuid"
)

// kiroResolver fetches the live model catalog from AWS CodeWhisperer's
// ListAvailableModels endpoint for an authenticated Kiro connection, then
// expands each upstream model into 9router-shaped variants
// (-thinking, -agentic, -thinking-agentic). Mirrors open-sse/services/
// kiroModels.js. The -thinking/-agentic suffixes do not exist on the Kiro
// upstream; the openai-to-kiro translator strips them before the request
// leaves the process.
//
// NOT YET PORTED: the real tokenRefresh subsystem (T027). On a 401 from
// ListAvailableModels, the resolver attempts a refresh via its TokenRefresher
// field; with the stub refresher that returns ErrTokenRefreshNotPorted, the
// resolver returns nil and the caller falls back to the static catalog. When
// T027 lands, inject the real refresher — no other change needed.
//
// The runtime User-Agent must match what Kiro IDE sends; the upstream rejects
// malformed UAs with 400.
type kiroResolver struct {
	cache     *Cache
	refresher TokenRefresher
	client    *http.Client
	// baseURL overrides the upstream ListAvailableModels host root for tests.
	// Empty means the production default https://q.<region>.amazonaws.com.
	baseURL string
}

const (
	kiroRuntimeSDKVersion = "1.0.0"
	kiroAgentOS           = "windows"
	kiroAgentOSVersion    = "10.0.26200"
	kiroNodeVersion       = "22.21.1"
	kiroVersion           = "0.10.32"
	kiroDefaultRegion     = "us-east-1"
	kiroFetchTimeout      = 30 * time.Second
	// kiroDefaultBaseRoot is the production upstream host root; the region is
	// interpolated between the scheme and the host.
	kiroDefaultBaseRoot = "https://q.%s.amazonaws.com"
)

// NewKiroResolver builds a Kiro live resolver with the given cache and token
// refresher. Pass StubTokenRefresher() until T027 is ported.
func NewKiroResolver(cache *Cache, refresher TokenRefresher) LiveModelResolver {
	if cache == nil {
		cache = NewCache(5 * time.Minute)
	}
	if refresher == nil {
		refresher = StubTokenRefresher()
	}
	return &kiroResolver{
		cache:     cache,
		refresher: refresher,
		client:    &http.Client{Timeout: kiroFetchTimeout},
	}
}

func (k *kiroResolver) ProviderID() string { return "kiro" }

// init registers a default kiro resolver with a fresh cache and the stub
// token refresher. This keeps /v1/models compiling and working (falling
// back to the static catalog on 401) with zero wiring. The composition root
// (wire.go) overrides this registration with NewKiroResolver + the real
// KiroRefresher from internal/adapter/provider/resolver/tokenrefresh, which
// lives in a separate package to avoid an import cycle with this package.
func init() {
	Register(NewKiroResolver(nil, nil))
}

func (k *kiroResolver) Resolve(ctx context.Context, creds provider.Credentials, opts ResolveOpts) (*Result, error) {
	log := loggerOr(opts.Logger)
	if creds.AccessToken == "" {
		log.Debug("KIRO_MODELS: no accessToken; skipping live fetch")
		return nil, nil
	}

	key := kiroCacheKey(creds)
	if cached, ok := k.cache.Get(key); ok {
		return cached, nil
	}

	raw, err := k.fetchKiroCatalogRaw(ctx, creds)
	if err != nil {
		if is401(err) && refreshTokenOf(creds) != "" {
			refreshed, rerr := k.refresher.Refresh(ctx, refreshTokenOf(creds), creds.ProviderSpecificData, log)
			if rerr != nil {
				log.Warn("KIRO_MODELS: token refresh failed", "error", rerr)
				return nil, nil
			}
			if opts.OnCredentialsRefreshed != nil {
				_ = opts.OnCredentialsRefreshed(*refreshed)
			}
			next := creds
			next.AccessToken = refreshed.AccessToken
			if refreshed.RefreshToken != "" {
				setRefreshToken(&next, refreshed.RefreshToken)
			}
			raw2, err2 := k.fetchKiroCatalogRaw(ctx, next)
			if err2 != nil {
				log.Warn("KIRO_MODELS: retry after refresh failed", "error", err2)
				return nil, nil
			}
			raw = raw2
		} else {
			log.Warn("KIRO_MODELS: ListAvailableModels failed", "error", err)
			return nil, nil
		}
	}

	expanded := kiroExpand(raw)
	result := &Result{Models: expanded, RawModels: raw}
	k.cache.Set(key, result)
	return result, nil
}

// fetchKiroCatalogRaw calls ListAvailableModels and returns the .models array
// from the response, or a kiroHTTPError (carrying status) on failure.
func (k *kiroResolver) fetchKiroCatalogRaw(ctx context.Context, creds provider.Credentials) ([]map[string]any, error) {
	psd := creds.ProviderSpecificData
	profileArn, _ := psd["profileArn"].(string)
	region := regionFromProfileArn(profileArn)

	params := url.Values{}
	params.Set("origin", "AI_EDITOR")
	if profileArn != "" {
		params.Set("profileArn", profileArn)
	}
	u := fmt.Sprintf(kiroDefaultBaseRoot+"/ListAvailableModels?%s", region, params.Encode())
	if k.baseURL != "" {
		// Test override: baseURL already includes scheme+host; append path+query.
		u = k.baseURL + "/ListAvailableModels?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for hk, hv := range kiroFingerprintHeaders(creds) {
		req.Header.Set(hk, hv)
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, kiroHTTPError{status: resp.StatusCode, body: string(body)}
	}
	var data struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	if data.Models == nil {
		data.Models = []map[string]any{}
	}
	return data.Models, nil
}

type kiroHTTPError struct {
	status int
	body   string
}

func (e kiroHTTPError) Error() string {
	return fmt.Sprintf("Kiro ListAvailableModels %d: %s", e.status, e.body)
}

func is401(err error) bool {
	if e, ok := err.(kiroHTTPError); ok {
		return e.status == http.StatusUnauthorized
	}
	return false
}

// kiroFingerprintHeaders builds the per-account fingerprint headers Kiro
// upstream validates. Keyed off a stable credential identifier so the same
// account always presents the same machineId. Mirrors JS
// buildKiroFingerprintHeaders.
func kiroFingerprintHeaders(creds provider.Credentials) map[string]string {
	seed := "kiro-anonymous"
	psd := creds.ProviderSpecificData
	if v, ok := psd["clientId"].(string); ok && v != "" {
		seed = v
	} else if rt := refreshTokenOf(creds); rt != "" {
		seed = rt
	} else if v, ok := psd["profileArn"].(string); ok && v != "" {
		seed = v
	} else if creds.AccessToken != "" {
		seed = creds.AccessToken
	}
	sum := sha256.Sum256([]byte(seed))
	machineID := hex.EncodeToString(sum[:])

	ua := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s#%s lang/js md/nodejs#%s api/codewhispererruntime#%s m/N,E KiroIDE-%s-%s",
		kiroRuntimeSDKVersion, kiroAgentOS, kiroAgentOSVersion, kiroNodeVersion,
		kiroRuntimeSDKVersion, kiroVersion, machineID,
	)
	amzUA := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", kiroRuntimeSDKVersion, kiroVersion, machineID)

	return map[string]string{
		"User-Agent":                   ua,
		"x-amz-user-agent":             amzUA,
		"x-amzn-kiro-agent-mode":       "vibe",
		"x-amzn-codewhisperer-optout":   "true",
		"amz-sdk-request":              "attempt=1; max=1",
		"amz-sdk-invocation-id":        uuid.NewString(),
	}
}

// regionFromProfileArn extracts the region from an ARN like
// arn:aws:codewhisperer:us-east-1:123456789012:profile/ABC, defaulting to
// us-east-1.
func regionFromProfileArn(profileArn string) string {
	if profileArn == "" {
		return kiroDefaultRegion
	}
	parts := strings.Split(profileArn, ":")
	if len(parts) >= 4 && parts[3] != "" {
		return parts[3]
	}
	return kiroDefaultRegion
}

// kiroCacheKey builds a stable per-credential cache key, preferring the most
// stable identifier so different login sessions for the same account share
// a cache entry.
func kiroCacheKey(creds provider.Credentials) string {
	seed := "anonymous"
	psd := creds.ProviderSpecificData
	if v, ok := psd["profileArn"].(string); ok && v != "" {
		seed = v
	} else if v, ok := psd["clientId"].(string); ok && v != "" {
		seed = v
	} else if rt := refreshTokenOf(creds); rt != "" {
		seed = rt
	} else if creds.AccessToken != "" {
		seed = creds.AccessToken
	}
	sum := sha256.Sum256([]byte("kiro:" + seed))
	return hex.EncodeToString(sum[:])
}

// kiroExpand mirrors JS buildVariants + the per-model expansion loop: each
// upstream model becomes 4 variants (base, -thinking, -agentic,
// -thinking-agentic), except `auto` which skips -agentic (server-side model
// pick; the chunked-write agentic prompt is not meaningful).
func kiroExpand(raw []map[string]any) []ResolvedModel {
	expanded := []ResolvedModel{}
	for _, m := range raw {
		upstreamID, _ := firstString(m, "modelId", "id")
		if upstreamID == "" {
			continue
		}
		modelName, _ := firstString(m, "modelName")
		rate := toFloat(m["rateMultiplier"])
		display := kiroFormatDisplayName(modelName, upstreamID, rate)
		contextLength := int(toFloat(nestedFloat(m, "tokenLimits", "maxInputTokens")))
		if contextLength == 0 {
			contextLength = 200000
		}
		for _, v := range kiroBuildVariants(upstreamID, display) {
			expanded = append(expanded, ResolvedModel{
				ID:              v.id,
				Name:            v.name,
				Kind:            "llm",
				ContextLength:   contextLength,
				Capabilities:    v.cap,
				UpstreamModelID: upstreamID,
			})
		}
	}
	return expanded
}

type kiroVariant struct {
	id   string
	name string
	cap  *Capabilities
}

func kiroBuildVariants(upstream, displayName string) []kiroVariant {
	safe := stripSyntheticSuffixes(upstream)
	display := displayName
	if display == "" {
		display = "Kiro " + safe
	}
	isAuto := safe == "auto"
	variants := []kiroVariant{
		{id: safe, name: display, cap: &Capabilities{}},
		{id: safe + "-thinking", name: display + " (Thinking)", cap: &Capabilities{Thinking: true}},
	}
	if !isAuto {
		variants = append(variants,
			kiroVariant{id: safe + "-agentic", name: display + " (Agentic)", cap: &Capabilities{Agentic: true}},
			kiroVariant{id: safe + "-thinking-agentic", name: display + " (Thinking + Agentic)", cap: &Capabilities{Thinking: true, Agentic: true}},
		)
	}
	return variants
}

// kiroFormatDisplayName formats the display name including the rate
// multiplier when it is not ~1.0x.
func kiroFormatDisplayName(modelName, modelID string, rate float64) string {
	base := strings.TrimSpace(modelName)
	if base == "" {
		base = modelID
	}
	if base == "" {
		base = "Kiro"
	}
	if rate <= 0 || abs(rate-1.0) < 1e-9 {
		return "Kiro " + base
	}
	return fmt.Sprintf("Kiro %s (%.1fx credit)", base, rate)
}

func stripSyntheticSuffixes(id string) string {
	out := strings.TrimSuffix(id, "-agentic")
	out = strings.TrimSuffix(out, "-thinking")
	return out
}

// --- small JSON helpers ---

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

func nestedFloat(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// refreshTokenOf reads the refreshToken from ProviderSpecificData, mirroring
// v1.go's credential resolution (the connection's refreshToken is stored in
// providerSpecificData["refreshToken"]).
func refreshTokenOf(creds provider.Credentials) string {
	if creds.ProviderSpecificData == nil {
		return ""
	}
	if v, ok := creds.ProviderSpecificData["refreshToken"].(string); ok {
		return v
	}
	return ""
}

// setRefreshToken writes the refreshToken back into ProviderSpecificData.
func setRefreshToken(creds *provider.Credentials, token string) {
	if creds.ProviderSpecificData == nil {
		creds.ProviderSpecificData = map[string]any{}
	}
	creds.ProviderSpecificData["refreshToken"] = token
}