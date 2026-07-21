package proxyembeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/embedding"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// mockAdapter captures the inputs BuildURL/BuildHeaders/BuildBody receive and
// returns a fixed normalized body, so tests can assert wiring without real
// upstream shape conversion.
type mockAdapter struct {
	url       string
	gotModel  string
	gotParams embedding.Params
	gotCreds  domainProv.Credentials
}

func (m *mockAdapter) BuildURL(model string, creds domainProv.Credentials, p embedding.Params) string {
	m.gotModel = model
	m.gotCreds = creds
	m.gotParams = p
	return m.url
}
func (m *mockAdapter) BuildHeaders(creds domainProv.Credentials, _ embedding.Params) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if creds.APIKey != "" {
		h.Set("Authorization", "Bearer "+creds.APIKey)
	} else if creds.AccessToken != "" {
		h.Set("Authorization", "Bearer "+creds.AccessToken)
	}
	return h
}
func (m *mockAdapter) BuildBody(model string, p embedding.Params) ([]byte, error) {
	return json.Marshal(map[string]any{"model": model, "input": p.Input})
}
func (m *mockAdapter) Normalize(body []byte, _ string) ([]byte, error) {
	// Wrap into OpenAI shape to exercise extractPromptTokens.
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	out := map[string]any{
		"object": "list",
		"data":   []map[string]any{{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}}},
		"model":  raw["model"],
		"usage":  map[string]any{"prompt_tokens": float64(42), "total_tokens": float64(42)},
	}
	return json.Marshal(out)
}

func newHandler(t *testing.T, srvURL string) (*Handler, *mockAdapter) {
	t.Helper()
	mock := &mockAdapter{url: srvURL}
	return New(Dependencies{
		LookupAdapter: func(string) (embedding.Adapter, bool) { return mock, true },
		// Use a real fetch against the httptest server (no proxy needed).
		Fetch: func(ctx context.Context, client *http.Client, req *http.Request, _ proxy.Options, _ proxy.ProxyFetchOptions, _ *proxy.Fallback) (*http.Response, error) {
			return client.Do(req)
		},
		HTTPClient: &http.Client{},
	}), mock
}

func TestHandle_Success(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1,0.2]}}`))
	}))
	defer srv.Close()

	h, mock := newHandler(t, srv.URL)
	res := h.Handle(context.Background(), Request{
		Body:        []byte(`{"model":"text-embedding-3-small","input":"hello","dimensions":256}`),
		ProviderID:  "openai",
		Model:       "text-embedding-3-small",
		Credentials: domainProv.Credentials{APIKey: "sk-test"},
		UserAgent:   "9gouter-test/1.0",
	})

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, err = %v", res.StatusCode, res.Err)
	}
	// gotPath is empty for the srv.URL root; nothing to assert here, but keep
	// the variable referenced so it is not flagged as unused.
	_ = gotPath
	if gotAuth != "Bearer sk-test" {
		t.Errorf("upstream Authorization = %q", gotAuth)
	}
	if !bytes.Contains([]byte(gotBody), []byte(`"input":"hello"`)) {
		t.Errorf("upstream body = %q", gotBody)
	}
	if mock.gotModel != "text-embedding-3-small" {
		t.Errorf("adapter got model = %q", mock.gotModel)
	}
	// Body must be the normalized OpenAI shape.
	var out map[string]any
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("normalize output: %v", err)
	}
	if out["object"] != "list" {
		t.Errorf("object = %v", out["object"])
	}
}

func TestHandle_MissingInput(t *testing.T) {
	h, _ := newHandler(t, "http://unused")
	res := h.Handle(context.Background(), Request{
		Body:       []byte(`{"model":"m"}`),
		ProviderID: "openai",
		Model:      "m",
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestHandle_NoAdapter(t *testing.T) {
	h := New(Dependencies{
		LookupAdapter: func(string) (embedding.Adapter, bool) { return nil, false },
	})
	res := h.Handle(context.Background(), Request{Body: []byte(`{"model":"m","input":"x"}`), ProviderID: "weird", Model: "m"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestHandle_UpstreamNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	h, _ := newHandler(t, srv.URL)
	res := h.Handle(context.Background(), Request{
		Body:        []byte(`{"model":"m","input":"x"}`),
		ProviderID:  "openai",
		Model:       "m",
		Credentials: domainProv.Credentials{APIKey: "k"},
	})
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	if res.Err == nil {
		t.Fatal("expected error on non-2xx")
	}
}

func TestHandle_BearerFallbackToAccessToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1]}}`))
	}))
	defer srv.Close()

	h, _ := newHandler(t, srv.URL)
	res := h.Handle(context.Background(), Request{
		Body:        []byte(`{"model":"m","input":"x"}`),
		ProviderID:  "openai",
		Model:       "m",
		Credentials: domainProv.Credentials{AccessToken: "tok-abc"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want Bearer tok-abc", gotAuth)
	}
}

func TestHandle_InvalidJSON(t *testing.T) {
	h, _ := newHandler(t, "http://unused")
	res := h.Handle(context.Background(), Request{Body: []byte(`{not json`), ProviderID: "openai", Model: "m"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestExtractPromptTokens(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"usage":{"prompt_tokens":128,"total_tokens":128}}`, 128},
		{`{"usage":{"total_tokens":64}}`, 64},
		{`{"usage":{"input_tokens":32}}`, 32},
		{`{"usage":{}}`, 0},
		{`{}`, 0},
		{`not json`, 0},
	}
	for _, c := range cases {
		if got := extractPromptTokens([]byte(c.body)); got != c.want {
			t.Errorf("extractPromptTokens(%q) = %d, want %d", c.body, got, c.want)
		}
	}
}