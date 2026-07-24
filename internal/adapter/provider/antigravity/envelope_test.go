package antigravityexec

// envelope_test.go pins the 71cd5b2f Antigravity IDE-fingerprint half: the
// request envelope (project/model/userAgent/requestType/requestId) wrapped
// around the Gemini-shaped request body, the 64000 maxOutputTokens cap, the
// blacklisted-thinking-field strip, and the IDE-shaped deterministic
// requestId (uuidFromSeed → agent/<conv>/<ts>/<traj>/<step>). Pure-logic
// TransformRequest tests + one E2E Execute test that captures the upstream
// body via a real httptest.Server (no mock executor; real BaseExecutor.Execute
// through the wrapper override).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// envelopeTransform runs TransformRequest on a fresh executor and returns the
// decoded envelope map.
func envelopeTransform(t *testing.T, body string, creds domain.Credentials) map[string]any {
	t.Helper()
	e := New(base.Config{ID: "antigravity"})
	out, err := e.TransformRequest("gemini-2.5-pro", json.RawMessage(body), true, creds)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, string(out))
	}
	return m
}

// TestEnvelope_Shape verifies the envelope wraps the request body in the
// project/model/userAgent/requestType/requestId/request fields.
func TestEnvelope_Shape(t *testing.T) {
	env := envelopeTransform(t, `{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.7}}}`, domain.Credentials{})
	if env["project"] == "" {
		t.Error("missing envelope.project")
	}
	if env["model"] != "gemini-2.5-pro" {
		t.Errorf("model=%v want gemini-2.5-pro", env["model"])
	}
	if env["userAgent"] != "antigravity" {
		t.Errorf("userAgent=%v want antigravity", env["userAgent"])
	}
	if env["requestType"] != "agent" {
		t.Errorf("requestType=%v want agent", env["requestType"])
	}
	req, ok := env["request"].(map[string]any)
	if !ok {
		t.Fatal("envelope.request is not an object")
	}
	if _, ok := req["contents"].([]any); !ok {
		t.Errorf("envelope.request.contents lost: %v", req["contents"])
	}
}

// TestEnvelope_RequestIdIDEShape verifies the requestId matches the IDE shape
// agent/<conversationId>/<timestamp>/<trajectoryId>/<step>.
func TestEnvelope_RequestIdIDEShape(t *testing.T) {
	env := envelopeTransform(t, `{}`, domain.Credentials{ProviderSpecificData: map[string]any{"_connectionId": "conn-7"}})
	rid, _ := env["requestId"].(string)
	if !strings.HasPrefix(rid, "agent/") {
		t.Fatalf("requestId=%q want agent/ prefix", rid)
	}
	parts := strings.Split(strings.TrimPrefix(rid, "agent/"), "/")
	if len(parts) != 4 {
		t.Fatalf("requestId parts=%v want 4", parts)
	}
	for _, ch := range parts[1] { // timestamp digits
		if ch < '0' || ch > '9' {
			t.Fatalf("timestamp part %q must be digits", parts[1])
		}
	}
	for _, ch := range parts[3] { // step digits
		if ch < '0' || ch > '9' {
			t.Fatalf("step part %q must be digits", parts[3])
		}
	}
	// conversationId and trajectoryId are uuid-shaped (contain a dash).
	if !strings.Contains(parts[0], "-") {
		t.Errorf("conversationId %q not uuid-shaped", parts[0])
	}
	if !strings.Contains(parts[2], "-") {
		t.Errorf("trajectoryId %q not uuid-shaped", parts[2])
	}
}

// TestEnvelope_RequestIdDeterministic verifies the conversationId/trajectoryId
// are deterministic for the same session (uuidFromSeed), so a replay yields the
// same ids (only the timestamp differs).
func TestEnvelope_RequestIdDeterministic(t *testing.T) {
	creds := domain.Credentials{ProviderSpecificData: map[string]any{"_connectionId": "stable-conn"}}
	a := envelopeTransform(t, `{}`, creds)
	b := envelopeTransform(t, `{}`, creds)
	ra, _ := a["requestId"].(string)
	rb, _ := b["requestId"].(string)
	pa := strings.Split(strings.TrimPrefix(ra, "agent/"), "/")
	pb := strings.Split(strings.TrimPrefix(rb, "agent/"), "/")
	if pa[0] != pb[0] {
		t.Errorf("conversationId not deterministic: %q vs %q", pa[0], pb[0])
	}
	if pa[2] != pb[2] {
		t.Errorf("trajectoryId not deterministic: %q vs %q", pa[2], pb[2])
	}
	if pa[1] == pb[1] {
		// Timestamps could collide on a fast machine, but a mismatch is the
		// expected norm — only assert it is not a hard requirement.
	}
}

// TestEnvelope_RequestIdIdempotent verifies a body already carrying an
// IDE-shaped requestId is preserved verbatim.
func TestEnvelope_RequestIdIdempotent(t *testing.T) {
	existing := "agent/abc-123/1700000000/def-456/3"
	env := envelopeTransform(t, `{"requestId":"`+existing+`"}`, domain.Credentials{})
	if env["requestId"] != existing {
		t.Errorf("requestId=%v want preserved %q", env["requestId"], existing)
	}
}

// TestEnvelope_RequestIdNonIDENotPreserved verifies a non-IDE-shaped requestId
// is replaced (not carried through).
func TestEnvelope_RequestIdNonIDENotPreserved(t *testing.T) {
	bogus := "random-agent-uuid-xyz"
	env := envelopeTransform(t, `{"requestId":"`+bogus+`"}`, domain.Credentials{})
	rid, _ := env["requestId"].(string)
	if rid == bogus {
		t.Errorf("non-IDE requestId %q must be replaced, not preserved", bogus)
	}
	if !strings.HasPrefix(rid, "agent/") {
		t.Errorf("replacement requestId=%q must be IDE-shaped", rid)
	}
}

// TestEnvelope_MaxOutputCap verifies generationConfig.maxOutputTokens above
// 64000 is clamped to 64000.
func TestEnvelope_MaxOutputCap(t *testing.T) {
	env := envelopeTransform(t, `{"request":{"generationConfig":{"maxOutputTokens":128000}}}`, domain.Credentials{})
	gc, _ := env["request"].(map[string]any)["generationConfig"].(map[string]any)
	if v := int(gc["maxOutputTokens"].(float64)); v != 64000 {
		t.Errorf("maxOutputTokens=%v want 64000 (capped)", gc["maxOutputTokens"])
	}
}

// TestEnvelope_MaxOutputUnderCapPreserved verifies a cap under 64000 is left
// untouched.
func TestEnvelope_MaxOutputUnderCapPreserved(t *testing.T) {
	env := envelopeTransform(t, `{"request":{"generationConfig":{"maxOutputTokens":8192}}}`, domain.Credentials{})
	gc, _ := env["request"].(map[string]any)["generationConfig"].(map[string]any)
	if v := int(gc["maxOutputTokens"].(float64)); v != 8192 {
		t.Errorf("maxOutputTokens=%v want 8192 (under cap, preserved)", gc["maxOutputTokens"])
	}
}

// TestEnvelope_StripBlacklisted verifies the blacklisted thinking fields are
// stripped from both the top-level body and request.
func TestEnvelope_StripBlacklisted(t *testing.T) {
	env := envelopeTransform(t, `{"thinking":{"budget":1000},"thinkingConfig":{"include":true},"request":{"contents":[],"thinking":"x","reasoning_effort":"high"}}`, domain.Credentials{})
	if _, ok := env["thinking"]; ok {
		t.Error("top-level thinking not stripped")
	}
	if _, ok := env["thinkingConfig"]; ok {
		t.Error("top-level thinkingConfig not stripped")
	}
	req, _ := env["request"].(map[string]any)
	for _, k := range []string{"thinking", "reasoning_effort"} {
		if _, ok := req[k]; ok {
			t.Errorf("request.%s not stripped", k)
		}
	}
	if _, ok := req["contents"].([]any); !ok {
		t.Error("request.contents must be preserved")
	}
}

// TestEnvelope_SessionResolution verifies the session id fallback chain
// (request.sessionId → _connectionId → email → anonymous) drives the
// conversationId so distinct sessions yield distinct ids.
func TestEnvelope_SessionResolution(t *testing.T) {
	// request.sessionId wins.
	a := envelopeTransform(t, `{"request":{"sessionId":"sess-A"}}`, domain.Credentials{ProviderSpecificData: map[string]any{"_connectionId": "conn-X"}})
	// connectionId fallback.
	b := envelopeTransform(t, `{}`, domain.Credentials{ProviderSpecificData: map[string]any{"_connectionId": "conn-Y"}})
	// email fallback.
	c := envelopeTransform(t, `{}`, domain.Credentials{ProviderSpecificData: map[string]any{"email": "u@x.com"}})
	// anonymous.
	d := envelopeTransform(t, `{}`, domain.Credentials{})
	ridOf := func(m map[string]any) string {
		rid, _ := m["requestId"].(string)
		return strings.SplitN(strings.TrimPrefix(rid, "agent/"), "/", 2)[0]
	}
	if ridOf(a) == ridOf(b) {
		t.Error("sessionId-derived conversationId collided with connectionId-derived")
	}
	if ridOf(b) == ridOf(c) {
		t.Error("connectionId-derived collided with email-derived")
	}
	if ridOf(c) == ridOf(d) {
		t.Error("email-derived collided with anonymous")
	}
	// Anonymous is deterministic.
	d2 := envelopeTransform(t, `{}`, domain.Credentials{})
	if ridOf(d) != ridOf(d2) {
		t.Error("anonymous conversationId not deterministic")
	}
}

// TestEnvelope_StepFromContents verifies the requestId step is contents*2-1.
func TestEnvelope_StepFromContents(t *testing.T) {
	env := envelopeTransform(t, `{"request":{"contents":[{"role":"user"},{"role":"model"},{"role":"user"}]}}`, domain.Credentials{})
	rid, _ := env["requestId"].(string)
	step := strings.Split(strings.TrimPrefix(rid, "agent/"), "/")[3]
	if step != "5" {
		t.Errorf("step=%s want 5 (3 contents * 2 - 1)", step)
	}
}

// TestEnvelope_ProjectFromCredentials verifies a credential projectId is used
// verbatim.
func TestEnvelope_ProjectFromCredentials(t *testing.T) {
	env := envelopeTransform(t, `{}`, domain.Credentials{ProviderSpecificData: map[string]any{"projectId": "proj-42"}})
	if env["project"] != "proj-42" {
		t.Errorf("project=%v want proj-42", env["project"])
	}
}

// TestExecute_EnvelopeSentUpstream verifies the E2E Execute path applies the
// envelope to the body actually sent upstream: a capturing httptest server
// receives the wrapped envelope (project/model/userAgent/requestType/requestId),
// not the raw request body.
func TestExecute_EnvelopeSentUpstream(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	defer srv.Close()

	e := New(base.Config{
		ID:      "antigravity",
		BaseURL: srv.URL,
		Format:  "antigravity",
	})
	e.Fetch = mockFetchTo(srv)

	req := domain.ExecRequest{
		Model:  "gemini-2.5-pro",
		Body:   json.RawMessage(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":200000}}}`),
		Stream: true,
		Credentials: domain.Credentials{
			APIKey:               "tok",
			ProviderSpecificData: map[string]any{"_connectionId": "conn-e2e", "projectId": "proj-e2e"},
		},
	}
	resp, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer resp.Response.Body.Close()
	resp.Done()

	var up map[string]any
	if err := json.Unmarshal([]byte(captured), &up); err != nil {
		t.Fatalf("upstream body not JSON: %v (body=%s)", err, captured)
	}
	if up["project"] != "proj-e2e" {
		t.Errorf("upstream.project=%v want proj-e2e", up["project"])
	}
	if up["model"] != "gemini-2.5-pro" {
		t.Errorf("upstream.model=%v want gemini-2.5-pro", up["model"])
	}
	if up["userAgent"] != "antigravity" {
		t.Errorf("upstream.userAgent=%v want antigravity", up["userAgent"])
	}
	if up["requestType"] != "agent" {
		t.Errorf("upstream.requestType=%v want agent", up["requestType"])
	}
	rid, _ := up["requestId"].(string)
	if !strings.HasPrefix(rid, "agent/") {
		t.Errorf("upstream.requestId=%q want agent/ prefix", rid)
	}
	gc, _ := up["request"].(map[string]any)["generationConfig"].(map[string]any)
	if v := int(gc["maxOutputTokens"].(float64)); v != 64000 {
		t.Errorf("upstream maxOutputTokens=%d want 64000 (capped on real path)", v)
	}
}

// TestExecute_TransformErrorShortCircuits verifies a TransformRequest error
// (unreachable for the passthrough-fallback path, but the Execute override
// returns it) surfaces. A nil body passes through without error, so exercise
// the success path here; the error path is structurally unreachable because
// TransformRequest never returns an error.
func TestExecute_NilBodyEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	defer srv.Close()

	e := New(base.Config{ID: "antigravity", BaseURL: srv.URL, Format: "antigravity"})
	e.Fetch = mockFetchTo(srv)
	resp, err := e.Execute(context.Background(), domain.ExecRequest{Model: "m", Body: nil, Stream: true})
	if err != nil {
		t.Fatalf("Execute nil body: %v", err)
	}
	defer resp.Response.Body.Close()
	resp.Done()
	if resp.Response.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.Response.StatusCode)
	}
}

// keep proxy import referenced (mockFetchTo uses it).
var _ = proxy.Options{}
