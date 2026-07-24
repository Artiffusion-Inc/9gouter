package resolver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// kilocode_test.go pins the 713c5637 kilocode gateway live resolver: an
// unauthenticated fetch of api.kilo.ai/api/gateway/models shaped like
// OpenRouter's {data:[...]} response, narrowed by the openrouter-free filter
// (pricing.prompt == "0" && pricing.completion == "0" && context_length >=
// 200000), projected to {id,name,contextLength} and sorted by contextLength
// descending — the exact filter the legacy JS dashboard used for the combo
// picker (src/app/api/providers/suggested-models/filters.js). Real httptest
// server via the liveSwap transport; no mock resolver.

// TestKilocodeResolve_FreeFilter verifies the openrouter-free filter keeps
// only free + long-context models and sorts them by contextLength desc.
func TestKilocodeResolve_FreeFilter(t *testing.T) {
	body := `{"data":[
		{"id":"deepseek/deepseek-r1","name":"DeepSeek R1","context_length":640000,"pricing":{"prompt":"0","completion":"0"}},
		{"id":"paid/gpt-x","name":"Paid X","context_length":200000,"pricing":{"prompt":"0.00001","completion":"0.00001"}},
		{"id":"free/short","name":"Short Free","context_length":128000,"pricing":{"prompt":"0","completion":"0"}},
		{"id":"meta/llama-3","name":"Llama 3 1M","context_length":1000000,"pricing":{"prompt":"0","completion":"0"}}
	]}`
	srv := liveServer(t, 0, body, nil)
	r := NewKilocodeResolver(nil).(*kilocodeResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{
		ProviderSpecificData: map[string]any{"_connectionId": "kc-1"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	// paid model dropped (non-zero pricing), short free model dropped
	// (context_length 128000 < 200000). Two remain, sorted desc by context.
	if len(res.Models) != 2 {
		t.Fatalf("expected 2 free+long-context models, got %d: %+v", len(res.Models), res.Models)
	}
	if res.Models[0].ID != "meta/llama-3" || res.Models[0].ContextLength != 1000000 {
		t.Errorf("first (largest) = %+v, want meta/llama-3 @ 1000000", res.Models[0])
	}
	if res.Models[1].ID != "deepseek/deepseek-r1" || res.Models[1].ContextLength != 640000 {
		t.Errorf("second = %+v, want deepseek/deepseek-r1 @ 640000", res.Models[1])
	}
	if res.Models[0].Kind != "llm" {
		t.Errorf("kind=%q want llm", res.Models[0].Kind)
	}
	if res.Models[0].UpstreamModelID != res.Models[0].ID {
		t.Errorf("UpstreamModelID must mirror id (no variant expansion): %q vs %q", res.Models[0].UpstreamModelID, res.Models[0].ID)
	}
}

// TestKilocodeResolve_NumericPricing verifies numeric 0 pricing is also treated
// as free (the JS filter compared string "0"; OpenRouter ships strings, but we
// tolerate numbers so a future upstream shape change does not silently empty
// the catalog).
func TestKilocodeResolve_NumericPricing(t *testing.T) {
	body := `{"data":[
		{"id":"a/one","name":"One","context_length":300000,"pricing":{"prompt":0,"completion":0}}
	]}`
	srv := liveServer(t, 0, body, nil)
	r := NewKilocodeResolver(nil).(*kilocodeResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || len(res.Models) != 1 || res.Models[0].ID != "a/one" {
		t.Fatalf("numeric-0 pricing must be treated as free, got %v", res)
	}
}

// TestKilocodeResolve_BareArray verifies the bare-array response shape
// (`json.data ?? json.models ?? json`).
func TestKilocodeResolve_BareArray(t *testing.T) {
	body := `[{"id":"b/one","name":"B One","context_length":250000,"pricing":{"prompt":"0","completion":"0"}}]`
	srv := liveServer(t, 0, body, nil)
	r := NewKilocodeResolver(nil).(*kilocodeResolver)
	withSwap(r, srv.URL)
	res, _ := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if res == nil || len(res.Models) != 1 || res.Models[0].ID != "b/one" {
		t.Fatalf("bare-array shape not parsed, got %v", res)
	}
}

// TestKilocodeResolve_ModelsWrap verifies the {models:[...]} response shape.
func TestKilocodeResolve_ModelsWrap(t *testing.T) {
	body := `{"models":[{"id":"c/one","name":"C One","context_length":250000,"pricing":{"prompt":"0","completion":"0"}}]}`
	srv := liveServer(t, 0, body, nil)
	r := NewKilocodeResolver(nil).(*kilocodeResolver)
	withSwap(r, srv.URL)
	res, _ := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if res == nil || len(res.Models) != 1 || res.Models[0].ID != "c/one" {
		t.Fatalf("{models:[...]} shape not parsed, got %v", res)
	}
}

// TestKilocodeResolve_NoCredentials verifies the unauthenticated gateway
// resolves without any accessToken/apiKey (unlike grok-cli/kimchi, which return
// nil,nil on empty token). The connection id is the cache key; creds may be
// entirely empty.
func TestKilocodeResolve_NoCredentials(t *testing.T) {
	body := `{"data":[{"id":"d/one","name":"D","context_length":200000,"pricing":{"prompt":"0","completion":"0"}}]}`
	srv := liveServer(t, 0, body, nil)
	r := NewKilocodeResolver(nil).(*kilocodeResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || len(res.Models) != 1 {
		t.Fatalf("unauthenticated gateway must resolve with empty creds, got %v err=%v", res, err)
	}
}

// TestKilocodeResolve_Non200 returns nil on a non-2xx (caller falls back to the
// 8-model static catalog in registry.go).
func TestKilocodeResolve_Non200(t *testing.T) {
	srv := liveServer(t, http.StatusBadGateway, `{"error":"upstream"}`, nil)
	r := NewKilocodeResolver(nil).(*kilocodeResolver)
	withSwap(r, srv.URL)
	out, _ := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if out != nil {
		t.Fatalf("expected nil result on 502, got %v", out)
	}
}

// TestKilocodeResolve_EmptyAfterFilter returns nil when the filter strips
// everything (no free long-context model present) so the caller falls back to
// static — the resolver must not return an empty Result.
func TestKilocodeResolve_EmptyAfterFilter(t *testing.T) {
	body := `{"data":[{"id":"paid/x","name":"X","context_length":500000,"pricing":{"prompt":"0.01","completion":"0.01"}}]}`
	srv := liveServer(t, 0, body, nil)
	r := NewKilocodeResolver(nil).(*kilocodeResolver)
	withSwap(r, srv.URL)
	out, _ := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if out != nil {
		t.Fatalf("expected nil when filter strips everything, got %v", out)
	}
}

// TestKilocodeResolve_CacheHit verifies a second resolve reuses the cached
// catalog and does not hit the upstream again.
func TestKilocodeResolve_CacheHit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"e/one","name":"E","context_length":200000,"pricing":{"prompt":"0","completion":"0"}}]}`))
	}))
	t.Cleanup(srv.Close)
	r := NewKilocodeResolver(nil).(*kilocodeResolver)
	withSwap(r, srv.URL)
	creds := provider.Credentials{ProviderSpecificData: map[string]any{"_connectionId": "kc-cache"}}
	if _, err := r.Resolve(context.Background(), creds, ResolveOpts{}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, err := r.Resolve(context.Background(), creds, ResolveOpts{}); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (second resolve must hit cache)", calls)
	}
}

// TestKilocodeResolve_Dedup verifies repeated ids in the upstream payload are
// collapsed to a single entry.
func TestKilocodeResolve_Dedup(t *testing.T) {
	body := `{"data":[
		{"id":"f/dup","name":"Dup","context_length":200000,"pricing":{"prompt":"0","completion":"0"}},
		{"id":"f/dup","name":"Dup Again","context_length":999999,"pricing":{"prompt":"0","completion":"0"}}
	]}`
	srv := liveServer(t, 0, body, nil)
	r := NewKilocodeResolver(nil).(*kilocodeResolver)
	withSwap(r, srv.URL)
	res, _ := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if res == nil || len(res.Models) != 1 {
		t.Fatalf("expected 1 (deduped), got %d", len(res.Models))
	}
}

// TestKilocodeResolve_RegistryLookup verifies the resolver is registered under
// "kilocode" so v1models.resolveLiveCatalogs finds it for active kilocode
// connections.
func TestKilocodeResolve_RegistryLookup(t *testing.T) {
	ResetRegistry()
	Register(NewKilocodeResolver(nil))
	if Lookup("kilocode") == nil {
		t.Fatal(`Lookup("kilocode")=nil after Register`)
	}
	if !Has("kilocode") {
		t.Fatal(`Has("kilocode")=false after Register`)
	}
}

// Compile-time: kilocodeResolver satisfies the interface.
var _ LiveModelResolver = (*kilocodeResolver)(nil)

var _ = json.Marshal
