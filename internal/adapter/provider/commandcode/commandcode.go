// Package commandcodeexec ports the CommandCode executor.
package commandcodeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9router/internal/domain/format"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends BaseExecutor for CommandCode NDJSON upstreams.
type Executor struct {
	*base.BaseExecutor
}

// New creates a CommandCode executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("commandcode", cfg)}
}

// TransformRequest forces stream=true upstream.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	var m map[string]any
	if len(body) == 0 {
		m = map[string]any{"stream": true}
	} else if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	m["stream"] = true
	out, _ := json.Marshal(m)
	return out, nil
}

// BuildHeaders adds the per-request x-session-id header.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	base.SetHeaderExact(h, "x-session-id", "00000000-0000-0000-0000-000000000000")
	return h
}

// Execute wraps the NDJSON response as OpenAI SSE.
func (e *Executor) Execute(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	result, err := e.BaseExecutor.Execute(ctx, req)
	if err != nil {
		return provider.Resp{}, err
	}
	if result.Response == nil || result.Response.Body == nil || result.Response.StatusCode >= 400 {
		return result, nil
	}
	bodyBytes, err := io.ReadAll(result.Response.Body)
	result.Response.Body.Close()
	if err != nil {
		return provider.Resp{}, err
	}
	state := translator.InitState(format.Commandcode)
	state["model"] = req.Model
	var sseLines []byte
	for _, line := range strings.Split(string(bodyBytes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		chunks, err := translator.TranslateResponse(format.Openai, format.Commandcode, json.RawMessage(line), state)
		if err != nil || chunks == nil {
			continue
		}
		for _, c := range chunks {
			sseLines = append(sseLines, []byte(fmt.Sprintf("data: %s\n\n", c))...)
		}
	}
	sseLines = append(sseLines, []byte("data: [DONE]\n\n")...)
	result.Response = &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(sseLines)),
		Request:    result.Response.Request,
	}
	return result, nil
}
