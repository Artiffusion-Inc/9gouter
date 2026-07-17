// Package gemini implements the Gemini-to-OpenAI response translator and the
// OpenAI-to-Gemini/Gemini-CLI request translators. It registers itself on the
// translator registry at init time.
package gemini

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9router/internal/adapter/translator/shared"
	"github.com/Artiffusion-Inc/9router/internal/domain/format"
)

const (
	openaiBlockText     = "text"
	openaiBlockImage    = "image"
	openaiBlockImageURL = "image_url"
	openaiBlockFunction = "function"
	geminiRoleUser      = "user"
	geminiRoleModel     = "model"
	defaultImageMime    = "image/png"
)

var dataURIRe = regexp.MustCompile(`^data:([^;]+);base64,(.+)$`)

var defaultSafetySettings = []map[string]any{
	{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "OFF"},
	{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "OFF"},
	{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "OFF"},
	{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "OFF"},
	{"category": "HARM_CATEGORY_CIVIC_INTEGRITY", "threshold": "OFF"},
}

func init() {
	translator.RegisterRequest(format.Openai, format.Gemini, openaiToGeminiTranslator{})
	translator.RegisterRequest(format.Openai, format.GeminiCli, openaiToGeminiCLITranslator{})
	translator.RegisterRequest(format.Openai, format.Vertex, openaiToVertexTranslator{})
	translator.RegisterResponse(format.Gemini, format.Openai, geminiToOpenaiTranslator{})
	translator.RegisterResponse(format.GeminiCli, format.Openai, geminiToOpenaiTranslator{})
	translator.RegisterResponse(format.Antigravity, format.Openai, geminiToOpenaiTranslator{})
	translator.RegisterResponse(format.Vertex, format.Openai, geminiToOpenaiTranslator{})
}

type openaiToGeminiTranslator struct{}

func (openaiToGeminiTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	gemini, err := openaiToGeminiBase(model, body, defaultAGSignature)
	if err != nil {
		return nil, err
	}
	return json.Marshal(gemini)
}

type openaiToGeminiCLITranslator struct{}

func (openaiToGeminiCLITranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	gemini, err := openaiToGeminiBase(model, body, defaultGeminiCLISignature)
	if err != nil {
		return nil, err
	}
	if tools, ok := gemini["tools"].([]any); ok && len(tools) > 0 {
		if first, ok := tools[0].(map[string]any); ok {
			if fds, ok := first["functionDeclarations"].([]any); ok {
				for _, fdAny := range fds {
					fd, ok := fdAny.(map[string]any)
					if !ok {
						continue
					}
					if params, ok := fd["parameters"]; ok {
						fd["parameters"] = cleanJSONSchemaForAntigravity(params)
					}
				}
			}
		}
	}
	return json.Marshal(gemini)
}

type openaiToVertexTranslator struct{}

func (openaiToVertexTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	gemini, err := openaiToGeminiBase(model, body, defaultVertexSignature)
	if err != nil {
		return nil, err
	}
	postProcessForVertex(gemini)
	return json.Marshal(gemini)
}

const (
	defaultAGSignature         = "EuwGCukGAXLI2nxwZIq54WWSoL/YN0P3TsDZ7zRnLi8g0S4aVr2HUGxvaHKySuY6HAVzcE0GPGjXrytLIldxthSvfxgUlJh6Qa9Z+Oj5QZBlYdg6HaJ6yuY5R7waE6rdwBsRf7Ft2j3DJ9rMi9qhWFqApewYtPhls3VHtuvND3l8Rm09+lbAXQs6KKWEWrxNLKTBkfpMgXhRERc/TQRMZu1twAablm6/Zk1tsYRvfWKLsNbeKF+CCojJdXJKvnR/8Ouuoa+Y2Ti20hcW7aZIIjZDFYPU//k6Ybmhg69J/imbFai2ckhfLaisqdDkdoIiBJScTOUvYqP6AE9d4MsydSC+UlhIMk4hoP76R8vUSCZRMkjOaDXstf/QoVZKbt94wyRZgAJ1G0BqI8L5ow86kLpA4wJEtxsRGymOE4bKUvApveBakYDNM9APkf+LbtbzWSseGjoZcSlycF9iN8Q2XNYKRrHbv3Lr5Y8JjdH/5y/6SHkNehTEZugaeGnSPSyCTWto1kQgHpxdWmhkLfJGNUGLmue7Mesj4TSms4J33mRpYVhNB/J333FCqIP0hr/E7BkkjEn7yZ4X7SQlh+xKPurapsnHRwiKmtsilmEFrnTE9iQr+pMr6M29qqFNv1tr5yumbaJw8JW9sB15tNsRv+dW6BjNanbsKz7HCgKUBc8tGy+7YuhXzAfViyRefcjK7eZW0Fbyt7AbybJTKz78W8NH7ye6LAwzOebXpeZ4D43fNIt8bKh26qgduSQv/7o+pAflkuqHZ99YWgHQ8h8OkZFi3eOiSYjsjhdZ/czWOdoPI/OnqIldzMPF5YlrKBLFX8VhRKVmqgsmWf5PHGulHhMkVlS+XG2UIseGy69ARa93D78Gsa+1n1kJr7EEB7Rh+27vUMxVYLdz1yMSvE5nalTAlg/ZeG8+XQ0cHuAI3KbQpHW2Q++RdXfm5JzD5WdJZUU+Zn8t8UUn85BH4RxZLeE0qJikgSsKoYVBc6YhiMjhPgkR95ReimY4Z0xCJdRo1gjexOFeODZMpQF6Yxnoic7IrdgsFA3iePTbFnPp3IAM1fAThWhXJUn3QInUOTd5o1qmTmn6REbL15g/JQNl+dqUoPkhleeb2V3kjqp1okmO3wMZbPknR3S1LZNmlS72/iBQUm+n2b/RCn4PjmM2"
	defaultGeminiCLISignature = "CiQBjz1rX/AlslZWMe5RgBt4Tv9j4+YNZTTez+JH2/+5oAlICygKXgGPPWtf7/Sux9eLYap/bmYAdPqFThLXj+l7o0DLu/hdgU98MA9ZrlRDNHXx+T0tuY8AcnjPZbiDyOq2bE11Fjhsk6p5axqayaapC/Pt9GczcgIQf1z15WTxCeKWAPYKYQGPPWtfDYj0nlNFNoTlU39RC91Z16xFKJ2MLEmkm+NvimsoOJ6be3g2BssNPtJ/9BKDXRA5cVs17tBeeW72lH8TMB5999udtxHM2SiUsnWsrHlfVuGSCpNQQ+5REw8HNvEKkgEBjz1rXzBNWrqZGbjun55K+vgYPBhJO2qZ67uRWXUA5/qcU12U/mbi5XoA3swoxYE8LEXfZvFFC9WG/W28QNCA0Qd4Trk/WkWiAwZmB8a84Fs14rkv3wqyxwFavPkJorqurAfd2XzGiFy0sB0ITCOPYi1HzDGV5WfXk6b9k+jT66/RuzGa8EcSOWo/QtC3Bkhgowo4AY89a1/f/tw8A02zjIoK7JVDAbf8W4UfmbApJJhwXIiGtu1M0JItObx7g2reYqT+HHL2Q/R4VDc="
	defaultVertexSignature     = "CiQ1wY...vertex-signature-placeholder"
)

type geminiToOpenaiTranslator struct{}

func (geminiToOpenaiTranslator) TranslateResponse(chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(chunk, &body); err != nil {
		return nil, fmt.Errorf("unmarshal gemini chunk: %w", err)
	}
	out := geminiToOpenAIResponse(body, state)
	if out == nil {
		return nil, nil
	}
	results := make([]json.RawMessage, 0, len(out))
	for _, c := range out {
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		results = append(results, b)
	}
	return results, nil
}

func openaiToGeminiBase(model string, raw json.RawMessage, signature string) (map[string]any, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}

	result := map[string]any{
		"model":            model,
		"contents":         []any{},
		"generationConfig": map[string]any{},
		"safetySettings":   defaultSafetySettings,
	}
	gc := result["generationConfig"].(map[string]any)

	if v, ok := body["temperature"]; ok {
		gc["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		gc["topP"] = v
	}
	if v, ok := body["top_k"]; ok {
		gc["topK"] = v
	}
	if v, ok := body["max_tokens"]; ok {
		gc["maxOutputTokens"] = v
	}

	tcID2Name := map[string]string{}
	toolResponses := map[string]any{}
	var messages []map[string]any
	if rawMsgs, ok := body["messages"].([]any); ok {
		for _, m := range rawMsgs {
			if msg, ok := m.(map[string]any); ok {
				messages = append(messages, msg)
			}
		}
	}
	for _, msg := range messages {
		if msg["role"] == "assistant" {
			if rawTCs, ok := msg["tool_calls"].([]any); ok {
				for _, raw := range rawTCs {
					tc, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					if typ, _ := tc["type"].(string); typ != openaiBlockFunction {
						continue
					}
					fn, _ := tc["function"].(map[string]any)
					if id, _ := tc["id"].(string); id != "" {
						tcID2Name[id] = fmt.Sprintf("%v", fn["name"])
					}
				}
			}
		}
		if msg["role"] == "tool" {
			if id, _ := msg["tool_call_id"].(string); id != "" {
				toolResponses[id] = msg["content"]
			}
		}
	}

	contents := []map[string]any{}
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		content := msg["content"]
		if role == "system" && len(messages) > 1 {
			result["systemInstruction"] = map[string]any{
				"role": geminiRoleUser,
				"parts": []any{
					map[string]any{"text": extractTextContent(content)},
				},
			}
		} else if role == "user" || (role == "system" && len(messages) == 1) {
			parts := convertOpenAIContentToParts(content)
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": geminiRoleUser, "parts": parts})
			}
		} else if role == "assistant" {
			parts := []map[string]any{}
			if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
				parts = append(parts, map[string]any{"thought": true, "text": rc})
				parts = append(parts, map[string]any{"thoughtSignature": signature, "text": ""})
			}
			if content != nil {
				text := extractTextContent(content)
				if text != "" {
					parts = append(parts, map[string]any{"text": text})
				}
			}
			if rawTCs, ok := msg["tool_calls"].([]any); ok {
				toolCallIds := []string{}
				for _, raw := range rawTCs {
					tc, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					if typ, _ := tc["type"].(string); typ != openaiBlockFunction {
						continue
					}
					fn, _ := tc["function"].(map[string]any)
					name, _ := fn["name"].(string)
					argsRaw := fn["arguments"]
					args := tryParseJSON(argsRaw)
					parts = append(parts, map[string]any{
						"thoughtSignature": signature,
						"functionCall": map[string]any{
							"id":   tc["id"],
							"name": sanitizeGeminiFunctionName(name),
							"args": args,
						},
					})
					if id, _ := tc["id"].(string); id != "" {
						toolCallIds = append(toolCallIds, id)
					}
				}
				if len(parts) > 0 {
					contents = append(contents, map[string]any{"role": geminiRoleModel, "parts": parts})
				}
				hasActual := false
				for _, fid := range toolCallIds {
					if _, ok := toolResponses[fid]; ok {
						hasActual = true
						break
					}
				}
				if hasActual {
					toolParts := []map[string]any{}
					for _, fid := range toolCallIds {
						resp, ok := toolResponses[fid]
						if !ok {
							continue
						}
						name := tcID2Name[fid]
						if name == "" {
							name = inferNameFromID(fid)
						}
						parsedResp := tryParseJSON(resp)
						if parsedResp == nil {
							parsedResp = map[string]any{"result": resp}
						} else if _, ok := parsedResp.(map[string]any); !ok {
							parsedResp = map[string]any{"result": parsedResp}
						}
						toolParts = append(toolParts, map[string]any{
							"functionResponse": map[string]any{
								"id":       fid,
								"name":     sanitizeGeminiFunctionName(name),
								"response": map[string]any{"result": parsedResp},
							},
						})
					}
					if len(toolParts) > 0 {
						contents = append(contents, map[string]any{"role": geminiRoleUser, "parts": toolParts})
					}
				}
				continue
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": geminiRoleModel, "parts": parts})
			}
		}
	}

	if rawTools, ok := body["tools"].([]any); ok && len(rawTools) > 0 {
		functionDeclarations := []map[string]any{}
		for _, t := range rawTools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := tool["name"].(string); ok && name != "" {
				if schema, ok := tool["input_schema"]; ok {
					functionDeclarations = append(functionDeclarations, map[string]any{
						"name":        sanitizeGeminiFunctionName(name),
						"description": fmt.Sprintf("%v", tool["description"]),
						"parameters":  cleanJSONSchemaForAntigravity(schema),
					})
					continue
				}
			}
			if typ, _ := tool["type"].(string); typ == openaiBlockFunction {
				fn, _ := tool["function"].(map[string]any)
				name, _ := fn["name"].(string)
				functionDeclarations = append(functionDeclarations, map[string]any{
					"name":        sanitizeGeminiFunctionName(name),
					"description": fmt.Sprintf("%v", fn["description"]),
					"parameters":  cleanJSONSchemaForAntigravity(fn["parameters"]),
				})
			}
		}
		if len(functionDeclarations) > 0 {
			result["tools"] = []any{map[string]any{"functionDeclarations": functionDeclarations}}
		}
	}

	result["contents"] = normalizeGeminiContents(contents)
	return result, nil
}

func postProcessForVertex(body map[string]any) {
	contents, ok := body["contents"].([]any)
	if !ok {
		return
	}
	for _, cAny := range contents {
		c, ok := cAny.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := c["parts"].([]any)
		if !ok {
			continue
		}
		for _, pAny := range parts {
			part, ok := pAny.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := part["thoughtSignature"]; ok {
				part["thoughtSignature"] = defaultVertexSignature
			}
			if fc, ok := part["functionCall"].(map[string]any); ok {
				delete(fc, "id")
			}
			if fr, ok := part["functionResponse"].(map[string]any); ok {
				delete(fr, "id")
			}
		}
	}
}

func normalizeGeminiContents(contents []map[string]any) []any {
	out := []any{}
	for _, c := range contents {
		if c["role"] == "" || c["parts"] == nil {
			continue
		}
		parts, ok := c["parts"].([]map[string]any)
		if !ok || len(parts) == 0 {
			continue
		}
		if len(out) > 0 {
			last, _ := out[len(out)-1].(map[string]any)
			if last != nil && last["role"] == c["role"] {
				lastParts, _ := last["parts"].([]any)
				last["parts"] = append(lastParts, toAnySlice(parts)...)
				continue
			}
		}
		out = append(out, map[string]any{
			"role":  c["role"],
			"parts": toAnySlice(parts),
		})
	}
	return out
}

func toAnySlice(parts []map[string]any) []any {
	out := make([]any, len(parts))
	for i, p := range parts {
		out[i] = p
	}
	return out
}

func convertOpenAIContentToParts(content any) []map[string]any {
	parts := []map[string]any{}
	if content == nil {
		return parts
	}
	switch c := content.(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"text": c})
		}
	case []any:
		for _, itemAny := range c {
			item, ok := itemAny.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := item["type"].(string)
			switch typ {
			case openaiBlockText:
				if t, ok := item["text"].(string); ok && t != "" {
					parts = append(parts, map[string]any{"text": t})
				}
			case openaiBlockImageURL:
				url := ""
				if u, ok := item["image_url"].(string); ok {
					url = u
				} else if iu, ok := item["image_url"].(map[string]any); ok {
					url, _ = iu["url"].(string)
				}
				if m := dataURIRe.FindStringSubmatch(url); m != nil {
					parts = append(parts, map[string]any{"inlineData": map[string]any{"mime_type": m[1], "data": m[2]}})
				}
			case openaiBlockImage:
				if src, ok := item["source"].(map[string]any); ok {
					mime := fmt.Sprintf("%v", src["media_type"])
					data := fmt.Sprintf("%v", src["data"])
					parts = append(parts, map[string]any{"inlineData": map[string]any{"mime_type": mime, "data": data}})
				}
			}
		}
	}
	return parts
}

func extractTextContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var texts []string
		for _, itemAny := range c {
			if item, ok := itemAny.(map[string]any); ok {
				if item["type"] == openaiBlockText {
					if t, ok := item["text"].(string); ok {
						texts = append(texts, t)
					}
				}
			}
		}
		return strings.Join(texts, "")
	}
	return ""
}

func tryParseJSON(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func inferNameFromID(id string) string {
	parts := strings.Split(id, "-")
	if len(parts) > 2 {
		return strings.Join(parts[:len(parts)-2], "-")
	}
	if len(parts) > 0 {
		return parts[0]
	}
	return "tool"
}

func sanitizeGeminiFunctionName(name string) string {
	if name == "" {
		return "_unknown"
	}
	re := regexp.MustCompile(`[^a-zA-Z0-9_.:\\-]`)
	sanitized := re.ReplaceAllString(name, "_")
	if matched, _ := regexp.MatchString(`^[a-zA-Z_]`, sanitized); !matched {
		sanitized = "_" + sanitized
	}
	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}
	return sanitized
}

func geminiToOpenAIResponse(chunk map[string]any, state map[string]any) []map[string]any {
	if chunk == nil {
		return nil
	}
	response := chunk
	if r, ok := chunk["response"].(map[string]any); ok {
		response = r
	}
	candidates, ok := response["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return nil
	}
	candidate, ok := candidates[0].(map[string]any)
	if !ok {
		return nil
	}

	results := []map[string]any{}
	content, _ := candidate["content"].(map[string]any)

	if state["messageId"] == nil || state["messageId"] == "" {
		state["messageId"] = response["responseId"]
		if state["messageId"] == nil || state["messageId"] == "" {
			state["messageId"] = shared.GenerateMessageID()
		}
		state["model"] = response["modelVersion"]
		if state["model"] == nil || state["model"] == "" {
			state["model"] = "gemini"
		}
		state["functionIndex"] = 0
		state["geminiToolCallCount"] = 0
id, created, model := chunkMeta(state)
		state["toolTs"] = time.Now().UnixMilli()
		results = append(results, shared.BuildChunk(id, created, model, map[string]any{"role": "assistant"}, nil))
	}

	if parts, ok := content["parts"].([]any); ok {
		for _, partAny := range parts {
			part, ok := partAny.(map[string]any)
			if !ok {
				continue
			}
			hasThoughtSig := part["thoughtSignature"] != nil || part["thought_signature"] != nil
			isThought := part["thought"] == true

			if hasThoughtSig {
				if t, ok := part["text"].(string); ok && t != "" {
					delta := map[string]any{"content": t}
					if isThought {
						delta = shared.ReasoningDelta(t)
					}
id, created, model := chunkMeta(state)
					results = append(results, shared.BuildChunk(id, created, model, delta, nil))
				}
				if fc, ok := part["functionCall"].(map[string]any); ok {
					results = append(results, emitFunctionCall(fc, state))
				}
				continue
			}

			if t, ok := part["text"].(string); ok && t != "" {
				delta := map[string]any{"content": t}
				if isThought {
					delta = shared.ReasoningDelta(t)
				}
id, created, model := chunkMeta(state)
				results = append(results, shared.BuildChunk(id, created, model, delta, nil))
			}
			if fc, ok := part["functionCall"].(map[string]any); ok {
				results = append(results, emitFunctionCall(fc, state))
			}
			inlineData, _ := part["inlineData"].(map[string]any)
			if inlineData == nil {
				inlineData, _ = part["inline_data"].(map[string]any)
			}
			if inlineData != nil && inlineData["data"] != nil {
				mimeType := fmt.Sprintf("%v", inlineData["mimeType"])
				if mimeType == "" {
					mimeType = fmt.Sprintf("%v", inlineData["mime_type"])
				}
				if mimeType == "" {
					mimeType = defaultImageMime
				}
id, created, model := chunkMeta(state)
				results = append(results, shared.BuildChunk(id, created, model, map[string]any{
					"images": []any{
						map[string]any{
							"type":      openaiBlockImageURL,
							"image_url": map[string]any{"url": encodeDataURI(mimeType, fmt.Sprintf("%v", inlineData["data"]))},
						},
					},
				}, nil))
			}
		}
	}

	if usageMeta, ok := response["usageMetadata"].(map[string]any); ok {
		u := shared.ToOpenAIUsage(usageMeta, "gemini")
		if u != nil {
			state["usage"] = u
		}
	}

	if finishReason, ok := candidate["finishReason"].(string); ok && finishReason != "" {
		fr := shared.ToOpenAIFinish(finishReason, "gemini")
		if fr == "stop" && shared.Number(state["geminiToolCallCount"]) > 0 {
			fr = "tool_calls"
		}
		id, created, model := chunkMeta(state)
	finalChunk := shared.BuildChunk(id, created, model, map[string]any{}, fr)
		if u, ok := state["usage"].(map[string]any); ok {
			finalChunk["usage"] = u
		}
		results = append(results, finalChunk)
		state["finishReason"] = fr
	}

	if len(results) == 0 {
		return nil
	}
	return results
}

func chunkMeta(state map[string]any) (string, int, string) {
	id := fmt.Sprintf("chatcmpl-%v", state["messageId"])
	created := 0
	if v, ok := state["created"].(int); ok {
		created = v
	}
	model := "gemini"
	if v, ok := state["model"].(string); ok && v != "" {
		model = v
	}
	return id, created, model
}

func emitFunctionCall(functionCall map[string]any, state map[string]any) map[string]any {
	rawName := fmt.Sprintf("%v", functionCall["name"])
	fcName := rawName
	if m, ok := state["toolNameMap"].(map[string]string); ok {
		if orig, ok := m[rawName]; ok {
			fcName = orig
		}
	}
	args := map[string]any{}
	if a, ok := functionCall["args"].(map[string]any); ok {
		args = a
	}
	toolCallIndex := 0
	if v, ok := state["functionIndex"].(int); ok {
		toolCallIndex = v
	}
	state["functionIndex"] = toolCallIndex + 1
	count := shared.Number(state["geminiToolCallCount"]) + 1
	state["geminiToolCallCount"] = count

	ts := int64(0)
	if v, ok := state["toolTs"].(int64); ok {
		ts = v
	}
	if ts == 0 {
		ts = time.Now().UnixMilli()
		state["toolTs"] = ts
	}
	toolCall := map[string]any{
		"id":    fmt.Sprintf("%s-%d-%d", fcName, ts, toolCallIndex),
		"index": toolCallIndex,
		"type":  "function",
		"function": map[string]any{
			"name":      fcName,
			"arguments": marshalJSON(args),
		},
	}
	id, created, model := chunkMeta(state)
	return shared.BuildChunk(id, created, model, map[string]any{"tool_calls": []any{toolCall}}, nil)
}

func encodeDataURI(mimeType, data string) string {
	return fmt.Sprintf("data:%s;base64,%s", mimeType, data)
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func cleanJSONSchemaForAntigravity(schema any) any {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}

	out := shallowCopy(m)
	convertConstToEnum(out)
	convertEnumValuesToStrings(out)
	mergeAllOf(out)
	flattenAnyOfOneOf(out)
	flattenTypeArrays(out)
	ensureObjectType(out)
	removeUnsupportedKeywords(out)
	cleanupRequired(out)
	addPlaceholders(out)
	return out
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func removeUnsupportedKeywords(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		return
	}
	for _, k := range unsupportedSchemaConstraints {
		delete(m, k)
	}
	for _, k := range unsupportedSchemaConstraints {
		delete(m, k)
	}
	for k := range m {
		if strings.HasPrefix(k, "x-") {
			delete(m, k)
		}
	}
	for _, v := range m {
		removeUnsupportedKeywords(v)
	}
}

var unsupportedSchemaConstraints = []string{
	"minLength", "maxLength", "exclusiveMinimum", "exclusiveMaximum",
	"minItems", "maxItems", "format", "default", "examples", "$schema", "$defs",
	"definitions", "const", "$ref", "$comment", "deprecated", "readOnly", "writeOnly",
	"additionalProperties", "propertyNames", "patternProperties", "enumDescriptions",
	"anyOf", "oneOf", "allOf", "not", "dependencies", "dependentSchemas", "dependentRequired",
	"title", "optional", "if", "then", "else", "contentMediaType", "contentEncoding",
	"cornerRadius", "fillColor", "fontFamily", "fontSize", "fontWeight", "gap", "padding",
	"strokeColor", "strokeThickness", "textColor",
}

func convertConstToEnum(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		return
	}
	if c, ok := m["const"]; ok {
		m["enum"] = []any{c}
		delete(m, "const")
	}
	for _, v := range m {
		convertConstToEnum(v)
	}
}

func convertEnumValuesToStrings(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		return
	}
	if enum, ok := m["enum"].([]any); ok {
		for i, v := range enum {
			enum[i] = fmt.Sprintf("%v", v)
		}
		if m["type"] == nil {
			m["type"] = "string"
		}
	}
	for _, v := range m {
		convertEnumValuesToStrings(v)
	}
}

func mergeAllOf(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		return
	}
	if allOf, ok := m["allOf"].([]any); ok {
		merged := map[string]any{}
		props := map[string]any{}
		required := []string{}
		for _, itemAny := range allOf {
			item, ok := itemAny.(map[string]any)
			if !ok {
				continue
			}
			if p, ok := item["properties"].(map[string]any); ok {
				for k, v := range p {
					props[k] = v
				}
			}
			if r, ok := item["required"].([]any); ok {
				for _, rAny := range r {
					required = append(required, fmt.Sprintf("%v", rAny))
				}
			}
		}
		if len(props) > 0 {
			merged["properties"] = props
		}
		if len(required) > 0 {
			merged["required"] = required
		}
		delete(m, "allOf")
		for k, v := range merged {
			if existing, ok := m[k]; ok {
				if existingMap, ok := existing.(map[string]any); ok {
					if vMap, ok := v.(map[string]any); ok {
						for kk, vv := range existingMap {
							if _, exists := vMap[kk]; !exists {
								vMap[kk] = vv
							}
						}
					}
				}
			} else {
				m[k] = v
			}
		}
	}
	for _, v := range m {
		mergeAllOf(v)
	}
}

func flattenAnyOfOneOf(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if arr, ok := m[key].([]any); ok && len(arr) > 0 {
			var nonNull []any
			for _, item := range arr {
				if itemMap, ok := item.(map[string]any); ok && itemMap["type"] != "null" {
					nonNull = append(nonNull, item)
				}
			}
			if len(nonNull) > 0 {
				best := selectBestSchema(nonNull)
				if bestMap, ok := best.(map[string]any); ok {
					delete(m, key)
					for k, v := range bestMap {
						m[k] = v
					}
				}
			}
		}
	}
	for _, v := range m {
		flattenAnyOfOneOf(v)
	}
}

func selectBestSchema(schemas []any) any {
	bestIdx := 0
	bestScore := -1
	for i, item := range schemas {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		score := 0
		typ := fmt.Sprintf("%v", itemMap["type"])
		if typ == "object" || itemMap["properties"] != nil {
			score = 3
		} else if typ == "array" || itemMap["items"] != nil {
			score = 2
		} else if typ != "" && typ != "null" {
			score = 1
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	return schemas[bestIdx]
}

func flattenTypeArrays(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		return
	}
	if types, ok := m["type"].([]any); ok {
		var nonNull []string
		for _, t := range types {
			if fmt.Sprintf("%v", t) != "null" {
				nonNull = append(nonNull, fmt.Sprintf("%v", t))
			}
		}
		if len(nonNull) > 0 {
			m["type"] = nonNull[0]
		} else {
			m["type"] = "string"
		}
	}
	for _, v := range m {
		flattenTypeArrays(v)
	}
}

func ensureObjectType(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		return
	}
	if m["properties"] != nil && m["type"] == nil {
		m["type"] = "object"
	}
	for _, v := range m {
		ensureObjectType(v)
	}
}

func cleanupRequired(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		return
	}
	if required, ok := m["required"].([]any); ok {
		props, _ := m["properties"].(map[string]any)
		valid := []string{}
		for _, rAny := range required {
			r := fmt.Sprintf("%v", rAny)
			if props != nil {
				if _, ok := props[r]; ok {
					valid = append(valid, r)
				}
			}
		}
		if len(valid) == 0 {
			delete(m, "required")
		} else {
			m["required"] = toAnyStringSlice(valid)
		}
	}
	for _, v := range m {
		cleanupRequired(v)
	}
}

func toAnyStringSlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func addPlaceholders(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		return
	}
	if m["type"] == "object" {
		props, _ := m["properties"].(map[string]any)
		if props == nil || len(props) == 0 {
			m["properties"] = map[string]any{
				"reason": map[string]any{
					"type":        "string",
					"description": "Brief explanation of why you are calling this tool",
				},
			}
			m["required"] = []any{"reason"}
		}
	}
	for _, v := range m {
		addPlaceholders(v)
	}
}
