package projectid

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// codeAssistServer builds an httptest server that handles loadCodeAssist and
// onboardUser, recording the Authorization header and request bodies.
func codeAssistServer(t *testing.T, loadAssistBody string, onboardResponses []string) (*httptest.Server, *int32) {
	t.Helper()
	var onboardCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1internal:loadCodeAssist", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("loadCodeAssist Authorization=%q want Bearer", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(loadAssistBody))
	})
	mux.HandleFunc("/v1internal:onboardUser", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&onboardCalls, 1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		idx := int(n) - 1
		if idx >= len(onboardResponses) {
			idx = len(onboardResponses) - 1
		}
		_, _ = w.Write([]byte(onboardResponses[idx]))
	})
	srv := httptest.NewServer(mux)
	// Redirect the package constants to the test server by overriding them —
	// the fetcher reads loadCodeAssistURL/onboardUserURL package vars.
	t.Cleanup(func() {
		srv.Close()
	})
	return srv, &onboardCalls
}

// redirectURLs rewrites the package-level endpoint URLs to the test server so
// the fetcher hits the local handler. Restored on cleanup.
func redirectURLs(t *testing.T, srvURL string) {
	t.Helper()
	origLoad := loadCodeAssistURL
	origOnboard := onboardUserURL
	loadCodeAssistURL = srvURL + "/v1internal:loadCodeAssist"
	onboardUserURL = srvURL + "/v1internal:onboardUser"
	t.Cleanup(func() {
		loadCodeAssistURL = origLoad
		onboardUserURL = origOnboard
	})
}

// TestForConnection_LoadCodeAssistDirect returns the project id straight from
// loadCodeAssist (cloudaicompanionProject as a string) with no onboarding.
func TestForConnection_LoadCodeAssistDirect(t *testing.T) {
	srv, _ := codeAssistServer(t, `{"cloudaicompanionProject":"real-proj-123"}`, nil)
	redirectURLs(t, srv.URL)
	f := New(srv.Client())
	defer f.Stop()

	pid, err := f.ForConnection(context.Background(), "conn-a", "tok")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pid != "real-proj-123" {
		t.Fatalf("pid=%q want real-proj-123", pid)
	}
	// Cached: a second call must not re-fetch (server has no second handler).
	if pid2, _ := f.ForConnection(context.Background(), "conn-a", "tok"); pid2 != "real-proj-123" {
		t.Fatalf("cached pid=%q want real-proj-123", pid2)
	}
}

// TestForConnection_ObjectProjectID accepts the object form {id:...}.
func TestForConnection_ObjectProjectID(t *testing.T) {
	srv, _ := codeAssistServer(t, `{"cloudaicompanionProject":{"id":"obj-proj-456"}}`, nil)
	redirectURLs(t, srv.URL)
	f := New(srv.Client())
	defer f.Stop()

	pid, _ := f.ForConnection(context.Background(), "conn-b", "tok")
	if pid != "obj-proj-456" {
		t.Fatalf("pid=%q want obj-proj-456", pid)
	}
}

// TestForConnection_OnboardFallback polls onboardUser when loadCodeAssist
// returns no project, until done=true carries the project id.
func TestForConnection_OnboardFallback(t *testing.T) {
	srv, onboardCalls := codeAssistServer(t,
		`{"allowedTiers":[{"id":"default-tier","isDefault":true}]}`,
		[]string{
			`{"done":false}`,
			`{"done":true,"response":{"cloudaicompanionProject":"onboard-proj-789"}}`,
		},
	)
	redirectURLs(t, srv.URL)
	f := New(srv.Client())
	defer f.Stop()
	// Speed up the retry sleep so the test does not wait 2s per attempt.
	orig := onboardRetryDelay
	onboardRetryDelay = 5 * time.Millisecond
	t.Cleanup(func() { onboardRetryDelay = orig })

	pid, err := f.ForConnection(context.Background(), "conn-c", "tok")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pid != "onboard-proj-789" {
		t.Fatalf("pid=%q want onboard-proj-789", pid)
	}
	if calls := atomic.LoadInt32(onboardCalls); calls != 2 {
		t.Fatalf("expected 2 onboard attempts (not done then done), got %d", calls)
	}
}

// TestForConnection_EmptyAccessToken returns "" without fetching.
func TestForConnection_EmptyAccessToken(t *testing.T) {
	f := New(http.DefaultClient)
	defer f.Stop()
	if pid, err := f.ForConnection(context.Background(), "conn", ""); err != nil || pid != "" {
		t.Fatalf("empty token must return empty, got %q err=%v", pid, err)
	}
}

// TestForConnection_LoadCodeAssistFailure returns "" on a non-2xx.
func TestForConnection_LoadCodeAssistFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token"}`))
	}))
	t.Cleanup(srv.Close)
	redirectURLs(t, srv.URL)
	f := New(srv.Client())
	defer f.Stop()

	pid, err := f.ForConnection(context.Background(), "conn-d", "tok")
	if err != nil {
		t.Fatalf("failure must be swallowed (JS null contract), got err=%v", err)
	}
	if pid != "" {
		t.Fatalf("failure must return empty pid, got %q", pid)
	}
}

// TestForConnection_ConcurrentCoalesces proves concurrent calls for the same
// connection share a single fetch (inflight dedup).
func TestForConnection_ConcurrentCoalesces(t *testing.T) {
	release := make(chan struct{})
	var loadCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loadCalls, 1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cloudaicompanionProject":"coalesced-proj"}`))
	}))
	t.Cleanup(srv.Close)
	redirectURLs(t, srv.URL)
	f := New(srv.Client())
	defer f.Stop()

	type res struct {
		pid string
		err error
	}
	results := make([]res, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pid, err := f.ForConnection(context.Background(), "conn-e", "tok")
			results[i] = res{pid, err}
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls := atomic.LoadInt32(&loadCalls); calls != 1 {
		t.Fatalf("concurrent calls must coalesce into 1 fetch, got %d", calls)
	}
	for i, r := range results {
		if r.err != nil || r.pid != "coalesced-proj" {
			t.Fatalf("caller %d got pid=%q err=%v", i, r.pid, r.err)
		}
	}
}

// TestInvalidate drops the cached entry so the next call re-fetches.
func TestInvalidate(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cloudaicompanionProject":"p-1"}`))
	}))
	t.Cleanup(srv.Close)
	redirectURLs(t, srv.URL)
	f := New(srv.Client())
	defer f.Stop()

	if pid, _ := f.ForConnection(context.Background(), "conn-f", "tok"); pid != "p-1" {
		t.Fatalf("first pid=%q want p-1", pid)
	}
	f.Invalidate("conn-f")
	if pid, _ := f.ForConnection(context.Background(), "conn-f", "tok"); pid != "p-1" {
		t.Fatalf("post-invalidate pid=%q want p-1", pid)
	}
	if calls != 2 {
		t.Fatalf("invalidate must force a re-fetch, got %d calls", calls)
	}
}

// TestGetCacheFreshness verifies the cache hit path does not fetch.
func TestGetCacheFreshness(t *testing.T) {
	f := New(http.DefaultClient)
	defer f.Stop()
	if _, ok := f.Get("nope"); ok {
		t.Fatal("unknown connection must not be a cache hit")
	}
}

// Verify the metadata body carries the expected enum values.
func TestLoadCodeAssistMetadata(t *testing.T) {
	m := loadCodeAssistMetadata()
	if m["ideType"] != ideTypeAntigravity {
		t.Errorf("ideType=%v want %d", m["ideType"], ideTypeAntigravity)
	}
	if m["pluginType"] != pluginTypeGemini {
		t.Errorf("pluginType=%v want %d", m["pluginType"], pluginTypeGemini)
	}
	// platform is runtime-derived; just ensure it is one of the known values.
	p, _ := m["platform"].(int)
	if p < 0 || p > platformWindowsAMD64 {
		t.Errorf("platform=%d out of range", p)
	}
}

// extractProjectID sanity for both shapes.
func TestExtractProjectID(t *testing.T) {
	if id := extractProjectID(cloudAIProject{str: "abc"}); id != "abc" {
		t.Errorf("string shape: got %q", id)
	}
	if id := extractProjectID(cloudAIProject{id: "xyz"}); id != "xyz" {
		t.Errorf("object shape: got %q", id)
	}
	if id := extractProjectID(cloudAIProject{}); id != "" {
		t.Errorf("empty: got %q", id)
	}
}