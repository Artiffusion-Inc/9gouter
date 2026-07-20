package embedding

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// geminiBase is the Gemini v1beta root used for embedContent /
// batchEmbedContents, mirroring open-sse/handlers/embeddingProviders/gemini.js.
const geminiBase = "https://generativelanguage.googleapis.com/v1beta"

// geminiAdapter translates the OpenAI /v1/embeddings request shape into the
// Gemini embedContent (single input) / batchEmbedContents (array input) shape
// and normalizes the response back to OpenAI shape.
type geminiAdapter struct{}

func (geminiAdapter) BuildURL(model string, creds domainProv.Credentials, p Params) string {
	path := modelPath(model)
	op := "embedContent"
	if isArrayInput(p.Input) {
		op = "batchEmbedContents"
	}
	key := creds.APIKey
	if key == "" {
		key = creds.AccessToken
	}
	return geminiBase + "/" + path + ":" + op + "?key=" + url.QueryEscape(key)
}

func (geminiAdapter) BuildHeaders(_ domainProv.Credentials, _ Params) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return h
}

func (geminiAdapter) BuildBody(model string, p Params) ([]byte, error) {
	m := modelPath(model)
	outDim := normalizeDimensions(p.Dimensions)
	hasDim := outDim > 0
	if items, ok := p.Input.([]any); ok {
		requests := make([]map[string]any, 0, len(items))
		for _, item := range items {
			requests = append(requests, geminiRequest(m, item, hasDim, outDim))
		}
		return json.Marshal(map[string]any{"requests": requests})
	}
	return json.Marshal(geminiRequest(m, p.Input, hasDim, outDim))
}

// geminiRequest builds a single embedContent request entry.
func geminiRequest(model string, input any, hasDim bool, outDim int) map[string]any {
	req := map[string]any{
		"model":   model,
		"content": map[string]any{"parts": []map[string]any{{"text": toString(input)}}},
	}
	if hasDim {
		req["outputDimensionality"] = outDim
	}
	return req
}

// Normalize converts a Gemini embeddings response into the OpenAI shape.
// Handles embedContent ({embedding:{values}}) and batchEmbedContents
// ({embeddings:[{values}]}) as well as already-OpenAI-shaped passthrough.
func (geminiAdapter) Normalize(body []byte, model string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	// Passthrough when already OpenAI-shaped.
	if obj, _ := raw["object"].(string); obj == "list" {
		if _, ok := raw["data"].([]any); ok {
			return body, nil
		}
	}
	var items []map[string]any
	if embeddings, ok := raw["embeddings"].([]any); ok {
		for idx, emb := range embeddings {
			if m, ok := emb.(map[string]any); ok {
				items = append(items, map[string]any{
					"object":   "embedding",
					"index":    idx,
					"embedding": valuesOf(m),
				})
			}
		}
	} else if emb, ok := raw["embedding"].(map[string]any); ok {
		items = []map[string]any{{
			"object":   "embedding",
			"index":    0,
			"embedding": valuesOf(emb),
		}}
	}
	out := map[string]any{
		"object": "list",
		"data":   items,
		"model":  model,
		"usage": map[string]any{
			"prompt_tokens": 0,
			"total_tokens":  0,
		},
	}
	return json.Marshal(out)
}

// valuesOf returns the embedding vector from a Gemini embedding object
// ({values:[...]}), defaulting to [].
func valuesOf(m map[string]any) any {
	if v, ok := m["values"]; ok {
		return v
	}
	return []any{}
}

// modelPath ensures a "models/" prefix, mirroring the JS modelPath helper.
func modelPath(model string) string {
	if strings.HasPrefix(model, "models/") {
		return model
	}
	return "models/" + model
}

// isArrayInput reports whether the input is an array (batch path).
func isArrayInput(input any) bool {
	_, ok := input.([]any)
	return ok
}

// toString coerces an input item to its string form.
func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}