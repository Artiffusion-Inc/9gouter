package rtk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// HeadroomStats mirrors the data returned by the Headroom /v1/compress endpoint.
type HeadroomStats struct {
	TokensBefore int               `json:"tokens_before"`
	TokensAfter  int               `json:"tokens_after"`
	TokensSaved  int               `json:"tokens_saved"`
	Messages     []HeadroomMessage `json:"messages,omitempty"`
}

// HeadroomMessage is one message in a Headroom response.
type HeadroomMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// HeadroomDiagnostics captures structured size info for HEADROOM logging.
type HeadroomDiagnostics struct {
	Reason   string
	Endpoint string
	Before   *SizeSnapshot
	After    *SizeSnapshot
}

// SizeSnapshot captures before/after body/message byte sizes.
type SizeSnapshot struct {
	BodyBytes    int `json:"bodyBytes"`
	MessageBytes int `json:"messageBytes"`
}

// HeadroomConfig configures the optional Headroom external compression proxy.
type HeadroomConfig struct {
	Enabled              bool
	URL                  string
	Model                string
	Format               format.Format
	CompressUserMessages bool
	TimeoutMs            int
	Diagnostics          *HeadroomDiagnostics
	Client               *http.Client
}

// CompressWithHeadroom mirrors open-sse/rtk/headroom.js compressWithHeadroom.
// It is fail-open: returns nil on any error. The caller logs the structured
// HEADROOM lines when stats is non-nil, and logs the skipped: branch when
// enabled but stats is nil.
func CompressWithHeadroom(body map[string]any, cfg HeadroomConfig) (*HeadroomStats, error) {
	if cfg.Diagnostics != nil {
		cfg.Diagnostics.Before = captureSizeSnapshot(body)
	}
	if !cfg.Enabled {
		setDiagnostic(cfg.Diagnostics, "disabled")
		return nil, nil
	}
	if cfg.URL == "" {
		setDiagnostic(cfg.Diagnostics, "missing proxy URL")
		return nil, nil
	}
	if body == nil {
		setDiagnostic(cfg.Diagnostics, "missing request body")
		return nil, nil
	}

	timeoutMs := cfg.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 3000
	}
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}

	switch cfg.Format {
	case format.Claude:
		stats, err := compressClaudeViaHeadroom(body, cfg, client, timeoutMs)
		if err != nil {
			setDiagnostic(cfg.Diagnostics, err.Error())
			return nil, nil
		}
		if cfg.Diagnostics != nil {
			cfg.Diagnostics.After = captureSizeSnapshot(body)
		}
		return stats, nil
	case format.OpenaiResponses:
		stats, err := compressResponsesViaHeadroom(body, cfg, client, timeoutMs)
		if err != nil {
			setDiagnostic(cfg.Diagnostics, err.Error())
			return nil, nil
		}
		if cfg.Diagnostics != nil {
			cfg.Diagnostics.After = captureSizeSnapshot(body)
		}
		return stats, nil
	case format.Kiro:
		stats, err := compressKiroViaHeadroom(body, cfg, client, timeoutMs)
		if err != nil {
			setDiagnostic(cfg.Diagnostics, err.Error())
			return nil, nil
		}
		if cfg.Diagnostics != nil {
			cfg.Diagnostics.After = captureSizeSnapshot(body)
		}
		return stats, nil
	default:
		key := ""
		if _, ok := body["messages"].([]any); ok {
			key = "messages"
		} else if _, ok := body["input"].([]any); ok {
			key = "input"
		}
		if key == "" {
			setDiagnostic(cfg.Diagnostics, fmt.Sprintf("unsupported %s request shape", cfg.Format))
			return nil, nil
		}
		stats, err := callCompress(cfg.URL, body[key].([]any), cfg.Model, timeoutMs, cfg.CompressUserMessages, client, cfg.Diagnostics)
		if err != nil {
			setDiagnostic(cfg.Diagnostics, err.Error())
			return nil, nil
		}
		body[key] = messagesToAny(stats.Messages)
		if cfg.Diagnostics != nil {
			cfg.Diagnostics.After = captureSizeSnapshot(body)
		}
		return stats, nil
	}
}

func compressClaudeViaHeadroom(body map[string]any, cfg HeadroomConfig, client *http.Client, timeoutMs int) (*HeadroomStats, error) {
	return compressClaudeViaHeadroomImpl(body, cfg, client, timeoutMs)
}

func compressResponsesViaHeadroom(body map[string]any, cfg HeadroomConfig, client *http.Client, timeoutMs int) (*HeadroomStats, error) {
	return compressResponsesViaHeadroomImpl(body, cfg, client, timeoutMs)
}

func compressKiroViaHeadroom(body map[string]any, cfg HeadroomConfig, client *http.Client, timeoutMs int) (*HeadroomStats, error) {
	return compressKiroViaHeadroomImpl(body, cfg, client, timeoutMs)
}

func messagesToAny(msgs []HeadroomMessage) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		out[i] = map[string]any{"role": m.Role, "content": m.Content}
	}
	return out
}

func callCompress(url string, messages []any, model string, timeoutMs int, compressUserMessages bool, client *http.Client, diag *HeadroomDiagnostics) (*HeadroomStats, error) {
	endpoint := buildCompressEndpoint(url)
	if diag != nil {
		diag.Endpoint = maskEndpoint(endpoint)
	}
	payload := map[string]any{"messages": messages, "model": model}
	if compressUserMessages {
		payload["config"] = map[string]any{"compress_user_messages": true}
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	ctx := contextWithTimeout(timeoutMs)
	defer ctxCancel(ctx)
	req, err := http.NewRequestWithContext(ctx.ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", scrubFetchError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy returned HTTP %d", resp.StatusCode)
	}
	var data struct {
		Messages []HeadroomMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(data.Messages) == 0 {
		return nil, fmt.Errorf("proxy response missing messages[]")
	}
	return &HeadroomStats{Messages: data.Messages}, nil
}

func buildCompressEndpoint(url string) string {
	u := strings.TrimSuffix(url, "/")
	return u + "/v1/compress"
}

func maskEndpoint(endpoint string) string {
	endpoint = strings.Split(endpoint, "?")[0]
	endpoint = strings.Split(endpoint, "#")[0]
	re := regexp.MustCompile(`://[^/@\s]+@`)
	endpoint = re.ReplaceAllString(endpoint, "://")
	return endpoint
}

func scrubFetchError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	msg = regexp.MustCompile(`://[^/@\s]+@`).ReplaceAllString(msg, "://")
	msg = regexp.MustCompile(`(https?://[^\s?#]+)[?#][^\s)]*`).ReplaceAllString(msg, "$1")
	return fmt.Errorf("%s", msg)
}

func captureSizeSnapshot(body map[string]any) *SizeSnapshot {
	b, _ := json.Marshal(body)
	messages := messagePayload(body)
	var mb []byte
	if messages != nil {
		mb, _ = json.Marshal(messages)
	}
	return &SizeSnapshot{BodyBytes: len(b), MessageBytes: len(mb)}
}

func messagePayload(body map[string]any) []any {
	if m, ok := body["messages"].([]any); ok {
		return m
	}
	if i, ok := body["input"].([]any); ok {
		return i
	}
	// Kiro carries its conversation in conversationState; project it to OpenAI
	// messages so the size snapshot reflects the compressible payload (#2488).
	if projection := collectKiroHeadroomMessages(body); projection != nil {
		out := make([]any, len(projection.messages))
		for i, m := range projection.messages {
			out[i] = m
		}
		return out
	}
	return nil
}

func setDiagnostic(diag *HeadroomDiagnostics, reason string) {
	if diag != nil && diag.Reason == "" {
		diag.Reason = reason
	}
}

// FormatHeadroomLog returns a compact log line from HeadroomStats.
func FormatHeadroomLog(stats *HeadroomStats) string {
	if stats == nil {
		return ""
	}
	before := stats.TokensBefore
	after := stats.TokensAfter
	delta := stats.TokensSaved
	var pct string
	if before > 0 {
		pct = fmt.Sprintf("%.1f", float64(delta)/float64(before)*100)
	} else {
		pct = "0"
	}
	afterPart := ""
	if after != 0 {
		afterPart = fmt.Sprintf(" after=%d", after)
	}
	return fmt.Sprintf("reported token delta=%d before=%d%s (%s%%)", delta, before, afterPart, pct)
}

// FormatHeadroomSizeLog returns the body/message byte size transition.
func FormatHeadroomSizeLog(diag *HeadroomDiagnostics) string {
	if diag == nil || diag.Before == nil || diag.After == nil {
		return ""
	}
	return fmt.Sprintf("body=%dB→%dB messages=%dB→%dB",
		diag.Before.BodyBytes, diag.After.BodyBytes,
		diag.Before.MessageBytes, diag.After.MessageBytes)
}

// IsHeadroomPhantomSavings detects when Headroom reports savings but the outbound
// JSON shrank less than minShrinkRatio.
func IsHeadroomPhantomSavings(stats *HeadroomStats, diag *HeadroomDiagnostics, minShrinkRatio float64) bool {
	if stats == nil || stats.TokensSaved <= 0 {
		return false
	}
	before := diag.Before.BodyBytes
	after := diag.After.BodyBytes
	if before <= 0 || after <= 0 {
		return false
	}
	return float64(after) >= float64(before)*(1-minShrinkRatio)
}

// contextWithTimeout is a tiny helper abstracting context creation so the file
// compiles even if the import list is minimal. Real callers pass a context.
var contextWithTimeout = contextWithTimeoutReal
var ctxCancel = ctxCancelReal

type contextHandle struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func contextWithTimeoutReal(ms int) *contextHandle {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(ms)*time.Millisecond)
	return &contextHandle{ctx: ctx, cancel: cancel}
}

func ctxCancelReal(h *contextHandle) {
	if h != nil && h.cancel != nil {
		h.cancel()
	}
}
