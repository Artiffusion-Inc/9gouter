// Package kiro — conversation.go ports open-sse/translator/concerns/
// kiroConversation.js (upstream 16cb40fd). The canonicalizer transforms a
// loosely-shaped OpenAI/Claude→Kiro conversation into the strict Kiro wire
// format: alternating user/assistant turns, adjacent one-to-one tool
// use/result pairs, and tool specs only on the currentMessage.
//
// Without canonicalization Kiro rejects malformed conversations with 400
// "toolUseId not found" or "duplicate tool names" errors. The canonicalizer
// repairs orphaned tool results, missing tool results, invalid tool specs,
// and non-alternating turns by flattening structured tool data into text
// when it cannot be salvaged.
package kiro

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	kiroToolNameMaxLen       = 64
	kiroToolDescriptionMaxLen = 10237
	kiroToolIDMaxLen          = 64
)

var (
	toolIDPattern   = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	toolNameReplace  = regexp.MustCompile(`[^a-zA-Z0-9_-]`)
)

// kiroNameMap maps source tool names → normalized Kiro names.
type kiroNameMap map[string]string

// canonicalizeResult is the output of canonicalizeKiroConversation.
type canonicalizeResult struct {
	History        []map[string]any
	CurrentMessage map[string]any
	Repairs        kiroRepairs
	Valid          bool
	Errors         []string
}

type kiroRepairs struct {
	MissingResults   int
	OrphanResults    int
	InvalidToolUses  int
}

// normalizeKiroToolSpecs converts OpenAI/Claude tool definitions into Kiro
// toolSpecification entries and returns the specs + a name map for
// reconciling tool calls. Mirrors normalizeKiroToolSpecs in kiroConversation.js.
func normalizeKiroToolSpecs(tools []any) ([]any, kiroNameMap) {
	specs := []any{}
	nameMap := kiroNameMap{}
	usedNames := map[string]bool{}

	for i, toolAny := range tools {
		tool, ok := toolAny.(map[string]any)
		if !ok {
			continue
		}
		rawName := ""
		if fn, ok := tool["function"].(map[string]any); ok {
			rawName, _ = fn["name"].(string)
		}
		if rawName == "" {
			rawName, _ = tool["name"].(string)
		}
		rawName = strings.TrimSpace(rawName)
		if rawName == "" {
			continue
		}
		if _, exists := nameMap[rawName]; exists {
			continue
		}
		name := uniqueName(rawName, i, usedNames)
		nameMap[rawName] = name

		desc := ""
		if fn, ok := tool["function"].(map[string]any); ok {
			desc, _ = fn["description"].(string)
		}
		if desc == "" {
			desc, _ = tool["description"].(string)
		}
		if desc == "" {
			desc = fmt.Sprintf("Tool: %s", rawName)
		}
		desc = trimCodePoints(desc, kiroToolDescriptionMaxLen)

		var schema map[string]any
		if fn, ok := tool["function"].(map[string]any); ok {
			schema, _ = fn["parameters"].(map[string]any)
		}
		if schema == nil {
			schema, _ = tool["parameters"].(map[string]any)
		}
		if schema == nil {
			schema, _ = tool["input_schema"].(map[string]any)
		}
		if schema == nil {
			schema = map[string]any{}
		}
		schema = normalizeRootSchema(schema)

		specs = append(specs, map[string]any{
			"toolSpecification": map[string]any{
				"name":        name,
				"description": desc,
				"inputSchema": map[string]any{"json": schema},
			},
		})
	}
	return specs, nameMap
}

func uniqueName(rawName string, index int, usedNames map[string]bool) string {
	cleaned := strings.TrimSpace(rawName)
	cleaned = toolNameReplace.ReplaceAllString(cleaned, "_")
	cleaned = strings.ReplaceAll(cleaned, "_", "_")
	cleaned = strings.Trim(cleaned, "_")
	base := trimCodePoints(cleaned, kiroToolNameMaxLen)
	if base == "" {
		base = fmt.Sprintf("tool_%d", index+1)
	}
	candidate := base
	suffix := 2
	for usedNames[candidate] {
		tail := fmt.Sprintf("_%d", suffix)
		suffix++
		if len(base) > kiroToolNameMaxLen-len(tail) {
			candidate = base[:kiroToolNameMaxLen-len(tail)] + tail
		} else {
			candidate = base + tail
		}
	}
	usedNames[candidate] = true
	return candidate
}

func trimCodePoints(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// normalizeRootSchema ensures the tool input schema is a clean object with
// properties + required. Mirrors normalizeRootSchema in kiroConversation.js.
func normalizeRootSchema(schema map[string]any) map[string]any {
	cleaned := cleanSchemaValue(schema)
	if cleaned == nil {
		cleaned = map[string]any{}
	}
	cleaned["type"] = "object"
	props, ok := cleaned["properties"].(map[string]any)
	if !ok || props == nil {
		cleaned["properties"] = map[string]any{}
	}
	if req, ok := cleaned["required"].([]any); ok {
		seen := map[string]bool{}
		filtered := []any{}
		for _, r := range req {
			if s, ok := r.(string); ok {
				if _, exists := cleaned["properties"].(map[string]any)[s]; exists && !seen[s] {
					seen[s] = true
					filtered = append(filtered, s)
				}
			}
		}
		if len(filtered) > 0 {
			cleaned["required"] = filtered
		} else {
			delete(cleaned, "required")
		}
	}
	return cleaned
}

func cleanSchemaValue(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	cleaned := map[string]any{}
	for k, child := range m {
		if k == "additionalProperties" {
			continue
		}
		if k == "required" {
			if arr, ok := child.([]any); ok && len(arr) == 0 {
				continue
			}
		}
		if childMap, ok := child.(map[string]any); ok {
			cleaned[k] = cleanSchemaValue(childMap)
		} else if childArr, ok := child.([]any); ok {
			cleaned[k] = cleanSchemaArray(childArr)
		} else {
			cleaned[k] = child
		}
	}
	return cleaned
}

func cleanSchemaArray(arr []any) []any {
	out := make([]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			cleaned := cleanSchemaValue(m)
			if cleaned != nil {
				out = append(out, cleaned)
			} else {
				out = append(out, m)
			}
		} else {
			out = append(out, item)
		}
	}
	return out
}

// toolCallText produces a text fallback for an invalid tool call.
func toolCallText(name string, input any) string {
	return fmt.Sprintf("[Tool call: %s(%s)]", ifEmpty(name, "unknown"), toJSONStr(input))
}

func toolResultText(result map[string]any) string {
	content := ""
	if arr, ok := result["content"].([]any); ok {
		parts := []string{}
		for _, p := range arr {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		content = strings.Join(parts, "\n")
	} else if s, ok := result["content"].(string); ok {
		content = s
	}
	suffix := ""
	if result["status"] == "error" {
		suffix = " (error)"
	}
	return fmt.Sprintf("[Tool result%s: %s]", suffix, content)
}

func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func toJSONStr(v any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// canonicalizeKiroConversation takes the output of convertOpenAIMessagesToKiro
// (history + currentMessage + toolSpecs + nameMap) and produces a strict Kiro
// wire conversation. Mirrors canonicalizeKiroConversation in kiroConversation.js.
func canonicalizeKiroConversation(history []map[string]any, currentMessage map[string]any, modelID string, toolSpecs []any, nameMap kiroNameMap) canonicalizeResult {
	turns := normalizeTurns(history, currentMessage, modelID)
	repairs := kiroRepairs{}
	specNames := map[string]bool{}
	for _, spec := range toolSpecs {
		if sm, ok := spec.(map[string]any); ok {
			if ts, ok := sm["toolSpecification"].(map[string]any); ok {
				if name, ok := ts["name"].(string); ok && name != "" {
					specNames[name] = true
				}
			}
		}
	}
	usedIDs := map[string]bool{}

	for i := 0; i < len(turns); i += 2 {
		user, ok := turns[i]["userInputMessage"].(map[string]any)
		if !ok {
			continue
		}
		if i == 0 {
			if ctx, ok := user["userInputMessageContext"].(map[string]any); ok {
				if results, ok := ctx["toolResults"].([]any); ok && len(results) > 0 {
					for _, r := range results {
						if rm, ok := r.(map[string]any); ok {
							appendTextTo(user, toolResultText(rm))
							repairs.OrphanResults++
						}
					}
					delete(ctx, "toolResults")
					cleanUserContext(user)
				}
			}
		}
		if i+2 >= len(turns) {
			continue
		}
		assistant, hasAssistant := turns[i+1]["assistantResponseMessage"].(map[string]any)
		nextUser, hasNextUser := turns[i+2]["userInputMessage"].(map[string]any)
		if hasAssistant && hasNextUser {
			reconcileToolPair(assistant, nextUser, i+1, nameMap, specNames, usedIDs, &repairs)
		}
	}

	// Final currentMessage gets tool specs.
	finalCurrent := turns[len(turns)-1]
	finalUIM, ok := finalCurrent["userInputMessage"].(map[string]any)
	if ok {
		if finalUIM["userInputMessageContext"] == nil {
			finalUIM["userInputMessageContext"] = map[string]any{}
		}
		ctx, _ := finalUIM["userInputMessageContext"].(map[string]any)
		if len(toolSpecs) > 0 {
			ctx["tools"] = cloneValue(toolSpecs)
		}
		cleanUserContext(finalUIM)
	}

	finalHistory := turns[:len(turns)-1]
	validation := validateKiroConversation(finalHistory, finalCurrent, toolSpecs)
	if !validation.Valid {
		flattenAllStructuredTools(turns, &repairs)
		finalHistory = turns[:len(turns)-1]
		validation = validateKiroConversation(finalHistory, finalCurrent, toolSpecs)
	}

	return canonicalizeResult{
		History:        finalHistory,
		CurrentMessage: finalCurrent,
		Repairs:        repairs,
		Valid:          validation.Valid,
		Errors:         validation.Errors,
	}
}

// normalizeTurns ensures alternating user/assistant turns, merging consecutive
// same-role turns and prepending/appending "continue" turns as needed.
func normalizeTurns(history []map[string]any, currentMessage map[string]any, modelID string) []map[string]any {
	rawTurns := make([]map[string]any, 0, len(history)+1)
	rawTurns = append(rawTurns, history...)
	if currentMessage != nil {
		rawTurns = append(rawTurns, currentMessage)
	}

	turns := []map[string]any{}
	for _, raw := range rawTurns {
		isUser := raw["userInputMessage"] != nil
		isAssistant := raw["assistantResponseMessage"] != nil
		if isUser == isAssistant {
			continue
		}
		var turn map[string]any
		if isUser {
			turn = map[string]any{"userInputMessage": cloneValue(raw["userInputMessage"])}
		} else {
			turn = map[string]any{"assistantResponseMessage": cloneValue(raw["assistantResponseMessage"])}
		}
		if len(turns) > 0 {
			prev := turns[len(turns)-1]
			if isUser {
				if prevUIM, ok := prev["userInputMessage"].(map[string]any); ok {
					if uim, ok := turn["userInputMessage"].(map[string]any); ok {
						mergeUser(prevUIM, uim)
						continue
					}
				}
			} else {
				if prevARM, ok := prev["assistantResponseMessage"].(map[string]any); ok {
					if arm, ok := turn["assistantResponseMessage"].(map[string]any); ok {
						mergeAssistant(prevARM, arm)
						continue
					}
				}
			}
		}
		turns = append(turns, turn)
	}

	// Ensure first turn is user.
	if len(turns) > 0 {
		if _, ok := turns[0]["assistantResponseMessage"]; ok {
			turns = append([]map[string]any{{"userInputMessage": map[string]any{"content": "continue", "modelId": modelID}}}, turns...)
		}
	}
	// Ensure last turn is user.
	if len(turns) == 0 {
		turns = append(turns, map[string]any{"userInputMessage": map[string]any{"content": "continue", "modelId": modelID}})
	} else if _, ok := turns[len(turns)-1]["assistantResponseMessage"]; ok {
		turns = append(turns, map[string]any{"userInputMessage": map[string]any{"content": "continue", "modelId": modelID}})
	}

	// Normalize content + modelId.
	for _, turn := range turns {
		if uim, ok := turn["userInputMessage"].(map[string]any); ok {
			content := toText(uim["content"])
			content = strings.TrimSpace(content)
			if content == "" {
				content = "continue"
			}
			uim["content"] = content
			if uim["modelId"] == nil || uim["modelId"] == "" {
				uim["modelId"] = modelID
			}
			if ctx, ok := uim["userInputMessageContext"].(map[string]any); ok {
				delete(ctx, "tools")
				if len(ctx) == 0 {
					delete(uim, "userInputMessageContext")
				}
			}
		} else if arm, ok := turn["assistantResponseMessage"].(map[string]any); ok {
			content := toText(arm["content"])
			content = strings.TrimSpace(content)
			if content == "" {
				content = "..."
			}
			arm["content"] = content
		}
	}

	return turns
}

func mergeUser(target, source map[string]any) {
	appendTextTo(target, toText(source["content"]))
	if imgs, ok := source["images"].([]any); ok && len(imgs) > 0 {
		targetImgs, _ := target["images"].([]any)
		target["images"] = append(targetImgs, imgs...)
	}
	if ctx, ok := source["userInputMessageContext"].(map[string]any); ok {
		if results, ok := ctx["toolResults"].([]any); ok && len(results) > 0 {
			targetCtx, _ := target["userInputMessageContext"].(map[string]any)
			if targetCtx == nil {
				targetCtx = map[string]any{}
				target["userInputMessageContext"] = targetCtx
			}
			targetResults, _ := targetCtx["toolResults"].([]any)
			targetCtx["toolResults"] = append(targetResults, results...)
		}
	}
}

func mergeAssistant(target, source map[string]any) {
	appendTextTo(target, toText(source["content"]))
	if toolUses, ok := source["toolUses"].([]any); ok && len(toolUses) > 0 {
		targetUses, _ := target["toolUses"].([]any)
		target["toolUses"] = append(targetUses, toolUses...)
	}
}

func appendTextTo(target map[string]any, extra string) {
	if extra == "" {
		return
	}
	existing := toText(target["content"])
	if existing != "" {
		target["content"] = existing + "\n\n" + extra
	} else {
		target["content"] = extra
	}
}

func toText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return toJSONStr(v)
	}
}

func cloneValue(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

// reconcileToolPair ensures each tool call has a matching result and valid spec.
// Invalid calls are flattened to text. Mirrors reconcileToolPair in kiroConversation.js.
func reconcileToolPair(assistant, user map[string]any, turnIndex int, nameMap kiroNameMap, specNames map[string]bool, usedIDs map[string]bool, repairs *kiroRepairs) {
	calls, _ := assistant["toolUses"].([]any)
	var results []any
	if ctx, ok := user["userInputMessageContext"].(map[string]any); ok {
		results, _ = ctx["toolResults"].([]any)
	}

	normalizedResults := make([]map[string]any, 0, len(results))
	for _, r := range results {
		if rm, ok := r.(map[string]any); ok {
			normalizedResults = append(normalizedResults, normalizeToolResult(rm))
		}
	}

	if len(calls) == 0 {
		if len(normalizedResults) > 0 {
			for _, r := range normalizedResults {
				appendTextTo(user, toolResultText(r))
				repairs.OrphanResults++
			}
		}
		if ctx, ok := user["userInputMessageContext"].(map[string]any); ok {
			delete(ctx, "toolResults")
			cleanUserContext(user)
		}
		return
	}

	// Build call queues by toolUseId.
	type callRecord struct {
		call       map[string]any
		callIndex  int
		key        string
		mappedName string
		input      any
		result     *map[string]any
	}
	callQueues := map[string][]*callRecord{}
	records := make([]*callRecord, 0, len(calls))
	for i, c := range calls {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		key, _ := call["toolUseId"].(string)
		mappedName := ""
		if originalName, ok := call["name"].(string); ok {
			if mapped, exists := nameMap[originalName]; exists {
				mappedName = mapped
			} else {
				mappedName = originalName
			}
		}
		input := normalizeToolInput(call["input"])
		rec := &callRecord{call: call, callIndex: i, key: key, mappedName: mappedName, input: input}
		queue := callQueues[key]
		queue = append(queue, rec)
		callQueues[key] = queue
		records = append(records, rec)
	}

	// Match results to calls.
	var orphanResults []map[string]any
	for _, result := range normalizedResults {
		id, _ := result["toolUseId"].(string)
		queue, exists := callQueues[id]
		if !exists {
			orphanResults = append(orphanResults, result)
			continue
		}
		matched := false
		for _, rec := range queue {
			if rec.result == nil {
				r := result
				rec.result = &r
				matched = true
				break
			}
		}
		if !matched {
			orphanResults = append(orphanResults, result)
		}
	}

	keptCalls := []any{}
	keptResults := []any{}
	for _, rec := range records {
		hasSpec := rec.mappedName != "" && specNames[rec.mappedName]
		valid := rec.result != nil && hasSpec && rec.input != nil
		if !valid {
			appendTextTo(assistant, toolCallText(rec.mappedName, rec.call["input"]))
			if rec.result == nil {
				repairs.MissingResults++
			} else {
				if hasSpec && rec.input != nil {
					repairs.InvalidToolUses++
				} else {
					repairs.InvalidToolUses++
				}
				appendTextTo(user, toolResultText(*rec.result))
				repairs.OrphanResults++
			}
			continue
		}
		toolUseID := reserveToolID(rec.key, turnIndex, rec.callIndex, rec.mappedName, usedIDs)
		keptCalls = append(keptCalls, map[string]any{
			"toolUseId": toolUseID,
			"name":      rec.mappedName,
			"input":     rec.input,
		})
		resultCopy := *rec.result
		resultCopy["toolUseId"] = toolUseID
		keptResults = append(keptResults, resultCopy)
	}

	if len(orphanResults) > 0 {
		for _, r := range orphanResults {
			appendTextTo(user, toolResultText(r))
			repairs.OrphanResults++
		}
	}

	if len(keptCalls) > 0 {
		assistant["toolUses"] = keptCalls
	} else {
		delete(assistant, "toolUses")
	}
	ctx, _ := user["userInputMessageContext"].(map[string]any)
	if ctx == nil {
		ctx = map[string]any{}
	}
	if len(keptResults) > 0 {
		ctx["toolResults"] = keptResults
		user["userInputMessageContext"] = ctx
	} else {
		delete(ctx, "toolResults")
		user["userInputMessageContext"] = ctx
	}
	cleanUserContext(user)
}

func normalizeToolResult(result map[string]any) map[string]any {
	content := []any{}
	if arr, ok := result["content"].([]any); ok {
		for _, p := range arr {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					content = append(content, map[string]any{"text": t})
				}
			}
		}
	} else if s, ok := result["content"].(string); ok {
		content = append(content, map[string]any{"text": s})
	}
	if len(content) == 0 {
		content = []any{map[string]any{"text": ""}}
	}
	return map[string]any{
		"toolUseId": ifEmpty(result["toolUseId"].(string), ""),
		"status":    ifStatus(result["status"]),
		"content":   content,
	}
}

func ifStatus(s any) string {
	if s == "error" {
		return "error"
	}
	return "success"
}

func normalizeToolInput(input any) any {
	if m, ok := input.(map[string]any); ok && !isArray(input) {
		return cloneValue(m)
	}
	if s, ok := input.(string); ok {
		var parsed map[string]any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			return parsed
		}
		return nil
	}
	if input == nil {
		return map[string]any{}
	}
	return nil
}

func isArray(v any) bool {
	_, ok := v.([]any)
	return ok
}

func reserveToolID(value string, turnIndex, callIndex int, name string, usedIDs map[string]bool) string {
	sanitized := ""
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sanitized += string(r)
		}
	}
	generated := fmt.Sprintf("call_msg%d_tc%d_%s", turnIndex, callIndex, ifEmpty(name, "tool"))
	base := generated
	if toolIDPattern.MatchString(sanitized) && sanitized != "" {
		base = trimCodePoints(sanitized, kiroToolIDMaxLen)
	}
	candidate := base
	suffix := 2
	for usedIDs[candidate] {
		tail := fmt.Sprintf("_%d", suffix)
		suffix++
		if len(base) > kiroToolIDMaxLen-len(tail) {
			candidate = base[:kiroToolIDMaxLen-len(tail)] + tail
		} else {
			candidate = base + tail
		}
	}
	usedIDs[candidate] = true
	return candidate
}

func cleanUserContext(user map[string]any) {
	ctx, ok := user["userInputMessageContext"].(map[string]any)
	if !ok {
		return
	}
	if trs, ok := ctx["toolResults"].([]any); ok && len(trs) == 0 {
		delete(ctx, "toolResults")
	}
	if tools, ok := ctx["tools"].([]any); ok && len(tools) == 0 {
		delete(ctx, "tools")
	}
	if len(ctx) == 0 {
		delete(user, "userInputMessageContext")
	}
}

// kiroValidation holds the result of validateKiroConversation.
type kiroValidation struct {
	Valid  bool
	Errors []string
}

func validateKiroConversation(history []map[string]any, currentMessage map[string]any, toolSpecs []any) kiroValidation {
	errors := []string{}
	turns := append(history, currentMessage)
	specNames := map[string]bool{}
	for _, spec := range toolSpecs {
		if sm, ok := spec.(map[string]any); ok {
			if ts, ok := sm["toolSpecification"].(map[string]any); ok {
				if name, ok := ts["name"].(string); ok && name != "" {
					specNames[name] = true
				}
			}
		}
	}
	usedIDs := map[string]bool{}

	for i, turn := range turns {
		expectedUser := i%2 == 0
		isUser := turn["userInputMessage"] != nil
		if isUser != expectedUser {
			errors = append(errors, fmt.Sprintf("role:%d", i))
		}
		if !isUser {
			arm, _ := turn["assistantResponseMessage"].(map[string]any)
			calls, _ := arm["toolUses"].([]any)
			var results []any
			if i+1 < len(turns) {
				if nextUIM, ok := turns[i+1]["userInputMessage"].(map[string]any); ok {
					if ctx, ok := nextUIM["userInputMessageContext"].(map[string]any); ok {
						results, _ = ctx["toolResults"].([]any)
					}
				}
			}
			callIDs := map[string]bool{}
			for _, c := range calls {
				if cm, ok := c.(map[string]any); ok {
					id, _ := cm["toolUseId"].(string)
					callIDs[id] = true
				}
			}
			resultIDs := map[string]bool{}
			for _, r := range results {
				if rm, ok := r.(map[string]any); ok {
					id, _ := rm["toolUseId"].(string)
					resultIDs[id] = true
				}
			}
			if len(calls) != len(results) {
				errors = append(errors, fmt.Sprintf("pair:%d", i))
			}
			for id := range callIDs {
				if !resultIDs[id] {
					errors = append(errors, fmt.Sprintf("pair:%d", i))
					break
				}
			}
			for _, c := range calls {
				if cm, ok := c.(map[string]any); ok {
					id, _ := cm["toolUseId"].(string)
					if id == "" || usedIDs[id] {
						errors = append(errors, fmt.Sprintf("id:%d", i))
					}
					usedIDs[id] = true
					name, _ := cm["name"].(string)
					if !specNames[name] {
						errors = append(errors, fmt.Sprintf("spec:%d", i))
					}
				}
			}
		} else if i == 0 {
			if uim, ok := turn["userInputMessage"].(map[string]any); ok {
				if ctx, ok := uim["userInputMessageContext"].(map[string]any); ok {
					if results, ok := ctx["toolResults"].([]any); ok && len(results) > 0 {
						errors = append(errors, "orphan:0")
					}
				}
			}
		}
	}
	if currentMessage == nil {
		errors = append(errors, "current")
	} else if uim, ok := currentMessage["userInputMessage"].(map[string]any); ok {
		if toText(uim["content"]) == "" {
			errors = append(errors, "current")
		}
	} else {
		errors = append(errors, "current")
	}
	return kiroValidation{Valid: len(errors) == 0, Errors: errors}
}

func flattenAllStructuredTools(turns []map[string]any, repairs *kiroRepairs) {
	for _, turn := range turns {
		if arm, ok := turn["assistantResponseMessage"].(map[string]any); ok {
			if calls, ok := arm["toolUses"].([]any); ok && len(calls) > 0 {
				for _, c := range calls {
					if cm, ok := c.(map[string]any); ok {
						appendTextTo(arm, toolCallText(toStr(cm["name"]), cm["input"]))
					}
				}
				repairs.InvalidToolUses += len(calls)
				delete(arm, "toolUses")
			}
		}
		if uim, ok := turn["userInputMessage"].(map[string]any); ok {
			if ctx, ok := uim["userInputMessageContext"].(map[string]any); ok {
				if results, ok := ctx["toolResults"].([]any); ok && len(results) > 0 {
					for _, r := range results {
						if rm, ok := r.(map[string]any); ok {
							appendTextTo(uim, toolResultText(rm))
							repairs.OrphanResults++
						}
					}
					delete(ctx, "toolResults")
					cleanUserContext(uim)
				}
			}
		}
	}
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
