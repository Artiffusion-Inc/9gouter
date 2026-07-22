// Package pricing ports the legacy JS pricing model (open-sse/providers/pricing.js)
// into Go: canonical per-model rates (MODEL_PRICING), provider-specific overrides
// (PROVIDER_PRICING), glob-pattern fallbacks (PATTERN_PRICING), and the
// calculateCostFromTokens formula. All rates are USD per 1M tokens.
//
// This is the hard-coded fallback chain the dashboard and usage accounting rely
// on. User overrides stored in the kv table (repo.PricingRepo) are merged on top
// by the Resolver — see Resolve. The hard-coded tables themselves are not persisted.
//
// Fallback order (first match wins):
//  1. user override (kv) for provider+model
//  2. PROVIDER_PRICING[provider][model]  — hard-coded provider override
//  3. MODEL_PRICING[model]               — canonical model price
//  4. PATTERN_PRICING                     — glob pattern match (e.g. "codex-*")
package pricing

// Rate is the per-1M-token price for one model. Fields mirror the JS pricing
// object: input, output, cached, reasoning, cache_creation. cached/reasoning/
// cache_creation fall back to input/output respectively when zero.
type Rate struct {
	Input          float64 `json:"input"`
	Output         float64 `json:"output"`
	Cached         float64 `json:"cached"`
	Reasoning      float64 `json:"reasoning"`
	CacheCreation  float64 `json:"cache_creation"`
}

// Tokens is the cache-inclusive token breakdown calculateCostFromTokens expects.
// PromptTokens is cache-inclusive (it already contains Cached + CacheCreation as
// subsets); the formula subtracts both so they are not charged at the full input
// rate. Mirrors the legacy canonicalizeUsage convention.
type Tokens struct {
	PromptTokens       int `json:"prompt_tokens"`
	CompletionTokens  int `json:"completion_tokens"`
	CachedTokens       int `json:"cached_tokens"`
	ReasoningTokens    int `json:"reasoning_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

// modelPricing is the canonical, provider-agnostic price table. Verbatim from
// open-sse/providers/pricing.js MODEL_PRICING.
var modelPricing = map[string]Rate{
	// === Anthropic / Claude ===
	"claude-opus-4-6":          {Input: 5.00, Output: 25.00, Cached: 0.50, Reasoning: 25.00, CacheCreation: 6.25},
	"claude-opus-4-5-20251101": {Input: 5.00, Output: 25.00, Cached: 0.50, Reasoning: 25.00, CacheCreation: 6.25},
	"claude-sonnet-4-6":        {Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 15.00, CacheCreation: 3.75},
	"claude-sonnet-4-5-20250929": {Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 15.00, CacheCreation: 3.75},
	"claude-haiku-4-5-20251001":  {Input: 1.00, Output: 5.00, Cached: 0.10, Reasoning: 5.00, CacheCreation: 1.25},
	"claude-sonnet-4-20250514":   {Input: 3.00, Output: 15.00, Cached: 1.50, Reasoning: 15.00, CacheCreation: 3.00},
	"claude-opus-4-20250514":     {Input: 15.00, Output: 25.00, Cached: 7.50, Reasoning: 112.50, CacheCreation: 15.00},
	"claude-3-5-sonnet-20241022": {Input: 3.00, Output: 15.00, Cached: 1.50, Reasoning: 15.00, CacheCreation: 3.00},
	"claude-haiku-4.5":          {Input: 0.50, Output: 2.50, Cached: 0.05, Reasoning: 3.75, CacheCreation: 0.50},
	"claude-opus-4.1":           {Input: 5.00, Output: 25.00, Cached: 0.50, Reasoning: 37.50, CacheCreation: 5.00},
	"claude-opus-4.5":           {Input: 5.00, Output: 25.00, Cached: 0.50, Reasoning: 37.50, CacheCreation: 5.00},
	"claude-opus-4.6":           {Input: 5.00, Output: 25.00, Cached: 0.50, Reasoning: 37.50, CacheCreation: 5.00},
	"claude-sonnet-4":           {Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 22.50, CacheCreation: 3.00},
	"claude-sonnet-4.5":         {Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 22.50, CacheCreation: 3.00},
	"claude-sonnet-4.6":         {Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 22.50, CacheCreation: 3.00},
	"claude-opus-4-5-thinking":  {Input: 5.00, Output: 25.00, Cached: 0.50, Reasoning: 37.50, CacheCreation: 5.00},
	"claude-opus-4-6-thinking": {Input: 5.00, Output: 25.00, Cached: 0.50, Reasoning: 37.50, CacheCreation: 5.00},
	"claude-fable-5":            {Input: 10.00, Output: 50.00, Cached: 1.00, Reasoning: 50.00, CacheCreation: 12.50},

	// === OpenAI / GPT ===
	"gpt-3.5-turbo":   {Input: 0.50, Output: 1.50, Cached: 0.25, Reasoning: 2.25, CacheCreation: 0.50},
	"gpt-4":           {Input: 2.50, Output: 10.00, Cached: 1.25, Reasoning: 15.00, CacheCreation: 2.50},
	"gpt-4-turbo":     {Input: 10.00, Output: 30.00, Cached: 5.00, Reasoning: 45.00, CacheCreation: 10.00},
	"gpt-4o":          {Input: 2.50, Output: 10.00, Cached: 1.25, Reasoning: 15.00, CacheCreation: 2.50},
	"gpt-4o-mini":     {Input: 0.15, Output: 0.60, Cached: 0.075, Reasoning: 0.90, CacheCreation: 0.15},
	"gpt-4.1":         {Input: 2.50, Output: 10.00, Cached: 1.25, Reasoning: 15.00, CacheCreation: 2.50},
	"gpt-5":           {Input: 1.25, Output: 10.00, Cached: 0.625, Reasoning: 10.00, CacheCreation: 1.25},
	"gpt-5-mini":      {Input: 0.25, Output: 2.00, Cached: 0.125, Reasoning: 2.00, CacheCreation: 0.25},
	"gpt-5-codex":     {Input: 1.25, Output: 10.00, Cached: 0.625, Reasoning: 10.00, CacheCreation: 1.25},
	"gpt-5.1":         {Input: 1.25, Output: 10.00, Cached: 0.625, Reasoning: 10.00, CacheCreation: 1.25},
	"gpt-5.1-codex":   {Input: 1.25, Output: 10.00, Cached: 0.625, Reasoning: 10.00, CacheCreation: 1.25},
	"gpt-5.1-codex-mini":      {Input: 1.50, Output: 6.00, Cached: 0.75, Reasoning: 9.00, CacheCreation: 1.50},
	"gpt-5.1-codex-mini-high": {Input: 2.00, Output: 8.00, Cached: 1.00, Reasoning: 12.00, CacheCreation: 2.00},
	"gpt-5.1-codex-max":       {Input: 8.00, Output: 32.00, Cached: 4.00, Reasoning: 48.00, CacheCreation: 8.00},
	"gpt-5.2":                {Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75},
	"gpt-5.2-codex":          {Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75},
	"gpt-5.3-codex":          {Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75},
	"gpt-5.3-codex-spark":    {Input: 3.00, Output: 12.00, Cached: 0.30, Reasoning: 12.00, CacheCreation: 3.00},
	"gpt-5.6":                {Input: 2.50, Output: 15.00, Cached: 0.25, Reasoning: 15.00, CacheCreation: 2.50},
	"gpt-5.6-luna":           {Input: 1.00, Output: 6.00, Cached: 0.10, Reasoning: 6.00, CacheCreation: 1.00},
	"gpt-5.6-terra":          {Input: 2.50, Output: 15.00, Cached: 0.25, Reasoning: 15.00, CacheCreation: 2.50},
	"gpt-5.6-sol":            {Input: 5.00, Output: 30.00, Cached: 0.50, Reasoning: 30.00, CacheCreation: 5.00},
	"o1":                     {Input: 15.00, Output: 60.00, Cached: 7.50, Reasoning: 90.00, CacheCreation: 15.00},
	"o1-mini":                {Input: 3.00, Output: 12.00, Cached: 1.50, Reasoning: 18.00, CacheCreation: 3.00},

	// === Gemini ===
	"gemini-3-flash-preview":     {Input: 0.50, Output: 3.00, Cached: 0.03, Reasoning: 4.50, CacheCreation: 0.50},
	"gemini-3-pro-preview":       {Input: 2.00, Output: 12.00, Cached: 0.25, Reasoning: 18.00, CacheCreation: 2.00},
	"gemini-3.1-pro-low":         {Input: 2.00, Output: 12.00, Cached: 0.25, Reasoning: 18.00, CacheCreation: 2.00},
	"gemini-3.1-pro-high":        {Input: 4.00, Output: 18.00, Cached: 0.50, Reasoning: 27.00, CacheCreation: 4.00},
	"gemini-pro-agent":           {Input: 4.00, Output: 18.00, Cached: 0.50, Reasoning: 27.00, CacheCreation: 4.00},
	"gemini-3-flash-agent":       {Input: 0.50, Output: 3.00, Cached: 0.03, Reasoning: 4.50, CacheCreation: 0.50},
	"gemini-3.5-flash-low":       {Input: 0.50, Output: 3.00, Cached: 0.03, Reasoning: 4.50, CacheCreation: 0.50},
	"gemini-3.5-flash-extra-low": {Input: 0.50, Output: 3.00, Cached: 0.03, Reasoning: 4.50, CacheCreation: 0.50},
	"gemini-3-flash":             {Input: 0.50, Output: 3.00, Cached: 0.03, Reasoning: 4.50, CacheCreation: 0.50},
	"gemini-2.5-pro":              {Input: 2.00, Output: 12.00, Cached: 0.25, Reasoning: 18.00, CacheCreation: 2.00},
	"gemini-2.5-flash":            {Input: 0.30, Output: 2.50, Cached: 0.03, Reasoning: 3.75, CacheCreation: 0.30},
	"gemini-2.5-flash-lite":       {Input: 0.15, Output: 1.25, Cached: 0.015, Reasoning: 1.875, CacheCreation: 0.15},

	// === Qwen ===
	"qwen3-coder-plus":  {Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00},
	"qwen3-coder-flash": {Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50},

	// === Kimi ===
	// Official platform.kimi.ai: cache-hit / cache-miss / output per 1M tokens.
	"kimi-k3":                  {Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 15.00, CacheCreation: 3.00},
	"k3":                       {Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 15.00, CacheCreation: 3.00},
	"kimi-k2.7-code":           {Input: 0.95, Output: 4.00, Cached: 0.19, Reasoning: 4.00, CacheCreation: 0.95},
	"kimi-k2.7-code-highspeed": {Input: 1.90, Output: 8.00, Cached: 0.38, Reasoning: 8.00, CacheCreation: 1.90},
	"kimi-for-coding":          {Input: 0.95, Output: 4.00, Cached: 0.19, Reasoning: 4.00, CacheCreation: 0.95},
	"kimi-for-coding-highspeed": {Input: 1.90, Output: 8.00, Cached: 0.38, Reasoning: 8.00, CacheCreation: 1.90},
	"kimi-k2":                  {Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00},
	"kimi-k2-thinking":         {Input: 1.50, Output: 6.00, Cached: 0.75, Reasoning: 9.00, CacheCreation: 1.50},
	"kimi-k2.5":                {Input: 1.20, Output: 4.80, Cached: 0.60, Reasoning: 7.20, CacheCreation: 1.20},
	"kimi-k2.5-thinking":       {Input: 1.80, Output: 7.20, Cached: 0.90, Reasoning: 10.80, CacheCreation: 1.80},
	"kimi-k2.6":                {Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00},
	"kimi-latest":              {Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00},

	// === DeepSeek ===
	"deepseek-chat":       {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-reasoner":   {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-r1":         {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-v3.2-chat":  {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-v3.2-reasoner": {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-v4-flash":     {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-v4-pro":       {Input: 0.435, Output: 0.87, Cached: 0.003625, Reasoning: 0.87, CacheCreation: 0.435},

	// === GLM ===
	"glm-4.6":  {Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50},
	"glm-4.6v": {Input: 0.75, Output: 3.00, Cached: 0.375, Reasoning: 4.50, CacheCreation: 0.75},
	"glm-4.7":  {Input: 0.75, Output: 3.00, Cached: 0.375, Reasoning: 4.50, CacheCreation: 0.75},
	"glm-5":    {Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00},

	// === MiniMax ===
	"MiniMax-M3":   {Input: 0.30, Output: 1.20, Cached: 0.06, Reasoning: 1.80, CacheCreation: 0.30},
	"MiniMax-M2.1": {Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50},
	"MiniMax-M2.5": {Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50},
	"MiniMax-M2.7": {Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50},
	"minimax-m2.1": {Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50},
	"minimax-m2.5": {Input: 0.60, Output: 2.40, Cached: 0.30, Reasoning: 3.60, CacheCreation: 0.60},

	// === Grok ===
	"grok-code-fast-1": {Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50},

	// === OpenRouter fallback ===
	"auto": {Input: 2.00, Output: 8.00, Cached: 1.00, Reasoning: 12.00, CacheCreation: 2.00},

	// === Misc ===
	"oswe-vscode-prime":   {Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00},
	"gpt-oss-120b-medium": {Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50},
	"vision-model":        {Input: 1.50, Output: 6.00, Cached: 0.75, Reasoning: 9.00, CacheCreation: 1.50},
	"coder-model":         {Input: 1.50, Output: 6.00, Cached: 0.75, Reasoning: 9.00, CacheCreation: 1.50},
}

// providerPricing holds hard-coded provider-specific overrides. Only entries
// where price differs from modelPricing. Keyed by provider alias or provider id.
var providerPricing = map[string]map[string]Rate{
	// GitHub Copilot (gh) — explicit override, matches canonical gpt-5.3-codex rate.
	"gh": {
		"gpt-5.3-codex": {Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75},
	},
}

// patternPricing is the glob fallback, first match wins (order matters).
type patternEntry struct {
	pattern string
	rate    Rate
}

var patternPricing = []patternEntry{
	// --- Codex variants ---
	patternEntry{pattern: "*-codex-xhigh", rate: Rate{Input: 10.00, Output: 40.00, Cached: 5.00, Reasoning: 60.00, CacheCreation: 10.00}},
	patternEntry{pattern: "*-codex-high", rate: Rate{Input: 8.00, Output: 32.00, Cached: 4.00, Reasoning: 48.00, CacheCreation: 8.00}},
	patternEntry{pattern: "*-codex-max", rate: Rate{Input: 8.00, Output: 32.00, Cached: 4.00, Reasoning: 48.00, CacheCreation: 8.00}},
	patternEntry{pattern: "*-codex-mini-*", rate: Rate{Input: 1.50, Output: 6.00, Cached: 0.75, Reasoning: 9.00, CacheCreation: 1.50}},
	patternEntry{pattern: "*-codex-mini", rate: Rate{Input: 1.50, Output: 6.00, Cached: 0.75, Reasoning: 9.00, CacheCreation: 1.50}},
	patternEntry{pattern: "*-codex-low", rate: Rate{Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75}},
	patternEntry{pattern: "*-codex-none", rate: Rate{Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75}},
	patternEntry{pattern: "*-codex-spark", rate: Rate{Input: 3.00, Output: 12.00, Cached: 0.30, Reasoning: 12.00, CacheCreation: 3.00}},
	patternEntry{pattern: "codex-*", rate: Rate{Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75}},
	patternEntry{pattern: "*-codex", rate: Rate{Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75}},

	// --- Claude ---
	patternEntry{pattern: "claude-opus-*", rate: Rate{Input: 5.00, Output: 25.00, Cached: 0.50, Reasoning: 25.00, CacheCreation: 6.25}},
	patternEntry{pattern: "claude-sonnet-*", rate: Rate{Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 15.00, CacheCreation: 3.75}},
	patternEntry{pattern: "claude-haiku-*", rate: Rate{Input: 1.00, Output: 5.00, Cached: 0.10, Reasoning: 5.00, CacheCreation: 1.25}},
	patternEntry{pattern: "claude-*", rate: Rate{Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 15.00, CacheCreation: 3.75}},

	// --- Gemini (specific first, generic last) ---
	patternEntry{pattern: "gemini-*-flash-lite", rate: Rate{Input: 0.15, Output: 1.25, Cached: 0.015, Reasoning: 1.875, CacheCreation: 0.15}},
	patternEntry{pattern: "gemini-*-flash", rate: Rate{Input: 0.30, Output: 2.50, Cached: 0.03, Reasoning: 3.75, CacheCreation: 0.30}},
	patternEntry{pattern: "gemini-*-pro", rate: Rate{Input: 2.00, Output: 12.00, Cached: 0.25, Reasoning: 18.00, CacheCreation: 2.00}},
	patternEntry{pattern: "gemini-3-*", rate: Rate{Input: 0.50, Output: 3.00, Cached: 0.03, Reasoning: 4.50, CacheCreation: 0.50}},
	patternEntry{pattern: "gemini-2.5-*", rate: Rate{Input: 0.30, Output: 2.50, Cached: 0.03, Reasoning: 3.75, CacheCreation: 0.30}},
	patternEntry{pattern: "gemini-*", rate: Rate{Input: 0.50, Output: 3.00, Cached: 0.03, Reasoning: 4.50, CacheCreation: 0.50}},

	// --- GPT (specific first, generic last) ---
	patternEntry{pattern: "gpt-5.6-*", rate: Rate{Input: 2.50, Output: 15.00, Cached: 0.25, Reasoning: 15.00, CacheCreation: 2.50}},
	patternEntry{pattern: "gpt-5.3-*", rate: Rate{Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75}},
	patternEntry{pattern: "gpt-5.2-*", rate: Rate{Input: 1.75, Output: 14.00, Cached: 0.175, Reasoning: 14.00, CacheCreation: 1.75}},
	patternEntry{pattern: "gpt-5.1-*", rate: Rate{Input: 1.25, Output: 10.00, Cached: 0.625, Reasoning: 10.00, CacheCreation: 1.25}},
	patternEntry{pattern: "gpt-5-*", rate: Rate{Input: 1.25, Output: 10.00, Cached: 0.625, Reasoning: 10.00, CacheCreation: 1.25}},
	patternEntry{pattern: "gpt-5*", rate: Rate{Input: 1.25, Output: 10.00, Cached: 0.625, Reasoning: 10.00, CacheCreation: 1.25}},
	patternEntry{pattern: "gpt-4o-*", rate: Rate{Input: 0.15, Output: 0.60, Cached: 0.075, Reasoning: 0.90, CacheCreation: 0.15}},
	patternEntry{pattern: "gpt-4o", rate: Rate{Input: 2.50, Output: 10.00, Cached: 1.25, Reasoning: 15.00, CacheCreation: 2.50}},
	patternEntry{pattern: "gpt-4*", rate: Rate{Input: 2.50, Output: 10.00, Cached: 1.25, Reasoning: 15.00, CacheCreation: 2.50}},

	// --- o1 / o-series ---
	patternEntry{pattern: "o1-*", rate: Rate{Input: 3.00, Output: 12.00, Cached: 1.50, Reasoning: 18.00, CacheCreation: 3.00}},
	patternEntry{pattern: "o1", rate: Rate{Input: 15.00, Output: 60.00, Cached: 7.50, Reasoning: 90.00, CacheCreation: 15.00}},
	patternEntry{pattern: "o3-*", rate: Rate{Input: 10.00, Output: 40.00, Cached: 5.00, Reasoning: 60.00, CacheCreation: 10.00}},
	patternEntry{pattern: "o4-*", rate: Rate{Input: 2.00, Output: 8.00, Cached: 1.00, Reasoning: 12.00, CacheCreation: 2.00}},

	// --- Qwen ---
	patternEntry{pattern: "qwen3-coder-*", rate: Rate{Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00}},
	patternEntry{pattern: "qwen*-coder-*", rate: Rate{Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00}},
	patternEntry{pattern: "qwen*", rate: Rate{Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50}},

	// --- Kimi ---
	patternEntry{pattern: "kimi-*-thinking", rate: Rate{Input: 1.80, Output: 7.20, Cached: 0.90, Reasoning: 10.80, CacheCreation: 1.80}},
	patternEntry{pattern: "kimi-k3*", rate: Rate{Input: 3.00, Output: 15.00, Cached: 0.30, Reasoning: 15.00, CacheCreation: 3.00}},
	patternEntry{pattern: "kimi-k2*", rate: Rate{Input: 1.20, Output: 4.80, Cached: 0.60, Reasoning: 7.20, CacheCreation: 1.20}},
	patternEntry{pattern: "kimi-*", rate: Rate{Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00}},

	// --- DeepSeek ---
	patternEntry{pattern: "deepseek-*reasoner*", rate: Rate{Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14}},
	patternEntry{pattern: "deepseek-r*", rate: Rate{Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14}},
	patternEntry{pattern: "deepseek-v*", rate: Rate{Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14}},
	patternEntry{pattern: "deepseek-*", rate: Rate{Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14}},

	// --- GLM ---
	patternEntry{pattern: "glm-5*", rate: Rate{Input: 1.00, Output: 4.00, Cached: 0.50, Reasoning: 6.00, CacheCreation: 1.00}},
	patternEntry{pattern: "glm-4*", rate: Rate{Input: 0.75, Output: 3.00, Cached: 0.375, Reasoning: 4.50, CacheCreation: 0.75}},
	patternEntry{pattern: "glm-*", rate: Rate{Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50}},

	// --- MiniMax ---
	patternEntry{pattern: "MiniMax-*", rate: Rate{Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50}},
	patternEntry{pattern: "minimax-*", rate: Rate{Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50}},

	// --- Grok ---
	patternEntry{pattern: "grok-code-*", rate: Rate{Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50}},
	patternEntry{pattern: "grok-*", rate: Rate{Input: 0.50, Output: 2.00, Cached: 0.25, Reasoning: 3.00, CacheCreation: 0.50}},
}