package embedding

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

func creds(apiKey, accessToken string) domainProv.Credentials {
	return domainProv.Credentials{APIKey: apiKey, AccessToken: accessToken}
}

// --- Lookup ---

func TestLookup_OpenAICompat(t *testing.T) {
	a, ok := Lookup("openai")
	if !ok {
		t.Fatal("openai should resolve")
	}
	if got := a.BuildURL("text-embedding-3-small", domainProv.Credentials{}, Params{}); got != "https://api.openai.com/v1/embeddings" {
		t.Errorf("openai url = %q", got)
	}
}

func TestLookup_Gemini(t *testing.T) {
	a, ok := Lookup("gemini")
	if !ok {
		t.Fatal("gemini should resolve")
	}
	u := a.BuildURL("text-embedding-004", creds("K", ""), Params{Input: "hi"})
	if !strings.HasPrefix(u, "https://generativelanguage.googleapis.com/v1beta/models/text-embedding-004:embedContent?key=K") {
		t.Errorf("gemini single url = %q", u)
	}
	uBatch := a.BuildURL("text-embedding-004", creds("K", ""), Params{Input: []any{"a", "b"}})
	if !strings.Contains(uBatch, ":batchEmbedContents") {
		t.Errorf("gemini batch url = %q", uBatch)
	}
}

func TestLookup_NodeProvider(t *testing.T) {
	a, ok := Lookup("custom-embedding-foo")
	if !ok {
		t.Fatal("custom-embedding node should resolve")
	}
	c := domainProv.Credentials{ProviderSpecificData: map[string]any{"baseUrl": "https://embed.example.com/v1"}}
	if got := a.BuildURL("m", c, Params{}); got != "https://embed.example.com/v1/embeddings" {
		t.Errorf("node url = %q", got)
	}
	// baseUrl with trailing slash + /embeddings suffix must not double.
	c2 := domainProv.Credentials{ProviderSpecificData: map[string]any{"baseUrl": "https://embed.example.com/v1/embeddings/"}}
	if got := a.BuildURL("m", c2, Params{}); got != "https://embed.example.com/v1/embeddings" {
		t.Errorf("node url (suffixed) = %q", got)
	}
}

func TestLookup_DeriveFromChat(t *testing.T) {
	// cerebras has no explicit embeddings URL but a /v1/chat/completions chat URL.
	a, ok := Lookup("cerebras")
	if !ok {
		t.Fatal("cerebras should derive an embeddings adapter")
	}
	if got := a.BuildURL("m", domainProv.Credentials{}, Params{}); got != "https://api.cerebras.ai/v1/embeddings" {
		t.Errorf("cerebras derived url = %q", got)
	}
}

func TestLookup_Unknown(t *testing.T) {
	if _, ok := Lookup("does-not-exist"); ok {
		t.Fatal("unknown provider should not resolve")
	}
}

// --- openAIAdapter body/headers ---

func TestOpenAIBuildBody(t *testing.T) {
	a := openAIAdapter{providerID: "openai"}
	body, err := a.BuildBody("text-embedding-3-small", Params{Input: "hello", EncodingFormat: "float", Dimensions: 256})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if m["model"] != "text-embedding-3-small" {
		t.Errorf("model = %v", m["model"])
	}
	if m["input"] != "hello" {
		t.Errorf("input = %v", m["input"])
	}
	if m["encoding_format"] != "float" {
		t.Errorf("encoding_format = %v", m["encoding_format"])
	}
	if m["dimensions"] != float64(256) {
		t.Errorf("dimensions = %v (want 256)", m["dimensions"])
	}
}

func TestOpenAIBuildBody_OmitsInvalidDimensions(t *testing.T) {
	a := openAIAdapter{providerID: "openai"}
	body, _ := a.BuildBody("m", Params{Input: "x", Dimensions: "not-a-number"})
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if _, ok := m["dimensions"]; ok {
		t.Errorf("dimensions should be omitted for invalid value: %v", m["dimensions"])
	}
}

func TestOpenAIBuildHeaders_Bearer(t *testing.T) {
	a := openAIAdapter{providerID: "openai"}
	h := a.BuildHeaders(creds("sk-abc", ""), Params{})
	if h.Get("Authorization") != "Bearer sk-abc" {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
	if h.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", h.Get("Content-Type"))
	}
	// Falls back to accessToken when apiKey absent.
	h2 := a.BuildHeaders(creds("", "tok-123"), Params{})
	if h2.Get("Authorization") != "Bearer tok-123" {
		t.Errorf("Authorization (token) = %q", h2.Get("Authorization"))
	}
}

func TestOpenAIBuildBody_ArrayInput(t *testing.T) {
	a := openAIAdapter{providerID: "openai"}
	body, _ := a.BuildBody("m", Params{Input: []any{"a", "b"}})
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	arr, ok := m["input"].([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("input array = %v", m["input"])
	}
}

// --- geminiAdapter body/normalize ---

func TestGeminiBuildBody_Single(t *testing.T) {
	a := geminiAdapter{}
	body, err := a.BuildBody("text-embedding-004", Params{Input: "hello", Dimensions: 128})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if m["model"] != "models/text-embedding-004" {
		t.Errorf("model = %v", m["model"])
	}
	if m["outputDimensionality"] != float64(128) {
		t.Errorf("outputDimensionality = %v", m["outputDimensionality"])
	}
	content, _ := m["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 {
		t.Errorf("parts = %v", parts)
	}
}

func TestGeminiBuildBody_Batch(t *testing.T) {
	a := geminiAdapter{}
	body, _ := a.BuildBody("text-embedding-004", Params{Input: []any{"a", "b"}})
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	reqs, ok := m["requests"].([]any)
	if !ok || len(reqs) != 2 {
		t.Errorf("requests = %v", m["requests"])
	}
}

func TestGeminiNormalize_EmbedContent(t *testing.T) {
	a := geminiAdapter{}
	upstream := `{"embedding":{"values":[0.1,0.2,0.3]}}`
	out, err := a.Normalize([]byte(upstream), "models/text-embedding-004")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["object"] != "list" {
		t.Errorf("object = %v", m["object"])
	}
	data, _ := m["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data = %v", data)
	}
	first, _ := data[0].(map[string]any)
	if first["object"] != "embedding" {
		t.Errorf("data[0].object = %v", first["object"])
	}
	if first["index"] != float64(0) {
		t.Errorf("data[0].index = %v", first["index"])
	}
	emb, _ := first["embedding"].([]any)
	if len(emb) != 3 {
		t.Errorf("embedding = %v", first["embedding"])
	}
	if m["model"] != "models/text-embedding-004" {
		t.Errorf("model = %v", m["model"])
	}
}

func TestGeminiNormalize_BatchEmbedContents(t *testing.T) {
	a := geminiAdapter{}
	upstream := `{"embeddings":[{"values":[0.1]},{"values":[0.2,0.3]}]}`
	out, _ := a.Normalize([]byte(upstream), "models/m")
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	data, _ := m["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data = %v", data)
	}
	second, _ := data[1].(map[string]any)
	if second["index"] != float64(1) {
		t.Errorf("data[1].index = %v", second["index"])
	}
}

func TestGeminiNormalize_PassthroughOpenAIShape(t *testing.T) {
	a := geminiAdapter{}
	original := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,2]}],"model":"m","usage":{"prompt_tokens":3,"total_tokens":3}}`
	out, _ := a.Normalize([]byte(original), "m")
	if string(out) != original {
		t.Errorf("passthrough should be byte-identical; got %s", out)
	}
}

// --- normalizeDimensions ---

func TestNormalizeDimensions(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{256, 256},
		{float64(128), 128},
		{int64(64), 64},
		{"512", 512},
		{"", 0},
		{"nope", 0},
		{-1, 0}, // negative -> 0 (omitted)
		{nil, 0},
	}
	for _, c := range cases {
		if got := normalizeDimensions(c.in); got != c.want {
			t.Errorf("normalizeDimensions(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- header auth fallback sanity for node adapter ---

func TestNodeAdapterHeaders(t *testing.T) {
	h := nodeOpenAIAdapter{}.BuildHeaders(creds("sk-x", ""), Params{})
	if h.Get("Authorization") != "Bearer sk-x" {
		t.Errorf("node Authorization = %q", h.Get("Authorization"))
	}
}

// Compile-time: ensure adapters satisfy the interface.
var _ Adapter = openAIAdapter{}
var _ Adapter = nodeOpenAIAdapter{}
var _ Adapter = geminiAdapter{}
var _ = http.Header{}