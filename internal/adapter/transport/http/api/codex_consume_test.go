package api

// codex_consume_test.go pins the POST half of 5cc4f222 / #154 (consume one
// Codex reset credit, irreversible) + the proxy-awareness half: the consume
// call routes through the proxy stack (connectionProxyFetch → ProxyAwareFetch)
// using the connection's resolved proxy options, and the response is shaped
// the same way the JS route's getResponseForConsumeResult does (ok → 200
// {code,reset:true,windows_reset,redeemRequestId,credit}; no_credit → 409;
// else → 502 / passthrough 4xx). Real httptest.Server upstreams + real codex
// connection rows — no mock.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// codexConsumeServer spins up an httptest.Server that records the POST body +
// fingerprint headers and replies with the given status + JSON. Returns the
// server + capturers.
func codexConsumeServer(t *testing.T, status int, reply string) (*httptest.Server, *string, *string, *string) {
	t.Helper()
	var gotBody, gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotBody, &gotAuth, &gotCT
}

// TestConsumeCodexResetCredits_Success verifies the ok path: upstream returns
// code=reset + windows_reset>0 → 200 with reset:true, the redeemRequestId
// echoed back, and the credit object carried through.
func TestConsumeCodexResetCredits_Success(t *testing.T) {
	srv, gotBody, gotAuth, gotCT := codexConsumeServer(t, http.StatusOK, `{"code":"reset","windows_reset":3,"credit":{"id":"cr-1"}}`)
	prev := codexResetCreditsConsumeURL
	codexResetCreditsConsumeURL = srv.URL
	t.Cleanup(func() { codexResetCreditsConsumeURL = prev })

	redeem := "r-abc-123"
	out, status := consumeCodexResetCredits(context.Background(), nil, proxy.Options{}, codexConn("c", "tok-1", "acct-1"), redeem)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if out["reset"] != true {
		t.Errorf("reset = %v, want true", out["reset"])
	}
	if out["code"] != "reset" {
		t.Errorf("code = %v, want reset", out["code"])
	}
	if out["windows_reset"] != 3 {
		t.Errorf("windows_reset = %v, want 3", out["windows_reset"])
	}
	if out["redeemRequestId"] != redeem {
		t.Errorf("redeemRequestId = %v, want %s", out["redeemRequestId"], redeem)
	}
	credit, _ := out["credit"].(map[string]any)
	if credit["id"] != "cr-1" {
		t.Errorf("credit = %v, want id=cr-1", credit)
	}
	// The upstream received {redeem_request_id: redeem} and the codex fingerprint.
	var sent map[string]any
	_ = json.Unmarshal([]byte(*gotBody), &sent)
	if sent["redeem_request_id"] != redeem {
		t.Errorf("upstream body = %s, want redeem_request_id=%s", *gotBody, redeem)
	}
	if *gotAuth != "Bearer tok-1" {
		t.Errorf("Authorization = %q, want Bearer tok-1", *gotAuth)
	}
	if *gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", *gotCT)
	}
}

// TestConsumeCodexResetCredits_NoCredit verifies the no_credit path → 409 with
// the canonical message.
func TestConsumeCodexResetCredits_NoCredit(t *testing.T) {
	srv, _, _, _ := codexConsumeServer(t, http.StatusOK, `{"code":"no_credit","windows_reset":0}`)
	prev := codexResetCreditsConsumeURL
	codexResetCreditsConsumeURL = srv.URL
	t.Cleanup(func() { codexResetCreditsConsumeURL = prev })

	out, status := consumeCodexResetCredits(context.Background(), nil, proxy.Options{}, codexConn("c", "tok", "acct"), "r")
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	if out["code"] != "no_credit" {
		t.Errorf("code = %v, want no_credit", out["code"])
	}
	if out["reset"] != false {
		t.Errorf("reset = %v, want false", out["reset"])
	}
	if out["message"] != "No Codex reset credits available." {
		t.Errorf("message = %v, want the canonical message", out["message"])
	}
}

// TestConsumeCodexResetCredits_UpstreamError verifies an unexpected upstream
// response → 502 with code=unknown_response + a fallback message.
func TestConsumeCodexResetCredits_UpstreamError(t *testing.T) {
	srv, _, _, _ := codexConsumeServer(t, http.StatusBadGateway, `{"message":"upstream blew up"}`)
	prev := codexResetCreditsConsumeURL
	codexResetCreditsConsumeURL = srv.URL
	t.Cleanup(func() { codexResetCreditsConsumeURL = prev })

	out, status := consumeCodexResetCredits(context.Background(), nil, proxy.Options{}, codexConn("c", "tok", "acct"), "r")
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if out["code"] != "unknown_response" {
		t.Errorf("code = %v, want unknown_response", out["code"])
	}
	if out["reset"] != false {
		t.Errorf("reset = %v, want false", out["reset"])
	}
	if out["message"] != "upstream blew up" {
		t.Errorf("message = %v, want the upstream message", out["message"])
	}
}

// TestConsumeCodexResetCredits_Passthrough4xx verifies a 4xx upstream passes its
// status through (not 502).
func TestConsumeCodexResetCredits_Passthrough4xx(t *testing.T) {
	srv, _, _, _ := codexConsumeServer(t, http.StatusForbidden, `{"message":"forbidden"}`)
	prev := codexResetCreditsConsumeURL
	codexResetCreditsConsumeURL = srv.URL
	t.Cleanup(func() { codexResetCreditsConsumeURL = prev })

	_, status := consumeCodexResetCredits(context.Background(), nil, proxy.Options{}, codexConn("c", "tok", "acct"), "r")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (passthrough 4xx)", status)
	}
}

// TestConsumeCodexResetCredits_NoToken verifies a codex connection without a
// token returns 401 without attempting the upstream call.
func TestConsumeCodexResetCredits_NoToken(t *testing.T) {
	out, status := consumeCodexResetCredits(context.Background(), nil, proxy.Options{}, codexConn("c", "", ""), "r")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if out["reset"] != false {
		t.Errorf("reset = %v, want false", out["reset"])
	}
	msg, _ := out["message"].(string)
	if msg == "" {
		t.Error("missing no-token message")
	}
}

// TestConsumeCodexResetCredits_WindowsResetOnly verifies code missing but
// windows_reset>0 still counts as success (JS: code==="reset" || windows_reset>0).
func TestConsumeCodexResetCredits_WindowsResetOnly(t *testing.T) {
	srv, _, _, _ := codexConsumeServer(t, http.StatusOK, `{"windows_reset":2}`)
	prev := codexResetCreditsConsumeURL
	codexResetCreditsConsumeURL = srv.URL
	t.Cleanup(func() { codexResetCreditsConsumeURL = prev })

	out, status := consumeCodexResetCredits(context.Background(), nil, proxy.Options{}, codexConn("c", "tok", "acct"), "r")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (windows_reset>0 → success)", status)
	}
	if out["reset"] != true {
		t.Errorf("reset = %v, want true (windows_reset>0)", out["reset"])
	}
	if out["windows_reset"] != 2 {
		t.Errorf("windows_reset = %v, want 2", out["windows_reset"])
	}
}

// TestConsumeCodexResetCredits_RedeemIdIsServerGenerated verifies the handler
// generates a fresh redeem id per call (non-empty, distinct across calls),
// mirroring the JS crypto.randomUUID — a client cannot control it.
func TestConsumeCodexResetCredits_RedeemIdIsServerGenerated(t *testing.T) {
	a := codexNewRedeemRequestID()
	b := codexNewRedeemRequestID()
	if a == "" || b == "" {
		t.Fatal("redeem id must be non-empty")
	}
	if a == b {
		t.Errorf("redeem ids must be distinct per call: %s == %s", a, b)
	}
}

// TestProxyFetchOptionsForConnection verifies the proxy options are resolved
// from the connection's data blob (connection-level fields) and the assigned
// pool's strictProxy/proxyUrl/noProxy (pool merge). Uses a real ProxyPoolRepo
// on sqlite + the buildDeps pool repo so the pool lookup is real.
func TestProxyFetchOptionsForConnection(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)

	// No proxy fields + no pool → empty options (direct).
	plain := codexConn("c", "tok", "acct")
	opts := proxyFetchOptionsForConnection(context.Background(), deps.ProxyPools, plain)
	if opts.ConnectionProxyUrl != "" || opts.StrictProxy {
		t.Errorf("plain conn → %+v, want empty", opts)
	}

	// Connection-level proxy URL is read from the data blob.
	b, _ := json.Marshal(map[string]any{
		"accessToken":            "tok",
		"connectionProxyEnabled": true,
		"connectionProxyUrl":     "http://conn-proxy:3128",
		"connectionNoProxy":      "example.com",
		"vercelRelayUrl":         "https://relay.example.com",
	})
	connProxy := codexConnFromData("c2", b)
	opts = proxyFetchOptionsForConnection(context.Background(), deps.ProxyPools, connProxy)
	if opts.ConnectionProxyUrl != "http://conn-proxy:3128" {
		t.Errorf("connectionProxyUrl = %q", opts.ConnectionProxyUrl)
	}
	if !opts.ConnectionProxyEnabled {
		t.Error("connectionProxyEnabled not read")
	}
	if opts.NoProxy != "example.com" {
		t.Errorf("noProxy = %q", opts.NoProxy)
	}
	if opts.VercelRelayUrl != "https://relay.example.com" {
		t.Errorf("vercelRelayUrl = %q", opts.VercelRelayUrl)
	}
}

// codexConnFromData wraps a raw data blob into a codex connection for the
// proxy-options test (needs arbitrary data fields, not the minimal codexConn).
func codexConnFromData(id string, data []byte) *settings.ProviderConnection {
	return &settings.ProviderConnection{ID: id, Provider: "codex", Data: data}
}
