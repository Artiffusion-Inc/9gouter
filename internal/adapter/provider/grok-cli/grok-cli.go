// Package grokcliexec ports the Grok CLI / Grok Build executor.
package grokcliexec

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor for Grok CLI. Per-request state (session id,
// request id, turn index, resolved model, agent id) is set in TransformRequest
// and read in BuildHeaders — the proxychat path calls TransformRequest before
// BuildHeaders, so the headers reflect the current request.
type Executor struct {
	*base.BaseExecutor
	currentSessionID string
	currentReqID     string
	currentTurnIdx   int
	currentModel     string
	currentAgentID   string
}

// New creates a Grok CLI executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("grok-cli", cfg), currentTurnIdx: 1}
}

// BuildURL returns the base Codex Responses URL.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	url := e.Config.BaseURL
	if url == "" {
		url = grokCliBaseURL + "/responses"
	}
	return url
}

// BuildHeaders emits the Grok Build subscription protocol headers
// (x-grok-session-id / x-grok-conv-id / x-grok-req-id / x-grok-turn-idx /
// x-grok-agent-id / x-grok-model-override / x-email / x-userid) on top of the
// Base header set (which carries Authorization + the registry static
// User-Agent / x-grok-client-identifier / x-grok-client-version). The
// session/conv/req ids and turn index are resolved in TransformRequest; if
// TransformRequest has not run for this request (e.g. a direct headers probe),
// they fall back to per-call UUIDs / turn 1 so the header set is never empty.
//
// Upstream 59b78282 removed x-authenticateresponse + x-compaction-at; this
// BuildHeaders never emitted them, so nothing to drop — the registry static
// set carries the surviving x-grok-client-* trio.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)

	sessionID := e.currentSessionID
	if sessionID == "" {
		if sid, ok := creds.ProviderSpecificData["sessionId"].(string); ok && sid != "" {
			sessionID = sid
		} else {
			sessionID = uuid.NewString()
		}
	}
	// CLI uses one id for both session + conversation.
	h.Set("x-grok-session-id", sessionID)
	h.Set("x-grok-conv-id", sessionID)

	reqID := e.currentReqID
	if reqID == "" {
		reqID = uuid.NewString()
	}
	h.Set("x-grok-req-id", reqID)

	turn := e.currentTurnIdx
	if turn < 1 {
		turn = 1
	}
	h.Set("x-grok-turn-idx", itoa(turn))

	if e.currentAgentID != "" {
		h.Set("x-grok-agent-id", e.currentAgentID)
	}
	if e.currentModel != "" {
		h.Set("x-grok-model-override", e.currentModel)
	}

	psd := creds.ProviderSpecificData
	if email, ok := psd["email"].(string); ok && email != "" {
		h.Set("x-email", email)
	}
	for _, k := range []string{"userId", "principalId"} {
		if v, ok := psd[k].(string); ok && v != "" {
			h.Set("x-userid", v)
			break
		}
	}
	return h
}

// TransformRequest normalizes the Grok Build Responses API request shape per
// upstream 59b78282: resolve session/agent/turn, normalize input items + tools,
// gate reasoning.effort by model (grok-build does not get effort but still gets
// encrypted_content), and apply the final RESPONSES_API_ALLOWLIST filter.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	var m map[string]any
	if len(body) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}

	psd := creds.ProviderSpecificData

	// Resolve session + agent ids first (turn index depends on the session).
	e.currentSessionID = resolveGrokCliSessionID(m, psd)
	e.currentReqID = uuid.NewString()
	e.currentAgentID = resolveGrokCliAgentID(psd)

	// Normalize input shape: string input → single user message; messages → input.
	if input, ok := m["input"].(string); ok {
		m["input"] = []any{map[string]any{"type": "message", "role": "user", "content": input}}
	}
	if arr, ok := m["input"].([]any); ok && len(arr) == 0 {
		m["input"] = []any{map[string]any{"type": "message", "role": "user", "content": "..."}}
	} else if _, hasInput := m["input"]; !hasInput {
		if messages, ok := m["messages"].([]any); ok && len(messages) > 0 {
			var input []any
			for _, raw := range messages {
				msg, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				content := "..."
				if c, ok := msg["content"].(string); ok {
					content = c
				}
				input = append(input, map[string]any{"type": "message", "role": msg["role"], "content": content})
			}
			m["input"] = input
		} else {
			m["input"] = []any{map[string]any{"type": "message", "role": "user", "content": "..."}}
		}
	}
	delete(m, "messages")

	// Item-type + tool normalization (59b78282).
	normalizeGrokCliInput(m)
	stripStoredItemReferences(m)
	normalizeGrokCliTools(m)

	// Turn index depends on the (now-normalized) input + session.
	input, _ := m["input"].([]any)
	e.currentTurnIdx = resolveGrokCliTurnIdx(e.currentSessionID, input)

	// Resolve model: strip an effort suffix from the model id, prefer an explicit
	// body.model, and keep the resolved id for the x-grok-model-override header.
	modelEffort := resolveEffortFromModel(model)
	if modelEffort != "" {
		model = strings.TrimSuffix(model, "-"+modelEffort)
	}
	if bodyModel, ok := m["model"].(string); ok && bodyModel != "" {
		model = bodyModel
	}
	e.currentModel = model
	m["model"] = model

	// Reasoning: gate effort by model (grok-build does not get effort), default
	// summary to "concise", drop reasoning_effort, and request
	// reasoning.encrypted_content whenever reasoning is present and effort is
	// not "none" (the condition changed in 59b78282 to fire even when effort is
	// absent — grok-build still wants encrypted_content).
	supportsEffort := supportsGrokCliReasoningEffort(model)
	reasoning, _ := m["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{"summary": "concise"}
		if supportsEffort {
			eff := ""
			if re, ok := m["reasoning_effort"].(string); ok && re != "" {
				eff = re
			} else if modelEffort != "" {
				eff = modelEffort
			}
			reasoning["effort"] = normalizeGrokCliEffort(eff)
		}
	} else {
		if supportsEffort {
			eff, _ := reasoning["effort"].(string)
			if eff == "" {
				if re, ok := m["reasoning_effort"].(string); ok && re != "" {
					eff = re
				} else if modelEffort != "" {
					eff = modelEffort
				}
			}
			reasoning["effort"] = normalizeGrokCliEffort(eff)
		} else {
			delete(reasoning, "effort")
		}
		if s, _ := reasoning["summary"].(string); s == "" {
			reasoning["summary"] = "concise"
		}
	}
	m["reasoning"] = reasoning
	delete(m, "reasoning_effort")

	if eff, _ := reasoning["effort"].(string); eff != "none" {
		include, _ := m["include"].([]any)
		found := false
		for _, v := range include {
			if v == "reasoning.encrypted_content" {
				found = true
				break
			}
		}
		if !found {
			m["include"] = append(include, "reasoning.encrypted_content")
		}
	}

	m["stream"] = true
	m["store"] = false

	// Final allowlist filter replaces the prior delete-list; it drops every key
	// the Responses API does not accept (messages, max_tokens, ..., plus any
	// normalization-helper residue).
	applyResponsesAllowlist(m)

	out, _ := json.Marshal(m)
	return out, nil
}

// itoa is a strconv-free int→string to keep the import list tight.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
