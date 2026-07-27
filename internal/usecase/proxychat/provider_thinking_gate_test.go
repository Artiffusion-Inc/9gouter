package proxychat

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

type fakePTGateRepo struct {
	data  []byte
	err   error
	calls atomic.Int64
}

func (f *fakePTGateRepo) Get(ctx context.Context) (*settings.Settings, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &settings.Settings{Data: f.data}, nil
}

func ptBlob(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestProviderThinkingGate_NoOverrideWhenMissing(t *testing.T) {
	g := &providerThinkingGate{settings: &fakePTGateRepo{data: ptBlob(t, map[string]any{})}}
	if mode := g.Mode(context.Background(), "openai"); mode != "" {
		t.Fatalf("missing providerThinking should yield empty mode, got %q", mode)
	}
}

func TestProviderThinkingGate_ReadsModePerProvider(t *testing.T) {
	g := &providerThinkingGate{settings: &fakePTGateRepo{data: ptBlob(t, map[string]any{
		"providerThinking": map[string]any{
			"openai":    map[string]any{"mode": "high"},
			"anthropic": map[string]any{"mode": "off"},
			"gemini":    map[string]any{"mode": "auto"},
		},
	})}}
	if mode := g.Mode(context.Background(), "openai"); mode != "high" {
		t.Fatalf("openai mode = %q, want high", mode)
	}
	if mode := g.Mode(context.Background(), "anthropic"); mode != "off" {
		t.Fatalf("anthropic mode = %q, want off", mode)
	}
	// "auto" is treated as "no override" → empty.
	if mode := g.Mode(context.Background(), "gemini"); mode != "" {
		t.Fatalf("gemini auto should yield empty (no inject), got %q", mode)
	}
	// Unknown provider → empty.
	if mode := g.Mode(context.Background(), "unknown"); mode != "" {
		t.Fatalf("unknown provider should yield empty, got %q", mode)
	}
}

func TestProviderThinkingGate_CachesWithinTTL(t *testing.T) {
	repo := &fakePTGateRepo{data: ptBlob(t, map[string]any{
		"providerThinking": map[string]any{"openai": map[string]any{"mode": "high"}},
	})}
	g := &providerThinkingGate{settings: repo}
	_ = g.Mode(context.Background(), "openai")
	_ = g.Mode(context.Background(), "openai")
	_ = g.Mode(context.Background(), "openai")
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("expected 1 DB read (cached), got %d", got)
	}
}

func TestProviderThinkingGate_RereadsAfterTTL(t *testing.T) {
	repo := &fakePTGateRepo{data: ptBlob(t, map[string]any{})}
	g := &providerThinkingGate{settings: repo}
	_ = g.Mode(context.Background(), "openai")
	g.mu.Lock()
	g.cachedAt = time.Now().Add(-providerThinkingCacheTTL - time.Second)
	g.mu.Unlock()
	_ = g.Mode(context.Background(), "openai")
	if got := repo.calls.Load(); got != 2 {
		t.Fatalf("expected 2 DB reads after TTL expiry, got %d", got)
	}
}

func TestProviderThinkingGate_ReadErrorReturnsEmptyAndCaches(t *testing.T) {
	repo := &fakePTGateRepo{err: errors.New("boom")}
	g := &providerThinkingGate{settings: repo}
	if mode := g.Mode(context.Background(), "openai"); mode != "" {
		t.Fatalf("DB error should yield empty mode, got %q", mode)
	}
	_ = g.Mode(context.Background(), "openai")
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("error result must be cached, got %d reads", got)
	}
}

func TestProviderThinkingGate_NilRepoReturnsNilGate(t *testing.T) {
	if NewProviderThinkingGate(nil) != nil {
		t.Fatalf("nil repo must yield nil gate")
	}
}

func TestProviderThinkingGate_EmptyProviderReturnsEmpty(t *testing.T) {
	repo := &fakePTGateRepo{data: ptBlob(t, map[string]any{
		"providerThinking": map[string]any{"openai": map[string]any{"mode": "high"}},
	})}
	g := &providerThinkingGate{settings: repo}
	if mode := g.Mode(context.Background(), ""); mode != "" {
		t.Fatalf("empty provider should yield empty, got %q", mode)
	}
	if got := repo.calls.Load(); got != 0 {
		t.Fatalf("empty provider should short-circuit without DB read, got %d", got)
	}
}

func TestInjectProviderThinking_On(t *testing.T) {
	body := json.RawMessage(`{"model":"gpt-4","messages":[]}`)
	out := injectProviderThinking(body, "on")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	th, ok := m["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking block, got %v", m["thinking"])
	}
	if typ, _ := th["type"].(string); typ != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled", typ)
	}
	if bt, _ := th["budget_tokens"].(float64); bt != 10000 {
		t.Fatalf("budget_tokens = %v, want 10000", bt)
	}
}

func TestInjectProviderThinking_Off(t *testing.T) {
	body := json.RawMessage(`{"model":"claude-3","messages":[]}`)
	out := injectProviderThinking(body, "off")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	th, ok := m["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking block, got %v", m["thinking"])
	}
	if typ, _ := th["type"].(string); typ != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled", typ)
	}
}

func TestInjectProviderThinking_EffortLevel(t *testing.T) {
	for _, mode := range []string{"low", "medium", "high", "xhigh", "max", "none", "thinking"} {
		body := json.RawMessage(`{"model":"gpt-4","messages":[]}`)
		out := injectProviderThinking(body, mode)
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("unmarshal mode %q: %v", mode, err)
		}
		if re, _ := m["reasoning_effort"].(string); re != mode {
			t.Fatalf("mode %q: reasoning_effort = %q, want %q", mode, re, mode)
		}
	}
}

func TestInjectProviderThinking_AutoIsNoOp(t *testing.T) {
	body := json.RawMessage(`{"model":"gpt-4","messages":[]}`)
	out := injectProviderThinking(body, "auto")
	if string(out) != string(body) {
		t.Fatalf("auto should be a no-op, got %s", string(out))
	}
}

func TestInjectProviderThinking_EmptyIsNoOp(t *testing.T) {
	body := json.RawMessage(`{"model":"gpt-4","messages":[]}`)
	out := injectProviderThinking(body, "")
	if string(out) != string(body) {
		t.Fatalf("empty mode should be a no-op, got %s", string(out))
	}
}

func TestInjectProviderThinking_ClientFieldWins(t *testing.T) {
	// "on" must not overwrite an existing client thinking block.
	body := json.RawMessage(`{"model":"claude-3","thinking":{"type":"adaptive"},"messages":[]}`)
	out := injectProviderThinking(body, "on")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	th, _ := m["thinking"].(map[string]any)
	if typ, _ := th["type"].(string); typ != "adaptive" {
		t.Fatalf("client thinking.type = %q, want preserved adaptive", typ)
	}

	// effort mode must not overwrite an existing reasoning_effort.
	body = json.RawMessage(`{"model":"gpt-4","reasoning_effort":"low","messages":[]}`)
	out = injectProviderThinking(body, "high")
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if re, _ := m["reasoning_effort"].(string); re != "low" {
		t.Fatalf("client reasoning_effort = %q, want preserved low", re)
	}
}

func TestInjectProviderThinking_InvalidJSONReturnsOriginal(t *testing.T) {
	body := json.RawMessage(`{not json`)
	out := injectProviderThinking(body, "on")
	if string(out) != string(body) {
		t.Fatalf("invalid JSON should return body unchanged, got %s", string(out))
	}
}

// TestIsThinkingEnabled_RespondsToProviderThinkingInjection confirms the
// stall-timeout detection path sees the injected override: a body with no
// thinking markers becomes "reasoning enabled" after an "on"/"high" injection,
// so the reasoning stall timeout applies — mirroring JS chatCore.js:402 reading
// isThinkingEnabled on the post-injection body.
func TestIsThinkingEnabled_RespondsToProviderThinkingInjection(t *testing.T) {
	plain := json.RawMessage(`{"model":"gpt-4","messages":[]}`)
	if isThinkingEnabled(plain, nil, "gpt-4") {
		t.Fatalf("plain body should not be reasoning-enabled")
	}
	injected := injectProviderThinking(plain, "high")
	if !isThinkingEnabled(injected, nil, "gpt-4") {
		t.Fatalf("after 'high' injection body should be reasoning-enabled (reasoning_effort set)")
	}
	injectedOn := injectProviderThinking(plain, "on")
	if !isThinkingEnabled(injectedOn, nil, "gpt-4") {
		t.Fatalf("after 'on' injection body should be reasoning-enabled (thinking.type=enabled)")
	}
}
