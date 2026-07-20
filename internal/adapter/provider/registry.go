// Package provider is the provider adapter: registry, base executor wiring, and
// provider-specific config. It mirrors open-sse/executors/index.js + default.js.
package provider

import (
	"fmt"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/antigravity"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/azure"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/codebuddy"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/codex"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/commandcode"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/cursor"
	defexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/gemini-cli"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/github"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/grok-cli"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/grok-web"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/iflow"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/kimchi"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/kiro"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/mimo-free"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/ollama-local"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/opencode"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/opencode-go"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/perplexity-web"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/qoder"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/qwen"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/vertex"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/xiaomi-tokenplan"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
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
	"antigravity": {
		BaseURLs: []string{"https://cloudcode-pa.googleapis.com"},
		Format:   "antigravity",
		Headers: map[string]string{
			"User-Agent": "google-api-nodejs-client/9.15.1",
		},
		Retry: map[int]base.RetryEntry{
			429: {Attempts: 6, DelayMs: 2000},
			500: {Attempts: 3, DelayMs: 3000},
			503: {Attempts: 3, DelayMs: 2000},
		},
	},
	"assemblyai": {
		BaseURL: "https://api.assemblyai.com/v1/audio/transcriptions",
	},
	"azure": {
		BaseURL: "",
		Headers: map[string]string{},
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
	"codex": {
		BaseURL: "https://chatgpt.com/backend-api/codex/responses",
		Format:  "openai-responses",
		Headers: map[string]string{
			"originator":  "codex_cli_rs",
			"User-Agent":  "codex_cli_rs/0.136.0",
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
	},
	"cohere": {
		BaseURL: "https://api.cohere.ai/v1/chat/completions",
	},
	"commandcode": {
		BaseURL: "https://api.commandcode.ai/alpha/generate",
		Format:  "commandcode",
		Headers: map[string]string{
			"x-command-code-version": "0.25.7",
			"x-cli-environment":      "cli",
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
	},
	"cursor": {
		BaseURL:   "https://api2.cursor.sh",
		URLSuffix: "/aiserver.v1.ChatService/StreamUnifiedChatWithTools",
		Format:    "cursor",
		Headers: map[string]string{
			"connect-accept-encoding": "gzip",
			"connect-protocol-version":  "1",
			"Content-Type":              "application/connect+proto",
			"User-Agent":                "connect-es/1.6.1",
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
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
		Catalog: domain.ProviderCatalog{
			ID:           "gemini",
			Alias:        "gemini",
			ServiceKinds: []string{"llm", "embedding", "image", "imageToText", "webSearch", "tts", "stt"},
			Models: []domain.Model{
				{ID: "gemini-3.1-pro-preview", Name: "Gemini 3.1 Pro Preview"},
				{ID: "gemini-3.1-flash-lite-preview", Name: "Gemini 3.1 Flash Lite Preview"},
				{ID: "gemini-3-flash-preview", Name: "Gemini 3 Flash Preview"},
				{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro"},
				{ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash"},
				{ID: "gemini-2.5-flash-lite", Name: "Gemini 2.5 Flash Lite"},
				{ID: "gemma-4-31b-it", Name: "Gemma 4 31B IT"},
				{ID: "gemini-embedding-2-preview", Name: "Gemini Embedding 2 Preview", Kind: "embedding"},
				{ID: "gemini-embedding-001", Name: "Gemini Embedding 001", Kind: "embedding"},
				{ID: "text-embedding-005", Name: "Text Embedding 005", Kind: "embedding"},
				{ID: "text-embedding-004", Name: "Text Embedding 004 (Legacy)", Kind: "embedding"},
				{ID: "embedding-001", Name: "Embedding 001", Kind: "embedding"},
				{ID: "gemini-3.1-flash-image-preview", Name: "Gemini 3.1 Flash Image (Nano Banana 2)", Kind: "image"},
				{ID: "gemini-3-pro-image-preview", Name: "Gemini 3 Pro Image (Nano Banana Pro)", Kind: "image"},
				{ID: "gemini-2.5-flash-image", Name: "Gemini 2.5 Flash Image (Nano Banana)", Kind: "image"},
				{ID: "gemini-3.1-flash-tts-preview", Name: "Gemini 3.1 Flash TTS", Kind: "tts"},
				{ID: "gemini-2.5-flash-preview-tts", Name: "Gemini 2.5 Flash TTS", Kind: "tts"},
				{ID: "gemini-2.5-pro-preview-tts", Name: "Gemini 2.5 Pro TTS", Kind: "tts"},
			},
		},
	},
	"gemini-cli": {
		BaseURL: "https://cloudcode-pa.googleapis.com/v1internal",
		Format:  "gemini-cli",
		Headers: map[string]string{
			"X-Goog-Api-Client": "google-genai-sdk/1.41.0 gl-node/v22.19.0",
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
	},
	"gitlab": {
		BaseURL: "https://gitlab.com/api/v4/chat/completions",
	},
	"github": {
		BaseURL:      "https://api.githubcopilot.com/chat/completions",
		BaseURLs:     []string{"https://api.githubcopilot.com/chat/completions", "https://api.githubcopilot.com/responses", "https://api.githubcopilot.com/v1/messages"},
		Format:       "openai",
		Headers: map[string]string{
			"copilot-integration-id":              "vscode-chat",
			"editor-version":                      "vscode/1.110.0",
			"editor-plugin-version":               "copilot-chat/0.38.0",
			"user-agent":                          "GitHubCopilotChat/0.38.0",
			"openai-intent":                     "conversation-panel",
			"x-github-api-version":                "2025-04-01",
			"x-vscode-user-agent-library-version": "electron-fetch",
			"X-Initiator":                         "user",
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
	},
	"glm": {
		BaseURL:   "https://api.z.ai/api/anthropic/v1/messages",
		Format:    "claude",
		URLSuffix: "?beta=true",
		Headers:   claudeAPIHeaders,
		Auth: base.AuthDescriptor{
			Combined:         true,
			Header:           "x-api-key",
			Scheme:           "raw",
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
	"grok-web": {
		BaseURL: "https://grok.com/rest/app-chat/conversations/new",
		Format:  "grok-web",
		NoAuth:  true,
	},
	"groq": {
		BaseURL: "https://api.groq.com/openai/v1/chat/completions",
	},
	"hyperbolic": {
		BaseURL: "https://api.hyperbolic.xyz/v1/chat/completions",
	},
	"iflow": {
		BaseURL: "https://apis.iflow.cn/v1/chat/completions",
		Headers: map[string]string{
			"User-Agent": "iFlow-Cli",
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
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
			Combined:         true,
			Header:           "x-api-key",
			Scheme:           "raw",
			AnthropicVersion: true,
		},
	},
	"kimi-coding": {
		BaseURL:   "https://api.kimi.com/coding/v1/messages",
		Format:    "claude",
		URLSuffix: "?beta=true",
		Headers:   claudeAPIHeaders,
		Auth: base.AuthDescriptor{
			Combined:         true,
			Header:           "x-api-key",
			Scheme:           "raw",
			Hooks:            []string{"kimiHeaders"},
			AnthropicVersion: true,
		},
	},
	"kiro": {
		BaseURLs: []string{
			"https://runtime.us-east-1.kiro.dev/generateAssistantResponse",
			"https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
			"https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		},
		Format: "kiro",
		Headers: map[string]string{
			"Accept":          "application/vnd.amazon.eventstream",
			"X-Amz-Target":    "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
			"User-Agent":      "AWS-SDK-JS/3.0.0 kiro-ide/1.0.0",
			"X-Amz-User-Agent": "aws-sdk-js/3.0.0 kiro-ide/1.0.0",
		},
		Retry: map[int]base.RetryEntry{
			429: {Attempts: 0, DelayMs: 2000},
		},
	},
	"minimax": {
		BaseURL:   "https://api.minimax.io/anthropic/v1/messages",
		Format:    "claude",
		URLSuffix: "?beta=true",
		Headers:   claudeAPIHeaders,
		Auth: base.AuthDescriptor{
			Combined:         true,
			Header:           "x-api-key",
			Scheme:           "raw",
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
			Combined:         true,
			Header:           "x-api-key",
			Scheme:           "raw",
			AnthropicVersion: true,
		},
	},
	"mistral": {
		BaseURL: "https://api.mistral.ai/v1/chat/completions",
	},
	"mimo-free": {
		BaseURL: "https://api.xiaomimimo.com/api/free-ai/openai/chat",
		NoAuth:  true,
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
		Catalog: domain.ProviderCatalog{
			ID:           "ollama",
			Alias:        "ollama",
			ServiceKinds: []string{"llm"},
			Models: []domain.Model{
				{ID: "gpt-oss:120b", Name: "GPT OSS 120B"},
				{ID: "kimi-k2.5", Name: "Kimi K2.5"},
				{ID: "glm-5", Name: "GLM 5"},
				{ID: "minimax-m2.5", Name: "MiniMax M2.5"},
				{ID: "glm-4.7-flash", Name: "GLM 4.7 Flash"},
				{ID: "qwen3.5", Name: "Qwen3.5"},
				{ID: "minimax-m3", Name: "MiniMax M3"},
			},
		},
	},
	"ollama-local": {
		BaseURL: "http://localhost:11434/api/chat",
		Format:  "ollama",
		Catalog: domain.ProviderCatalog{
			ID:           "ollama-local",
			Alias:        "ollama-local",
			ServiceKinds: []string{"llm"},
		},
	},
	"openai": {
		BaseURL: "https://api.openai.com/v1/chat/completions",
	},
	"opencode": {
		BaseURL: "https://opencode.ai",
		Headers: map[string]string{
			"x-opencode-client": "desktop",
		},
		NoAuth: true,
	},
	"opencode-go": {
		BaseURL: "https://opencode.ai/zen/go/v1/chat/completions",
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
	"perplexity-web": {
		BaseURL: "https://www.perplexity.ai/rest/sse/perplexity_ask",
		Format:  "perplexity-web",
		NoAuth:  true,
	},
	"qoder": {
		BaseURL:  "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation",
		TimeoutMs: 120000,
	},
	"qwen": {
		BaseURL: "https://portal.qwen.ai/v1/chat/completions",
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
	"vertex": {
		BaseURL: "https://aiplatform.googleapis.com",
		Format:  "vertex",
	},
	"vertex-partner": {
		BaseURL: "https://aiplatform.googleapis.com",
		Format:  "openai",
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
	"xiaomi-tokenplan": {
		BaseURL: "https://token-plan-sgp.xiaomimimo.com/v1/chat/completions",
		BaseURLs: []string{
			"https://token-plan-sgp.xiaomimimo.com/v1",
			"https://token-plan-cn.xiaomimimo.com/v1",
			"https://token-plan-ams.xiaomimimo.com/v1",
		},
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
	id    string
	alias string
	exec  domain.Executor
}

func (p p) ID() string             { return p.id }
func (p p) Alias() string          { return p.alias }
func (p p) Executor() domain.Executor { return p.exec }

// newDefault returns a DefaultExecutor for providers without custom logic.
func newDefault(id string, cfg base.Config) domain.Executor {
	cfg.ID = id
	return defexec.New(id, cfg)
}

// Lookup resolves a provider id or alias to a Provider. It returns an error
// for unknown ids.
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
	switch id {
	case "antigravity":
		exec = antigravityexec.New(cfg)
	case "azure":
		exec = azureexec.New(cfg)
	case "codebuddy-cn":
		exec = codebuddyexec.New(cfg)
	case "codex":
		exec = codexec.New(cfg)
	case "commandcode":
		exec = commandcodeexec.New(cfg)
	case "cursor":
		exec = cursorexec.New(cfg)
	case "gemini-cli":
		exec = geminicliexec.New(cfg)
	case "github":
		exec = githubexec.New(cfg)
	case "grok-cli":
		exec = grokcliexec.New(cfg)
	case "grok-web":
		exec = grokwebexec.New(cfg)
	case "iflow":
		exec = iflowexec.New(cfg)
	case "kimchi":
		exec = kimchiexec.New(cfg)
	case "kiro":
		exec = kiroexec.New(cfg)
	case "mimo-free":
		exec = mimofreeexec.New(cfg)
	case "ollama-local":
		exec = ollamalocalexec.New(cfg)
	case "opencode":
		exec = opencodeexec.New(cfg)
	case "opencode-go":
		exec = opencodegoexec.New(cfg)
	case "perplexity-web":
		exec = perplexitywebexec.New(cfg)
	case "qoder":
		exec = qoderexec.New(cfg)
	case "qwen":
		exec = qwenexec.New(cfg)
	case "vertex":
		exec = vertexexec.New(id, cfg)
	case "vertex-partner":
		exec = vertexexec.New(id, cfg)
	case "xiaomi-tokenplan":
		exec = xiaomitokenplanexec.New(cfg)
	default:
		exec = newDefault(id, cfg)
	}
	return p{id: id, alias: alias, exec: exec}, nil
}

// Alias returns the canonical alias for a provider id.
func Alias(providerID string) string {
	return providerAlias(providerID)
}

// ChatBaseURL returns the raw chat BaseURL from the static provider config, or
// "" if the provider is unknown. It is the embeddings adapter's fallback path:
// for providers without an explicit embeddings URL it derives the embeddings
// endpoint by swapping the chat suffix (/chat/completions, /messages, ...) for
// /embeddings under the same versioned API root.
func ChatBaseURL(providerID string) string {
	id, ok := aliases[providerID]
	if !ok {
		return ""
	}
	cfg, ok := configs[id]
	if !ok {
		return ""
	}
	return cfg.BaseURL
}

// Catalog returns the static provider catalog (alias, models, serviceKinds)
// for a provider id, or false if the provider is unknown or has no catalog.
// Models may be empty for providers whose catalog is populated at runtime by
// live-model resolvers (not yet ported) or compatible-fetch — callers should
// treat an empty Models list as "no static catalog".
func Catalog(providerID string) (domain.ProviderCatalog, bool) {
	id, ok := aliases[providerID]
	if !ok {
		return domain.ProviderCatalog{}, false
	}
	cfg, ok := configs[id]
	if !ok {
		return domain.ProviderCatalog{}, false
	}
	cat := cfg.Catalog
	if cat.ID == "" {
		cat.ID = id
	}
	if cat.Alias == "" {
		cat.Alias = providerAlias(id)
	}
	if len(cat.ServiceKinds) == 0 {
		cat.ServiceKinds = []string{"llm"}
	}
	return cat, true
}

// AllCatalogs returns the static catalog for every configured provider.
// Providers without an explicit Catalog still appear with their alias and a
// default ["llm"] service kind, but an empty Models list. This is the data
// source for GET /v1/models (static portion).
func AllCatalogs() []domain.ProviderCatalog {
	out := make([]domain.ProviderCatalog, 0, len(configs))
	for id, cfg := range configs {
		cat := cfg.Catalog
		if cat.ID == "" {
			cat.ID = id
		}
		if cat.Alias == "" {
			cat.Alias = providerAlias(id)
		}
		if len(cat.ServiceKinds) == 0 {
			cat.ServiceKinds = []string{"llm"}
		}
		out = append(out, cat)
	}
	return out
}
