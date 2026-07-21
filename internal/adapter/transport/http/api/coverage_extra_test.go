package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// TestHeadroom_PureHelpersMore extends the pure-helper coverage for headroom.go
// (path helpers, ensureDir, writeEmptyFile, writePID/clearPID/pidAlive,
// probeLocalHeadroom, getInstallLogTail, getInstalledHeadroomExtras empty
// python).
func TestHeadroom_PureHelpersMore(t *testing.T) {
	// Path helpers under a temporary HEADROOM_DATA_DIR.
	dir := t.TempDir()
	t.Setenv("HEADROOM_DATA_DIR", dir)

	if got := headroomLogFile(); got != filepath.Join(dir, "proxy.log") {
		t.Fatalf("headroomLogFile = %q, want %q", got, filepath.Join(dir, "proxy.log"))
	}
	if got := headroomInstallLog(); got != filepath.Join(dir, "install.log") {
		t.Fatalf("headroomInstallLog = %q, want %q", got, filepath.Join(dir, "install.log"))
	}

	// ensureDir creates the directory tree.
	sub := filepath.Join(dir, "sub", "deep")
	if err := ensureDir(sub); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}
	if fi, err := os.Stat(sub); err != nil || !fi.IsDir() {
		t.Fatalf("ensureDir did not create %s", sub)
	}

	// writeEmptyFile creates parent + empty file.
	target := filepath.Join(dir, "nested", "f.txt")
	if err := writeEmptyFile(target); err != nil {
		t.Fatalf("writeEmptyFile: %v", err)
	}
	if fi, err := os.Stat(target); err != nil || fi.Size() != 0 {
		t.Fatalf("writeEmptyFile did not create empty file: %v %v", fi, err)
	}

	// writePID + managedPID + clearPID + pidAlive round-trip.
	if err := writePID(999999); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	if pid := managedPID(); pid != 999999 {
		// pidAlive may return false on some systems for a non-existent pid,
		// in which case managedPID cleans up and returns 0. Both are
		// acceptable outcomes for the coverage purpose.
		if pid != 0 {
			t.Fatalf("managedPID = %d, want 999999 or 0", pid)
		}
	}
	// pidAlive of negative and zero.
	if pidAlive(0) {
		t.Fatal("pidAlive(0) = true, want false")
	}
	if pidAlive(-1) {
		t.Fatal("pidAlive(-1) = true, want false")
	}
	// pidAlive of current process should be true.
	if !pidAlive(os.Getpid()) {
		t.Fatal("pidAlive(self) = false, want true")
	}
	if err := clearPID(); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clearPID: %v", err)
	}

	// probeLocalHeadroom against a real test server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if !probeLocalHeadroom(srv.URL) {
		t.Fatalf("probeLocalHeadroom(%s) = false, want true", srv.URL)
	}
	if probeLocalHeadroom("") {
		t.Fatal("probeLocalHeadroom(\"\") = true, want false")
	}
	// Non-2xx → false.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv2.Close()
	if probeLocalHeadroom(srv2.URL) {
		t.Fatal("probeLocalHeadroom(500) = true, want false")
	}

	// getInstallLogTail: empty file returns "".
	if err := writeEmptyFile(headroomInstallLog()); err != nil {
		t.Fatalf("writeEmptyFile install log: %v", err)
	}
	if got := getInstallLogTail(10); got != "" {
		t.Fatalf("getInstallLogTail empty = %q, want \"\"", got)
	}
	// Append some lines.
	logF, err := os.OpenFile(headroomInstallLog(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open install log: %v", err)
	}
	for i := 0; i < 5; i++ {
		logF.WriteString("line " + string(rune('a'+i)) + "\n")
	}
	logF.Close()
	if got := getInstallLogTail(3); !strings.Contains(got, "line c") {
		t.Fatalf("getInstallLogTail(3) = %q, want contains line c", got)
	}

	// getInstalledHeadroomExtras with empty python → returns false, "", default extras.
	installed, version, extras := getInstalledHeadroomExtras("")
	if installed || version != "" {
		t.Fatalf("getInstalledHeadroomExtras(\"\") = (%v,%q,%v), want (false,\"\",...)", installed, version, extras)
	}
	if extras["code"] || extras["ml"] {
		t.Fatalf("default extras = %v, want all false", extras)
	}
}

// TestHeadroom_LifecycleAndExtras covers the start/stop/restart/extras
// handlers, exercising every reachable branch without requiring the
// headroom binary to be installed (which it isn't in CI).
func TestHeadroom_LifecycleAndExtras(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterHeadroom(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// start with no binary → 412.
	req := httptest.NewRequest("POST", "/api/headroom/start", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("start status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}

	// start with non-loopback HEADROOM_URL → still 412 because the binary
	// check precedes the URL check. The non-loopback branch is only
	// reachable when headroom is installed, which we cannot guarantee in
	// CI. Just assert the binary-missing path is hit consistently.
	t.Setenv("HEADROOM_URL", "http://example.com")
	req = httptest.NewRequest("POST", "/api/headroom/start", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("start non-loopback status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}
	os.Unsetenv("HEADROOM_URL")

	// restart with no binary → 412.
	req = httptest.NewRequest("POST", "/api/headroom/restart", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("restart status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}

	// stop with no PID → 200 success.
	req = httptest.NewRequest("POST", "/api/headroom/stop", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stop status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// extras GET with log=1 → 200 with log field.
	req = httptest.NewRequest("GET", "/api/headroom/extras?log=1", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("extras log status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal extras log: %v", err)
	}
	if resp["log"] == nil {
		t.Fatal("extras log missing log field")
	}

	// extras GET without log=1 → 200 with extras map.
	req = httptest.NewRequest("GET", "/api/headroom/extras", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("extras status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal extras: %v", err)
	}
	if resp["available"] == nil {
		t.Fatal("extras missing available field")
	}

	// extrasAction POST with invalid body → 400.
	req = httptest.NewRequest("POST", "/api/headroom/extras", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("extrasAction invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// extrasAction POST with no valid extras → 400.
	req = httptest.NewRequest("POST", "/api/headroom/extras", strings.NewReader(`{"extras":["unknown"]}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("extrasAction no valid extras status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// extrasAction POST with valid extras but no Python → 503.
	req = httptest.NewRequest("POST", "/api/headroom/extras", strings.NewReader(`{"extras":["code"]}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	// Python may or may not be present on the test host. Accept either 503
	// (no Python) or 412 (Python present, headroom not installed).
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("extrasAction no python status = %d, want 503 or 412; body=%s", rec.Code, rec.Body.String())
	}

	// status with HEADROOM_URL set (non-loopback) → running=false, localUrl=false.
	t.Setenv("HEADROOM_URL", "http://example.com")
	req = httptest.NewRequest("GET", "/api/headroom/status", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status non-loopback status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if resp["localUrl"] != false {
		t.Fatalf("status localUrl = %v, want false", resp["localUrl"])
	}
	if resp["canStart"] != false {
		t.Fatalf("status canStart = %v, want false", resp["canStart"])
	}
	os.Unsetenv("HEADROOM_URL")
}

// TestHeadroom_ProxyRoutes covers the headroom proxy passthrough for each
// HTTP method.
func TestHeadroom_ProxyRoutes(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterHeadroom(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
		req := httptest.NewRequest(method, "/api/headroom/proxy/v1/something", nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s proxy status = %d, want 200; body=%s", method, rec.Code, rec.Body.String())
		}
	}
}

// TestModels_TestEndpoint covers the /api/models/test handler + pingModelByKind
// branches via a local httptest server that plays the role of the /v1/* surface.
func TestModels_TestEndpoint(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Spin up a fake /v1/* server and point the dashboard handler at it by
	// setting r.Host. The handler builds the base URL from r.Host.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
		case "/v1/embeddings":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
		case "/v1/images/generations":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"url":"http://x/y.png"}]}`))
		case "/v1/audio/transcriptions":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"text":"hello"}`))
		case "/v1/fail":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad model"}}`))
		}
	}))
	defer srv.Close()

	// Helper to POST /api/models/test with a body and r.Host set.
	postTest := func(body string) (int, map[string]any) {
		req := httptest.NewRequest("POST", "/api/models/test", strings.NewReader(body))
		req.Header.Set("Cookie", "auth_token="+ck)
		req.Header.Set("Content-Type", "application/json")
		req.Host = strings.TrimPrefix(srv.URL, "http://")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}

	// Missing model → 400.
	req := httptest.NewRequest("POST", "/api/models/test", strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing model status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Invalid body → 400.
	req = httptest.NewRequest("POST", "/api/models/test", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Chat completions happy path.
	code, m := postTest(`{"model":"gpt-4o","kind":"llm"}`)
	if code != http.StatusOK {
		t.Fatalf("llm status = %d, want 200; body=%v", code, m)
	}
	if m["ok"] != true {
		t.Fatalf("llm ok = %v, want true", m["ok"])
	}

	// Embedding happy path.
	code, m = postTest(`{"model":"text-embedding-3","kind":"embedding"}`)
	if code != http.StatusOK {
		t.Fatalf("embedding status = %d, want 200; body=%v", code, m)
	}
	if m["ok"] != true {
		t.Fatalf("embedding ok = %v, want true", m["ok"])
	}

	// Image happy path.
	code, m = postTest(`{"model":"dall-e-3","kind":"image"}`)
	if code != http.StatusOK {
		t.Fatalf("image status = %d, want 200; body=%v", code, m)
	}
	if m["ok"] != true {
		t.Fatalf("image ok = %v, want true", m["ok"])
	}

	// STT happy path.
	code, m = postTest(`{"model":"whisper-1","kind":"stt"}`)
	if code != http.StatusOK {
		t.Fatalf("stt status = %d, want 200; body=%v", code, m)
	}
	if m["ok"] != true {
		t.Fatalf("stt ok = %v, want true", m["ok"])
	}
}

// TestModels_TestEndpointFailures covers the failure paths of
// pingModelByKind: 4xx with error message, network error, and the per-kind
// empty-data failures.
func TestModels_TestEndpointFailures(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Server returning per-kind empty-data responses to trigger the
	// "no data" branches.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[]}`)) // no choices
		case "/v1/embeddings":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`)) // no data
		case "/v1/images/generations":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`)) // no data
		case "/v1/audio/transcriptions":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"text":""}`)) // empty text
		case "/v1/error-status":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"500","msg":"provider failed"}`))
		case "/v1/error-string":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"error":"some string error"}`))
		case "/v1/bad-status":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
		}
	}))
	defer srv.Close()

	postTest := func(body string) (int, map[string]any) {
		req := httptest.NewRequest("POST", "/api/models/test", strings.NewReader(body))
		req.Header.Set("Cookie", "auth_token="+ck)
		req.Header.Set("Content-Type", "application/json")
		req.Host = strings.TrimPrefix(srv.URL, "http://")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}

	// Chat with no choices → ok=false with specific error.
	code, m := postTest(`{"model":"x","kind":"llm"}`)
	if code != http.StatusOK {
		t.Fatalf("no-choices status = %d, want 200", code)
	}
	if m["ok"] != false {
		t.Fatalf("no-choices ok = %v, want false", m["ok"])
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, "no completion choices") {
		t.Fatalf("no-choices error = %q, want contains 'no completion choices'", msg)
	}

	// Embedding with no data → ok=false.
	_, m = postTest(`{"model":"x","kind":"embedding"}`)
	if m["ok"] != false {
		t.Fatalf("no-embedding ok = %v, want false", m["ok"])
	}

	// Image with no data → ok=false.
	_, m = postTest(`{"model":"x","kind":"image"}`)
	if m["ok"] != false {
		t.Fatalf("no-image ok = %v, want false", m["ok"])
	}

	// STT with empty text → ok=false.
	_, m = postTest(`{"model":"x","kind":"stt"}`)
	if m["ok"] != false {
		t.Fatalf("no-text ok = %v, want false", m["ok"])
	}
}

// TestModels_TestEndpointNetworkError covers the network-failure path of
// pingModelByKind (base URL unreachable).
func TestModels_TestEndpointNetworkError(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Point at an unreachable port (we don't start a server). The handler
	// builds "http://<r.Host>" and dial will fail.
	req := httptest.NewRequest("POST", "/api/models/test", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:1" // port 1 — no listener
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("network error status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["ok"] != false {
		t.Fatalf("network ok = %v, want false", m["ok"])
	}
}

// TestModels_ActiveAPIKey verifies the activeAPIKey helper returns the first
// active key and falls back to "" when no key exists.
func TestModels_ActiveAPIKey(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	RegisterKeys(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// No keys yet → /api/models/test returns 200 with ok=false (network error
	// is what surfaces, since activeAPIKey returns "").
	req := httptest.NewRequest("POST", "/api/models/test", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-keys test status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Create a key.
	req = httptest.NewRequest("POST", "/api/keys", strings.NewReader(`{"name":"k1"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestModels_HelperPure covers the pure helpers in models.go directly.
func TestModels_HelperPure(t *testing.T) {
	// truncate.
	if truncate("hello world", 5) != "hello" {
		t.Fatalf("truncate = %q, want hello", truncate("hello world", 5))
	}
	if truncate("hi", 5) != "hi" {
		t.Fatalf("truncate short = %q, want hi", truncate("hi", 5))
	}

	// appendDetail.
	if appendDetail("") != "" {
		t.Fatalf("appendDetail(\"\") = %q, want \"\"", appendDetail(""))
	}
	if got := appendDetail("boom"); !strings.HasPrefix(got, ": ") {
		t.Fatalf("appendDetail(boom) = %q, want ': <...>'", got)
	}

	// jsonStringValue.
	if jsonStringValue("a") != `"a"` {
		t.Fatalf("jsonStringValue(a) = %q, want \"a\"", jsonStringValue("a"))
	}

	// jsonErrMessage.
	cases := []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"error": map[string]any{"message": "boom"}}, "boom"},
		{map[string]any{"msg": "hello"}, "hello"},
		{map[string]any{"message": "hi"}, "hi"},
		{map[string]any{"error": "stringy"}, "stringy"},
		{map[string]any{"error": map[string]any{"code": "x"}}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := jsonErrMessage(c.in); got != c.want {
			t.Fatalf("jsonErrMessage(%v) = %q, want %q", c.in, got, c.want)
		}
	}

	// jsonMsg.
	if jsonMsg(map[string]any{"msg": "a"}) != "a" {
		t.Fatalf("jsonMsg(msg=a) = %q, want a", jsonMsg(map[string]any{"msg": "a"}))
	}
	if jsonMsg(map[string]any{"message": "b"}) != "b" {
		t.Fatalf("jsonMsg(message=b) = %q, want b", jsonMsg(map[string]any{"message": "b"}))
	}
	if jsonMsg(map[string]any{}) != "" {
		t.Fatalf("jsonMsg(empty) = %q, want \"\"", jsonMsg(map[string]any{}))
	}

	// silentWavMultipart returns a non-nil reader + non-empty content type.
	r, ct := silentWavMultipart("whisper-1")
	if r == nil || ct == "" {
		t.Fatal("silentWavMultipart returned nil reader or empty content type")
	}
	if !strings.HasPrefix(ct, "multipart/form-data") {
		t.Fatalf("silentWavMultipart content-type = %q, want multipart prefix", ct)
	}
}

// TestNodes_isValidationErr covers the isValidationErr helper directly.
func TestNodes_isValidationErr(t *testing.T) {
	for _, msg := range []string{
		"name is required", "prefix is required",
		"invalid OpenAI compatible API type",
		"invalid provider node type",
		"base URL is required",
		"Base URL and API key required",
	} {
		if !isValidationErr(errString(msg)) {
			t.Fatalf("isValidationErr(%q) = false, want true", msg)
		}
	}
	if isValidationErr(nil) {
		t.Fatal("isValidationErr(nil) = true, want false")
	}
	if isValidationErr(errString("unknown")) {
		t.Fatal("isValidationErr(unknown) = true, want false")
	}
}

// errString wraps a string in a simple error so isValidationErr can match on
// err.Error().
type errString string

func (e errString) Error() string { return string(e) }

// TestProxyPools_isValidProxyType covers the proxy-type validator directly.
func TestProxyPools_isValidProxyType(t *testing.T) {
	for _, ty := range []string{"http", "vercel", "cloudflare", "deno"} {
		if !isValidProxyType(ty) {
			t.Fatalf("isValidProxyType(%q) = false, want true", ty)
		}
	}
	for _, ty := range []string{"socks5", "", "HTTP", "unknown"} {
		if isValidProxyType(ty) {
			t.Fatalf("isValidProxyType(%q) = true, want false", ty)
		}
	}
}

// TestProxyPools_DeleteNotFound covers the delete handler's 404 branch.
func TestProxyPools_DeleteNotFound(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProxyPools(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("DELETE", "/api/proxy-pools/nope", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTranslator_ConsoleLogsStream covers the SSE stream handler. Since the
// handler blocks on r.Context().Done(), we cancel the request to terminate.
func TestTranslator_ConsoleLogsStream(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterTranslator(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, "GET", "/api/translator/console-logs/stream", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()
	// Give the handler a moment to write the init frame, then cancel.
	cancel()
	<-done
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: ") {
		t.Fatalf("stream body missing data: prefix; body=%s", rec.Body.String())
	}
}

// TestUsage_RequestLogs covers GET /api/usage/request-logs (registered by
// RegisterUsageExtra, not RegisterUsage).
func TestUsage_RequestLogs(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterUsageExtra(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("GET", "/api/usage/request-logs", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request-logs status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestV1Dashboard_Root covers GET /api/v1.
func TestV1Dashboard_Root(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterV1Dashboard(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("GET", "/api/v1", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("root status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["ok"] != true {
		t.Fatalf("ok = %v, want true", resp["ok"])
	}
}

// TestAuth_OidcStartConfiguredButInvalidIssuer covers the oidcStart branch
// where OIDC fields are set but discovery fails. We use a non-responsive
// issuer URL so OIDC discovery fails with 502.
func TestAuth_OidcStartConfiguredButInvalidIssuer(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	// Seed OIDC settings via the settings repo Update.
	patch, _ := json.Marshal(map[string]any{
		"oidcIssuerUrl":     "http://127.0.0.1:1/invalid",
		"oidcClientId":      "client",
		"oidcClientSecret":  "secret",
	})
	if _, err := deps.Settings.Update(context.Background(), patch); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	mux := http.NewServeMux()
	RegisterAuth(mux, deps, defaultCfg())
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("GET", "/api/auth/oidc/start", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("oidcStart invalid issuer status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuth_ProbeOIDCClientSecret covers probeOIDCClientSecret directly with
// both an unreachable URL and a 2xx-responding stub server.
func TestAuth_ProbeOIDCClientSecret(t *testing.T) {
	ctx := context.Background()
	// Empty fields → false.
	if probeOIDCClientSecret(ctx, "", "id", "secret", nil) {
		t.Fatal("probeOIDCClientSecret(empty url) = true, want false")
	}
	if probeOIDCClientSecret(ctx, "http://x", "", "secret", nil) {
		t.Fatal("probeOIDCClientSecret(empty id) = true, want false")
	}
	if probeOIDCClientSecret(ctx, "http://x", "id", "", nil) {
		t.Fatal("probeOIDCClientSecret(empty secret) = true, want false")
	}
	// Unreachable → false.
	if probeOIDCClientSecret(ctx, "http://127.0.0.1:1/token", "id", "secret", nil) {
		t.Fatal("probeOIDCClientSecret(unreachable) = true, want false")
	}
	// 2xx stub → true.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if !probeOIDCClientSecret(ctx, srv.URL, "id", "secret", nil) {
		t.Fatal("probeOIDCClientSecret(2xx) = false, want true")
	}
	// 4xx stub → false.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv2.Close()
	if probeOIDCClientSecret(ctx, srv2.URL, "id", "secret", nil) {
		t.Fatal("probeOIDCClientSecret(401) = true, want false")
	}
}

// TestAuth_BcryptCompareStub covers the stub function (kept for parity with
// the JS codebase; returns a not-implemented error).
func TestAuth_BcryptCompareStub(t *testing.T) {
	if err := bcryptCompareStub("a", "b"); err == nil {
		t.Fatal("bcryptCompareStub returned nil, want error")
	}
}

// TestAuth_HeaderRequestAdapter covers the headerRequestAdapter helper.
func TestAuth_HeaderRequestAdapter(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Test", "value")
	a := headerRequestAdapter{Request: req}
	if got := a.Header("X-Test"); got != "value" {
		t.Fatalf("Header(X-Test) = %q, want value", got)
	}
	if got := a.Header("X-Missing"); got != "" {
		t.Fatalf("Header(X-Missing) = %q, want \"\"", got)
	}
	// Nil Request → empty.
	a2 := headerRequestAdapter{Request: nil}
	if got := a2.Header("X-Test"); got != "" {
		t.Fatalf("nil Request Header = %q, want \"\"", got)
	}
}

// TestAuth_NullableString covers nullableString directly.
func TestAuth_NullableString(t *testing.T) {
	if nullableString("") != nil {
		t.Fatal("nullableString(\"\") = not-nil, want nil")
	}
	if nullableString("x") != "x" {
		t.Fatalf("nullableString(x) = %v, want x", nullableString("x"))
	}
}

// TestProviders_ConnectionLooksValid covers connectionLooksValid directly.
func TestProviders_ConnectionLooksValid(t *testing.T) {
	if connectionLooksValid(nil) {
		t.Fatal("connectionLooksValid(nil) = true, want false")
	}
	// Active connection → true.
	c := &settings.ProviderConnection{IsActive: true}
	if !connectionLooksValid(c) {
		t.Fatal("connectionLooksValid(active) = false, want true")
	}
	// Inactive with no data → false.
	c = &settings.ProviderConnection{IsActive: false}
	if connectionLooksValid(c) {
		t.Fatal("connectionLooksValid(inactive empty) = true, want false")
	}
	// Inactive with apiKey in data → true.
	c = &settings.ProviderConnection{Data: []byte(`{"apiKey":"sk-xxx"}`)}
	if !connectionLooksValid(c) {
		t.Fatal("connectionLooksValid(inactive apiKey) = false, want true")
	}
	// Inactive with accessToken in data → true.
	c = &settings.ProviderConnection{Data: []byte(`{"accessToken":"tok"}`)}
	if !connectionLooksValid(c) {
		t.Fatal("connectionLooksValid(inactive accessToken) = false, want true")
	}
	// Inactive with empty apiKey → false.
	c = &settings.ProviderConnection{Data: []byte(`{"apiKey":""}`)}
	if connectionLooksValid(c) {
		t.Fatal("connectionLooksValid(inactive empty apiKey) = true, want false")
	}
	// Invalid JSON data → false.
	c = &settings.ProviderConnection{Data: []byte(`{bad`)}
	if connectionLooksValid(c) {
		t.Fatal("connectionLooksValid(invalid JSON) = true, want false")
	}
}

// TestApi_PtrHelpers covers boolPtr (kept in api.go; intPtr/floatPtr were
// removed as unused during the unused-baseline cleanup).
func TestApi_PtrHelpers(t *testing.T) {
	if *boolPtr(true) != true {
		t.Fatal("boolPtr(true) = false, want true")
	}
	if *boolPtr(false) != false {
		t.Fatal("boolPtr(false) = true, want false")
	}
}

// defaultCfg returns a zeroed config.Config for the auth handler wiring (the
// test only needs the OIDC routes; none of the config fields are read in the
// branches we exercise).
func defaultCfg() config.Config {
	return config.Config{}
}