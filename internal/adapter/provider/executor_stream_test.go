package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// streamTestCase drives a canned upstream server through a single provider.
type streamTestCase struct {
	pid       string
	creds     domain.Credentials
	wantPath  string // optional substring of the upstream URL path
	serverFmt string // "sse" (default) or "ndjson" for commandcode
}

// TestProviderStreamingUsage executes every ported provider against an
// httptest.Server returning a canned stream and asserts the response can be
// read and contains a usage frame.
func TestProviderStreamingUsage(t *testing.T) {
	apiKeyCreds := domain.Credentials{APIKey: "sk-test-APIKEY"}
	oauthCreds := domain.Credentials{AccessToken: "tok-test-ACCESS"}

	cases := []streamTestCase{
		{pid: "antigravity", creds: oauthCreds},
		{pid: "azure", creds: apiKeyCreds},
		{pid: "codebuddy-cn", creds: apiKeyCreds},
		{pid: "codex", creds: apiKeyCreds},
		{pid: "commandcode", creds: apiKeyCreds, serverFmt: "ndjson"},
		// cursor is exercised by its own AgentService duplex test in the cursor
		// package (execute_test.go); the generic mock-fetch harness below cannot
		// target the h2-only agent.api5 Run RPC.
		{pid: "gemini-cli", creds: oauthCreds},
		{pid: "github", creds: oauthCreds},
		{pid: "grok-cli", creds: oauthCreds},
		{pid: "grok-web", creds: domain.Credentials{}},
		{pid: "iflow", creds: apiKeyCreds},
		{pid: "kimchi", creds: apiKeyCreds},
		// kiro is exercised by its own binary-EventStream integrity test in the
		// kiro package (execute_test.go); the generic mock-fetch harness serves
		// SSE/ndjson, but Kiro's Execute drains a binary AWS EventStream body
		// through the integrity gate and synthesizes OpenAI SSE, so a canned
		// SSE server does not exercise its real path.
		{pid: "mimo-free", creds: domain.Credentials{}},
		{pid: "ollama-local", creds: domain.Credentials{}},
		{pid: "opencode", creds: domain.Credentials{}},
		{pid: "opencode-go", creds: apiKeyCreds},
		{pid: "perplexity-web", creds: apiKeyCreds},
		{pid: "qoder", creds: apiKeyCreds},
		{pid: "qwen", creds: apiKeyCreds},
		{pid: "vertex", creds: apiKeyCreds},
		{pid: "xiaomi-tokenplan", creds: apiKeyCreds},
		// Default executor coverage.
		{pid: "openai", creds: apiKeyCreds},
		{pid: "claude", creds: apiKeyCreds},
	}

	for _, tc := range cases {
		t.Run(tc.pid, func(t *testing.T) {
			p, err := Lookup(tc.pid)
			if err != nil {
				t.Fatalf("lookup %s: %v", tc.pid, err)
			}

			server := newStreamServer(tc.serverFmt)
			defer server.Close()

			exec := p.Executor()
			var gotURL string
			setMockFetch(exec, func(ctx context.Context, client *http.Client, req *http.Request, opts proxy.Options, pfo proxy.ProxyFetchOptions, fb *proxy.Fallback) (*http.Response, error) {
				gotURL = req.URL.String()
				// Rewrite request to hit the local test server while preserving
				// headers and body built by the executor.
				srvURL, _ := url.Parse(server.URL)
				req.URL.Scheme = srvURL.Scheme
				req.URL.Host = srvURL.Host
				return client.Do(req)
			})

			body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
			resp, err := exec.Execute(context.Background(), domain.ExecRequest{
				Model:       "test-model",
				Body:        body,
				Stream:      true,
				Credentials: tc.creds,
			})
			if err != nil {
				t.Fatalf("execute %s: %v", tc.pid, err)
			}
			if resp.Response == nil {
				t.Fatalf("execute %s: nil response", tc.pid)
			}
			if resp.Response.StatusCode != http.StatusOK {
				t.Fatalf("execute %s: status %d", tc.pid, resp.Response.StatusCode)
			}
			if tc.wantPath != "" && !strings.Contains(gotURL, tc.wantPath) {
				t.Errorf("execute %s: url %q does not contain %q", tc.pid, gotURL, tc.wantPath)
			}

			respBody, err := io.ReadAll(resp.Response.Body)
			resp.Response.Body.Close()
			if err != nil {
				t.Fatalf("read %s body: %v", tc.pid, err)
			}
			if len(respBody) == 0 {
				t.Fatalf("execute %s: empty response body", tc.pid)
			}
			if !strings.Contains(string(respBody), "data:") {
				t.Errorf("execute %s: response missing SSE frame", tc.pid)
			}
			if !strings.Contains(string(respBody), "usage") {
				t.Errorf("execute %s: response missing usage frame", tc.pid)
			}
		})
	}
}

// newStreamServer returns an httptest server that mimics a streaming upstream.
func newStreamServer(format string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "expected POST", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if format == "ndjson" {
			fmt.Fprintln(w, `{"type":"text-delta","text":"Hello"}`)
			fmt.Fprintln(w, `{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":5,"outputTokens":1,"totalTokens":6}}`)
			fmt.Fprintln(w, `{"type":"finish"}`)
			return
		}

		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// setMockFetch replaces the BaseExecutor.Fetch field on any executor that
// embeds *base.BaseExecutor directly or via *defaultexec.DefaultExecutor.
func setMockFetch(exec domain.Executor, mock base.Fetcher) {
	v := reflect.ValueOf(exec)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	// Direct *base.BaseExecutor embed.
	if f := v.FieldByName("BaseExecutor"); f.IsValid() && f.Kind() == reflect.Ptr && !f.IsNil() {
		baseElem := f.Elem()
		if fetch := baseElem.FieldByName("Fetch"); fetch.IsValid() && fetch.CanSet() {
			fetch.Set(reflect.ValueOf(mock))
			return
		}
	}

	// Wrapped via *defaultexec.DefaultExecutor.
	if f := v.FieldByName("DefaultExecutor"); f.IsValid() && f.Kind() == reflect.Ptr && !f.IsNil() {
		baseField := f.Elem().FieldByName("BaseExecutor")
		if baseField.IsValid() && baseField.Kind() == reflect.Ptr && !baseField.IsNil() {
			baseElem := baseField.Elem()
			if fetch := baseElem.FieldByName("Fetch"); fetch.IsValid() && fetch.CanSet() {
				fetch.Set(reflect.ValueOf(mock))
				return
			}
		}
	}

	panic(fmt.Sprintf("cannot inject mock Fetch into %T", exec))
}
