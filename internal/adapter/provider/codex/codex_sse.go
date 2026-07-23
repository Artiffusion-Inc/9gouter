package codexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// codex_sse.go ports the capacity/overloaded SSE-peek half of decolua/9router
// #2452 (0c55d49a part B). The Codex Responses API can return a 200-OK SSE body
// whose frames describe a transient error rather than a completion:
//   - "selected model is at capacity" / "model_at_capacity": the model is busy;
//     rotate to another account (503 → account fallback).
//   - "server_is_overloaded" / "service_unavailable_error": retry the SAME
//     account, then surface 503.
// To detect these the executor peeks the first 256KB of a 200-OK streaming
// body, breaks early on a user-output delta (so a real completion is never
// mistaken for an error), and on a match replaces the body with a synthetic 503
// JSON error. On no match it re-assembles the peeked prefix + the remaining
// upstream body so the client stream is byte-for-byte intact.

const (
	codexSsePeekBytes = 256 * 1024
	// codexModelCapacityMessage is the canonical capacity error text surfaced
	// to the client when the upstream SSE frame lacks a structured message.
	codexModelCapacityMessage = "Selected model is at capacity. Please try a different model."
	// codexSseRetryAttempts/DelayMs match DEFAULT_RETRY_CONFIG for HTTP 503 in
	// the base executor (attempts 3, delay 2000ms). Used by the overloaded
	// retry-same-account loop before surfacing 503.
	codexSseRetryAttempts = 3
	codexSseRetryDelayMs  = 2000
)

var (
	codexSseAccountFallbackPatterns = []string{
		"selected model is at capacity",
		"model_at_capacity",
	}
	codexSseRetryPatterns = []string{
		"server_is_overloaded",
		"service_unavailable_error",
	}
	// codexSseUserOutputPatterns mark the transition from metadata frames to
	// real model output — once seen, the body is a genuine completion and the
	// peek stops (no false-positive capacity match).
	codexSseUserOutputPatterns = []string{
		"event: response.output_text.delta",
		"event: response.function_call_arguments.delta",
		`"type":"response.output_text.delta"`,
		`"type":"response.function_call_arguments.delta"`,
	}
)

// peekResult describes what the peek found.
type peekResult struct {
	matched         string
	message         string
	accountFallback bool
	replacementBody io.ReadCloser
}

// Execute overrides BaseExecutor.Execute to peek the streaming response body
// for transient SSE errors before handing it downstream. Non-streaming calls,
// non-200 responses, and bodies that show no error pattern are passed through
// unchanged (with the body re-assembled for the no-match case).
func (e *Executor) Execute(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	const capacityStatus = http.StatusServiceUnavailable // 503
	retryAttempts := codexSseRetryAttempts
	retryDelayMs := codexSseRetryDelayMs
	if e.Config.Retry != nil {
		if entry, ok := e.Config.Retry[capacityStatus]; ok && entry.Attempts > 0 {
			retryAttempts = entry.Attempts
			retryDelayMs = entry.DelayMs
		}
	}

	for attempt := 0; ; attempt++ {
		result, err := e.BaseExecutor.Execute(ctx, req)
		if err != nil {
			return result, err
		}
		// Only peek streaming 200-OK bodies; everything else is passthrough.
		if !req.Stream || result.Response == nil || result.Response.StatusCode != http.StatusOK {
			return result, nil
		}

		peek := peekSseTransientError(result.Response.Body)
		if peek.matched == "" {
			// No error matched: re-assemble prefix + remaining body so the
			// client stream is intact. The original resp.Response.Body is
			// consumed by the re-assembled reader's Close.
			if peek.replacementBody != nil {
				result.Response.Body = peek.replacementBody
			}
			return result, nil
		}

		// Matched a transient error. Close the upstream body + fetch context
		// before synthesizing (we will not stream it downstream).
		result.Response.Body.Close()
		if result.Done != nil {
			result.Done()
			result.Done = nil
		}

		if peek.accountFallback {
			// Capacity → rotate accounts: return 503 so the account-fallback
			// loop in v1.go tries the next connection.
			result.Response = codexSseErrorResponse(capacityStatus, peek.message)
			return result, nil
		}

		// Overloaded → retry the SAME account then surface 503.
		if attempt >= retryAttempts {
			result.Response = codexSseErrorResponse(capacityStatus, peek.message)
			return result, nil
		}
		select {
		case <-time.After(time.Duration(retryDelayMs) * time.Millisecond):
		case <-ctx.Done():
			return provider.Resp{}, ctx.Err()
		}
	}
}

// peekSseTransientError reads up to codexSsePeekBytes of body, classifying the
// buffered text. It returns:
//   - matched != "" with accountFallback/retry: a transient SSE error was found;
//     replacementBody is nil (the caller synthesizes a 503).
//   - matched == "" with replacementBody != nil: no error; the caller must use
//     replacementBody (the original body has been partially read).
//   - matched == "" with replacementBody == nil: nothing was read (empty/nil
//     body); the caller leaves the response as-is.
//
// The peek breaks early as soon as a user-output delta appears, since a real
// completion never carries a capacity/overloaded error frame after output
// starts.
func peekSseTransientError(body io.ReadCloser) peekResult {
	if body == nil {
		return peekResult{}
	}
	var buf bytes.Buffer
	// Read in modest chunks so we can stop the moment a pattern matches.
	tmp := make([]byte, 8192)
	for buf.Len() < codexSsePeekBytes {
		n, err := body.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		lower := strings.ToLower(buf.String())
		// Capacity patterns take precedence (rotate accounts).
		if hit := matchFirst(lower, codexSseAccountFallbackPatterns); hit != "" {
			return peekResult{
				matched:         hit,
				message:         extractSseErrorMessage(buf.String(), hit),
				accountFallback: true,
			}
		}
		if hit := matchFirst(lower, codexSseRetryPatterns); hit != "" {
			return peekResult{
				matched: hit,
				message: extractSseErrorMessage(buf.String(), hit),
			}
		}
		if matchAny(lower, codexSseUserOutputPatterns) {
			break
		}
		if err != nil {
			// io.EOF or read error: stop reading. Whatever was buffered is the
			// prefix; the remaining body (if any, on a non-EOF read error) is
			// best-effort included via MultiReader.
			break
		}
	}
	// No error matched → re-assemble prefix + remaining body.
	prefix := buf.Bytes()
	return peekResult{
		replacementBody: &prefixThenBody{
			prefix: bytes.NewReader(prefix),
			body:   body,
		},
	}
}

// prefixThenBody serves the peeked prefix bytes first, then the remaining
// upstream body. Close closes the upstream body.
type prefixThenBody struct {
	prefix *bytes.Reader
	body   io.ReadCloser
}

func (p *prefixThenBody) Read(dst []byte) (int, error) {
	if p == nil {
		return 0, io.EOF
	}
	if p.prefix.Len() > 0 {
		return p.prefix.Read(dst)
	}
	if p.body == nil {
		return 0, io.EOF
	}
	return p.body.Read(dst)
}

func (p *prefixThenBody) Close() error {
	if p == nil || p.body == nil {
		return nil
	}
	return p.body.Close()
}

// matchFirst returns the first pattern contained in lower, "" if none.
func matchFirst(lower string, patterns []string) string {
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return p
		}
	}
	return ""
}

// matchAny reports whether any pattern is contained in lower.
func matchAny(lower string, patterns []string) bool {
	return matchFirst(lower, patterns) != ""
}

// findNestedMessage walks a parsed SSE data JSON value for the first string
// "message" field (error.message / response.error.message / nested), mirroring
// findNestedMessage in open-sse/executors/codex.js (depth-limited to 6).
func findNestedMessage(v any, depth int) string {
	if depth > 6 || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return ""
	case []any:
		for _, item := range x {
			if m := findNestedMessage(item, depth+1); m != "" {
				return m
			}
		}
		return ""
	case map[string]any:
		if s, ok := x["message"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		if errObj, ok := x["error"].(map[string]any); ok {
			if s, ok := errObj["message"].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
		if respObj, ok := x["response"].(map[string]any); ok {
			if errObj, ok := respObj["error"].(map[string]any); ok {
				if s, ok := errObj["message"].(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
		for _, child := range x {
			if m := findNestedMessage(child, depth+1); m != "" {
				return m
			}
		}
	}
	return ""
}

// extractSseErrorMessage mirrors extractSseErrorMessage: prefer the exact
// canonical capacity message, else walk each `data:` SSE line for a nested
// error.message; fall back to fallback (the matched pattern) or the canonical
// capacity message.
func extractSseErrorMessage(text, fallback string) string {
	if m := matchFirst(strings.ToLower(text), []string{strings.ToLower(codexModelCapacityMessage)}); m != "" {
		// Return the canonical-cased message, not the lowercased match.
		if strings.Contains(text, codexModelCapacityMessage) {
			return codexModelCapacityMessage
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var v any
		if json.Unmarshal([]byte(data), &v) == nil {
			if m := findNestedMessage(v, 0); m != "" {
				return m
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return codexModelCapacityMessage
}

// codexSseErrorResponse builds a synthetic JSON error response for a peeked
// transient SSE error, mirroring codexSseErrorResponse in codex.js.
func codexSseErrorResponse(status int, message string) *http.Response {
	if message == "" {
		message = codexModelCapacityMessage
	}
	errType := "upstream_error"
	code := "upstream_error"
	if status >= 500 {
		errType = "server_error"
	}
	if status == http.StatusServiceUnavailable {
		code = "service_unavailable"
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
	resp := &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	return resp
}
