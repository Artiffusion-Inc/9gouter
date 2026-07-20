package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// copilotResolver fetches the live model catalog from GitHub Copilot's
// /models endpoint for an authenticated GitHub connection, returning only
// chat-capable models the account's policy allows. Mirrors
// open-sse/services/copilotModels.js.
//
// The Copilot token (a short-lived token exchanged from the GitHub access
// token) is read from ProviderSpecificData.copilotToken. On a 401/403 the
// resolver refreshes via the CopilotRefresher (token exchange) and retries
// once, persisting the new token through OnCredentialsRefreshed.
type copilotResolver struct {
	cache     *Cache
	refresher TokenRefresher // *tokenrefresh.CopilotRefresher
	client    *http.Client
	baseURL   string
	vscode    string
	chatVer   string
	userAgent string
	apiVer    string
}

const (
	copilotModelsURL    = "https://api.githubcopilot.com/models"
	copilotFetchTimeout = 10 * time.Second
	copilotCacheTTL     = 5 * time.Minute
)

// NewCopilotResolver builds a GitHub Copilot live resolver. refresher is the
// Copilot token-exchange refresher (tokenrefresh.NewCopilotRefresher); pass
// nil to disable refresh (resolver falls back to static on 401).
func NewCopilotResolver(cache *Cache, refresher TokenRefresher, appVersion string) LiveModelResolver {
	if cache == nil {
		cache = NewCache(copilotCacheTTL)
	}
	return &copilotResolver{
		cache:     cache,
		refresher: refresher,
		client:    &http.Client{Timeout: copilotFetchTimeout},
		vscode:    "1.110.0",
		chatVer:   "0.38.0",
		userAgent: "GitHubCopilotChat/0.38.0",
		apiVer:    "2025-04-01",
	}
}

func (r *copilotResolver) ProviderID() string { return "github" }

func (r *copilotResolver) Resolve(ctx context.Context, creds provider.Credentials, opts ResolveOpts) (*Result, error) {
	log := loggerOr(opts.Logger)
	token := copilotToken(creds)
	if token == "" {
		log.Debug("COPILOT_MODELS: no copilotToken/accessToken; skipping live fetch")
		return nil, nil
	}
	key := copilotCacheKey(creds)
	if cached, ok := r.cache.Get(key); ok {
		return cached, nil
	}
	raw, err := r.fetch(ctx, token)
	if err != nil {
		// 401/403 → refresh the Copilot token from the GitHub access token and retry.
		if is401or403(err) && creds.AccessToken != "" && r.refresher != nil {
			log.Info("COPILOT_MODELS: got 401/403; refreshing Copilot token")
			refreshed, rerr := refreshDeduped(ctx, r.refresher, creds, creds.AccessToken, opts.ProxyOptions, log)
			if rerr != nil || refreshed == nil || refreshed.AccessToken == "" {
				log.Warn("COPILOT_MODELS: token refresh did not return a token")
				return nil, nil
			}
			if opts.OnCredentialsRefreshed != nil {
				_ = opts.OnCredentialsRefreshed(*refreshed)
			}
			raw2, err2 := r.fetch(ctx, refreshed.AccessToken)
			if err2 != nil {
				log.Warn("COPILOT_MODELS: retry after refresh failed", "error", err2)
				return nil, nil
			}
			raw = raw2
		} else {
			log.Warn("COPILOT_MODELS: fetch failed", "error", err)
			return nil, nil
		}
	}
	models := expandCopilot(raw)
	if len(models) == 0 {
		return nil, nil
	}
	res := &Result{Models: models, RawModels: raw}
	r.cache.Set(key, res)
	return res, nil
}

func (r *copilotResolver) fetch(ctx context.Context, token string) ([]map[string]any, error) {
	url := copilotModelsURL
	if r.baseURL != "" {
		url = r.baseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
	req.Header.Set("editor-version", "vscode/"+r.vscode)
	req.Header.Set("editor-plugin-version", "copilot-chat/"+r.chatVer)
	req.Header.Set("user-agent", r.userAgent)
	req.Header.Set("x-github-api-version", r.apiVer)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot /models %d", resp.StatusCode)
	}
	var wrap struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, err
	}
	return wrap.Data, nil
}

// expandCopilot keeps only chat models the account is allowed to use
// (policy.state == "enabled"), deduped by id. Mirrors JS expandCatalog.
func expandCopilot(raw []map[string]any) []ResolvedModel {
	var out []ResolvedModel
	seen := map[string]bool{}
	for _, m := range raw {
		caps, _ := m["capabilities"].(map[string]any)
		if caps == nil || caps["type"] != "chat" {
			continue
		}
		if policy, ok := m["policy"].(map[string]any); ok {
			if state, _ := policy["state"].(string); state != "" && state != "enabled" {
				continue
			}
		}
		id, _ := m["id"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name, _ := m["name"].(string)
		if name == "" {
			name = id
		}
		out = append(out, ResolvedModel{ID: id, Name: name, Kind: "chat", UpstreamModelID: id})
	}
	return out
}

func copilotToken(creds provider.Credentials) string {
	if v, ok := creds.ProviderSpecificData["copilotToken"].(string); ok && v != "" {
		return v
	}
	return creds.AccessToken
}

func copilotCacheKey(creds provider.Credentials) string {
	seed := copilotToken(creds)
	if seed == "" {
		seed = "copilot-anonymous"
	}
	return stableHash("copilot:" + seed)
}

func is401or403(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, " 401") || contains(s, " 403")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}