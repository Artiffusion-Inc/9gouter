package http

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

// Package http — combo strategy resolution, porting the legacy JS combo
// handling:
//   - src/sse/handlers/chat.js (detect combo → read settings.comboStrategies[name]
//     then global comboStrategy → branch into handleComboChat / handleFusionChat),
//   - open-sse/services/combo.js (getRotatedModels round-robin state,
//     handleComboChat fallback-through-models).
//
// The Go chat path previously collapsed every combo to models[0], so a combo
// whose first model failed returned that failure instead of trying the next
// combo model. This file restores the two-axis fallback the JS had:
//   1. combo level — iterate combo models (fallback order, or round-robin
//      rotated order) and fall through to the next model on a fallback-worthy
//      error;
//   2. account level — for each combo model, the existing account-fallback loop
//      in handleChat rotates connections/accounts for that provider/model.
//
// Only the "fallback" and "round-robin" strategies are ported; "fusion"
// (which merges streams from multiple models) is a separate, larger slice and
// falls back to plain fallback here. round-robin keeps per-combo in-memory
// rotation state (mirroring open-sse/services/combo.js comboRotationState) so
// successive requests hit a rotating start model, bounded by the sticky limit
// (stay on the same model for N requests before advancing).

// comboStrategy is the per-combo dispatch strategy.
type comboStrategy string

const (
	comboStrategyFallback   comboStrategy = "fallback"
	comboStrategyRoundRobin comboStrategy = "round-robin"
	comboStrategyFusion     comboStrategy = "fusion" // not ported; treats as fallback
)

// comboRotationState mirrors open-sse/services/combo.js comboRotationState:
// per-combo-name rotation cursor advanced once the sticky-use count reaches
// the configured limit. Shared across requests for the lifetime of the
// process (same in-memory semantics as the JS build).
var comboRotationState = struct {
	sync.Mutex
	m map[string]comboRotationEntry
}{m: map[string]comboRotationEntry{}}

type comboRotationEntry struct {
	index               int
	consecutiveUseCount int
}

// resolveComboStrategy reads the combo dispatch strategy for a combo name:
// settings.comboStrategies[name].fallbackStrategy (per-combo override) wins,
// then the global settings.comboStrategy, then "fallback". Mirrors
// src/sse/handlers/chat.js:91-129 (comboStrategies[name]?.fallbackStrategy ||
// comboStrategy). data is the parsed settings blob.
func resolveComboStrategy(data map[string]any, comboName string) comboStrategy {
	if strats, ok := data["comboStrategies"].(map[string]any); ok {
		if entry, ok := strats[comboName].(map[string]any); ok {
			if s, ok := entry["fallbackStrategy"].(string); ok && s != "" {
				return comboStrategy(s)
			}
		}
	}
	if s, ok := data["comboStrategy"].(string); ok && s != "" {
		return comboStrategy(s)
	}
	return comboStrategyFallback
}

// comboStickyLimit reads settings.comboStickyRoundRobinLimit (number of
// requests to stay on the same combo model before advancing in round-robin),
// defaulting to 1 like open-sse/services/combo.js normalizeStickyLimit.
func comboStickyLimit(data map[string]any) int {
	if v, ok := data["comboStickyRoundRobinLimit"]; ok {
		switch n := v.(type) {
		case float64:
			if n >= 1 {
				return int(n)
			}
		case int:
			if n >= 1 {
				return n
			}
		}
	}
	return 1
}

// rotateComboModels returns the models reordered for the given combo + strategy.
// fallback keeps the stored order; round-robin rotates so the cursor model is
// first, then advances the cursor once the sticky-use count reaches the limit.
// fusion is treated as fallback (not ported). A single-model combo is never
// rotated. Mirrors open-sse/services/combo.js getRotatedModels.
func rotateComboModels(comboName string, models []string, strat comboStrategy, stickyLimit int) []string {
	if len(models) <= 1 {
		return models
	}
	if strat != comboStrategyRoundRobin {
		return models
	}
	comboRotationState.Lock()
	defer comboRotationState.Unlock()
	key := comboName
	if key == "" {
		key = "__default__"
	}
	entry := comboRotationState.m[key]
	currentIndex := entry.index % len(models)
	rotated := make([]string, len(models))
	for i := range models {
		rotated[i] = models[(currentIndex+i)%len(models)]
	}
	nextUseCount := entry.consecutiveUseCount + 1
	if nextUseCount >= stickyLimit {
		comboRotationState.m[key] = comboRotationEntry{
			index:               (currentIndex + 1) % len(models),
			consecutiveUseCount: 0,
		}
	} else {
		comboRotationState.m[key] = comboRotationEntry{
			index:               currentIndex,
			consecutiveUseCount: nextUseCount,
		}
	}
	return rotated
}

// ResetComboRotation clears the in-memory rotation state. It must be called by
// the dashboard combos API when a combo is created/updated/deleted so rotation
// honors the new model list immediately (mirrors open-sse/services/combo.js
// resetComboRotation). If comboName is empty, all combo state is cleared.
//
// Without this, a round-robin combo whose model list shrinks below the stored
// rotation index would keep advancing a cursor that no longer points at a real
// model, and a renamed combo would keep a stale entry under the old name.
func ResetComboRotation(comboName string) {
	comboRotationState.Lock()
	defer comboRotationState.Unlock()
	if comboName == "" {
		comboRotationState.m = map[string]comboRotationEntry{}
		return
	}
	delete(comboRotationState.m, comboName)
}

// resolveComboModels returns the combo's model list (rotated per strategy) and
// the resolved strategy if modelStr names a combo, or (nil, "") otherwise. The
// caller (handleChat) iterates these models and falls through on a
// fallback-worthy error. data is the parsed settings blob (for strategy/limit).
func (h *v1Handler) resolveComboModels(ctx context.Context, modelStr string, data map[string]any) ([]string, comboStrategy) {
	if strings.Contains(modelStr, "/") {
		return nil, ""
	}
	combo, err := h.deps.ComboRepo.GetByName(ctx, modelStr)
	if err != nil || combo == nil {
		return nil, ""
	}
	var models []string
	_ = json.Unmarshal(combo.Models, &models)
	if len(models) == 0 {
		return nil, ""
	}
	strat := resolveComboStrategy(data, modelStr)
	limit := comboStickyLimit(data)
	return rotateComboModels(modelStr, models, strat, limit), strat
}
