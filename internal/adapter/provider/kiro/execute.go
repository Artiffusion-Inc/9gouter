// execute.go wires the Kiro EventStream integrity gate (#100–#102) into the
// executor surface, mirroring execute() + attachIntegrityGate in
// open-sse/executors/kiro.js (upstream v0.5.40, commit 6994cd1f):
//
//	const result = await super.execute(args);          // fetch via proxy stack
//	if (result?.response?.ok) attachIntegrityGate(result, args);  // → OpenAI SSE
//
// The inherited BaseExecutor.Execute walks the baseUrls (runtime.kiro.dev →
// codewhisperer → q), advances on 429 / network / 5xx, and returns a
// provider.Resp whose Response.Body is the raw binary AWS EventStream. For a
// 2xx response this override drains that body through RunIntegrityGate (the
// first-attempt → bounded-retry flow) and returns a synthetic *http.Response
// whose body is already OpenAI SSE. Non-2xx responses pass through unchanged
// so the upstream handler can read the error body and trigger account
// fallback/cooldown (mirror kiro.js:317 `if (result?.response?.ok)`).
package kiroexec

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// kiroSSEHeaders is the Content-Type the synthesized OpenAI SSE response
// carries, matching the JS SSE_HEADERS.
var kiroSSEHeaders = http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-cache"}}

// Execute is the Kiro executor entrypoint: fetch through the inherited
// BaseExecutor (proxy-aware, fallback, retry-after), then run the EventStream
// integrity gate over the response body. Mirrors execute() in kiro.js:312-316.
func (e *Executor) Execute(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	resp, err := e.BaseExecutor.Execute(ctx, req)
	if err != nil {
		return resp, err
	}
	if resp.Response == nil || resp.Response.StatusCode < 200 || resp.Response.StatusCode >= 300 {
		// Non-2xx: pass through untransformed so the chat handler can read the
		// error body and classify status / trigger fallback (mirror the JS
		// `if (result?.response?.ok)` guard).
		return resp, nil
	}

	// 2xx: the body is a binary AWS EventStream. Drain it through the integrity
	// gate. Repair-enabled mirrors the per-credential override +
	// KIRO_TOOL_CALL_REPAIR env (kiro.js:321).
	repairEnabled := kiroRepairEnabled(req.Credentials)
	opts := DefaultIntegrityOptions(repairEnabled)

	upstreamModel := resolveKiroUpstreamModel(req.Model)
	contextWin := kiroContextWindow(upstreamModel, req.Credentials)

	// RunIntegrityGate owns the first attempt (resp.Response.Body) and, on a
	// repairable terminal, the one bounded retry. The retry re-issues the full
	// BaseExecutor.Execute with the repaired body so it inherits the same
	// proxy/fallback/retry-after path (mirror the JS
	// BaseExecutor.prototype.execute.call(this, {...args, body: repairBody})).
	origBody := req.Body
	retry := func(rctx context.Context, repairedBody json.RawMessage) (io.ReadCloser, int, error) {
		retryReq := req
		retryReq.Body = repairedBody
		retryResp, rerr := e.BaseExecutor.Execute(rctx, retryReq)
		if rerr != nil {
			return nil, 0, rerr
		}
		if retryResp.Response == nil {
			return nil, 0, nil
		}
		// Release the fetch context's cancel once the retry body is drained by
		// the gate; the gate's readIntegrityAttempt closes the body.
		body := retryResp.Response.Body
		done := retryResp.Done
		if done != nil {
			go func() {
				<-rctx.Done()
			}()
			// Wrap the body so Done is invoked on close (the gate Close()s it).
			body = &doneCloser{ReadCloser: body, done: done}
		}
		return body, retryResp.Response.StatusCode, nil
	}

	result := RunIntegrityGate(ctx, resp.Response.Body, retry, upstreamModel, contextWin, origBody, opts)

	// Release the first attempt's fetch context cancel now that its body has
	// been drained by the gate.
	if resp.Done != nil {
		resp.Done()
	}

	// Replace the EventStream body with the synthesized OpenAI SSE.
	synth := &http.Response{
		Status:        http.StatusText(http.StatusOK),
		StatusCode:    http.StatusOK,
		Header:        kiroSSEHeaders,
		Body:          io.NopCloser(bytes.NewReader(result.Bytes)),
		ContentLength: int64(len(result.Bytes)),
	}
	return provider.Resp{
		Response:        synth,
		URL:             resp.URL,
		Headers:         resp.Headers,
		TransformedBody: resp.TransformedBody,
	}, nil
}

// doneCloser wraps an io.ReadCloser so the fetch context's cancel (resp.Done) is
// invoked once the integrity gate finishes draining the retry body. The gate
// calls Close() on the body it receives; this forwards that Close to the
// underlying body AND releases the fetch context.
type doneCloser struct {
	io.ReadCloser
	done func()
}

func (d *doneCloser) Close() error {
	err := d.ReadCloser.Close()
	if d.done != nil {
		d.done()
	}
	return err
}

// kiroRepairEnabled mirrors the per-credential + env repair gating in
// attachIntegrityGate (kiro.js:321): disabled only when the credential
// explicitly sets kiroToolCallRepair=false. (Env-level KIRO_TOOL_CALL_REPAIR is
// owned by the caller's config; the default is enabled.)
func kiroRepairEnabled(creds provider.Credentials) bool {
	if creds.ProviderSpecificData == nil {
		return true
	}
	if v, ok := creds.ProviderSpecificData["kiroToolCallRepair"].(bool); ok && !v {
		return false
	}
	return true
}

// resolveKiroUpstreamModel strips the synthetic -agentic / -thinking suffixes to
// find the upstream-real model id, mirroring resolveKiroModel in kiroConstants.js.
// Only the upstream id is needed here (for capabilities / context window); the
// agentic/thinking flags are handled by the request translator's system-prompt
// injection, not by the executor.
func resolveKiroUpstreamModel(model string) string {
	upstream := model
	upstream = strings.TrimSuffix(upstream, kiroAgenticSuffix)
	upstream = strings.TrimSuffix(upstream, kiroThinkingSuffix)
	return upstream
}

const (
	kiroAgenticSuffix  = "-agentic"
	kiroThinkingSuffix = "-thinking"
)

// kiroContextWindow resolves the model context window for usage synthesis,
// mirroring getCapabilitiesForModel("kiro", capabilityModel).contextWindow || 200000
// (kiro.js:581). The Go rewrite has no shared capabilities table, so the
// per-credential contextWindow override wins, then a hardcoded GPT-5.6 = 272000
// (KIRO_GPT_5_6_CAPABILITIES), then the 200000 default.
func kiroContextWindow(upstreamModel string, creds provider.Credentials) int {
	if creds.ProviderSpecificData != nil {
		if v, ok := creds.ProviderSpecificData["contextWindow"].(float64); ok && v > 0 {
			return int(v)
		}
	}
	if strings.Contains(upstreamModel, "gpt-5.6") || strings.Contains(upstreamModel, "gpt-5-6") {
		return 272000
	}
	return 200000
}
