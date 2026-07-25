package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// mediaproviders_voices_test.go drives each voice-list fetcher against a real
// httptest.Server (no mocks): the fetcher issues its real upstream request, the
// server asserts the provider-specific auth header + request shape, and the
// fetcher parses the canned body into the {languages, byLang} shape the
// dashboard TtsExampleCard consumes. URL vars are swapped to the server's URL
// for the duration of each test, mirroring the codex reset-credits tests.

// voiceConn returns a connection carrying data.apiKey for the named provider.
func voiceConn(provider, apiKey string) *settings.ProviderConnection {
	b, _ := json.Marshal(map[string]any{"apiKey": apiKey})
	return &settings.ProviderConnection{ID: "vc1", Provider: provider, Data: b}
}

func voicesOf(t *testing.T, grp *voiceGroups, code string) []map[string]any {
	t.Helper()
	if grp == nil {
		t.Fatal("nil voiceGroups")
	}
	g, ok := grp.byLang[code]
	if !ok {
		t.Fatalf("no group for lang %q in %#v", code, grp.byLang)
	}
	raw, _ := g["voices"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		if vm, ok := v.(map[string]any); ok {
			out = append(out, vm)
		}
	}
	return out
}

func TestElevenLabsVoices_GroupsByLanguage(t *testing.T) {
	body := `{"voices":[
		{"voice_id":"v1","name":"Rachel","category":"premade","labels":{"language":"en","gender":"Female"},"verified_languages":[{"language":"es"},{"language":"en"}]},
		{"voice_id":"v2","name":"Antoni","category":"professional","is_owner":true,"labels":{"language":"fr","gender":"Male"}}
	]}`
	var gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("xi-api-key")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	prev := elevenLabsVoicesURL
	elevenLabsVoicesURL = srv.URL
	defer func() { elevenLabsVoicesURL = prev }()

	grp, err := fetchElevenLabsVoices(context.Background(), nil, proxy.Options{}, voiceConn("elevenlabs", "xi-key"), "xi-key")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotAuth != "xi-key" {
		t.Errorf("xi-api-key = %q want xi-key", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q want application/json", gotAccept)
	}
	// v1 primary "en" + verified "es" (en deduped). v2 primary "fr".
	en := voicesOf(t, grp, "en")
	if len(en) != 1 || en[0]["id"] != "v1" {
		t.Fatalf("en voices = %#v", en)
	}
	if en[0]["free_users_allowed"] != true { // category == premade
		t.Errorf("v1 free_users_allowed = %v want true", en[0]["free_users_allowed"])
	}
	if en[0]["gender"] != "Female" {
		t.Errorf("v1 gender = %v want Female", en[0]["gender"])
	}
	es := voicesOf(t, grp, "es")
	if len(es) != 1 || es[0]["id"] != "v1" {
		t.Fatalf("es voices = %#v (v1 verified lang)", es)
	}
	fr := voicesOf(t, grp, "fr")
	if len(fr) != 1 || fr[0]["id"] != "v2" {
		t.Fatalf("fr voices = %#v", fr)
	}
	if fr[0]["free_users_allowed"] != true { // is_owner true
		t.Errorf("v2 free_users_allowed = %v want true", fr[0]["free_users_allowed"])
	}
	// languages slice sorted by display name; "English" < "French" < "Spanish".
	langs := grp.languages()
	if len(langs) != 3 || langs[0]["code"] != "en" || langs[1]["code"] != "fr" || langs[2]["code"] != "es" {
		t.Fatalf("languages order = %#v", langs)
	}
	if langs[0]["name"] != "English" {
		t.Errorf("en name = %q want English", langs[0]["name"])
	}
}

func TestDeepgramVoices_GroupsByLanguage(t *testing.T) {
	body := `{"tts":[
		{"canonical_name":"aura-2-thalia-en","name":"Aura 2 Thalia","languages":["en"],"metadata":{"tags":["feminine"]}},
		{"canonical_name":"aura-2-hermes-es","name":"Aura 2 Hermes","metadata":{"tags":["masculine"]}}
	]}`
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	prev := deepgramModelsURL
	deepgramModelsURL = srv.URL
	defer func() { deepgramModelsURL = prev }()

	grp, err := fetchDeepgramVoices(context.Background(), nil, proxy.Options{}, voiceConn("deepgram", "dg-token"), "dg-token")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotAuth != "Token dg-token" {
		t.Errorf("Authorization = %q want Token dg-token", gotAuth)
	}
	en := voicesOf(t, grp, "en")
	if len(en) != 1 || en[0]["id"] != "aura-2-thalia-en" {
		t.Fatalf("en voices = %#v", en)
	}
	if en[0]["gender"] != "feminine" {
		t.Errorf("thalia gender = %v want feminine", en[0]["gender"])
	}
	// no languages array → inferred from canonical_name suffix "es".
	es := voicesOf(t, grp, "es")
	if len(es) != 1 || es[0]["id"] != "aura-2-hermes-es" {
		t.Fatalf("es voices = %#v", es)
	}
}

func TestInworldVoices_GroupsByLanguage(t *testing.T) {
	body := `{"voices":[
		{"voiceId":"ind1","displayName":"Indi","gender":"Female","languages":["en","es"]},
		{"voiceId":"ind2","displayName":"Mark","gender":"Male"}
	]}`
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	prev := inworldVoicesURL
	inworldVoicesURL = srv.URL
	defer func() { inworldVoicesURL = prev }()

	grp, err := fetchInworldVoices(context.Background(), nil, proxy.Options{}, voiceConn("inworld", "iw-basic"), "iw-basic")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotAuth != "Basic iw-basic" {
		t.Errorf("Authorization = %q want Basic iw-basic", gotAuth)
	}
	en := voicesOf(t, grp, "en")
	if len(en) != 2 {
		t.Fatalf("en voices = %#v", en)
	}
	es := voicesOf(t, grp, "es")
	if len(es) != 1 || es[0]["id"] != "ind1" {
		t.Fatalf("es voices = %#v", es)
	}
	// no languages → defaults to "en".
	voicesEn := map[string]bool{}
	for _, v := range en {
		voicesEn[v["id"].(string)] = true
	}
	if !voicesEn["ind2"] {
		t.Errorf("ind2 missing from en group: %#v", en)
	}
}

func TestMiniMaxVoices_GroupsByInferredLang(t *testing.T) {
	body := `{"base_resp":{"status_code":0},
		"system_voice":[{"voice_id":"English_expressive_narrator","voice_name":"Expressive Narrator"}],
		"voice_generation":[{"voice_id":"gen_42","voice_name":"My Gen Voice"}]}`
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	prev := minimaxVoiceEndpoints["minimax"]
	minimaxVoiceEndpoints["minimax"] = srv.URL
	defer func() { minimaxVoiceEndpoints["minimax"] = prev }()

	grp, err := fetchMiniMaxVoices(context.Background(), nil, proxy.Options{}, voiceConn("minimax", "mm-key"), "minimax", "mm-key", "all")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotAuth != "Bearer mm-key" {
		t.Errorf("Authorization = %q want Bearer mm-key", gotAuth)
	}
	var reqBody map[string]any
	_ = json.Unmarshal([]byte(gotBody), &reqBody)
	if reqBody["voice_type"] != "all" {
		t.Errorf("body voice_type = %v want all", reqBody["voice_type"])
	}
	// system_voice "English_..." → inferred lang "English"; voice_generation → "Custom".
	en := voicesOf(t, grp, "English")
	if len(en) != 1 || en[0]["id"] != "English_expressive_narrator" {
		t.Fatalf("English voices = %#v", en)
	}
	if en[0]["category"] != "system_voice" {
		t.Errorf("category = %v want system_voice", en[0]["category"])
	}
	custom := voicesOf(t, grp, "Custom")
	if len(custom) != 1 || custom[0]["id"] != "gen_42" {
		t.Fatalf("Custom voices = %#v", custom)
	}
	if custom[0]["name"] != "My Gen Voice · Generated" {
		t.Errorf("gen voice name = %q want 'My Gen Voice · Generated'", custom[0]["name"])
	}
}

func TestMiniMaxVoices_CnEndpointAndError(t *testing.T) {
	// Upstream base_resp.status_code != 0 → error with status_msg.
	body := `{"base_resp":{"status_code":1001,"status_msg":"invalid key"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	prev := minimaxVoiceEndpoints["minimax-cn"]
	minimaxVoiceEndpoints["minimax-cn"] = srv.URL
	defer func() { minimaxVoiceEndpoints["minimax-cn"] = prev }()

	_, err := fetchMiniMaxVoices(context.Background(), nil, proxy.Options{}, voiceConn("minimax-cn", "mm"), "minimax-cn", "mm", "all")
	if err == nil {
		t.Fatal("expected upstream error, got nil")
	}
	if err.Error() != "invalid key" {
		t.Errorf("err = %q want 'invalid key'", err.Error())
	}
}

func TestElevenLabsVoices_NoConnection(t *testing.T) {
	// Empty apiKey → no connection eligible (caller enforces; fetcher still
	// gets a 4xx which surfaces as a fetch error).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()
	prev := elevenLabsVoicesURL
	elevenLabsVoicesURL = srv.URL
	defer func() { elevenLabsVoicesURL = prev }()

	_, err := fetchElevenLabsVoices(context.Background(), nil, proxy.Options{}, voiceConn("elevenlabs", "bad"), "bad")
	if err == nil {
		t.Fatal("expected fetch error on 401, got nil")
	}
}

// TestMediaVoices_RoutesAuthAndNoConnection drives the mounted HTTP routes:
// unauthenticated → 401; authenticated but no elevenlabs connection → 400 with
// the JS-parity "No ... connection found" error; with a real connection + a
// swapped-upstream httptest.Server → 200 with {languages, byLang}.
func TestMediaVoices_RoutesAuthAndNoConnection(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterMediaProviders(mux, deps)
	authMw := authMiddleware(deps)(mux)

	// 401 without session.
	req := httptest.NewRequest("GET", "/api/media-providers/tts/elevenlabs/voices", nil)
	rec := httptest.NewRecorder()
	authMw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d want 401", rec.Code)
	}

	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// No connection yet → 400 "No elevenlabs connection found".
	req = httptest.NewRequest("GET", "/api/media-providers/tts/elevenlabs/voices", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	authMw.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-conn status = %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	var errBody map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &errBody) != nil || errBody["error"] != "No elevenlabs connection found" {
		t.Fatalf("error body = %s want 'No elevenlabs connection found'", rec.Body.String())
	}

	// Create an active elevenlabs connection via the providers route.
	pmux := http.NewServeMux()
	RegisterProviders(pmux, deps)
	createBody := `{"provider":"elevenlabs","apiKey":"xi-key","name":"EL","priority":1}`
	preq := httptest.NewRequest("POST", "/api/providers", strings.NewReader(createBody))
	preq.Header.Set("Cookie", "auth_token="+ck)
	preq.Header.Set("Content-Type", "application/json")
	prec := httptest.NewRecorder()
	authMiddleware(deps)(pmux).ServeHTTP(prec, preq)
	if prec.Code != http.StatusCreated {
		t.Fatalf("create connection = %d body=%s", prec.Code, prec.Body.String())
	}

	// Swap upstream to a real httptest.Server and hit the route.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("xi-api-key") != "xi-key" {
			t.Errorf("xi-api-key = %q want xi-key", r.Header.Get("xi-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"voices":[{"voice_id":"v1","name":"Rachel","category":"premade","labels":{"language":"en","gender":"Female"}}]}`)
	}))
	defer srv.Close()
	prev := elevenLabsVoicesURL
	elevenLabsVoicesURL = srv.URL
	defer func() { elevenLabsVoicesURL = prev }()

	req = httptest.NewRequest("GET", "/api/media-providers/tts/elevenlabs/voices", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	authMw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	byLang, _ := resp["byLang"].(map[string]any)
	en, _ := byLang["en"].(map[string]any)
	if en == nil {
		t.Fatalf("no en group in byLang: %#v", byLang)
	}
	voices, _ := en["voices"].([]any)
	if len(voices) != 1 {
		t.Fatalf("en voices = %d want 1", len(voices))
	}

	// lang filter → {voices:[...]} only.
	req = httptest.NewRequest("GET", "/api/media-providers/tts/elevenlabs/voices?lang=en", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	authMw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lang-filter status = %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var filtered map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("unmarshal filtered: %v body=%s", err, rec.Body.String())
	}
	fvoices, _ := filtered["voices"].([]any)
	if len(fvoices) != 1 {
		t.Fatalf("filtered voices = %d want 1", len(fvoices))
	}
	if _, hasByLang := filtered["byLang"]; hasByLang {
		t.Errorf("lang-filter response should not include byLang")
	}

	// minimax-cn ?provider=minimax-cn routing → still no connection → 400.
	req = httptest.NewRequest("GET", "/api/media-providers/tts/minimax/voices?provider=minimax-cn", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	authMw.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("minimax-cn no-conn status = %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}
