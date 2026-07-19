package proxyfetch

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/webfetch"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// fakeAdapter is a test double for webfetch.Adapter.
type fakeAdapter struct {
	result *webfetch.Result
	err    error
}

func (f fakeAdapter) Fetch(ctx context.Context, client *http.Client, creds domainProv.Credentials, p webfetch.Params, log webfetch.Logger) (*webfetch.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakeLookup struct {
	adapter webfetch.Adapter
	ok      bool
}

func (l fakeLookup) Lookup(providerID string) (webfetch.Adapter, bool) { return l.adapter, l.ok }

type captureLogger struct{ warns []string }

func (l *captureLogger) Infof(format string, args ...any)  {}
func (l *captureLogger) Warnf(format string, args ...any) { l.warns = append(l.warns, format) }
func (l *captureLogger) Debugf(format string, args ...any) {}

func TestHandle_UnsupportedProvider(t *testing.T) {
	h := New(Dependencies{
		LookupAdapter: fakeLookup{ok: false}.Lookup,
		Logger:        &captureLogger{},
		Config:        config.Config{},
	})
	res := h.Handle(context.Background(), Request{ProviderID: "nope"})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if res.Err == nil {
		t.Errorf("expected error for unsupported provider")
	}
}

func TestHandle_Success(t *testing.T) {
	adapter := fakeAdapter{result: &webfetch.Result{
		Provider: "jina-reader", URL: "https://example.com",
		Title: "Example", Format: "markdown", Text: "hello", CostUSD: "0.001",
	}}
	h := New(Dependencies{
		LookupAdapter: fakeLookup{adapter: adapter, ok: true}.Lookup,
		Logger:        &captureLogger{},
		Config:        config.Config{},
	})
	res := h.Handle(context.Background(), Request{
		ProviderID: "jina-reader",
		Params:     webfetch.Params{URL: "https://example.com", Format: "markdown"},
	})
	if res.Err != nil {
		t.Fatalf("unexpected err: %v", res.Err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	body := string(res.Body)
	// buildResponseJSON shape: provider, url, title, content.format/text/length, usage.fetch_cost_usd
	if !contains(body, `"provider":"jina-reader"`) {
		t.Errorf("body missing provider: %s", body)
	}
	if !contains(body, `"title":"Example"`) {
		t.Errorf("body missing title: %s", body)
	}
	if !contains(body, `"fetch_cost_usd":"0.001"`) {
		t.Errorf("body missing cost: %s", body)
	}
	if !contains(body, `"format":"markdown"`) {
		t.Errorf("body missing format: %s", body)
	}
	if !contains(body, `"length":5`) {
		t.Errorf("body missing length: %s", body)
	}
}

func TestHandle_NullTitleWhenEmpty(t *testing.T) {
	adapter := fakeAdapter{result: &webfetch.Result{
		Provider: "tavily", URL: "https://example.com", Format: "markdown", Text: "x",
	}}
	h := New(Dependencies{
		LookupAdapter: fakeLookup{adapter: adapter, ok: true}.Lookup,
		Logger:        &captureLogger{},
		Config:        config.Config{},
	})
	res := h.Handle(context.Background(), Request{ProviderID: "tavily"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !contains(string(res.Body), `"title":null`) {
		t.Errorf("empty title should encode as null: %s", res.Body)
	}
	if !contains(string(res.Body), `"fetch_cost_usd":null`) {
		t.Errorf("empty cost should encode as null: %s", res.Body)
	}
}

func TestHandle_UpstreamError_GatewayTimeout(t *testing.T) {
	adapter := fakeAdapter{err: errors.New("context deadline exceeded")}
	h := New(Dependencies{
		LookupAdapter: fakeLookup{adapter: adapter, ok: true}.Lookup,
		Logger:        &captureLogger{},
		Config:        config.Config{},
	})
	// Use a context already cancelled to force the cctx.Err() path → 504.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := h.Handle(ctx, Request{ProviderID: "jina-reader"})
	if res.StatusCode != http.StatusGatewayTimeout && res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 504 or 502", res.StatusCode)
	}
	if res.Err == nil {
		t.Errorf("expected error on upstream failure")
	}
}

func TestHandle_DefaultsApplied(t *testing.T) {
	// New with nil deps must select sensible defaults and not panic.
	h := New(Dependencies{})
	if h.deps.LookupAdapter == nil {
		t.Errorf("LookupAdapter default not applied")
	}
	if h.deps.HTTPClient == nil {
		t.Errorf("HTTPClient default not applied")
	}
	if h.deps.Logger == nil {
		t.Errorf("Logger default not applied")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}