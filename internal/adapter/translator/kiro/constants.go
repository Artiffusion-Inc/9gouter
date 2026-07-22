package kiro

// constants.go ports open-sse/config/kiroConstants.js (upstream v0.5.40) — the
// Kiro thinking/effort infrastructure that the simplified request translator
// previously omitted. This includes the GPT-5.6 native reasoning-effort path
// (upstream commits cef5dd4d / eb00222c): resolveKiroEffortPath returns
// "reasoning" for gpt-5.6* models so buildKiroAdditionalModelRequestFields emits
// {reasoning:{effort}} instead of the Claude {thinking,output_config} shape, and
// usesKiroNativeGptEffort suppresses the legacy <thinking_mode> prefix for those
// models so Kiro receives the native field rather than a prompt tag.
//
// The shared extractThinking parser is ported scoped to the shapes a Kiro
// request translator can actually see post-translation (Claude output_config /
// thinking, OpenAI reasoning_effort / reasoning.effort) plus the header / tag /
// model-suffix detectors — the Gemini/Qwen branches of the upstream parser are
// intentionally not ported (Kiro never receives those body shapes).

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// KIRO_THINKING_BUDGET_DEFAULT is the budget injected when a client requests
// thinking without an explicit budget (level / tag / suffix / header).
const KIRO_THINKING_BUDGET_DEFAULT = 16000

// effortToBudgetMap maps an effort level to a token budget. Mirrors upstream
// LEVEL_TO_BUDGET. "none" = no thinking (0); absent key = unrecognized effort.
var effortToBudgetMap = map[string]int{
	"none":    0,
	"minimal": 512,
	"low":     1024,
	"medium":  8192,
	"high":    24576,
	"xhigh":   32768,
	"max":     128000,
}

func effortToBudget(effort string) (int, bool) {
	b, ok := effortToBudgetMap[strings.ToLower(effort)]
	return b, ok
}

// thinkingCfg is the unified thinking intent extracted from a request body.
type thinkingCfg struct {
	mode   string // "none" | "auto" | "budget" | "level"
	budget int
	level  string
}

// extractThinking parses unified thinking intent from a post-translation body.
// Returns nil when no thinking intent is present. Scoped to Claude + OpenAI
// shapes (the only ones a Kiro request translator sees); the upstream parser
// also covers Gemini/Qwen, omitted here intentionally.
func extractThinking(body map[string]any) *thinkingCfg {
	if body == nil {
		return nil
	}

	// Claude output_config.effort (explicit) — priority over adaptive thinking.
	if oc, ok := nestedString(body, "output_config", "effort"); ok && oc != "" {
		e := strings.ToLower(oc)
		switch e {
		case "none", "off":
			return &thinkingCfg{mode: "none"}
		case "auto":
			return &thinkingCfg{mode: "auto"}
		default:
			return &thinkingCfg{mode: "level", level: e}
		}
	}

	// Claude shape: body.thinking.{type,budget_tokens}.
	if t, ok := body["thinking"].(map[string]any); ok {
		switch tt, _ := t["type"].(string); tt {
		case "disabled":
			return &thinkingCfg{mode: "none"}
		case "adaptive", "enabled":
			if b, ok := toInt(t["budget_tokens"]); ok && b > 0 {
				return &thinkingCfg{mode: "budget", budget: b}
			}
			return &thinkingCfg{mode: "auto"}
		}
	}

	// OpenAI chat / Responses shape: reasoning_effort or reasoning.effort.
	effort, ok := firstString(body, "reasoning_effort")
	if !ok {
		if r, ok := body["reasoning"].(map[string]any); ok {
			if e, ok := r["effort"].(string); ok {
				effort = e
			}
		}
	}
	if effort != "" {
		e := strings.ToLower(effort)
		switch e {
		case "none", "off":
			return &thinkingCfg{mode: "none"}
		case "auto":
			return &thinkingCfg{mode: "auto"}
		default:
			return &thinkingCfg{mode: "level", level: e}
		}
	}

	return nil
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	}
	return 0, false
}

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func nestedString(m map[string]any, keys ...string) (string, bool) {
	cur := m
	for i, k := range keys {
		if i == len(keys)-1 {
			if v, ok := cur[k].(string); ok {
				return v, true
			}
			return "", false
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return "", false
		}
		cur = next
	}
	return "", false
}

// resolveKiroThinkingBudget resolves the Kiro thinking budget a client asked for.
// Returns -1 (sentinel "no prefix") when thinking is disabled or absent, else a
// budget number to inject via buildThinkingSystemPrefix (which clamps 1..32000).
func resolveKiroThinkingBudget(body map[string]any, header http.Header, model string) int {
	const noBudget = -1
	if cfg := extractThinking(body); cfg != nil {
		switch cfg.mode {
		case "none":
			return noBudget
		case "level":
			if cfg.level == "disabled" {
				return noBudget
			}
			if b, ok := effortToBudget(cfg.level); ok {
				return b
			}
			return KIRO_THINKING_BUDGET_DEFAULT
		case "budget":
			return cfg.budget
		default:
			return KIRO_THINKING_BUDGET_DEFAULT
		}
	}

	if header != nil {
		if beta := header.Get("anthropic-beta"); strings.Contains(strings.ToLower(beta), "interleaved-thinking") {
			return KIRO_THINKING_BUDGET_DEFAULT
		}
	}

	if containsThinkingModeTag(body) {
		return KIRO_THINKING_BUDGET_DEFAULT
	}

	if model != "" {
		m := strings.ToLower(model)
		if strings.Contains(m, "thinking") || strings.Contains(m, "-reason") {
			return KIRO_THINKING_BUDGET_DEFAULT
		}
	}
	return noBudget
}

// buildThinkingSystemPrefix builds the magic system-prompt prefix that turns
// Kiro reasoning on, clamping the budget to 1..32000.
func buildThinkingSystemPrefix(budget int) string {
	if budget <= 0 {
		budget = KIRO_THINKING_BUDGET_DEFAULT
	}
	if budget > 32000 {
		budget = 32000
	}
	return "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>" + strconv.Itoa(budget) + "</max_thinking_length>"
}

// extractKiroEffortLevel extracts the Claude-style effort level a client asked
// for. "none"/"off"/"disabled" → "" (no field); "max"/"xhigh" → "high".
func extractKiroEffortLevel(body map[string]any) string {
	effort, ok := effortSource(body)
	if !ok {
		return ""
	}
	e := strings.ToLower(effort)
	switch e {
	case "none", "off", "disabled":
		return ""
	case "xhigh", "max":
		return "high"
	case "low", "medium", "high":
		return e
	}
	return ""
}

// extractKiroGptEffortLevel extracts the GPT-style effort level: unlike the
// Claude path, "max" maps to "xhigh" and "xhigh" is a valid wire value (Kiro
// CLI/KAS accepts low/medium/high/xhigh for GPT). "none" is omitted (Kiro CLI
// does not advertise an explicit GPT "none" wire value).
func extractKiroGptEffortLevel(body map[string]any) string {
	effort, ok := effortSource(body)
	if !ok {
		return ""
	}
	e := strings.ToLower(effort)
	switch e {
	case "max":
		return "xhigh"
	case "low", "medium", "high", "xhigh":
		return e
	}
	return ""
}

// effortSource resolves the effort string from any of the three shapes a Kiro
// request can carry it in: output_config.effort, reasoning_effort, reasoning.effort.
func effortSource(body map[string]any) (string, bool) {
	if e, ok := nestedString(body, "output_config", "effort"); ok {
		return e, true
	}
	if e, ok := firstString(body, "reasoning_effort"); ok {
		return e, true
	}
	if r, ok := body["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"].(string); ok {
			return e, true
		}
	}
	return "", false
}

// effortPath is the schema a model expects its effort field under.
type effortPath string

const (
	effortPathOutputConfig effortPath = "output_config" // Claude/Kiro adaptive
	effortPathReasoning    effortPath = "reasoning"     // GPT-5.6 native
)

// buildKiroAdditionalModelRequestFields builds the additionalModelRequestFields
// payload for the resolved effort path. Returns nil when no effort is set.
func buildKiroAdditionalModelRequestFields(body map[string]any, path effortPath) map[string]any {
	var effort string
	if path == effortPathReasoning {
		effort = extractKiroGptEffortLevel(body)
	} else {
		effort = extractKiroEffortLevel(body)
	}
	if effort == "" {
		return nil
	}
	if path == effortPathReasoning {
		// Mirrors Kiro CLI/KAS buildEffortRequestFields("reasoning") for GPT.
		return map[string]any{"reasoning": map[string]any{"effort": effort}}
	}
	// Mirrors Kiro CLI/KAS buildEffortRequestFields("output_config").
	return map[string]any{
		"thinking":      map[string]any{"type": "adaptive", "display": "summarized"},
		"output_config": map[string]any{"effort": effort},
	}
}

var (
	gpt56Re = regexp.MustCompile(`(?:^|[/.])gpt[/.]5[/.]6(?:[/.]|$)`)
	// claudeVersionRe matches "(^|[/.])claude([/.][a-z]+)*/<major>(/<minor>)?(.|$)"
	// after normalizing "-" → ".". Used to gate additionalModelRequestFields on
	// Claude 4.6+ (legacy 4.5 rejected it in live smoke).
	claudeVersionRe = regexp.MustCompile(`(?:^|[/.])claude(?:[/.][a-z]+)*[/.](\d+)(?:[/.](\d+))?(?:[/.]|$)`)
)

// resolveKiroEffortPath returns the schema a model expects its effort field
// under: "reasoning" for gpt-5.6*, "output_config" for supported Claude (4.6+),
// or "" (none) when the model does not accept additionalModelRequestFields.
func resolveKiroEffortPath(model string) effortPath {
	if model == "" {
		return ""
	}
	normalized := strings.ReplaceAll(strings.ToLower(model), "-", ".")
	if gpt56Re.MatchString(normalized) {
		return effortPathReasoning
	}
	if !strings.Contains(normalized, "claude") {
		return ""
	}
	m := claudeVersionRe.FindStringSubmatch(normalized)
	if m == nil {
		return ""
	}
	major := atoiSafe(m[1])
	minor := -1
	if m[2] != "" {
		minor = atoiSafe(m[2])
	}
	dateSuffixMinor := minor >= 1000
	// Kiro rejected additionalModelRequestFields on legacy 4.5 in live smoke.
	// Default future Claude/Kiro models to supported so new releases do not need
	// a code allowlist update.
	if major < 4 || (major == 4 && (minor == -1 || minor <= 5 || dateSuffixMinor)) {
		return ""
	}
	return effortPathOutputConfig
}

// supportsKiroAdditionalModelRequestFields reports whether the model accepts
// additionalModelRequestFields at all.
func supportsKiroAdditionalModelRequestFields(model string) bool {
	return resolveKiroEffortPath(model) != ""
}

// usesKiroNativeGptEffort reports whether this request will be served by the
// native GPT reasoning-effort path (model is gpt-5.6* AND an effort is present).
// When true the request translator must NOT inject the <thinking_mode> prefix
// — Kiro consumes the native reasoning field instead.
func usesKiroNativeGptEffort(body map[string]any, model string) bool {
	return resolveKiroEffortPath(model) == effortPathReasoning && extractKiroGptEffortLevel(body) != ""
}

// buildKiroAdditionalModelRequestFieldsForModel resolves effort fields for the
// given model. Returns nil when the model does not support them or no effort is set.
func buildKiroAdditionalModelRequestFieldsForModel(body map[string]any, model string) map[string]any {
	path := resolveKiroEffortPath(model)
	if path == "" {
		return nil
	}
	return buildKiroAdditionalModelRequestFields(body, path)
}

// --- model suffix resolution (9router synthetic -agentic / -thinking) ---

const (
	kiroAgenticSuffix  = "-agentic"
	kiroThinkingSuffix = "-thinking"
)

// kiroModelResolution is the result of stripping synthetic suffixes.
type kiroModelResolution struct {
	upstream string
	agentic  bool
	thinking bool
}

// resolveKiroModel strips the synthetic -agentic / -thinking suffixes a 9router
// caller may have appended, returning the real upstream model id and flags.
func resolveKiroModel(model string) kiroModelResolution {
	upstream := model
	agentic := strings.HasSuffix(upstream, kiroAgenticSuffix)
	if agentic {
		upstream = strings.TrimSuffix(upstream, kiroAgenticSuffix)
	}
	thinking := strings.HasSuffix(upstream, kiroThinkingSuffix)
	if thinking {
		upstream = strings.TrimSuffix(upstream, kiroThinkingSuffix)
	}
	return kiroModelResolution{upstream: upstream, agentic: agentic, thinking: thinking}
}

// --- <thinking_mode> tag detection in inbound content ---

func containsThinkingModeTag(body map[string]any) bool {
	if body == nil {
		return false
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role != "system" && role != "user" {
				continue
			}
			switch c := msg["content"].(type) {
			case string:
				if containsTagInText(c) {
					return true
				}
			case []any:
				for _, part := range c {
					if pm, ok := part.(map[string]any); ok {
						if t, ok := pm["text"].(string); ok && containsTagInText(t) {
							return true
						}
					}
				}
			}
		}
	}
	if s, ok := body["system"].(string); ok && containsTagInText(s) {
		return true
	}
	return false
}

func containsTagInText(text string) bool {
	if !strings.Contains(text, "<thinking_mode>") {
		return false
	}
	return strings.Contains(text, "<thinking_mode>enabled</thinking_mode>") ||
		strings.Contains(text, "<thinking_mode>interleaved</thinking_mode>")
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}