package rtk

import (
	"encoding/json"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/format"
)

const systemPromptSep = "\n\n"

// InjectSystemPrompt appends an instruction into the system message of the final
// request body, dispatching by format. It mirrors open-sse/rtk/systemInject.js.
func InjectSystemPrompt(body map[string]any, f format.Format, prompt string) {
	if body == nil || prompt == "" {
		return
	}
	switch f {
	case format.Claude:
		injectClaudeSystem(body, prompt)
	case format.Gemini, format.GeminiCli, format.Vertex, format.Antigravity:
		injectGeminiSystem(body, prompt)
	default:
		injectMessagesSystem(body, prompt)
	}
}

func injectMessagesSystem(body map[string]any, prompt string) {
	if instructions, ok := body["instructions"].(string); ok {
		if instructions != "" {
			body["instructions"] = instructions + systemPromptSep + prompt
		} else {
			body["instructions"] = prompt
		}
		return
	}

	var arr []any
	if msgs, ok := body["messages"].([]any); ok {
		arr = msgs
	} else if input, ok := body["input"].([]any); ok {
		arr = input
	}
	if arr == nil {
		return
	}

	idx := -1
	for i, mRaw := range arr {
		m, ok := mRaw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			idx = i
			break
		}
	}
	if idx >= 0 {
		appendToOpenAIMessage(arr[idx].(map[string]any), prompt)
	} else {
		// prepend system message
		if idx == -1 {
			body["messages"] = append([]any{map[string]any{"role": "system", "content": prompt}}, arr...)
		} else {
			// should not happen because idx found
			appendToOpenAIMessage(arr[idx].(map[string]any), prompt)
		}
	}
}

func appendToOpenAIMessage(msg map[string]any, prompt string) {
	if content, ok := msg["content"].(string); ok {
		msg["content"] = content + systemPromptSep + prompt
		return
	}
	if contentArr, ok := msg["content"].([]any); ok {
		msg["content"] = append(contentArr, map[string]any{"type": "input_text", "text": prompt})
		return
	}
	msg["content"] = prompt
}

func injectClaudeSystem(body map[string]any, prompt string) {
	if sys, ok := body["system"].(string); ok && sys != "" {
		body["system"] = sys + systemPromptSep + prompt
		return
	}
	if sysArr, ok := body["system"].([]any); ok {
		block := map[string]any{"type": "text", "text": prompt}
		lastCacheIdx := -1
		for i := len(sysArr) - 1; i >= 0; i-- {
			b, ok := sysArr[i].(map[string]any)
			if !ok {
				continue
			}
			if _, ok := b["cache_control"]; ok {
				lastCacheIdx = i
				break
			}
		}
		if lastCacheIdx >= 0 {
			body["system"] = append(append(sysArr[:lastCacheIdx], block), sysArr[lastCacheIdx:]...)
		} else {
			body["system"] = append(sysArr, block)
		}
		return
	}
	body["system"] = prompt
}

func injectGeminiSystem(body map[string]any, prompt string) {
	target := body
	if req, ok := body["request"].(map[string]any); ok {
		target = req
	}
	key := "systemInstruction"
	if _, ok := target["system_instruction"]; ok {
		key = "system_instruction"
	}
	if sys, ok := target[key].(map[string]any); ok {
		if parts, ok := sys["parts"].([]any); ok {
			sys["parts"] = append(parts, map[string]any{"text": prompt})
			return
		}
	}
	target[key] = map[string]any{"parts": []any{map[string]any{"text": prompt}}}
}

// CavemanPrompt returns the prompt for a caveman level.
func CavemanPrompt(level string) string {
	return cavemanPrompts[level]
}

// PonytailPrompt returns the prompt for a ponytail level.
func PonytailPrompt(level string) string {
	return ponytailPrompts[level]
}

// InjectCaveman injects the caveman prompt at level into body for format f.
func InjectCaveman(body map[string]any, f format.Format, level string) {
	if p, ok := cavemanPrompts[level]; ok {
		InjectSystemPrompt(body, f, p)
	}
}

// InjectPonytail injects the ponytail prompt at level into body for format f.
func InjectPonytail(body map[string]any, f format.Format, level string) {
	if p, ok := ponytailPrompts[level]; ok {
		InjectSystemPrompt(body, f, p)
	}
}

// PxpipeSummary describes the result of a pxpipe transform.
type PxpipeSummary struct {
	Applied          bool    `json:"applied"`
	Reason           string  `json:"reason,omitempty"`
	OriginalChars    int     `json:"originalChars,omitempty"`
	CompressedChars  int     `json:"compressedBodyChars,omitempty"`
	ImagedChars      int     `json:"imagedChars,omitempty"`
	ImageCount       int     `json:"imageCount,omitempty"`
	ImageBytes       int     `json:"imageBytes,omitempty"`
	TokensBeforeEst  int     `json:"tokensBeforeEst,omitempty"`
	TokensAfterEst   int     `json:"tokensAfterEst,omitempty"`
	TokensSavedEst   int     `json:"tokensSavedEst,omitempty"`
	SavedPct         float64 `json:"savedPct,omitempty"`
	DurationMs       int     `json:"durationMs,omitempty"`
	CacheOwnsControl bool    `json:"cacheOwnsControl,omitempty"`
}

// PxpipeResult is returned by the optional pxpipe transform. The Body field is
// non-nil only when the transform applied and changed the request body.
type PxpipeResult struct {
	Body    map[string]any
	Summary *PxpipeSummary
}

// PxpipeTransform is the function shape injected by the host. The JS host
// provides transformAnthropicMessages; tests stub it.
type PxpipeTransform func(args PxpipeTransformArgs) (*PxpipeTransformResult, error)

// PxpipeTransformArgs is the input to a pxpipe transform.
type PxpipeTransformArgs struct {
	Body            []byte
	Model           string
	MinCompressChars int
}

// PxpipeTransformResult is the raw result from a pxpipe transform.
type PxpipeTransformResult struct {
	Applied bool
	Body    []byte
	Reason  string
	Detail  string
	Info    PxpipeInfo
	Cache   PxpipeCache
}

// PxpipeInfo carries metadata about the pxpipe compression.
type PxpipeInfo struct {
	CompressedChars int `json:"compressedChars"`
	ImageCount      int `json:"imageCount"`
	ImageBytes      int `json:"imageBytes"`
	ImageTokens     int `json:"imageTokens"`
	ImagePixels     int `json:"imagePixels"`
	BaselineTokens  int `json:"baselineTokens"`
}

// PxpipeCache reports cache ownership from the transform.
type PxpipeCache struct {
	OwnsCacheControl bool `json:"ownsCacheControl"`
}

const estCharsPerToken = 4
const defaultPxpipeTimeoutMs = 15000
const defaultPxpipeMinChars = 25000

// CompressWithPxpipe renders bulky Claude-format context as dense PNGs via the
// injected pxpipe transform. It fail-opens: any error returns a summary with
// applied=false and leaves body untouched. Mirrors open-sse/rtk/pxpipe.js.
func CompressWithPxpipe(body map[string]any, f format.Format, model string, minChars, timeoutMs int, transform PxpipeTransform) *PxpipeResult {
	summary := &PxpipeSummary{Applied: false}
	startedAt := nowMs()

	if transform == nil {
		summary.Reason = "not_installed"
		return &PxpipeResult{Summary: summary}
	}
	if body == nil {
		summary.Reason = "missing_body"
		return &PxpipeResult{Summary: summary}
	}
	if f != format.Claude {
		summary.Reason = "unsupported_format"
		return &PxpipeResult{Summary: summary}
	}

	threshold := defaultPxpipeMinChars
	if minChars > 0 {
		threshold = minChars
	}
	originalChars := bodyChars(body)
	if originalChars < threshold {
		summary.Reason = "below_threshold"
		summary.OriginalChars = originalChars
		return &PxpipeResult{Summary: summary}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		summary.Reason = "encode_error"
		summary.OriginalChars = originalChars
		return &PxpipeResult{Summary: summary}
	}

	budget := defaultPxpipeTimeoutMs
	if timeoutMs > 0 {
		budget = timeoutMs
	}

	// The JS implementation races the transform against a timer because the
	// transform is local CPU work that cannot be aborted. In Go we honor the
	// context of the request by wrapping the transform with a timeout; if the
	// transform cannot be interrupted the timeout still prevents us from using
	// a stale result.
	done := make(chan *PxpipeTransformResult, 1)
	go func() {
		res, _ := transform(PxpipeTransformArgs{Body: encoded, Model: model, MinCompressChars: threshold})
		done <- res
	}()

	var result *PxpipeTransformResult
	select {
	case result = <-done:
	case <-timeAfter(time.Duration(budget) * time.Millisecond):
		summary.Reason = "timeout"
		summary.OriginalChars = originalChars
		summary.DurationMs = nowMs() - startedAt
		return &PxpipeResult{Summary: summary}
	}

	if result == nil {
		summary.Reason = "timeout"
		summary.OriginalChars = originalChars
		summary.DurationMs = nowMs() - startedAt
		return &PxpipeResult{Summary: summary}
	}
	if !result.Applied {
		reason := result.Reason
		if reason == "" {
			reason = "passthrough"
		}
		summary.Reason = reason
		summary.OriginalChars = originalChars
		summary.DurationMs = nowMs() - startedAt
		return &PxpipeResult{Summary: summary}
	}

	var newBody map[string]any
	if err := json.Unmarshal(result.Body, &newBody); err != nil {
		summary.Reason = "decode_error"
		summary.OriginalChars = originalChars
		summary.DurationMs = nowMs() - startedAt
		return &PxpipeResult{Summary: summary}
	}

	compressedBodyChars := bodyChars(newBody)
	info := result.Info
	imagedChars := info.CompressedChars
	imageTokensEst := info.ImageTokens
	if imageTokensEst == 0 && info.ImagePixels > 0 {
		imageTokensEst = info.ImagePixels / 750
	}
	if imageTokensEst == 0 {
		imageTokensEst = info.ImageCount * 4761
	}
	tokensBeforeEst := info.BaselineTokens
	if tokensBeforeEst == 0 {
		tokensBeforeEst = estTokens(originalChars)
	}
	tokensAfterEst := estTokens(max(0, originalChars-imagedChars)) + imageTokensEst
	tokensSavedEst := max(0, tokensBeforeEst-tokensAfterEst)
	savedPct := 0.0
	if tokensBeforeEst > 0 {
		savedPct = float64(int(float64(tokensSavedEst)/float64(tokensBeforeEst)*10000)) / 100
	}

	summary.Applied = true
	summary.Reason = "applied"
	summary.OriginalChars = originalChars
	summary.CompressedChars = compressedBodyChars
	summary.ImagedChars = imagedChars
	summary.ImageCount = info.ImageCount
	summary.ImageBytes = info.ImageBytes
	summary.TokensBeforeEst = tokensBeforeEst
	summary.TokensAfterEst = tokensAfterEst
	summary.TokensSavedEst = tokensSavedEst
	summary.SavedPct = savedPct
	summary.DurationMs = nowMs() - startedAt
	summary.CacheOwnsControl = result.Cache.OwnsCacheControl

	return &PxpipeResult{Body: newBody, Summary: summary}
}

func bodyChars(body map[string]any) int {
	b, err := json.Marshal(body)
	if err != nil {
		return 0
	}
	return len(b)
}

func estTokens(chars int) int {
	return chars / estCharsPerToken
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// timeAfter returns a channel that receives the current time after duration.
var timeAfter = timeAfterReal

type timer interface{}

// stubbed in tests.
var nowMs = nowMsReal

func nowMsReal() int {
	return int(time.Now().UnixMilli())
}

func timeAfterReal(d time.Duration) <-chan time.Time {
	return time.After(d)
}
