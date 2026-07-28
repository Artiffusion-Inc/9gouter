package imageproxy

// antigravity.go implements the Antigravity image-generation adapter. Unlike
// the other adapters (which build an outbound *http.Request and hand it to
// HTTPExecutor), Antigravity delegates to the image-capable
// AntigravityImageExecutor boundary: the production adapter (app/wire.go)
// wraps the real Antigravity provider executor and preserves OAuth bearer
// auth, project-ID resolution, refresh/account behavior and the existing
// connection-aware proxy route. imageproxy never imports the antigravity
// provider package — the wire adapter bridges.
//
// The adapter builds the legacy Gemini image envelope (text-only merged
// contents + optional image inlineData for image-edit inputs), invokes the
// injected executor, and normalizes the Gemini candidates response into the
// OpenAI {created, data:[{b64_json}]} shape (or raw binary via the common
// decoder). It mirrors open-sse/handlers/imageProviders/antigravity.js
// (resolveImageInput + executeViaExecutor + normalize).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
)

// synthAntigravity builds the legacy Gemini image contents (text prompt +
// optional image inlineData from a validated data-URL input), invokes the
// injected AntigravityImageExecutor, and normalizes the Gemini candidates
// response into the OpenAI {created, data:[{b64_json}]} shape. Returns 501
// when no executor is injected (production wiring always provides one).
//
// The adapter never puts the credential in the URL — the wire adapter applies
// OAuth Bearer via the provider executor's BuildHeaders. The imageConfig
// generationConfig is applied by the provider executor's image transform
// (antigravity/image.go), NOT by the text `requestType:agent` envelope.
func (h *Handler) synthAntigravity(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	if h.deps.AntigravityExecutor == nil {
		return nil, "", http.StatusNotImplemented, fmt.Errorf("antigravity image executor not wired")
	}

	// Build the Gemini-shaped contents: text prompt first, then optional image
	// inlineData (image-edit input) unshifted before the text part, mirroring
	// the legacy resolveImageInput + parts.unshift(inlineData) order.
	parts := []any{map[string]any{"text": req.Prompt}}
	if len(req.Options.ImageInputs) > 0 {
		in := req.Options.ImageInputs[0]
		if in.Kind == "data" && in.B64 != "" {
			mime := in.MIME
			if mime == "" {
				mime = "image/png"
			}
			parts = append([]any{map[string]any{
				"inlineData": map[string]any{
					"mimeType": mime,
					"data":     in.B64,
				},
			}}, parts...)
		}
	}
	contents := []any{map[string]any{
		"role":  "user",
		"parts": parts,
	}}
	body, _ := json.Marshal(map[string]any{"contents": contents})

	resp, err := h.deps.AntigravityExecutor.ExecuteImage(ctx, AntigravityImageRequest{
		Model:       req.Model,
		Contents:    body,
		Credentials: req.Credentials,
	})
	if err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("antigravity: %w", err)
	}
	if resp.Err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("antigravity: %w", resp.Err)
	}
	if resp.StatusCode >= 400 {
		return nil, "", resp.StatusCode, upstreamError(resp.Body)
	}

	// Extract inline image data from the Gemini candidates response.
	images := extractAntigravityImages(resp.Body)
	if len(images) == 0 {
		// Legacy parity: empty image + revised_prompt fallback so the client
		// gets a well-formed OpenAI body even when upstream returned no image.
		out := map[string]any{
			"created": time.Now().Unix(),
			"data":    []any{map[string]any{"b64_json": "", "revised_prompt": req.Prompt}},
		}
		outBody, _ := json.Marshal(out)
		if req.ResponseFormat == "binary" {
			return nil, "", http.StatusBadGateway, fmt.Errorf("antigravity: no image in response")
		}
		return outBody, "application/json", http.StatusOK, nil
	}

	data := make([]any, 0, len(images))
	for _, b64 := range images {
		data = append(data, map[string]any{"b64_json": b64})
	}
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	}
	outBody, _ := json.Marshal(out)
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// extractAntigravityImages walks the Gemini candidates response and collects
// every `parts[].inlineData.data` base64 blob. It mirrors the legacy
// imageProviders/antigravity.js normalize (candidates || response.candidates
// → first candidate → content.parts → inlineData.data). Returns nil when no
// inline image data is present.
func extractAntigravityImages(respBody []byte) []string {
	var top map[string]any
	if err := json.Unmarshal(respBody, &top); err != nil {
		return nil
	}
	var out []string
	collectFrom := func(candsAny any) {
		cands, _ := candsAny.([]any)
		for _, c := range cands {
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
	collectFrom(top["candidates"])
	if resp, _ := top["response"].(map[string]any); resp != nil {
		collectFrom(resp["candidates"])
	}
	return out
}
