package provider

import (
	"testing"
)

func TestLookupGrokCli(t *testing.T) {
	p, err := Lookup("grok-cli")
	if err != nil {
		t.Fatalf("lookup grok-cli: %v", err)
	}
	if p.ID() != "grok-cli" {
		t.Fatalf("unexpected id %s", p.ID())
	}
}

// TestAlicodeIntlSplit pins the upstream v0.5.40 split (commit 55628eea):
// alicode-intl reverts to the coding-intl host (Coding Plan keys sk-sp-...),
// and alims-intl is a new sibling provider on the DashScope compatible-mode
// host (standard keys sk-...). The two key types use two different hosts and
// are not interchangeable.
func TestAlicodeIntlSplit(t *testing.T) {
	// alicode-intl → coding-intl host (Coding Plan keys).
	if got := ChatBaseURL("alicode-intl"); got != "https://coding-intl.dashscope.aliyuncs.com/v1/chat/completions" {
		t.Fatalf("alicode-intl BaseURL = %q, want coding-intl host", got)
	}
	if _, err := Lookup("alicode-intl"); err != nil {
		t.Fatalf("lookup alicode-intl: %v", err)
	}

	// alims-intl → DashScope compatible-mode host (standard keys), with a
	// static model catalog.
	if got := ChatBaseURL("alims-intl"); got != "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions" {
		t.Fatalf("alims-intl BaseURL = %q, want dashscope-intl compatible-mode host", got)
	}
	if _, err := Lookup("alims-intl"); err != nil {
		t.Fatalf("lookup alims-intl: %v", err)
	}
	cat, ok := Catalog("alims-intl")
	if !ok {
		t.Fatalf("Catalog(alims-intl) returned false; want a static catalog")
	}
	if cat.Alias != "alims-intl" {
		t.Errorf("alims-intl alias = %q, want alims-intl", cat.Alias)
	}
	if len(cat.Models) == 0 {
		t.Fatalf("alims-intl catalog has no models; want the upstream static list")
	}
	wantFirst := "qwen3.5-plus"
	if cat.Models[0].ID != wantFirst {
		t.Errorf("alims-intl first model = %q, want %q", cat.Models[0].ID, wantFirst)
	}

	// The two providers must resolve to different hosts (the whole point of
	// the split).
	a := ChatBaseURL("alicode-intl")
	m := ChatBaseURL("alims-intl")
	if a == m {
		t.Fatalf("alicode-intl and alims-intl share the same BaseURL %q; the split is broken", a)
	}
}

// TestNvidiaCatalog ports the ced51ed6 nvidia catalog + capabilities: the
// registry exposes the full NIM model list (LLM + embedding + stt + tts kinds)
// and capabilities enforce OpenAI-compatible reasoning for the LLM models.
func TestNvidiaCatalog(t *testing.T) {
	cat, ok := Catalog("nvidia")
	if !ok {
		t.Fatalf("Catalog(nvidia) returned false; want a static catalog")
	}
	wantModels := map[string]string{
		"minimaxai/minimax-m2.7":            "MiniMax M2.7",
		"minimaxai/minimax-m3":              "MiniMax M3",
		"z-ai/glm-5.2":                      "GLM 5.2",
		"deepseek-ai/deepseek-v4-pro":       "DeepSeek V4 Pro",
		"deepseek-ai/deepseek-v4-flash":     "DeepSeek V4 Flash",
		"moonshotai/kimi-k2.6":              "Kimi K2.6",
		"nvidia/nemotron-3-ultra-550b-a55b": "Nemotron 3 Ultra",
		"nvidia/nv-embedqa-e5-v5":           "NV EmbedQA E5 v5",
		"nvidia/parakeet-ctc-1.1b-asr":      "Parakeet CTC 1.1B",
		"fastpitch":                         "FastPitch",
		"tacotron2":                         "Tacotron2",
	}
	gotModels := map[string]string{}
	for _, m := range cat.Models {
		gotModels[m.ID] = m.Name
	}
	for id, name := range wantModels {
		if got, ok := gotModels[id]; !ok {
			t.Errorf("nvidia catalog missing model %q", id)
		} else if got != name {
			t.Errorf("nvidia model %q name = %q, want %q", id, got, name)
		}
	}
	// Service kinds: NIM serves llm + tts + embedding (stt is a model kind, not
	// a provider service kind in the JS registry which lists ["llm","tts","embedding"]).
	wantKinds := map[string]bool{"llm": true, "tts": true, "embedding": true}
	for _, k := range cat.ServiceKinds {
		delete(wantKinds, k)
	}
	if len(wantKinds) > 0 {
		t.Errorf("nvidia serviceKinds missing %v; got %v", wantKinds, cat.ServiceKinds)
	}
	// Non-LLM kinds on the model entries.
	wantKind := map[string]string{
		"nvidia/nv-embedqa-e5-v5":      "embedding",
		"nvidia/parakeet-ctc-1.1b-asr": "stt",
		"fastpitch":                    "tts",
		"tacotron2":                    "tts",
	}
	for _, m := range cat.Models {
		if k, ok := wantKind[m.ID]; ok && m.Kind != k {
			t.Errorf("nvidia model %q kind = %q, want %q", m.ID, m.Kind, k)
		}
	}
}
