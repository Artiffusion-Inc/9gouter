package proxychat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	defexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// geminiProv wraps the real DefaultExecutor (gemini config) behind the
// proxychat DomainProvider interface so the full Handle pipeline runs against
// a real httptest upstream — no mock executor.
type geminiProv struct{ exec provider.Executor }

func (g *geminiProv) ID() string                  { return "gemini" }
func (g *geminiProv) Executor() provider.Executor { return g.exec }

// geminiRegistryCfg mirrors internal/adapter/provider/registry.go "gemini".
func geminiRegistryCfg() base.Config {
	return base.Config{
		BaseURL: "https://generativelanguage.googleapis.com/v1beta/models",
		Format:  "gemini",
		Auth: base.AuthDescriptor{
			APIKey: &base.AuthSpec{Header: "x-goog-api-key", Scheme: "raw"},
			OAuth:  &base.AuthSpec{Header: "Authorization", Scheme: "bearer"},
		},
	}
}

// TestGeminiChat_OutgoingRequestShape is the T025 root-cause probe: drive a
// real /v1/chat/completions (OpenAI shape) request for a gemini model through
// the full Handle pipeline with a real DefaultExecutor, and capture the
// outgoing upstream URL path, auth header, and body. Legacy JS returns 200 on
// this exact request; the Go binary returns 404 ("upstream returned 404").
// This test pins what the Go executor actually sends so the divergence can
// be located (URL vs body vs header).
func TestGeminiChat_OutgoingRequestShape(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("x-goog-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"}}]}`)
	}))
	defer srv.Close()

	cfg := geminiRegistryCfg()
	cfg.BaseURL = srv.URL + "/v1beta/models"
	exec := defexec.New("gemini", cfg)
	t.Logf("BuildURL direct = %q (Format=%q)", exec.BuildURL("gemini-2.5-flash", false, 0, provider.Credentials{APIKey: "AIzaSyTESTKEY"}), cfg.Format)

	h := New(Dependencies{
		Registry:   func(id string) (DomainProvider, error) { return &geminiProv{exec: exec}, nil },
		UsageRepo:  &inMemoryUsageRepo{},
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     config.Config{StreamStallTimeout: config.DurationMs(180_000), StreamReadinessMaxTimeout: config.DurationMs(900_000)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		ProviderID:     "gemini",
		Model:          "gemini-2.5-flash",
		Stream:         false,
		Credentials:    provider.Credentials{APIKey: "AIzaSyTESTKEY", ProviderSpecificData: map[string]any{"_connectionId": "c1"}},
		ResponseWriter: rec,
	}

	res, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	t.Logf("status=%d body=%q", res.StatusCode, rec.Body.String())
	t.Logf("OUTGOING path=%q query=%q auth=%q", gotPath, gotQuery, gotAuth)
	t.Logf("OUTGOING body=%s", gotBody)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; outgoing path=%q body=%s", res.StatusCode, gotPath, gotBody)
	}
	if !strings.Contains(gotPath, "gemini-2.5-flash:generateContent") {
		t.Errorf("path = %q, want .../gemini-2.5-flash:generateContent", gotPath)
	}
	if gotAuth != "AIzaSyTESTKEY" {
		t.Errorf("x-goog-api-key = %q, want raw key", gotAuth)
	}
}

// TestGeminiChat_StreamEndpoint pins the streaming variant: the upstream call
// must hit streamGenerateContent?alt=sse, not the raw BaseURL (the pre-fix 404
// affected both non-stream and stream).
func TestGeminiChat_StreamEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}],\"role\":\"model\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := geminiRegistryCfg()
	cfg.BaseURL = srv.URL + "/v1beta/models"
	exec := defexec.New("gemini", cfg)

	h := New(Dependencies{
		Registry:   func(id string) (DomainProvider, error) { return &geminiProv{exec: exec}, nil },
		UsageRepo:  &inMemoryUsageRepo{},
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     config.Config{StreamStallTimeout: config.DurationMs(180_000), StreamReadinessMaxTimeout: config.DurationMs(900_000)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		ProviderID:     "gemini",
		Model:          "gemini-2.5-flash",
		Stream:         true,
		Credentials:    provider.Credentials{APIKey: "AIzaSyTESTKEY", ProviderSpecificData: map[string]any{"_connectionId": "c1"}},
		ResponseWriter: rec,
	}
	res, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream)", res.StatusCode)
	}
	if !strings.Contains(gotPath, "gemini-2.5-flash:streamGenerateContent?alt=sse") {
		t.Errorf("stream path = %q, want .../gemini-2.5-flash:streamGenerateContent?alt=sse", gotPath)
	}
}
