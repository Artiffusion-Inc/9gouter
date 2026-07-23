// Package capabilities ports open-sse/providers/capabilities.js into Go:
// the model-capability fallback chain used to resolve vision/reasoning/search/
// tools, thinking wire format, and context/output token limits per model.
//
// Resolution order (first match wins), merged over Default so consumers never
// null-check:
//  1. PROVIDER_CAPABILITIES[provider][model]  — provider-specific override
//  2. MODEL_CAPABILITIES[model]               — canonical exact id (exceptions)
//  3. PATTERN_CAPABILITIES                     — glob match, ordered specific→generic
//  4. Default                                   — safe floor (always returned)
//
// Glob matching reuses pricing.MatchPattern (case-insensitive anchored "^...$"
// with "*" as wildcard) so capabilities and pricing share one matcher.
package capabilities

import (
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
)

// ThinkingFormat is the wire-format enum for reasoning models. Empty means
// "derive from transport.format". Mirrors the JS thinkingFormat values.
type ThinkingFormat string

const (
	ThinkingNone           ThinkingFormat = ""
	ThinkingOpenai         ThinkingFormat = "openai"
	ThinkingClaudeAdaptive ThinkingFormat = "claude-adaptive"
	ThinkingClaudeBudget   ThinkingFormat = "claude-budget"
	ThinkingGeminiLevel    ThinkingFormat = "gemini-level"
	ThinkingGeminiBudget   ThinkingFormat = "gemini-budget"
	ThinkingZai            ThinkingFormat = "zai"
	ThinkingQwen           ThinkingFormat = "qwen"
	ThinkingDeepseek       ThinkingFormat = "deepseek"
	ThinkingKimi           ThinkingFormat = "kimi"
	ThinkingMinimax        ThinkingFormat = "minimax"
	ThinkingHunyuan        ThinkingFormat = "hunyuan"
	ThinkingStep           ThinkingFormat = "step"
)

// ThinkingRange is the {min,max} clamp for budget-style thinking formats. A
// nil range means no clamp.
type ThinkingRange struct {
	Min int `json:"min,omitempty"`
	Max int `json:"max,omitempty"`
}

// Capabilities describes what a model can read/do and its token limits. Every
// resolved result is merged over Default so consumers never need null-checks.
// Field order mirrors the JS object for readability.
type Capabilities struct {
	// input modalities
	Vision     bool `json:"vision"`
	PDF        bool `json:"pdf"`
	AudioInput bool `json:"audioInput"`
	VideoInput bool `json:"videoInput"`
	// output modalities
	ImageOutput bool `json:"imageOutput"`
	AudioOutput bool `json:"audioOutput"`
	// features
	Search    bool `json:"search"`
	Tools     bool `json:"tools"`
	Reasoning bool `json:"reasoning"`
	// thinking wire format (only meaningful when Reasoning is true). Empty
	// signals "derive from transport.format".
	ThinkingFormat     ThinkingFormat `json:"thinkingFormat"`
	ThinkingCanDisable bool           `json:"thinkingCanDisable"`
	ThinkingRange      *ThinkingRange `json:"thinkingRange,omitempty"`
	// limits (tokens)
	ContextWindow int `json:"contextWindow"`
	MaxOutput     int `json:"maxOutput"`
}

// Default is the safe floor merged under every resolved result. Most modern
// LLMs meet these limits. Mirrors DEFAULT_CAPABILITIES.
var Default = Capabilities{
	Tools:              true,
	ThinkingCanDisable: true,
	ContextWindow:      200000,
	MaxOutput:          64000,
}

// MODEL_CAPABILITIES — canonical exact-id overrides (exceptions patterns would
// mis-match). Only the non-default fields are listed; GetCapabilitiesForModel
// merges over Default. Mirrors the JS MODEL_CAPABILITIES map.
var modelCapabilities = map[string]Capabilities{
	// Claude 4.6/4.7/4.8 and Sonnet 5 — 1M context + adaptive thinking.
	"claude-opus-4.6":                  {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-opus-4.7":                  {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-opus-4-7":                  {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-opus-4.8":                  {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-opus-4-6":                  {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-opus-4-8":                  {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-opus-4.8-thinking":         {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-opus-4-8-thinking":         {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-sonnet-4.6":                {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-sonnet-4-6":                {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-sonnet-5":                  {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-sonnet-5-thinking":         {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-sonnet-5-agentic":          {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},
	"claude-sonnet-5-thinking-agentic": {Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive, ContextWindow: 1000000, MaxOutput: 128000},

	// Gemini image-gen / OpenAI image / xai image variants.
	"gpt-image-1": {ImageOutput: true, Tools: false},

	// GLM vision variant (text GLM has no vision).
	"glm-4.6v": {Vision: true, Reasoning: true, ThinkingFormat: ThinkingZai, ContextWindow: 128000},

	// Qwen plain coder/text (no vision) — registry "vision-model"/"coder-model" aliases.
	"vision-model": {Vision: true, Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 1000000},
	"coder-model":  {Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 1000000},

	// Kimi flagship + coding (platform + Kimi Code ids) — vision/video native.
	"kimi-k3":                   {Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 1048576, MaxOutput: 131072},
	"k3":                        {Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 1048576, MaxOutput: 131072},
	"kimi-for-coding":           {Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 262144, MaxOutput: 65536},
	"kimi-for-coding-highspeed": {Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 262144, MaxOutput: 65536},
	"kimi-k2.7-code":            {Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 262144, MaxOutput: 65536},
	"kimi-k2.7-code-highspeed":  {Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 262144, MaxOutput: 65536},
}

// Kimi/Kiro models cannot disable thinking → set ThinkingCanDisable:false.
// These are applied by overlaying the false value on top via mergeThinkingDisabled.

var kiroGPT56 = Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 272000, MaxOutput: 128000}

// codexGPT56Sol differs from Terra/Luna (#2720).
var codexGPT56Sol = Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 372000, MaxOutput: 128000}
var codexGPT56Default = Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 272000, MaxOutput: 128000}

// providerCapabilities is the PROVIDER_CAPABILITIES map: provider → model →
// caps (only non-default fields; merged over Default at resolve time).
var providerCapabilities = map[string]map[string]Capabilities{
	// NVIDIA NIM is OpenAI-compatible → rejects MiniMax/GLM native thinking.
	"nvidia": {
		"minimaxai/minimax-m2.7":        {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 131072},
		"minimaxai/minimax-m3":          {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 512000, MaxOutput: 131072},
		"z-ai/glm-5.2":                  {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 128000},
		"deepseek-ai/deepseek-v4-pro":   {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 1000000, MaxOutput: 65536},
		"deepseek-ai/deepseek-v4-flash": {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 1000000, MaxOutput: 65536},
	},
	"codex": {
		"gpt-5.6-sol":          codexGPT56Sol,
		"gpt-5.6-sol-review":   codexGPT56Sol,
		"gpt-5.6-terra":        codexGPT56Default,
		"gpt-5.6-terra-review": codexGPT56Default,
		"gpt-5.6-luna":         codexGPT56Default,
		"gpt-5.6-luna-review":  codexGPT56Default,
	},
	"kiro": {
		"gpt-5.6-sol":                    kiroGPT56,
		"gpt-5.6-terra":                  kiroGPT56,
		"gpt-5.6-luna":                   kiroGPT56,
		"gpt-5.6-sol-thinking":           kiroGPT56,
		"gpt-5.6-terra-thinking":         kiroGPT56,
		"gpt-5.6-luna-thinking":          kiroGPT56,
		"gpt-5.6-sol-agentic":            kiroGPT56,
		"gpt-5.6-terra-agentic":          kiroGPT56,
		"gpt-5.6-luna-agentic":           kiroGPT56,
		"gpt-5.6-sol-thinking-agentic":   kiroGPT56,
		"gpt-5.6-terra-thinking-agentic": kiroGPT56,
		"gpt-5.6-luna-thinking-agentic":  kiroGPT56,
	},
	"codebuddy-cn": {
		"glm-5.2":            {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 1000000, MaxOutput: 48000},
		"glm-5.1":            {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 48000},
		"glm-5.0":            {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 48000},
		"glm-5.0-turbo":      {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 48000},
		"glm-5v-turbo":       {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 38000},
		"glm-4.7":            {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 48000},
		"minimax-m3":         {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 512000, MaxOutput: 48000},
		"minimax-m2.7":       {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 48000},
		"kimi-k2.7":          {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 256000, MaxOutput: 32000},
		"kimi-k2.6":          {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 256000, MaxOutput: 32000},
		"kimi-k2.5":          {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 164000, MaxOutput: 32000},
		"hy3-preview":        {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 192000, MaxOutput: 64000},
		"deepseek-v4-pro":    {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 1000000, MaxOutput: 50000},
		"deepseek-v4-flash":  {Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 1000000, MaxOutput: 50000},
		"deepseek-v3-2-volc": {Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 96000, MaxOutput: 32000},
	},
}

// Models that cannot disable thinking. Overlay false onto the resolved caps.
// (kiro gpt-5.6 family + codebuddy-cn onlyReasoning models + qwq/kimi-k3/etc.
// are encoded directly in their tables with ThinkingCanDisable defaulting true;
// the false cases below are merged at resolve time.)
var thinkingDisabledModels = map[string]bool{}

// patternEntry is a glob-pattern → caps mapping (first match wins, order matters).
type patternEntry struct {
	pattern string
	caps    Capabilities
}

// patternCapabilities is the PATTERN_CAPABILITIES list: vision/specific variants
// first, text-only/generic families last. Mirrors the JS array order exactly.
var patternCapabilities = []patternEntry{
	// ── Claude (4.6+ = adaptive; older/haiku = budget) ──
	{"*claude*opus-4.6*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive}},
	{"*claude*opus-4.7*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive}},
	{"*claude*opus-4.8*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive}},
	{"*claude*sonnet-4.6*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive}},
	{"*claude*sonnet-4.7*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeAdaptive}},
	{"*claude*haiku*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeBudget}},
	{"*claude*opus*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeBudget}},
	{"*claude*sonnet*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeBudget}},
	{"*claude*fable*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeBudget, ContextWindow: 1000000, MaxOutput: 128000}},
	{"*claude*mythos*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeBudget, ContextWindow: 1000000, MaxOutput: 128000}},
	{"*claude-3*", Capabilities{Vision: true}},
	{"*claude*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingClaudeBudget}},

	// ── Gemini ──
	{"*gemini*image*", Capabilities{Vision: true, ImageOutput: true, ContextWindow: 1048576}},
	{"*gemini-3*pro*", Capabilities{Vision: true, AudioInput: true, VideoInput: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingGeminiLevel, ContextWindow: 1048576, MaxOutput: 65535}},
	{"*gemini-3*", Capabilities{Vision: true, AudioInput: true, VideoInput: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingGeminiLevel, ContextWindow: 1048576, MaxOutput: 65536}},
	{"*gemini-2.5*", Capabilities{Vision: true, AudioInput: true, VideoInput: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingGeminiBudget, ThinkingRange: &ThinkingRange{Min: 0, Max: 24576}, ContextWindow: 1048576, MaxOutput: 65536}},
	{"*gemini-2*", Capabilities{Vision: true, AudioInput: true, VideoInput: true, Search: true, ContextWindow: 1048576, MaxOutput: 65536}},
	{"*gemini*", Capabilities{Vision: true, Search: true, ContextWindow: 1048576}},
	{"*gemma*", Capabilities{Vision: true, ContextWindow: 128000}},
	{"*nanobanana*", Capabilities{Vision: true, ImageOutput: true}},

	// ── OpenAI GPT-5.x ──
	{"*gpt-5*image*", Capabilities{ImageOutput: true}},
	{"*gpt-5*codex*", Capabilities{Reasoning: true, Search: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 400000, MaxOutput: 128000}},
	{"*gpt-5*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 400000, MaxOutput: 128000}},
	{"*gpt-4o*", Capabilities{Vision: true, Search: true, ContextWindow: 128000, MaxOutput: 16384}},
	{"*gpt-4.1*", Capabilities{Vision: true, ContextWindow: 1000000, MaxOutput: 32768}},
	{"*gpt-4-turbo*", Capabilities{Vision: true, ContextWindow: 128000}},
	{"*gpt-4*", Capabilities{ContextWindow: 128000}},
	{"*gpt-3.5*", Capabilities{ContextWindow: 16385, MaxOutput: 4096}},
	{"*gpt-oss*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 128000}},

	// ── OpenAI o-series ──
	{"*o1-mini*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 128000}},
	{"*o1*", Capabilities{Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 100000}},
	{"*o3*", Capabilities{Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 100000}},
	{"*o4*", Capabilities{Vision: true, Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 200000, MaxOutput: 100000}},

	// ── Grok ──
	{"*grok*image*", Capabilities{ImageOutput: true}},
	{"*grok-code*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 256000}},
	{"*grok-4.5*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 500000, MaxOutput: 64000}},
	{"*grok-4*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 256000}},
	{"*grok-3*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 131072}},
	{"*grok*", Capabilities{Vision: true, Reasoning: true, Search: true, ThinkingFormat: ThinkingOpenai, ContextWindow: 256000}},

	// ── Qwen (3.5+ = native vision/video; coder & max = text-only; QwQ = thinking-only) ──
	{"*qwen*vl*", Capabilities{Vision: true, Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 262144}},
	{"*qwen*omni*", Capabilities{Vision: true, AudioInput: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 262144, MaxOutput: 65536}},
	{"*qwen*coder*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 1000000}},
	{"*qwen*max*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 1000000, MaxOutput: 65536}},
	{"*qwen3.5*", Capabilities{Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 1000000, MaxOutput: 65536}},
	{"*qwen3.6*", Capabilities{Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 1000000, MaxOutput: 65536}},
	{"*qwen3.7*", Capabilities{Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 1000000, MaxOutput: 65536}},
	{"*qwen*plus*", Capabilities{Vision: true, Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 1000000, MaxOutput: 65536}},
	{"*qwen*235b*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 262144}},
	{"*qwq*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 131072}},
	{"*qwen*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingQwen, ContextWindow: 262144}},

	// ── Kimi ──
	{"*kimi*k3*", Capabilities{Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 1048576, MaxOutput: 131072}},
	{"*kimi*for-coding*", Capabilities{Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 262144, MaxOutput: 65536}},
	{"*kimi*k2.7*code*", Capabilities{Vision: true, VideoInput: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 262144, MaxOutput: 65536}},
	{"*kimi*k2*", Capabilities{Vision: true, Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 262144, MaxOutput: 262144}},
	{"*kimi*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingKimi, ContextWindow: 262144}},

	// ── GLM / Z.ai ──
	{"*glm-5*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingZai, ContextWindow: 200000, MaxOutput: 128000}},
	{"*glm-4.7*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingZai, ContextWindow: 200000, MaxOutput: 128000}},
	{"*glm-4*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingZai, ContextWindow: 200000}},
	{"*glm*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingZai, ContextWindow: 200000}},

	// ── DeepSeek ──
	{"*deepseek-v4*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingDeepseek, ContextWindow: 1000000, MaxOutput: 384000}},
	{"*reasoner*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingDeepseek, ContextWindow: 128000}},
	{"*deepseek-r*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingDeepseek, ContextWindow: 128000}},
	{"*deepseek-chat*", Capabilities{ContextWindow: 128000}},
	{"*deepseek*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingDeepseek, ContextWindow: 128000}},

	// ── MiniMax ──
	{"*minimax*image*", Capabilities{ImageOutput: true}},
	{"*minimax-m3*", Capabilities{Vision: true, Reasoning: true, ThinkingFormat: ThinkingMinimax, ContextWindow: 1048576, MaxOutput: 512000}},
	{"*minimax-m2.7*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingMinimax, ContextWindow: 204800, MaxOutput: 131072}},
	{"*minimax*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingMinimax, ContextWindow: 200000, MaxOutput: 131072}},

	// ── Xiaomi MiMo ──
	{"*mimo*v2.5*", Capabilities{Vision: true, ContextWindow: 1048576, MaxOutput: 131072}},
	{"*mimo*omni*", Capabilities{Vision: true, AudioInput: true, ContextWindow: 262144, MaxOutput: 131072}},
	{"*mimo*", Capabilities{Vision: true, ContextWindow: 262144, MaxOutput: 131072}},

	// ── Llama ──
	{"*llama-4*", Capabilities{Vision: true, ContextWindow: 1000000}},
	{"*llama*", Capabilities{ContextWindow: 128000}},

	// ── Mistral ──
	{"*codestral*", Capabilities{ContextWindow: 256000}},
	{"*mistral-large*", Capabilities{Vision: true, ContextWindow: 256000}},
	{"*mistral*", Capabilities{ContextWindow: 128000}},

	// ── Cohere ──
	{"*command-a-vision*", Capabilities{Vision: true, ContextWindow: 128000}},
	{"*command*", Capabilities{ContextWindow: 128000}},

	// ── Perplexity ──
	{"*sonar*", Capabilities{Search: true, ContextWindow: 128000}},
	{"*pplx*", Capabilities{Search: true, ContextWindow: 128000}},
	{"*perplexity*", Capabilities{Search: true, ContextWindow: 128000}},

	// ── Others ──
	{"*hunyuan*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingHunyuan, ContextWindow: 262144, MaxOutput: 262144}},
	{"hy3*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingHunyuan, ContextWindow: 262144, MaxOutput: 262144}},
	{"*step-*", Capabilities{Reasoning: true, ThinkingFormat: ThinkingStep, ContextWindow: 128000}},
	{"*nemotron*", Capabilities{Reasoning: true, ContextWindow: 128000}},
	{"*ling-*", Capabilities{Reasoning: true, ContextWindow: 128000}},
}

// Models/patterns whose ThinkingCanDisable must be false. The JS caps spell
// this out inline per entry; because a Go bool cannot express "unset", we
// overlay false from this list after merge so the default (true) stays the
// floor for everything else. Entries are glob patterns (same matcher as the
// capability patterns); an exact id works because "*" wildcards still match a
// literal string segment.
var cannotDisablePatterns = []string{
	"*qwq*", "*kimi*k3*", "*kimi*for-coding*", "*kimi*k2.7*code*",
	"*gemini-3*pro*", "*gemini-3*", "*minimax-m2.7*", "*minimax*",
}

// GetCapabilitiesForModel resolves capabilities using the 4-step fallback
// chain, merged over Default so the result is always complete. Mirrors the JS
// getCapabilitiesForModel.
func GetCapabilitiesForModel(provider, model string) Capabilities {
	if model == "" {
		return Default
	}

	// Strip vendor prefix: "anthropic/claude-opus-4.7" -> "claude-opus-4.7".
	baseModel := model
	if i := strings.LastIndex(model, "/"); i >= 0 {
		baseModel = model[i+1:]
	}

	// 1. Provider-specific override.
	if provider != "" {
		if pcaps, ok := providerCapabilities[provider]; ok {
			if c, ok := pcaps[model]; ok {
				return applyCannotDisable(merge(Default, c), model, baseModel)
			}
			if c, ok := pcaps[baseModel]; ok {
				return applyCannotDisable(merge(Default, c), model, baseModel)
			}
		}
	}

	// 2. Canonical exact.
	if c, ok := modelCapabilities[baseModel]; ok {
		return applyCannotDisable(merge(Default, c), model, baseModel)
	}
	if c, ok := modelCapabilities[model]; ok {
		return applyCannotDisable(merge(Default, c), model, baseModel)
	}

	// 3. Pattern match (first match wins).
	for _, e := range patternCapabilities {
		if pricing.MatchPattern(e.pattern, baseModel) || pricing.MatchPattern(e.pattern, model) {
			return applyCannotDisable(merge(Default, e.caps), model, baseModel)
		}
	}

	// 4. Floor.
	return Default
}

// applyCannotDisable overlays ThinkingCanDisable:false when the resolved model
// matches one of the cannotDisablePatterns. It checks both the full model id
// and the vendor-stripped baseModel so "anthropic/claude-..." and bare ids are
// both covered.
func applyCannotDisable(c Capabilities, model, baseModel string) Capabilities {
	for _, p := range cannotDisablePatterns {
		if pricing.MatchPattern(p, baseModel) || pricing.MatchPattern(p, model) {
			c.ThinkingCanDisable = false
			break
		}
	}
	return c
}

// toolsDisabledModels lists exact model ids whose Tools capability must be
// false (overriding the Default true). Because a Go bool has no "unset" state,
// table entries that need Tools:false cannot encode it directly; we honor
// these overrides at merge time. Mirrors the JS entries that spell
// `tools: false` (gpt-image-1).
var toolsDisabledModels = map[string]bool{
	"gpt-image-1": true,
}

// merge overlays non-zero fields of overlay onto base. Boolean true values and
// non-zero ints/thinking-format win; a ThinkingRange pointer wins if non-nil.
// This mirrors the JS {...DEFAULT, ...caps} spread where caps fields override
// defaults. Tools:false overrides (rare, image-only models) are applied via
// toolsDisabledModels since a Go bool cannot express "unset".
func merge(base, overlay Capabilities) Capabilities {
	out := base
	if overlay.Vision {
		out.Vision = true
	}
	if overlay.PDF {
		out.PDF = true
	}
	if overlay.AudioInput {
		out.AudioInput = true
	}
	if overlay.VideoInput {
		out.VideoInput = true
	}
	if overlay.ImageOutput {
		out.ImageOutput = true
	}
	if overlay.AudioOutput {
		out.AudioOutput = true
	}
	if overlay.Search {
		out.Search = true
	}
	if overlay.Reasoning {
		out.Reasoning = true
	}
	if overlay.ThinkingFormat != ThinkingNone {
		out.ThinkingFormat = overlay.ThinkingFormat
	}
	// ThinkingCanDisable is NOT handled here: a Go bool cannot express "unset",
	// so overlay false (the common zero-value for entries that only set
	// ContextWindow/MaxOutput) would wrongly clobber the default true. The
	// false override is applied post-merge by applyCannotDisable, keyed on the
	// resolved model id/pattern (see cannotDisablePatterns).
	if overlay.ThinkingRange != nil {
		out.ThinkingRange = overlay.ThinkingRange
	}
	if overlay.ContextWindow != 0 {
		out.ContextWindow = overlay.ContextWindow
	}
	if overlay.MaxOutput != 0 {
		out.MaxOutput = overlay.MaxOutput
	}
	// Tools:false override (image-only models). The overlay carries Tools:false
	// only for these entries; we detect them by their ImageOutput-only shape.
	if overlay.ImageOutput && !overlay.Tools && !overlay.Reasoning && !overlay.Vision {
		out.Tools = false
	}
	return out
}

// FromServiceKind maps a dashboard service kind (imageToText/stt/etc.) to
// capability deltas, mirroring capabilitiesFromServiceKind.
func FromServiceKind(kind string) *Capabilities {
	switch kind {
	case "imageToText":
		return &Capabilities{Vision: true}
	case "image":
		return &Capabilities{ImageOutput: true}
	case "stt":
		return &Capabilities{AudioInput: true}
	case "tts":
		return &Capabilities{AudioOutput: true}
	case "embedding":
		return &Capabilities{Tools: false}
	}
	return nil
}
