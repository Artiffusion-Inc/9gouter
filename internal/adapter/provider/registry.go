// Package provider is the provider adapter: registry, base executor wiring, and
// provider-specific config. It mirrors open-sse/executors/index.js + default.js.
package provider

import (
	"context"
	"fmt"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	defexec "github.com/Artiffusion-Inc/9router/internal/adapter/provider/default"
	domain "github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Anthropic Claude API headers reused across claude-format providers.
var claudeAPIHeaders = map[string]string{
	"Anthropic-Version": "2023-06-01",
	"Anthropic-Beta":    "claude-code-20250219,interleaved-thinking-2025-05-14",
}

// claudeCLIHeaders is the static full Claude Code fingerprint used by the
// claude provider. Values match the JS registry exactly.
var claudeCLIHeaders = map[string]string{
	"Anthropic-Version":                  "2023-06-01",
	"Anthropic-Beta":                     "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advanced-tool-use-2025-11-20,effort-2025-11-24,structured-outputs-2025-12-15,fast-mode-2026-02-01,redact-thinking-2026-02-12,token-efficient-tools-2026-03-28",
	"Anthropic-Dangerous-Direct-Browser-Access": "true",
	"User-Agent":                           "claude-cli/2.1.92 (external, sdk-cli)",
	"X-App":                                "cli",
	"X-Stainless-Helper-Method":            "stream",
	"X-Stainless-Retry-Count":              "0",
	"X-Stainless-Runtime-Version":          "v24.14.0",
	"X-Stainless-Package-Version":          "0.80.0",
	"X-Stainless-Runtime":                  "node",
	"X-Stainless-Lang":                     "js",
	"X-Stainless-Arch":                     "arm64",
	"X-Stainless-Os":                       "MacOS",
	"X-Stainless-Timeout":                  "600",
}

// ProviderConfig is the static per-provider transport configuration. It is
// intentionally minimal: only the fields required by executors for the
// url/header golden contract plus enough shape to keep Execute honest.
var configs = map[string]base.Config{
	"alicode": {
		BaseURL: "https://coding.dashscope.aliyuncs.com/v1/chat/completions",
		Quirks:  base.Quirks{PreserveCacheControl: true},
	},
	"alicode-intl": {
		BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions",
		Quirks:  base.Quirks{PreserveCacheControl: true},
	},
	"anthropic": {
		BaseURL: "https://api.anthropic.com/v1/messages",
		Format:  "claude",
		Headers: map[string]string{
			"anthropic-version": "2023-06-01",
			"Anthropic-Beta":    "claude-code-20250219,interleaved-thinking-2025-05-14",
		},
	},
	"assemblyai": {
		BaseURL: "https://api.assemblyai.com/v1/audio/transcriptions",
	},
	"blackbox": {
		BaseURL: "https://api.blackbox.ai/v1/chat/completions",
	},
	"byteplus": {
		BaseURL: "https://ark.ap-southeast.bytepluses.com/api/coding/v3/chat/completions",
	},
	"cerebras": {
		BaseURL: "https://api.cerebras.ai/v1/chat/completions",
	},
	"chutes": {
		BaseURL: "https://llm.chutes.ai/v1/chat/completions",
	},
	"claude": {
		BaseURL:   "https://api.anthropic.com/v1/messages",
		Format:    "claude",
		URLSuffix: "?beta=true",
		Headers:   claudeCLIHeaders,
		Auth: base.AuthDescriptor{
			APIKey:           &base.AuthSpec{Header: "x-api-key", Scheme: "raw"},
			OAuth:            &base.AuthSpec{Header: "Authorization", Scheme: "bearer"},
			Hooks:            []string{"claudeOverlay"},
			AnthropicVersion: true,
		},
	},
	"cline": {
		BaseURL: "https://api.cline.bot/api/v1/chat/completions",
		Headers: map[string]string{
			"HTTP-Referer": "https://cline.bot",
			"X-Title":      "Cline",
		},
		Auth: base.AuthDescriptor{
			Combined: true,
			Header:   "Authorization",
			Scheme:   "bearer",
			Hooks:    []string{"clineHeaders"},
		},
	},
	"clinepass": {
		BaseURL: "https://api.cline.bot/api/v1/chat/completions",
		Headers: map[string]string{
			"HTTP-Referer": "https://cline.bot",
			"X-Title":      "Cline",
		},
		Auth: base.AuthDescriptor{
			Combined: true,
			Header:   "Authorization",
			Scheme:   "bearer",
			Hooks:    []string{"clineHeaders"},
		},
	},
	"cloudflare-ai": {
		BaseURL: "https://api.cloudflare.com/client/v4/accounts/{accountId}/ai/v1/chat/completions",
	},
	"codebuddy-cn": {
		BaseURL: "https://copilot.tencent.com/v2/chat/completions",
		Headers: map[string]string{
			"User-Agent":         "CLI/2.108.1 CodeBuddy/2.108.1",
			"X-Product":          "SaaS",
			"X-IDE-Type":         "CLI",
			"X-IDE-Name":         "CLI",
			"x-requested-with":   "XMLHttpRequest",
			"x-codebuddy-request": "1",
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
	},
	"cohere": {
		BaseURL: "https://api.cohere.ai/v1/chat/completions",
	},
	"deepgram": {
		BaseURL: "https://api.deepgram.com/v1/listen",
	},
	"deepseek": {
		BaseURL: "https://api.deepseek.com/chat/completions",
	},
	"featherless": {
		BaseURL: "https://api.featherless.ai/v1/chat/completions",
	},
	"fireworks": {
		BaseURL: "https://api.fireworks.ai/inference/v1/chat/completions",
	},
	"gemini": {
		BaseURL: "https://generativelanguage.googleapis.com/v1beta/models",
		Format:  "gemini",
		Auth: base.AuthDescriptor{
			APIKey: &base.AuthSpec{Header: "x-goog-api-key", Scheme: "raw"},
			OAuth:  &base.AuthSpec{Header: "Authorization", Scheme: "bearer"},
		},
	},
	"gitlab": {
		BaseURL: "https://gitlab.com/api/v4/chat/completions",
	},
	"glm": {
		BaseURL:   "https://api.z.ai/api/anthropic/v1/messages",
		Format:    "claude",
		URLSuffix: "?beta=true",
		Headers:   claudeAPIHeaders,
		Auth: base.AuthDescriptor{
			Combined: true,
			Header:   "x-api-key",
			Scheme:   "raw",
			AnthropicVersion: true,
		},
	},
	"glm-cn": {
		BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
	},
	"grok-cli": {
		BaseURL: "https://cli-chat-proxy.grok.com/v1/responses",
		Format:  "openai-responses",
		Headers: map[string]string{
			"User-Agent":               "grok-shell/0.2.99 (linux; x86_64)",
			"x-grok-client-identifier": "grok-shell",
			"x-grok-client-version":    "0.2.99",
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
		Retry: map[int]base.RetryEntry{
			429: {Attempts: 2, DelayMs: 2000},
			502: {Attempts: 2, DelayMs: 1500},
			503: {Attempts: 2, DelayMs: 1500},
		},
	},
	"groq": {
		BaseURL: "https://api.groq.com/openai/v1/chat/completions",
	},
	"hyperbolic": {
		BaseURL: "https://api.hyperbolic.xyz/v1/chat/completions",
	},
	"kilocode": {
		BaseURL: "https://api.kilo.ai/api/openrouter/chat/completions",
	},
	"kimchi": {
		BaseURL: "https://llm.kimchi.dev/openai/v1/chat/completions",
		Headers: map[string]string{
			"User-Agent": "kimchi/0.1.50",
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
	},
	"kimi": {
		BaseURL:   "https://api.kimi.com/coding/v1/messages",
		Format:    "claude",
		URLSuffix: "?beta=true",
		Headers:   claudeAPIHeaders,
		Auth: base.AuthDescriptor{
			Combined: true,
			Header:   "x-api-key",
			Scheme:   "raw",
			AnthropicVersion: true,
		},
	},
	"kimi-coding": {
		BaseURL:   "https://api.kimi.com/coding/v1/messages",
		Format:    "claude",
		URLSuffix: "?beta=true",
		Headers:   claudeAPIHeaders,
		Auth: base.AuthDescriptor{
			Combined: true,
			Header:   "x-api-key",
			Scheme:   "raw",
			Hooks:    []string{"kimiHeaders"},
			AnthropicVersion: true,
		},
	},
	"minimax": {
		BaseURL:   "https://api.minimax.io/anthropic/v1/messages",
		Format:    "claude",
		URLSuffix: "?beta=true",
		Headers:   claudeAPIHeaders,
		Auth: base.AuthDescriptor{
			Combined: true,
			Header:   "x-api-key",
			Scheme:   "raw",
			AnthropicVersion: true,
		},
		ReasoningInject: &base.ReasoningInject{Scope: "all"},
		Quirks:          base.Quirks{DropOutputConfig: true},
	},
	"minimax-cn": {
		BaseURL:   "https://api.minimaxi.com/anthropic/v1/messages",
		Format:    "claude",
		URLSuffix: "?beta=true",
		Headers:   claudeAPIHeaders,
		Auth: base.AuthDescriptor{
			Combined: true,
			Header:   "x-api-key",
			Scheme:   "raw",
			AnthropicVersion: true,
		},
	},
	"mistral": {
		BaseURL: "https://api.mistral.ai/v1/chat/completions",
	},
	"mmf": {
		BaseURL: "https://api.xiaomimimo.com/api/free-ai/openai/chat",
		NoAuth:  true,
	},
	"nanobanana": {
		BaseURL: "https://api.nanobananaapi.ai/v1/chat/completions",
	},
	"nebius": {
		BaseURL: "https://api.studio.nebius.ai/v1/chat/completions",
	},
	"nvidia": {
		BaseURL: "https://integrate.api.nvidia.com/v1/chat/completions",
	},
	"ollama": {
		BaseURL: "https://ollama.com/api/chat",
		Format:  "ollama",
	},
	"openai": {
		BaseURL: "https://api.openai.com/v1/chat/completions",
	},
	"openrouter": {
		BaseURL: "https://openrouter.ai/api/v1/chat/completions",
		Headers: map[string]string{
			"HTTP-Referer": "https://endpoint-proxy.local",
			"X-Title":      "Endpoint Proxy",
		},
	},
	"perplexity": {
		BaseURL: "https://api.perplexity.ai/chat/completions",
	},
	"perplexity-agent": {
		BaseURL: "https://api.perplexity.ai/v1/responses",
		Format:  "openai-responses",
	},
	"siliconflow": {
		BaseURL: "https://api.siliconflow.com/v1/chat/completions",
	},
	"together": {
		BaseURL: "https://api.together.xyz/v1/chat/completions",
	},
	"venice": {
		BaseURL: "https://api.venice.ai/api/v1/chat/completions",
	},
	"vercel-ai-gateway": {
		BaseURL: "https://ai-gateway.vercel.sh/v1/chat/completions",
	},
	"volcengine-ark": {
		BaseURL: "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions",
	},
	"xai": {
		BaseURL: "https://api.x.ai/v1/chat/completions",
	},
	"xiaomi-mimo": {
		BaseURL: "https://api.xiaomimimo.com/v1/chat/completions",
	},
}

// aliases maps alias -> canonical provider id, matching open-sse/executors/index.js.
var aliases = map[string]string{
	// Canonical ids are self-mapping.
	"alicode":          "alicode",
	"alicode-intl":     "alicode-intl",
	"anthropic":        "anthropic",
	"antigravity":      "antigravity",
	"assemblyai":       "assemblyai",
	"azure":            "azure",
	"blackbox":         "blackbox",
	"byteplus":         "byteplus",
	"cartesia":         "cartesia",
	"cerebras":         "cerebras",
	"chutes":           "chutes",
	"claude":           "claude",
	"cline":            "cline",
	"clinepass":        "clinepass",
	"cloudflare-ai":    "cloudflare-ai",
	"codebuddy-cn":     "codebuddy-cn",
	"codex":            "codex",
	"cohere":           "cohere",
	"commandcode":      "commandcode",
	"cursor":           "cursor",
	"deepgram":         "deepgram",
	"deepseek":         "deepseek",
	"featherless":      "featherless",
	"fireworks":        "fireworks",
	"gemini":           "gemini",
	"gemini-cli":       "gemini-cli",
	"github":           "github",
	"gitlab":           "gitlab",
	"glm":              "glm",
	"glm-cn":           "glm-cn",
	"grok-cli":         "grok-cli",
	"grok-web":         "grok-web",
	"groq":             "groq",
	"hyperbolic":       "hyperbolic",
	"iflow":            "iflow",
	"kilocode":         "kilocode",
	"kimchi":           "kimchi",
	"kimi":             "kimi",
	"kimi-coding":      "kimi-coding",
	"kiro":             "kiro",
	"minimax":          "minimax",
	"minimax-cn":       "minimax-cn",
	"mistral":          "mistral",
	"mimo-free":        "mimo-free",
	"mmf":              "mmf",
	"nanobanana":       "nanobanana",
	"nebius":           "nebius",
	"nvidia":           "nvidia",
	"ollama":           "ollama",
	"ollama-local":     "ollama-local",
	"openai":           "openai",
	"opencode":         "opencode",
	"opencode-go":      "opencode-go",
	"openrouter":       "openrouter",
	"perplexity":       "perplexity",
	"perplexity-agent": "perplexity-agent",
	"perplexity-web":   "perplexity-web",
	"playht":           "playht",
	"qoder":            "qoder",
	"qwen":             "qwen",
	"recraft":          "recraft",
	"siliconflow":      "siliconflow",
	"stability-ai":     "stability-ai",
	"together":         "together",
	"topaz":            "topaz",
	"tortoise":         "tortoise",
	"venice":           "venice",
	"vercel-ai-gateway": "vercel-ai-gateway",
	"vertex":           "vertex",
	"vertex-partner":   "vertex-partner",
	"volcengine-ark":   "volcengine-ark",
	"xai":              "xai",
	"xiaomi-mimo":      "xiaomi-mimo",
	"xiaomi-tokenplan": "xiaomi-tokenplan",
	// Short aliases from open-sse/executors/index.js.
	"cc":    "claude",
	"cl":    "cline",
	"cf":    "cloudflare-ai",
	"cbcn":  "codebuddy-cn",
	"cu":    "cursor",
	"gcli":  "grok-cli",
	"gb":    "grok-cli",
	"kmc":   "kimi-coding",
	"ark":   "volcengine-ark",
	"mimo":  "xiaomi-mimo",
	"pplx-agent":   "perplexity-agent",
	"pplx-responses": "perplexity-agent",
}

// providerAlias returns the canonical alias (short form) for a provider id.
// This is a best-effort mirror of the JS alias used for display/logging.
func providerAlias(id string) string {
	for alias, canonical := range aliases {
		if canonical == id && alias != id {
			return alias
		}
	}
	return id
}

// p is a provider implementation that wires a static id/alias to an executor.
type p struct {
	id      string
	alias   string
	exec    domain.Executor
}

func (p p) ID() string             { return p.id }
func (p p) Alias() string          { return p.alias }
func (p p) Executor() domain.Executor { return p.exec }

// Lookup resolves a provider id or alias to a Provider. It returns an error
// for unknown ids. Specialized executors not yet ported are registered as
// stubs so the registry stays intact.
func Lookup(providerID string) (domain.Provider, error) {
	id, ok := aliases[providerID]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", providerID)
	}
	cfg, ok := configs[id]
	if !ok {
		return nil, fmt.Errorf("provider %q not configured", providerID)
	}
	alias := providerAlias(id)
	cfg.ID = id

	var exec domain.Executor
	// Stubs: providers whose specialized executor is not yet ported return a
	// not-yet-implemented error on Execute while still exposing BuildURL and
	// BuildHeaders so the registry stays intact.
	if cfg.BaseURL == "" && len(cfg.BaseURLs) == 0 {
		exec = &notImplementedExecutor{DefaultExecutor: defexec.New(id, cfg)}
	} else {
		exec = defexec.New(id, cfg)
	}
	return p{id: id, alias: alias, exec: exec}, nil
}

// Alias returns the canonical alias for a provider id.
func Alias(providerID string) string {
	return providerAlias(providerID)
}

// notImplementedExecutor is used to keep the registry intact for providers
// whose full executor is not yet ported. BuildURL/BuildHeaders still work
// via default behavior; Execute returns a clear error.
type notImplementedExecutor struct {
	*defexec.DefaultExecutor
}

func (n *notImplementedExecutor) Execute(ctx context.Context, req domain.ExecRequest) (domain.Resp, error) {
	return domain.Resp{}, fmt.Errorf("provider %q executor not yet implemented", n.Provider)
}

func init() {
	// Register stub entries for providers not in the url-header golden subset
	// but required by the 25-provider plan registry so Lookup does not fail.
	stubs := []string{
		"antigravity", "azure", "gemini-cli", "github", "iflow", "qoder", "kiro",
		"codex", "cursor", "vertex", "vertex-partner", "qwen", "opencode",
		"opencode-go", "grok-web", "perplexity-web", "ollama-local", "commandcode",
		"xiaomi-tokenplan", "mimo-free",
	}
	for _, id := range stubs {
		if _, ok := configs[id]; ok {
			continue
		}
		configs[id] = base.Config{ID: id, BaseURL: ""}
	}
}
