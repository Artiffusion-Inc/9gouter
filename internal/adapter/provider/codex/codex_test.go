package codexec

import (
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// codex_test.go ports the regression coverage for decolua/9router #2452
// (0c55d49a) part A: service_tier=fast → priority + drop unsupported tiers, and
// reasoning.effort max → xhigh normalization in the existing-reasoning branch.
// Tests drive the real Executor.TransformRequest over the raw request body
// (no mock).

func newTestExecutor() *Executor {
	return New(base.Config{})
}

func transformBody(t *testing.T, e *Executor, model string, body string) map[string]any {
	t.Helper()
	out, err := e.TransformRequest(model, json.RawMessage(body), true, provider.Credentials{})
	if err != nil {
		t.Fatalf("TransformRequest error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal transformed body: %v (body=%s)", err, string(out))
	}
	return m
}

func TestNormalizeReasoningEffort(t *testing.T) {
	cases := map[string]string{
		"max":    "xhigh",
		"xhigh":  "xhigh",
		"high":   "high",
		"medium": "medium",
		"low":    "low",
		"":       "",
		"bogus":  "bogus",
	}
	for in, want := range cases {
		if got := normalizeReasoningEffort(in); got != want {
			t.Errorf("normalizeReasoningEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestServiceTierFastMapsToPriority verifies service_tier=fast is rewritten to
// priority (0c55d49a).
func TestServiceTierFastMapsToPriority(t *testing.T) {
	e := newTestExecutor()
	m := transformBody(t, e, "gpt-5.3-codex", `{"input":"hi","service_tier":"fast"}`)
	if got, _ := m["service_tier"].(string); got != "priority" {
		t.Fatalf("service_tier = %q, want priority", got)
	}
}

// TestServiceTierPriorityPreserved verifies an explicit priority tier is kept.
func TestServiceTierPriorityPreserved(t *testing.T) {
	e := newTestExecutor()
	m := transformBody(t, e, "gpt-5.3-codex", `{"input":"hi","service_tier":"priority"}`)
	if got, _ := m["service_tier"].(string); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (preserved)", got)
	}
}

// TestServiceTierUnsupportedDropped verifies non-fast, non-priority tiers are
// stripped so Codex does not 400 with routing_unsupported.
func TestServiceTierUnsupportedDropped(t *testing.T) {
	e := newTestExecutor()
	for _, tier := range []string{"auto", "scale", "bogus", "default"} {
		m := transformBody(t, e, "gpt-5.3-codex", `{"input":"hi","service_tier":"`+tier+`"}`)
		if _, has := m["service_tier"]; has {
			t.Errorf("service_tier=%q should be dropped, got %v", tier, m["service_tier"])
		}
	}
}

// TestServiceTierAbsentStaysAbsent verifies no service_tier key is invented.
func TestServiceTierAbsentStaysAbsent(t *testing.T) {
	e := newTestExecutor()
	m := transformBody(t, e, "gpt-5.3-codex", `{"input":"hi"}`)
	if _, has := m["service_tier"]; has {
		t.Errorf("service_tier should be absent, got %v", m["service_tier"])
	}
}

// TestReasoningEffortMaxNormalizedInExistingBranch verifies an explicit
// reasoning.effort=max on an existing reasoning object is clamped to xhigh.
// Before 0c55d49a the else branch left effort untouched and forwarded max.
func TestReasoningEffortMaxNormalizedInExistingBranch(t *testing.T) {
	e := newTestExecutor()
	m := transformBody(t, e, "gpt-5.3-codex", `{"input":"hi","reasoning":{"effort":"max"}}`)
	r, ok := m["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("reasoning object dropped")
	}
	if got, _ := r["effort"].(string); got != "xhigh" {
		t.Errorf("reasoning.effort = %q, want xhigh (max normalized)", got)
	}
	if got, _ := r["summary"].(string); got != "auto" {
		t.Errorf("reasoning.summary = %q, want auto (defaulted)", got)
	}
}

// TestReasoningEffortHighPassthrough verifies a non-max effort is unchanged.
func TestReasoningEffortHighPassthrough(t *testing.T) {
	e := newTestExecutor()
	m := transformBody(t, e, "gpt-5.3-codex", `{"input":"hi","reasoning":{"effort":"high"}}`)
	r, _ := m["reasoning"].(map[string]any)
	if got, _ := r["effort"].(string); got != "high" {
		t.Errorf("reasoning.effort = %q, want high (passthrough)", got)
	}
}

// TestReasoningEffortMissingFallsBackToModelSuffix verifies the existing-
// reasoning branch with a missing effort falls back to the model suffix effort
// (default low), not left empty.
func TestReasoningEffortMissingFallsBackToModelSuffix(t *testing.T) {
	e := newTestExecutor()
	// model suffix -high → effort high used as fallback when reasoning.effort absent.
	m := transformBody(t, e, "gpt-5.3-codex-high", `{"input":"hi","reasoning":{"summary":"auto"}}`)
	r, ok := m["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("reasoning object dropped")
	}
	if got, _ := r["effort"].(string); got != "high" {
		t.Errorf("reasoning.effort = %q, want high (model-suffix fallback)", got)
	}
}

// TestReasoningAbsentDefaultsToLow verifies the no-reasoning branch defaults
// effort to low (no model suffix) and max in reasoning_effort param is normalized.
func TestReasoningAbsentDefaultsToLow(t *testing.T) {
	e := newTestExecutor()
	m := transformBody(t, e, "gpt-5.3-codex", `{"input":"hi"}`)
	r, _ := m["reasoning"].(map[string]any)
	if got, _ := r["effort"].(string); got != "low" {
		t.Errorf("reasoning.effort = %q, want low (default)", got)
	}
}
