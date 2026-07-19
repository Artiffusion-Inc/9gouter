package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/resolver"
)

// stubTokenRefresher is a resolver.TokenRefresher that returns a fixed rotated
// access token, so the v1 reactive/proactive refresh paths can be exercised
// without hitting the network. It counts how many times Refresh ran.
type stubTokenRefresher struct {
	token  string
	calls  int32
}

func (s *stubTokenRefresher) Refresh(_ context.Context, _ string, _ map[string]any, _ resolver.ProxyOptions, _ resolver.Logger) (*resolver.RefreshedCredentials, error) {
	atomic.AddInt32(&s.calls, 1)
	return &resolver.RefreshedCredentials{AccessToken: s.token, ExpiresIn: 3600}, nil
}

// perAttemptChatHandler returns a different result per attempt count for a
// given connectionId, so the reactive retry test can make the first attempt
// 401 and the second (same connection, after refresh) 200.
type perAttemptChatHandler struct {
	// results[connectionId] is a slice consumed left-to-right per attempt.
	results map[string][]ChatResult
	seen    []string
}

func (s *perAttemptChatHandler) Handle(ctx context.Context, req ChatRequest, w http.ResponseWriter, sse *Writer) (ChatResult, error) {
	s.seen = append(s.seen, req.ConnectionID)
	seq := s.results[req.ConnectionID]
	if len(seq) == 0 {
		return ChatResult{StatusCode: http.StatusBadGateway, Err: errors.New("no result configured")}, nil
	}
	res := seq[0]
	s.results[req.ConnectionID] = seq[1:]
	if res.Err != nil {
		return res, nil
	}
	if res.Streamed {
		return res, nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","model":"claude-3","choices":[{"message":{"content":"ok"}}]}`))
	return res, nil
}

// TestV1_ChatCompletions_Reactive401RefreshRetries verifies #2703 Fix 2d: a
// 401 from the upstream triggers a refresh of the connection's credentials and
// a retry on the SAME connection (not a rotate to the next), so a temporarily
// expired access token is recovered in-flight. The chat handler returns 401 on
// the first attempt and 200 on the second; the stub refresher rotates the
// token; the client gets 200 and only one connection is used.
func TestV1_ChatCompletions_Reactive401RefreshRetries(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()
	// One connection with a refresh token. The access token is "stale-at" and
	// the stub refresher rotates it to "fresh-at" on refresh.
	mustCreateConnectionWithID(t, db, "claude-a", "claude",
		`{"accessToken":"stale-at","refreshToken":"rt","expiresAt":"2020-01-01T00:00:00Z","providerSpecificData":{"connectionProxyEnabled":false}}`)

	chat := &perAttemptChatHandler{results: map[string][]ChatResult{
		"claude-a": {
			{StatusCode: http.StatusUnauthorized, Err: errUnauthorized()},
			{StatusCode: http.StatusOK},
		},
	}}
	refresher := &stubTokenRefresher{token: "fresh-at"}
	deps := V1Deps{
		APIKeysRepo:     repo.NewAPIKeyRepo(db),
		SettingsRepo:    repo.NewSettingsRepo(db),
		ConnectionRepo:  repo.NewConnectionRepo(db),
		ComboRepo:       repo.NewComboRepo(db),
		AliasRepo:        repo.NewAliasRepo(db),
		NodeRepo:        repo.NewNodeRepo(db),
		ProxyPoolRepo:   repo.NewProxyPoolRepo(db),
		Chat:            chat,
		Config:          config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
		TokenRefreshers: map[string]resolver.TokenRefresher{"claude": refresher},
	}

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"model":"claude/claude-3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reactive refresh should recover the 401); body=%s", rec.Code, rec.Body.String())
	}
	if len(chat.seen) != 2 || chat.seen[0] != "claude-a" || chat.seen[1] != "claude-a" {
		t.Fatalf("expected 2 attempts both on claude-a (refresh+retry), got %v", chat.seen)
	}
	if atomic.LoadInt32(&refresher.calls) != 1 {
		t.Fatalf("expected exactly 1 refresh, got %d", refresher.calls)
	}
	// The rotated token must be persisted to the connection.
	conn, err := deps.ConnectionRepo.GetByID(context.Background(), "claude-a")
	if err != nil || conn == nil {
		t.Fatalf("get conn: %v", err)
	}
	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)
	if at, _ := data["accessToken"].(string); at != "fresh-at" {
		t.Errorf("persisted accessToken=%q want fresh-at", at)
	}
}

// TestV1_ChatCompletions_Reactive401NoRefreshableFallsBack verifies that when
// a 401 fires on a connection whose provider has no refresher, the loop falls
// through to the normal account fallback (rotate to the next connection)
// instead of looping.
func TestV1_ChatCompletions_Reactive401NoRefreshableFallsBack(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()
	mustCreateConnectionWithID(t, db, "claude-a", "claude",
		`{"accessToken":"stale-at","refreshToken":"rt","providerSpecificData":{"connectionProxyEnabled":false}}`)
	mustCreateConnectionWithID(t, db, "claude-b", "claude",
		`{"accessToken":"good-at","providerSpecificData":{"connectionProxyEnabled":false}}`)

	chat := &perAttemptChatHandler{results: map[string][]ChatResult{
		"claude-a": {{StatusCode: http.StatusUnauthorized, Err: errUnauthorized()}},
		"claude-b": {{StatusCode: http.StatusOK}},
	}}
	// Inject an explicit nil refresher for claude so lookupRefresher returns
	// nil (no refresh) and the loop rotates to the next connection. Without
	// this the real tokenrefresh.Lookup("claude") would try to hit the network.
	deps := V1Deps{
		APIKeysRepo:     repo.NewAPIKeyRepo(db),
		SettingsRepo:    repo.NewSettingsRepo(db),
		ConnectionRepo:  repo.NewConnectionRepo(db),
		ComboRepo:       repo.NewComboRepo(db),
		AliasRepo:        repo.NewAliasRepo(db),
		NodeRepo:        repo.NewNodeRepo(db),
		ProxyPoolRepo:   repo.NewProxyPoolRepo(db),
		Chat:            chat,
		Config:          config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
		TokenRefreshers: map[string]resolver.TokenRefresher{"claude": nil},
	}

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"model":"claude/claude-3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (should rotate to claude-b); body=%s", rec.Code, rec.Body.String())
	}
	if len(chat.seen) != 2 || chat.seen[0] != "claude-a" || chat.seen[1] != "claude-b" {
		t.Fatalf("expected a→b rotation (no refresher), got %v", chat.seen)
	}
}

// errUnauthorized returns the 401 error the chat path surfaces.
func errUnauthorized() error { return &unauthorizedErr{} }

type unauthorizedErr struct{}

func (*unauthorizedErr) Error() string { return "401 unauthorized" }