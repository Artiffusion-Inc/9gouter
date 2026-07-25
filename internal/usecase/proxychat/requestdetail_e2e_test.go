package proxychat

// requestdetail_e2e_test.go pins the observability request-detail path (#163):
// a real on-disk SQLite RequestDetailRepo + a fake ObservabilityGate wired into
// the proxychat Dependencies, so the chat path writes one requestDetails row per
// request across the success-streaming, success-non-streaming, and
// upstream-error paths. Before, the table stayed empty and the dashboard
// "Request Details" tab showed "No request details found".
//
// No mocks of the repo: it is the production SQLite path (sqlite.Open +
// db.SyncSchema + repo.NewRequestDetailRepo). The gate is a fixed-answer fake
// so the test can flip observability on/off without a settings blob.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/sqlite"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
)

// realDetailRepo returns a real *repo.RequestDetailRepo backed by a fresh
// on-disk SQLite (sqlite.Open + db.SyncSchema) — the exact persistence path the
// production binary uses.
func realDetailRepo(t *testing.T) *repo.RequestDetailRepo {
	t.Helper()
	conn, err := sqlite.Open(filepath.Join(t.TempDir(), "detail-test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := db.SyncSchema(conn); err != nil {
		t.Fatalf("SyncSchema: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return repo.NewRequestDetailRepo(conn)
}

// fakeGate is a fixed-answer ObservabilityGate so the test can flip observability
// on/off without touching the DB-backed settings blob.
type fakeGate struct{ on bool }

func (f fakeGate) Enabled(context.Context) (int, int, bool) {
	if !f.on {
		return 0, 0, false
	}
	return 1000, 5 * 1024, true
}

// makeJSONUpstream builds a non-streaming upstream HTTP response with a fixed
// JSON body, so the non-stream detail path has a real ProviderResponse to save.
func makeJSONUpstream(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestHandle_StreamingWritesRequestDetail: with observability ON, a streaming
// chat request writes exactly one requestDetails row carrying status=success,
// the upstream usage tokens, latency + streamMs, and the request config.
func TestHandle_StreamingWritesRequestDetail(t *testing.T) {
	detailRepo := realDetailRepo(t)
	usageRepo := realUsageRepo(t)

	upstreamBody := strings.Join([]string{
		`data: {"id":"cmpl-1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hello"}}]}`,
		`data: {"id":"cmpl-1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":45,"total_tokens":165,"cached_tokens":80,"completion_tokens_details":{"reasoning_tokens":12}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	exec := &stubExecutor{resp: makeSSEUpstream(upstreamBody)}

	h := New(Dependencies{
		Registry:          func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo:         usageRepo,
		RequestDetails:    detailRepo,
		ObservabilityGate: fakeGate{on: true},
		StreamPipe:        pipeAdapter{},
		JSONToSSE:         fakeJSONToSSE{},
		Pricing:           pricing.NewResolver(nil),
		Config:            configForTest(),
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		ProviderID:     "openai",
		Model:          "gpt-4",
		Stream:         true,
		ResponseWriter: rec,
	}
	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	page, err := detailRepo.Query(context.Background(), repo.RequestDetailFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Details) != 1 {
		t.Fatalf("expected 1 detail row, got %d", len(page.Details))
	}
	d := page.Details[0]
	if d.Status != "success" {
		t.Errorf("Status=%q want success", d.Status)
	}
	if d.Provider != "openai" || d.Model != "gpt-4" {
		t.Errorf("provider/model=%q/%q want openai/gpt-4", d.Provider, d.Model)
	}
	if len(d.Tokens) == 0 {
		t.Error("Tokens blob empty; streaming detail must carry the upstream usage")
	} else {
		var tok map[string]int
		if err := json.Unmarshal(d.Tokens, &tok); err == nil {
			if tok["completion_tokens"] != 45 {
				t.Errorf("tokens.completion_tokens=%d want 45", tok["completion_tokens"])
			}
			if tok["cached_tokens"] != 80 {
				t.Errorf("tokens.cached_tokens=%d want 80", tok["cached_tokens"])
			}
		}
	}
	if len(d.Request) == 0 {
		t.Error("Request config empty; detail must carry extractRequestConfig output")
	} else {
		var rc map[string]any
		if err := json.Unmarshal(d.Request, &rc); err == nil {
			if rc["stream"] != true {
				t.Errorf("request.stream=%v want true", rc["stream"])
			}
			if rc["model"] != "gpt-4" {
				t.Errorf("request.model=%v want gpt-4", rc["model"])
			}
		}
	}
	if len(d.Latency) == 0 {
		t.Error("Latency blob empty")
	} else {
		var lat map[string]any
		if err := json.Unmarshal(d.Latency, &lat); err == nil {
			if lat["streamMs"] == nil {
				t.Error("latency.streamMs nil; streaming detail must record TTFT→streamMs")
			}
		}
	}
}

// TestHandle_NonStreamWritesRequestDetail: the non-streaming path also writes
// one detail row with the provider response captured.
func TestHandle_NonStreamWritesRequestDetail(t *testing.T) {
	detailRepo := realDetailRepo(t)
	usageRepo := realUsageRepo(t)

	body := `{"id":"m-1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":20,"total_tokens":70}}`
	exec := &stubExecutor{resp: makeJSONUpstream(body)}

	h := New(Dependencies{
		Registry:          func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo:         usageRepo,
		RequestDetails:    detailRepo,
		ObservabilityGate: fakeGate{on: true},
		Pricing:           pricing.NewResolver(nil),
		Config:            configForTest(),
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
		ProviderID:     "openai",
		Model:          "gpt-4",
		Stream:         false,
		ResponseWriter: rec,
	}
	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	page, err := detailRepo.Query(context.Background(), repo.RequestDetailFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Details) != 1 {
		t.Fatalf("expected 1 detail row, got %d", len(page.Details))
	}
	d := page.Details[0]
	if d.Status != "success" {
		t.Errorf("Status=%q want success", d.Status)
	}
	if len(d.ProviderResponse) == 0 {
		t.Error("ProviderResponse empty; non-stream detail must capture the upstream body")
	}
}

// TestHandle_UpstreamErrorWritesRequestDetail: an upstream error still writes a
// detail row (status=error) so operators can see failed requests in the UI.
func TestHandle_UpstreamErrorWritesRequestDetail(t *testing.T) {
	detailRepo := realDetailRepo(t)
	usageRepo := realUsageRepo(t)

	exec := &stubExecutor{err: io.ErrUnexpectedEOF}

	h := New(Dependencies{
		Registry:          func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo:         usageRepo,
		RequestDetails:    detailRepo,
		ObservabilityGate: fakeGate{on: true},
		Pricing:           pricing.NewResolver(nil),
		Config:            configForTest(),
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
		ProviderID:     "openai",
		Model:          "gpt-4",
		Stream:         false,
		ResponseWriter: rec,
	}
	h.Handle(context.Background(), req) // error expected; detail still saved

	page, err := detailRepo.Query(context.Background(), repo.RequestDetailFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Details) != 1 {
		t.Fatalf("expected 1 detail row even on upstream error, got %d", len(page.Details))
	}
	if page.Details[0].Status != "error" {
		t.Errorf("Status=%q want error", page.Details[0].Status)
	}
}

// TestHandle_ObservabilityOffWritesNoDetail: with the gate OFF, no detail row
// is written (the saver is never called). This is the dashboard toggle contract.
func TestHandle_ObservabilityOffWritesNoDetail(t *testing.T) {
	detailRepo := realDetailRepo(t)
	usageRepo := realUsageRepo(t)

	body := `{"id":"m-1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`
	exec := &stubExecutor{resp: makeJSONUpstream(body)}

	h := New(Dependencies{
		Registry:          func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo:         usageRepo,
		RequestDetails:    detailRepo,
		ObservabilityGate: fakeGate{on: false},
		Pricing:           pricing.NewResolver(nil),
		Config:            configForTest(),
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
		ProviderID:     "openai",
		Model:          "gpt-4",
		Stream:         false,
		ResponseWriter: rec,
	}
	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	page, err := detailRepo.Query(context.Background(), repo.RequestDetailFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Details) != 0 {
		t.Fatalf("expected 0 detail rows with observability OFF, got %d", len(page.Details))
	}
}
