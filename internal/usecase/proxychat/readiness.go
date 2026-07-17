// Package proxychat implements the /v1 chat pipeline.
// readiness.go ports open-sse/utils/streamReadinessPolicy.js adaptive
// stream-readiness timeout policy into Go.
package proxychat

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

const (
	defaultMaxTimeoutMs        = 15 * 60 * 1000
	largeItemThreshold         = 150
	veryLargeItemThreshold     = 400
	toolHeavyThreshold         = 15
	largeCharThreshold         = 250_000
	veryLargeCharThreshold     = 750_000
	largeHistoryBumpMs         = 20_000
	veryLargeHistoryBumpMs     = 45_000
	toolHeavyBumpMs            = 15_000
	largePayloadBumpMs         = 20_000
	veryLargePayloadBumpMs     = 45_000
	codexGpt5xHighReasoningBump = 30_000
)

var gpt5xRe = regexp.MustCompile(`(?i)gpt-5(\.\d+)?`)

// ReadinessResult is the resolved stream-readiness/stall timeout plus reasons.
type ReadinessResult struct {
	TimeoutMs    time.Duration
	BaseTimeoutMs time.Duration
	Reasons      []string
}

// ResolveStreamReadinessTimeout resolves the effective stall/readiness timeout
// for a request. It only ever INCREASES the base timeout, never decreases, and
// clamps the result to maxTimeoutMs. A baseTimeoutMs <= 0 disables the policy.
func ResolveStreamReadinessTimeout(baseTimeoutMs, maxTimeoutMs time.Duration, provider, model string, body json.RawMessage) ReadinessResult {
	base := baseTimeoutMs
	if base <= 0 {
		return ReadinessResult{TimeoutMs: base, BaseTimeoutMs: base, Reasons: []string{"disabled"}}
	}
	max := maxTimeoutMs
	if max <= 0 {
		max = defaultMaxTimeoutMs * time.Millisecond
	}
	if max < base {
		max = base
	}

	var reasons []string
	timeout := base

	var bodyMap map[string]any
	_ = json.Unmarshal(body, &bodyMap)

	itemCount := maxInt(countArrayField(bodyMap, "input"), countArrayField(bodyMap, "messages"))
	toolCount := countArrayField(bodyMap, "tools")
	estimatedChars := estimateBodyChars(body)
	codexGpt5x := isCodexGpt5x(provider, model)
	codexHighReasoning := codexGpt5x && isHighReasoningEffort(model, bodyMap)

	if itemCount > veryLargeItemThreshold {
		timeout += veryLargeHistoryBumpMs * time.Millisecond
		reasons = append(reasons, "very_large_history")
	} else if itemCount > largeItemThreshold {
		timeout += largeHistoryBumpMs * time.Millisecond
		reasons = append(reasons, "large_history")
	}

	if toolCount >= toolHeavyThreshold {
		timeout += toolHeavyBumpMs * time.Millisecond
		reasons = append(reasons, "tool_heavy")
	}

	if estimatedChars > veryLargeCharThreshold {
		timeout += veryLargePayloadBumpMs * time.Millisecond
		reasons = append(reasons, "very_large_payload")
	} else if estimatedChars > largeCharThreshold {
		timeout += largePayloadBumpMs * time.Millisecond
		reasons = append(reasons, "large_payload")
	}

	if codexHighReasoning {
		timeout += codexGpt5xHighReasoningBump * time.Millisecond
		reasons = append(reasons, "codex_gpt_5_5_high_reasoning")
	} else if codexGpt5x && (itemCount > largeItemThreshold || toolCount >= toolHeavyThreshold) {
		timeout += codexGpt5xHighReasoningBump * time.Millisecond
		reasons = append(reasons, "codex_gpt_5_5_large_responses")
	}

	if timeout > max {
		timeout = max
	}
	if timeout == base {
		reasons = append(reasons, "base")
	}

	return ReadinessResult{TimeoutMs: timeout, BaseTimeoutMs: base, Reasons: reasons}
}

func countArrayField(body map[string]any, field string) int {
	v, ok := body[field]
	if !ok {
		return 0
	}
	arr, ok := v.([]any)
	if !ok {
		return 0
	}
	return len(arr)
}

func estimateBodyChars(body json.RawMessage) int {
	return len(body)
}

func isCodexGpt5x(provider, model string) bool {
	p := strings.ToLower(provider)
	m := strings.ToLower(model)
	return p == "codex" && gpt5xRe.MatchString(m)
}

func isHighReasoningEffort(model string, body map[string]any) bool {
	m := strings.ToLower(model)
	if strings.HasSuffix(m, "-high") {
		return true
	}

	if direct, ok := body["reasoning_effort"].(string); ok {
		return strings.ToLower(direct) == "high"
	}

	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if nested, ok := reasoning["effort"].(string); ok {
			return strings.ToLower(nested) == "high"
		}
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
