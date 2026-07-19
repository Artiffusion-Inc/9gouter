package videoproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

type captureLogger struct{}

func (captureLogger) Infof(string, ...any)  {}
func (captureLogger) Warnf(string, ...any)  {}
func (captureLogger) Debugf(string, ...any) {}

func creds(apiKey string) domainProv.Credentials {
	return domainProv.Credentials{APIKey: apiKey, ProviderSpecificData: map[string]any{"_connectionId": "conn-1"}}
}

func TestHandle_UnsupportedProvider(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "openai", Action: ActionGenerations})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestHandle_PostGenerations_RawPassthrough(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT, gotBody string
	var gotIdempotency string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotIdempotency = r.Header.Get("Idempotency-Key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"request_id":"req-1","status":"pending"}`)
	}))
	defer srv.Close()

	h := New(Dependencies{
		VideoBaseURL: func(string) string { return srv.URL },
		Logger:       captureLogger{},
		Config:       config.Config{},
	})
	res := h.Handle(context.Background(), Request{
		Action:         ActionGenerations,
		Body:           []byte(`{"model":"grok-imagine-video","prompt":"a cat"}`),
		ContentType:    "application/json",
		IdempotencyKey: "idem-123",
		ProviderID:     "xai",
		Credentials:    creds("k-xai"),
		ConnectionID:   "conn-1",
	})
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/generations" {
		t.Errorf("path = %q, want /generations", gotPath)
	}
	if gotAuth != "Bearer k-xai" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotIdempotency != "idem-123" {
		t.Errorf("Idempotency-Key = %q", gotIdempotency)
	}
	if gotBody != `{"model":"grok-imagine-video","prompt":"a cat"}` {
		t.Errorf("body forwarded = %q", gotBody)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if string(res.Body) != `{"request_id":"req-1","status":"pending"}` {
		t.Errorf("response body = %q", res.Body)
	}
	if res.ContentType != "application/json" {
		t.Errorf("response Content-Type = %q", res.ContentType)
	}
	if res.ConnectionID != "conn-1" {
		t.Errorf("ConnectionID = %q, want conn-1", res.ConnectionID)
	}
}

func TestHandle_GetPoll(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"request_id":"req-1","status":"done","video":{"url":"https://x.com/v.mp4"}}`)
	}))
	defer srv.Close()

	h := New(Dependencies{
		VideoBaseURL: func(string) string { return srv.URL },
		Logger:       captureLogger{},
		Config:       config.Config{},
	})
	res := h.Handle(context.Background(), Request{
		RequestID:   "req-1",
		ProviderID:  "xai",
		Credentials: creds("k"),
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/req-1" {
		t.Errorf("path = %q, want /req-1", gotPath)
	}
	if !contains(string(res.Body), `"status":"done"`) {
		t.Errorf("poll body = %q", res.Body)
	}
}

func TestHandle_TokenRefreshRetryOn401(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"token expired"}`)
			return
		}
		// Second call: Authorization should use the refreshed token.
		if r.Header.Get("Authorization") != "Bearer refreshed-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	refreshed := creds("will-be-overridden")
	refreshed.AccessToken = "refreshed-tok"
	h := New(Dependencies{
		VideoBaseURL: func(string) string { return srv.URL },
		RefreshCredentials: func(ctx context.Context, id string, c domainProv.Credentials) (domainProv.Credentials, bool, error) {
			return refreshed, true, nil
		},
		Logger: captureLogger{},
		Config: config.Config{},
	})
	res := h.Handle(context.Background(), Request{
		Action:      ActionGenerations,
		Body:        []byte(`{}`),
		ProviderID:  "xai",
		Credentials: creds("old-tok"),
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if calls != 2 {
		t.Errorf("expected 2 upstream calls (initial + refresh retry), got %d", calls)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after refresh retry", res.StatusCode)
	}
}

func TestHandle_InvalidAction(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "xai", Action: "bogus"})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestHandle_DefaultsApplied(t *testing.T) {
	h := New(Dependencies{})
	if h.deps.VideoBaseURL == nil {
		t.Error("VideoBaseURL default not applied")
	}
	if h.deps.HTTPClient == nil {
		t.Error("HTTPClient default not applied")
	}
	if h.deps.Logger == nil {
		t.Error("Logger default not applied")
	}
	if h.deps.VideoBaseURL("xai") != "https://api.x.ai/v1/videos" {
		t.Errorf("default xai base wrong")
	}
	if h.deps.VideoBaseURL("openai") != "" {
		t.Errorf("non-xai should have no video base")
	}
}

func TestHandle_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()
	h := New(Dependencies{
		VideoBaseURL: func(string) string { return srv.URL },
		Logger:       captureLogger{},
		Config:       config.Config{},
	})
	res := h.Handle(context.Background(), Request{Action: ActionGenerations, ProviderID: "xai", Credentials: creds("k")})
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (passthrough)", res.StatusCode)
	}
}

func TestCredentialToken(t *testing.T) {
	if got := credentialToken(domainProv.Credentials{AccessToken: "tok"}); got != "tok" {
		t.Errorf("accessToken precedence: %q", got)
	}
	if got := credentialToken(domainProv.Credentials{APIKey: "k"}); got != "k" {
		t.Errorf("apiKey fallback: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
