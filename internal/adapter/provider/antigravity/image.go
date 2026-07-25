package antigravityexec

// image.go ports the Antigravity image-generation half of the legacy
// open-sse/executors/antigravity.js transformRequest (isImageModel branch) +
// open-sse/handlers/imageProviders/antigravity.js normalize. It builds the
// `image_gen` envelope (a sibling of the text `agent` envelope from
// envelope.go) and extracts inline image data from the Gemini candidates
// response.
//
// The image path is intentionally separate from the text path: image models
// use a non-streaming `POST /v1internal:generateContent`, `requestType:
// "image_gen"`, a cleaned model name (terminal `-(\d+)x(\d+)$` suffix
// stripped), a fixed `generationConfig` (temperature/topP/topK/maxOutputTokens
// 8192 + imageConfig.aspectRatio), text-only merged contents, and NO
// tools/systemInstruction/safetySettings. Auth remains OAuth Bearer (handled
// by the executor / wire adapter), never `?key=` query.
//
// The envelope helpers (resolveAntigravitySessionID, antigravityProjectID,
// buildAntigravityIDERequestID, uuidFromSeed) are shared with envelope.go so
// session id, project id and the IDE-shaped requestId stay consistent with
// the text path. parseImageConfig and isImageModel mirror the JS legacy
// 1:1.

import (
	"encoding/json"
	"regexp"
	"strconv"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// imageModelPatterns mirrors the JS IMAGE_MODEL_PATTERNS (image, imagen,
// image-generation). Case-insensitive like the JS /i flag.
var imageModelPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)image`),
	regexp.MustCompile(`(?i)imagen`),
	regexp.MustCompile(`(?i)image-generation`),
}

// imageModelSuffixRe strips the terminal `-<w>x<h>` resolution/ratio suffix
// from an image model id (`gemini-3.1-flash-image-1024x768` →
// `gemini-3.1-flash-image`). Mirrors the JS `model.replace(/-(\d+)x(\d+)$/, "")`.
var imageModelSuffixRe = regexp.MustCompile(`-(\d+)x(\d+)$`)

// imageResolutionRe matches the terminal `<w>x<h>` suffix used by
// parseImageConfig.
var imageResolutionRe = regexp.MustCompile(`(\d+)x(\d+)$`)

// isImageModel reports whether the model id names an Antigravity image
// generation model. Mirrors the JS isImageModel.
func isImageModel(model string) bool {
	if model == "" {
		return false
	}
	for _, p := range imageModelPatterns {
		if p.MatchString(model) {
			return true
		}
	}
	return false
}

// parseImageConfig derives the `imageConfig.aspectRatio` from the model id's
// terminal `-(\d+)x(\d+)$` suffix. A small ratio (both dims ≤ 16, e.g.
// `16x9` → `16:9`) is emitted literally; a resolution (e.g. `1024x768`) is
// reduced by its GCD (`4:3`). Absent suffix → `1:1`. Mirrors the JS
// parseImageConfig.
func parseImageConfig(model string) map[string]string {
	config := map[string]string{"aspectRatio": "1:1"}
	m := imageResolutionRe.FindStringSubmatch(model)
	if m == nil {
		return config
	}
	w := atoiPositive(m[1])
	h := atoiPositive(m[2])
	if w <= 0 || h <= 0 {
		return config
	}
	if w <= 16 && h <= 16 {
		config["aspectRatio"] = strconv.Itoa(w) + ":" + strconv.Itoa(h)
		return config
	}
	d := gcd(w, h)
	if d == 0 {
		return config
	}
	config["aspectRatio"] = strconv.Itoa(w/d) + ":" + strconv.Itoa(h/d)
	return config
}

// cleanImageModel strips the terminal `-<w>x<h>` suffix from an image model
// id. Mirrors `model.replace(/-(\d+)x(\d+)$/, "")`.
func cleanImageModel(model string) string {
	return imageModelSuffixRe.ReplaceAllString(model, "")
}

// transformImageRequest builds the `image_gen` envelope around a
// Gemini-shaped chat body. It:
//   - strips the terminal `-<w>x<h>` suffix from the model id;
//   - merges every user message's text parts into text-only `contents`
//     (image parts / function parts / thoughts are dropped, mirroring the JS
//     `parts.filter(p => p.text !== undefined)`);
//   - sets the fixed `generationConfig` (temperature 1, topP 0.95, topK 40,
//     maxOutputTokens 8192, imageConfig.aspectRatio);
//   - resolves sessionId + projectId the same way as the text path;
//   - derives an IDE-shaped requestId with requestType "image_gen".
//
// The body shape mirrors the legacy image adapter: callers pass a Gemini
// `contents` body (the imageproxy synthAntigravity helper builds `{contents:
// [{role:"user", parts:[{text}, inlineData...]}]}`). Both `body.contents` and
// `body.request.contents` are accepted (the legacy transformRequest reads
// `body.request?.contents || body.contents`).
func (e *Executor) transformImageRequest(model string, body json.RawMessage, creds provider.Credentials) (json.RawMessage, error) {
	var m map[string]any
	if len(body) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}

	// Source contents: body.request.contents, else body.contents (the
	// imageproxy synthAntigravity helper passes body.contents directly).
	var srcContents []any
	if req, ok := m["request"].(map[string]any); ok {
		if c, ok := req["contents"].([]any); ok {
			srcContents = c
		}
	}
	if srcContents == nil {
		if c, ok := m["contents"].([]any); ok {
			srcContents = c
		}
	}

	// Text-only merged contents with image-edit inputs preserved: keep parts
	// carrying a `text` field (mirroring the legacy `parts.filter(p => p.text
	// !== undefined)`) AND inlineData parts (image-edit inputs the legacy
	// imageProviders/antigravity.js adapter explicitly unshifts before the
	// text part). The spec mandates antigravity accepts image-edit inputs;
	// dropping inlineData here would silently break img2img. Other part kinds
	// (functionCall, functionResponse, thought, thoughtSignature) are dropped
	// — image gen has no tool calling.
	contents := make([]any, 0, len(srcContents))
	for _, c := range srcContents {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		parts, _ := cm["parts"].([]any)
		kept := make([]any, 0, len(parts))
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if _, has := pm["text"]; has {
				kept = append(kept, map[string]any{"text": pm["text"]})
				continue
			}
			if _, has := pm["inlineData"]; has {
				kept = append(kept, map[string]any{"inlineData": pm["inlineData"]})
				continue
			}
		}
		if len(kept) > 0 {
			role, _ := cm["role"].(string)
			if role == "" {
				role = "user"
			}
			contents = append(contents, map[string]any{"role": role, "parts": kept})
		}
	}

	// resolveAntigravitySessionID reads request.sessionId / connectionId /
	// email. When the body has no `request` object the session still resolves
	// from credentials (the image proxy always carries connection creds).
	var reqMap map[string]any
	if r, ok := m["request"].(map[string]any); ok {
		reqMap = r
	}
	sessionID := resolveAntigravitySessionID(reqMap, creds)
	projectID := antigravityProjectID(creds, sessionID)

	cleanModel := cleanImageModel(model)
	request := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"temperature":     1.0,
			"topP":            0.95,
			"topK":            40,
			"maxOutputTokens": 8192,
			"imageConfig":     parseImageConfig(model),
		},
		"sessionId": sessionID,
		// No tools, no systemInstruction, no safetySettings for image gen.
	}

	requestType := "image_gen"
	ideReqID := buildAntigravityIDERequestID(m, request, creds, cleanModel, requestType, sessionID)

	envelope := map[string]any{
		"project":     projectID,
		"model":       cleanModel,
		"userAgent":   antigravityIDEUserAgent,
		"requestType": requestType,
		"requestId":   ideReqID,
		"request":     request,
	}
	return json.Marshal(envelope)
}

// extractImageFromResponse walks the Gemini candidates response and collects
// every `parts[].inlineData.data` base64 blob. It mirrors the legacy
// imageProviders/antigravity.js normalize: candidates (or
// response.candidates) → first candidate → content.parts → inlineData.data.
//
// Unlike the legacy normalize (which returns `{created, data:[{b64_json}]}`),
// this helper returns only the raw base64 slice; the imageproxy synthAntigravity
// helper shapes the OpenAI output (created, data, revised_prompt fallback).
func extractImageFromResponse(respBody []byte) []string {
	var top map[string]any
	if err := json.Unmarshal(respBody, &top); err != nil {
		return nil
	}
	var out []string
	// candidates may live at the top level or nested under "response" (legacy
	// normalize reads responseBody.candidates || responseBody.response.candidates).
	collectFrom := func(candidatesKey string) {
		candsAny, ok := top[candidatesKey].([]any)
		if !ok {
			return
		}
		for _, c := range candsAny {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			content, _ := cm["content"].(map[string]any)
			if content == nil {
				continue
			}
			parts, _ := content["parts"].([]any)
			for _, p := range parts {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				ind, _ := pm["inlineData"].(map[string]any)
				if ind == nil {
					continue
				}
				if data, _ := ind["data"].(string); data != "" {
					out = append(out, data)
				}
			}
		}
	}
	collectFrom("candidates")
	// Nested response.candidates fallback.
	if resp, _ := top["response"].(map[string]any); resp != nil {
		candsAny, _ := resp["candidates"].([]any)
		for _, c := range candsAny {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			content, _ := cm["content"].(map[string]any)
			if content == nil {
				continue
			}
			parts, _ := content["parts"].([]any)
			for _, p := range parts {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				ind, _ := pm["inlineData"].(map[string]any)
				if ind == nil {
					continue
				}
				if data, _ := ind["data"].(string); data != "" {
					out = append(out, data)
				}
			}
		}
	}
	return out
}

// gcd returns the greatest common divisor of a and b (Euclid). Mirrors the JS
// `const gcd = (a, b) => b ? gcd(b, a % b) : a`.
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// atoiPositive parses a non-negative integer from a decimal string, returning 0
// on any non-digit content. Used only by parseImageConfig on already-regex-
// matched digit groups, so the guard is defensive.
func atoiPositive(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
