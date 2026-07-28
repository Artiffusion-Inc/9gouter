package antigravityexec

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// envelope.go ports the 71cd5b2f Antigravity IDE-fingerprint half: the
// request envelope (project / model / userAgent / requestType / requestId)
// wrapped around the Gemini-shaped `request` body, with an IDE-shaped
// requestId derived deterministically from the session (uuidFromSeed →
// agent/<conversationId>/<timestamp>/<trajectoryId>/<step>) and a
// maxOutputTokens cap of 64000. Mirrors open-sse/executors/antigravity.js
// transformRequest + buildIdeRequestId.
//
// The Go antigravity executor is otherwise a thin BaseExecutor wrapper, so
// the envelope is constructed here in TransformRequest and the body is
// committed to the upstream by overriding Execute (which calls this
// TransformRequest explicitly before delegating to BaseExecutor.Execute).
// The double-system-prompt injection the JS fix removes does not exist in Go
// (the gemini translator never injected ANTIGRAVITY_DEFAULT_SYSTEM), so only
// the envelope + requestId + cap half is ported.

const (
	// maxAntigravityOutputTokens mirrors the JS MAX_ANTIGRAVITY_OUTPUT_TOKENS
	// (71cd5b2f bumped 16384 → 64000). Antigravity rejects larger caps.
	maxAntigravityOutputTokens = 64000
	// antigravityIDEUserAgent is the envelope userAgent field value (the HTTP
	// User-Agent header is set in registry.go).
	antigravityIDEUserAgent = "antigravity"
)

// blacklistedFields are body-root fields Google generateContent rejects
// (Claude/OpenAI/Qwen thinking fields). Mirrors ANTIGRAVITY_REQUEST_BLACKLIST.
var antigravityRequestBlacklist = []string{
	"output_config",
	"thinking",
	"reasoning_effort",
	"reasoning",
	"enable_thinking",
	"thinking_budget",
	"thinkingConfig",
}

// TransformRequest wraps the Gemini-shaped request body in the Antigravity
// envelope: { project, model, userAgent, requestType:"agent", requestId,
// request }. It strips blacklisted thinking fields, caps
// request.generationConfig.maxOutputTokens at 64000, and derives an IDE-shaped
// requestId from the session id (uuidFromSeed). A body already carrying an
// IDE-shaped requestId keeps it (idempotent). The session id resolves from
// request.sessionId, then credentials.connectionId / email, else "anonymous".
//
// Text-only MVP: image generation (image_gen requestType, imageConfig) is a
// separate feature and not part of 71cd5b2f; image models pass through with a
// capped envelope but no imageConfig reshaping.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	var m map[string]any
	if len(body) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}

	// Strip blacklisted thinking fields from the top-level body (set at root by
	// thinkingUnified in JS; harmless no-op in Go where they are not set there).
	stripBlacklisted(m)

	// request is the Gemini-shaped payload (contents/generationConfig/tools).
	req, _ := m["request"].(map[string]any)
	if req == nil {
		req = map[string]any{}
	}
	stripBlacklisted(req)

	// Cap maxOutputTokens at 64000.
	if gc, ok := req["generationConfig"].(map[string]any); ok {
		if cur, n := gc["maxOutputTokens"]; n {
			if asInt(cur) > maxAntigravityOutputTokens {
				gc["maxOutputTokens"] = maxAntigravityOutputTokens
			}
		}
	}

	// sessionId resolution mirrors resolveSessionId fallback chain.
	sessionID := resolveAntigravitySessionID(req, creds)

	// project: credentials.projectId, else generated (deterministic from
	// session so a replay yields the same project — the JS generateProjectId
	// is random; we make it deterministic to keep tests stable and avoid
	// Math/random which is unavailable in workflow scripts).
	projectID := antigravityProjectID(creds, sessionID)

	requestType := "agent"
	ideReqID := buildAntigravityIDERequestID(m, req, creds, model, requestType, sessionID)

	envelope := map[string]any{
		"project":     projectID,
		"model":       model,
		"userAgent":   antigravityIDEUserAgent,
		"requestType": requestType,
		"requestId":   ideReqID,
		"request":     req,
	}
	// Carry over any non-blacklisted top-level fields the body carried besides
	// `request` (e.g. tools handled by the translator already inside request).
	for k, v := range m {
		if k == "request" {
			continue
		}
		if _, keep := envelope[k]; !keep {
			envelope[k] = v
		}
	}
	return json.Marshal(envelope)
}

// Execute overrides BaseExecutor.Execute so the envelope from TransformRequest
// is actually applied. The embedded-method limitation (#142) means a
// TransformRequest override on this wrapper is NOT dispatched from the
// promoted *BaseExecutor.Execute, so we call it explicitly here (where the
// receiver is *Executor and the override dispatches), then delegate to
// BaseExecutor.Execute with the already-transformed body. BaseExecutor's own
// TransformRequest is a passthrough, so applying it twice is a no-op.
//
// Image models branch to a dedicated fetch path: the `image_gen` envelope from
// image.go, the non-streaming generateContent URL from the BuildURL override,
// and a single upstream call through the same Fetch/HTTPClient/ProxyOpts seam
// BaseExecutor uses. The image path bypasses BaseExecutor.Execute because
// BaseExecutor.BuildURL (called inside Execute) does NOT dispatch the
// Executor.BuildURL override (#142 again) and would drop the
// /v1internal:generateContent path segment. Retry/fallback parity is
// intentionally not reproduced for image gen — the legacy
// imageProviders/antigravity.js adapter did not retry image calls either.
func (e *Executor) Execute(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	if isImageModel(req.Model) {
		return e.executeImage(ctx, req)
	}
	transformed, err := e.TransformRequest(req.Model, req.Body, req.Stream, req.Credentials)
	if err != nil {
		return provider.Resp{}, fmt.Errorf("antigravity transform: %w", err)
	}
	req.Body = transformed
	return e.BaseExecutor.Execute(ctx, req)
}

// executeImage runs the image_gen generateContent call. It applies the image
// envelope, builds the non-streaming URL via the BuildURL override (which
// #142 prevents BaseExecutor.Execute from dispatching), and performs one
// upstream fetch through the same proxy-aware seam. Auth headers come from
// BaseExecutor.BuildHeaders (OAuth Bearer via the config AuthDescriptor), so
// the credential is never put in the URL.
func (e *Executor) executeImage(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	transformed, err := e.transformImageRequest(req.Model, req.Body, req.Credentials)
	if err != nil {
		return provider.Resp{}, fmt.Errorf("antigravity image transform: %w", err)
	}
	req.Body = transformed
	req.Stream = false // image gen is always non-streaming

	url := e.BuildURL(req.Model, false, 0, req.Credentials)
	headers := e.BuildHeaders(req.Credentials, false)

	bodyStr := string(transformed)
	if transformed == nil {
		bodyStr = ""
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(bodyStr))
	if err != nil {
		return provider.Resp{}, err
	}
	for k, vv := range headers {
		for _, v := range vv {
			upReq.Header.Add(k, v)
		}
	}

	// Reuse the BaseExecutor fetch seam so proxy options / logger / pinned
	// transport / per-connection ProxyFetchOptions resolution are identical to
	// the chat path. DoFetch clones the request onto a fetch context and
	// returns a cancel func the caller must invoke after consuming the body.
	resp, cancelFetch, err := e.DoFetch(ctx, upReq, req.Credentials)
	if err != nil {
		return provider.Resp{}, err
	}
	return provider.Resp{
		Response:        resp,
		URL:             url,
		Headers:         headers,
		TransformedBody: transformed,
		Done:            cancelFetch,
	}, nil
}

// buildAntigravityIDERequestID mirrors buildIdeRequestId: preserve an existing
// IDE-shaped body.requestId, else derive agent/<conversationId>/<now>/<trajectoryId>/<step>
// where conversationId/trajectoryId are uuidFromSeed of the session and
// step = max(1, contents*2-1).
func buildAntigravityIDERequestID(body, request map[string]any, creds provider.Credentials, model, requestType, sessionID string) string {
	if existing, ok := body["requestId"].(string); ok {
		if matchAntigravityIDERequestID(existing) {
			return existing
		}
	}
	conversationID := uuidFromSeed("antigravity:conversation:" + sessionID)
	trajectoryID := uuidFromSeed(fmt.Sprintf("antigravity:trajectory:%s:%s:%s", sessionID, model, requestType))
	step := 1
	if contents, ok := request["contents"].([]any); ok {
		n := len(contents)
		if n > 0 {
			step = n*2 - 1
			if step < 1 {
				step = 1
			}
		}
	}
	return fmt.Sprintf("agent/%s/%d/%s/%d", conversationID, time.Now().UnixMilli(), trajectoryID, step)
}

// resolveAntigravitySessionID mirrors the JS resolveSessionId fallback chain
// (scope "antigravity"): request.sessionId → connectionId → email → anonymous.
// connectionId/email/projectId live in Credentials.ProviderSpecificData (set
// by connectionCredentials in the v1 handler).
func resolveAntigravitySessionID(request map[string]any, creds provider.Credentials) string {
	if v, ok := request["sessionId"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v := psdString(creds, "_connectionId"); v != "" {
		return v
	}
	if v := psdString(creds, "email"); v != "" {
		return v
	}
	return "anonymous"
}

// antigravityProjectID returns credentials.projectId if present, else a
// deterministic project id derived from the session (the JS generateProjectId
// is random; deterministic keeps replays stable and avoids Math/random).
func antigravityProjectID(creds provider.Credentials, sessionID string) string {
	if v := psdString(creds, "projectId"); v != "" {
		return v
	}
	return uuidFromSeed("antigravity:project:" + sessionID)
}

// psdString reads a trimmed string field from ProviderSpecificData.
func psdString(creds provider.Credentials, key string) string {
	v, ok := creds.ProviderSpecificData[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// uuidFromSeed returns a deterministic RFC-4122 v5-ish UUID (variant 1, version
// 5) from a sha256 seed, mirroring the JS uuidFromSeed (sha256 first 16 bytes,
// fix version nibble to 5, fix variant nibble to 10xxxxxx).
func uuidFromSeed(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	b := make([]byte, 16)
	copy(b, sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xxxxxx
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// matchAntigravityIDERequestID reports whether s matches the IDE-shaped
// requestId. Kept simple (no regexp) to avoid a compiled-regex init cost on a
// hot path; the shape is agent/<conv>/<digits>/<traj>/<digits>.
func matchAntigravityIDERequestID(s string) bool {
	if !strings.HasPrefix(s, "agent/") {
		return false
	}
	rest := s[len("agent/"):]
	parts := strings.Split(rest, "/")
	if len(parts) != 4 {
		return false
	}
	for _, ch := range parts[1] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	for _, ch := range parts[3] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return parts[0] != "" && parts[2] != ""
}

// stripBlacklisted removes the antigravity-rejected thinking fields from a
// body object (in place).
func stripBlacklisted(obj map[string]any) {
	for _, k := range antigravityRequestBlacklist {
		delete(obj, k)
	}
}

// asInt extracts an int from a JSON-decoded numeric/string value.
func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		var i int
		_, _ = fmt.Sscanf(t, "%d", &i)
		return i
	}
	return 0
}
