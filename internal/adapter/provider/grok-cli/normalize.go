package grokcliexec

// normalize.go ports the request-shape normalization the Grok Build Responses
// API expects, from upstream 59b78282 (open-sse/executors/grok-cli.js): item
// type normalization (reasoning / custom_tool_call / function_call_output /
// function_call), stored-item-reference stripping, tool normalization (incl.
// type:"custom" freeform tools + tool_choice remap), and the final
// RESPONSES_API_ALLOWLIST filter. Without these the upstream rejects or
// mis-interprets multi-turn tool + reasoning payloads.

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	// serverIDPattern matches item ids with a server-side prefix that the
	// /responses endpoint with store=false will not re-resolve — they must be
	// dropped or their id stripped.
	grokCliServerIDPattern = regexp.MustCompile(`^(rs|fc|resp|msg)_`)
	// grokCliNativeItemID matches the native item ids the proxy echoes back
	// (rs_/msg_/fc_ + a UUID) which DO survive a store=false round-trip and must
	// be kept verbatim (not stripped).
	grokCliNativeItemID = regexp.MustCompile(`^(?:rs|msg|fc)_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// grokCliHostedToolTypes are server-side hosted tool types passed through
// unchanged. Mirrors HOSTED_TOOL_TYPES in the JS executor.
var grokCliHostedToolTypes = map[string]bool{
	"web_search":         true,
	"x_search":           true,
	"web_search_preview": true,
	"file_search":        true,
	"image_generation":   true,
	"code_interpreter":   true,
	"mcp":                true,
	"local_shell":        true,
}

// grokCliResponsesAllowlist is the final body key filter; everything else is
// dropped before the request leaves the process. Mirrors RESPONSES_API_ALLOWLIST.
var grokCliResponsesAllowlist = map[string]bool{
	"model": true, "input": true, "instructions": true, "tools": true,
	"tool_choice": true, "stream": true, "store": true, "reasoning": true,
	"include": true, "temperature": true, "top_p": true, "max_output_tokens": true,
	"parallel_tool_calls": true, "text": true, "metadata": true,
	"prompt_cache_key": true,
}

// grokCliFreeformToolParameters is the parameters object substituted for
// type:"custom" tools (GROK_CLI_FREEFORM_TOOL_PARAMETERS).
var grokCliFreeformToolParameters = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"input": map[string]any{"type": "string"},
	},
	"required": []any{"input"},
}

// grokCliReasoningEffortRe gates which models accept reasoning.effort. Only
// grok-4.5* does; grok-build does NOT (upstream supportsGrokCliReasoningEffort).
var grokCliReasoningEffortRe = regexp.MustCompile(`^grok-4\.5(?:$|-)`)

// supportsGrokCliReasoningEffort reports whether the model accepts a
// reasoning.effort field. grok-build does not — the executor must delete effort
// for it while still requesting reasoning.encrypted_content.
func supportsGrokCliReasoningEffort(model string) bool {
	return grokCliReasoningEffortRe.MatchString(model)
}

// normalizeGrokCliEffort mirrors normalizeGrokCliEffort: "max" → "xhigh", a
// known level passes through, everything else falls back to "high".
func normalizeGrokCliEffort(v string) string {
	effort := strings.ToLower(strings.TrimSpace(v))
	if effort == "max" {
		return "xhigh"
	}
	for _, level := range grokCliEffortLevels {
		if effort == level {
			return effort
		}
	}
	return "high"
}

// resolveEffortFromModel returns the effort level encoded as a model id suffix
// ("-high", "-xhigh", ...) or "".
func resolveEffortFromModel(modelID string) string {
	for _, level := range grokCliEffortLevels {
		if strings.HasSuffix(modelID, "-"+level) {
			return level
		}
	}
	return ""
}

// isNativeGrokCliItemID reports whether id is a proxy-native item id that must
// be preserved verbatim (not stripped in stripStoredItemReferences).
func isNativeGrokCliItemID(id string) bool {
	return grokCliNativeItemID.MatchString(id)
}

// stringifyGrokCliToolOutput mirrors stringifyGrokCliToolOutput: strings pass
// through, undefined/nil → "", everything else is JSON-encoded.
func stringifyGrokCliToolOutput(output any) string {
	switch t := output.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// normalizeGrokCliInputItem rewrites a single input item into the Responses API
// shape. Returns nil to drop the item entirely (e.g. a non-native reasoning
// item without encrypted_content). Mirrors normalizeGrokCliInputItem in JS.
func normalizeGrokCliInputItem(item map[string]any) map[string]any {
	if item == nil {
		return nil
	}
	delete(item, "internal_chat_message_metadata_passthrough")
	t, _ := item["type"].(string)

	switch t {
	case "reasoning":
		id, _ := item["id"].(string)
		ec, _ := item["encrypted_content"].(string)
		if !isNativeGrokCliItemID(id) || ec == "" {
			return nil
		}
		return item
	case "custom_tool_call":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		name := strings.TrimSpace(asStringPSD(item["name"]))
		if callID == "" || name == "" {
			return nil
		}
		input := item["input"]
		if input == nil {
			input = item["arguments"]
		}
		return map[string]any{
			"type":      "function_call",
			"call_id":   callID,
			"name":      name,
			"arguments": stringifyToolArgs(input),
		}
	case "custom_tool_call_output", "function_call_output":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		if callID == "" {
			return nil
		}
		return map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  stringifyGrokCliToolOutput(item["output"]),
		}
	case "function_call":
		callID, _ := item["call_id"].(string)
		name := strings.TrimSpace(asStringPSD(item["name"]))
		if callID == "" || name == "" {
			return nil
		}
		out := map[string]any{
			"type":    "function_call",
			"call_id": callID,
			"name":    name,
		}
		if id, ok := item["id"].(string); ok && isNativeGrokCliItemID(id) {
			out["id"] = id
		}
		args := item["arguments"]
		if s, ok := args.(string); ok {
			out["arguments"] = s
		} else {
			out["arguments"] = stringifyToolArgs(args)
		}
		if status, ok := item["status"].(string); ok && status != "" {
			out["status"] = status
		}
		return out
	default:
		return item
	}
}

// stringifyToolArgs wraps the custom_tool_call input into the {input: "..."}
// envelope the Responses API expects for freeform tools.
func stringifyToolArgs(args any) string {
	if args == nil {
		return "{\"input\":\"\"}"
	}
	if s, ok := args.(string); ok {
		return s
	}
	b, err := json.Marshal(map[string]any{"input": stringifyGrokCliToolOutput(args)})
	if err != nil {
		return "{\"input\":\"\"}"
	}
	return string(b)
}

// normalizeGrokCliInput rewrites body["input"]: normalizes each item, drops
// nils, then drops function_call_output items whose call_id has no matching
// function_call (mirrors the post-filter in JS).
func normalizeGrokCliInput(body map[string]any) {
	arr, ok := body["input"].([]any)
	if !ok {
		return
	}
	var out []any
	callIDs := map[string]bool{}
	for _, raw := range arr {
		item, ok := raw.(map[string]any)
		if !ok {
			// String items are handled by stripStoredItemReferences; keep non-string
			// non-map items only if they survive normalization — maps only here.
			continue
		}
		norm := normalizeGrokCliInputItem(item)
		if norm == nil {
			continue
		}
		if t, _ := norm["type"].(string); t == "function_call" {
			if id, _ := norm["call_id"].(string); id != "" {
				callIDs[id] = true
			}
		}
		out = append(out, norm)
	}
	// Drop orphan function_call_output items.
	filtered := out[:0]
	for _, raw := range out {
		item, ok := raw.(map[string]any)
		if !ok {
			filtered = append(filtered, raw)
			continue
		}
		if t, _ := item["type"].(string); t == "function_call_output" {
			id, _ := item["call_id"].(string)
			if id != "" && !callIDs[id] {
				continue
			}
		}
		filtered = append(filtered, raw)
	}
	body["input"] = filtered
}

// stripStoredItemReferences drops string items with a server-id prefix and
// item_reference entries, and deletes the id from items whose id is a
// server-prefix id that is not proxy-native (mirrors stripStoredItemReferences).
func stripStoredItemReferences(body map[string]any) {
	arr, ok := body["input"].([]any)
	if !ok {
		return
	}
	filtered := arr[:0]
	for _, raw := range arr {
		switch t := raw.(type) {
		case string:
			if grokCliServerIDPattern.MatchString(t) {
				continue
			}
			filtered = append(filtered, raw)
		case map[string]any:
			if it, _ := t["type"].(string); it == "item_reference" {
				continue
			}
			if id, _ := t["id"].(string); id != "" && grokCliServerIDPattern.MatchString(id) && !isNativeGrokCliItemID(id) {
				delete(t, "id")
			}
			filtered = append(filtered, raw)
		default:
			filtered = append(filtered, raw)
		}
	}
	body["input"] = filtered
}

// normalizeGrokCliTools rewrites body["tools"] + body["tool_choice"] into the
// Responses API shape: hosted tools pass through; function tools are rebuilt
// with a trimmed ≤128-char name; type:"custom" tools get the freeform
// parameters; tool_choice type:"custom"/"function" is remapped to
// {type:"function",name}. Empty tool sets delete both fields. Mirrors
// normalizeGrokCliTools in JS.
func normalizeGrokCliTools(body map[string]any) {
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) == 0 {
		delete(body, "tools")
		delete(body, "tool_choice")
		return
	}
	hostedTypes := map[string]bool{}
	validNames := map[string]bool{}
	var out []any
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := tool["type"].(string)
		fn, _ := tool["function"].(map[string]any)
		isHosted := t != "" && t != "function" && t != "custom" && grokCliHostedToolTypes[t]
		if isHosted {
			hostedTypes[t] = true
			out = append(out, tool)
			continue
		}
		isFunction := t == "function" || t == "custom" || t == "" || fn != nil
		name := strings.TrimSpace(asStringPSD(tool["name"]))
		if name == "" && fn != nil {
			name = strings.TrimSpace(asStringPSD(fn["name"]))
		}
		if name == "" {
			continue
		}
		if !isFunction {
			continue
		}
		var params any
		if t == "custom" {
			params = grokCliFreeformToolParameters
		} else if p, ok := tool["parameters"]; ok && p != nil {
			params = p
		} else if p, ok := fn["parameters"]; ok && p != nil {
			params = p
		} else {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		built := map[string]any{
			"type":       "function",
			"name":       truncate(name, 128),
			"parameters": params,
		}
		if d, ok := tool["description"]; ok && d != nil {
			built["description"] = d
		} else if d, ok := fn["description"]; ok && d != nil {
			built["description"] = d
		}
		validNames[built["name"].(string)] = true
		out = append(out, built)
	}
	if len(out) == 0 {
		delete(body, "tools")
		delete(body, "tool_choice")
		return
	}
	body["tools"] = out

	tc, ok := body["tool_choice"].(map[string]any)
	if !ok {
		// tool_choice may be a string ("auto"/"none"/"required") — pass through.
		return
	}
	choiceType, _ := tc["type"].(string)
	if choiceType == "function" || choiceType == "custom" {
		rawName, _ := tc["name"].(string)
		if rawName == "" {
			if inner, ok := tc["function"].(map[string]any); ok {
				rawName, _ = inner["name"].(string)
			}
		}
		name := truncate(strings.TrimSpace(rawName), 128)
		if name == "" || !validNames[name] {
			delete(body, "tool_choice")
			return
		}
		body["tool_choice"] = map[string]any{"type": "function", "name": name}
		return
	}
	if choiceType != "" && !hostedTypes[choiceType] {
		delete(body, "tool_choice")
	}
}

// applyResponsesAllowlist deletes every body key not in
// grokCliResponsesAllowlist. Called last so normalization helpers can use
// transient fields without them leaking onto the wire.
func applyResponsesAllowlist(body map[string]any) {
	for k := range body {
		if !grokCliResponsesAllowlist[k] {
			delete(body, k)
		}
	}
}

// asStringPSD is the local string coercion (mirrors asString in the resolver,
// duplicated here to avoid a resolver→executor import cycle).
func asStringPSD(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case nil:
		return ""
	default:
		return strings.TrimSpace(jsonString(t))
	}
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
