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

// TestGrokCliCatalog pins the upstream 59b78282 static model catalog: grok-build
// (the Grok Build subscription model) + the grok-4.5 family with effort-suffix
// aliases that remap to the bare grok-4.5 upstream id. grok-cli also has a live
// resolver, so this static set is the fallback when the live fetch is down +
// the source of the effort variants the live endpoint does not enumerate.
func TestGrokCliCatalog(t *testing.T) {
	cat, ok := Catalog("grok-cli")
	if !ok {
		t.Fatalf("Catalog(grok-cli) returned false; want a static catalog")
	}
	if cat.Alias != "gcli" {
		t.Errorf("grok-cli alias = %q, want gcli", cat.Alias)
	}
	want := map[string]string{
		"grok-build":      "",
		"grok-4.5":        "",
		"grok-4.5-high":   "grok-4.5",
		"grok-4.5-medium": "grok-4.5",
		"grok-4.5-low":    "grok-4.5",
	}
	got := map[string]string{}
	for _, m := range cat.Models {
		got[m.ID] = m.UpstreamModelID
	}
	if len(cat.Models) != len(want) {
		t.Errorf("grok-cli catalog has %d models, want %d", len(cat.Models), len(want))
	}
	for id, upstream := range want {
		if g, ok := got[id]; !ok {
			t.Errorf("grok-cli catalog missing model %q", id)
		} else if g != upstream {
			t.Errorf("grok-cli model %q upstreamModelId = %q, want %q", id, g, upstream)
		}
	}
	// gcli + gb aliases must resolve to the grok-cli catalog.
	for _, alias := range []string{"gcli", "gb"} {
		ac, ok := Catalog(alias)
		if !ok {
			t.Errorf("Catalog(%q) returned false; want grok-cli catalog via alias", alias)
			continue
		}
		if len(ac.Models) != len(want) {
			t.Errorf("Catalog(%q) has %d models, want %d (grok-cli catalog)", alias, len(ac.Models), len(want))
		}
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

// TestKimiCatalog pins the merged dual-auth kimi provider's static model list
// (upstream 68566f53). kimi has no live resolver, so /v1/models and dashboard
// validation rely on this catalog; without it the provider advertised no
// models after the kimi-coding merge.
func TestKimiCatalog(t *testing.T) {
	if _, err := Lookup("kimi"); err != nil {
		t.Fatalf("lookup kimi: %v", err)
	}
	cat, ok := Catalog("kimi")
	if !ok {
		t.Fatalf("Catalog(kimi) returned false; want a static catalog")
	}
	if cat.Alias != "kimi" {
		t.Errorf("kimi alias = %q, want kimi", cat.Alias)
	}
	wantModels := map[string]string{
		"kimi-k3":                   "Kimi K3",
		"k3":                        "Kimi K3 (Code)",
		"kimi-for-coding":           "Kimi for Coding",
		"kimi-for-coding-highspeed": "Kimi for Coding Highspeed",
		"kimi-k2.7-code":            "Kimi K2.7 Code",
		"kimi-k2.7-code-highspeed":  "Kimi K2.7 Code Highspeed",
		"kimi-k2.6":                 "Kimi K2.6",
		"kimi-k2.5":                 "Kimi K2.5",
		"kimi-k2.5-thinking":        "Kimi K2.5 Thinking",
		"kimi-latest":               "Kimi Latest",
	}
	if len(cat.Models) != len(wantModels) {
		t.Fatalf("kimi catalog has %d models, want %d", len(cat.Models), len(wantModels))
	}
	for _, m := range cat.Models {
		want, ok := wantModels[m.ID]
		if !ok {
			t.Errorf("kimi catalog has unexpected model %q", m.ID)
			continue
		}
		if m.Name != want {
			t.Errorf("kimi model %q name = %q, want %q", m.ID, m.Name, want)
		}
	}
	// kimi-coding / kmc aliases must still resolve to the merged kimi catalog.
	for _, alias := range []string{"kimi-coding", "kmc"} {
		ac, ok := Catalog(alias)
		if !ok {
			t.Errorf("Catalog(%q) returned false; want the kimi catalog via alias", alias)
			continue
		}
		if len(ac.Models) != len(wantModels) {
			t.Errorf("Catalog(%q) has %d models, want %d (kimi catalog)", alias, len(ac.Models), len(wantModels))
		}
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

// TestBlackboxCatalog ports the 940a35e0 blackbox catalog overhaul: the
// registry exposes 10 latest models with an upstreamModelId prefix (the id
// the dashboard shows vs the raw upstream id the request is remapped to).
func TestBlackboxCatalog(t *testing.T) {
	cat, ok := Catalog("blackbox")
	if !ok {
		t.Fatalf("Catalog(blackbox) returned false; want a static catalog")
	}
	want := map[string]string{
		"claude-fable-5":    "blackboxai/anthropic/claude-fable-5",
		"claude-opus-4.8":   "blackboxai/anthropic/claude-opus-4.8",
		"claude-sonnet-4.6": "blackboxai/anthropic/claude-sonnet-4.6",
		"gpt-5.5":           "blackboxai/openai/gpt-5.5",
		"gpt-5.4-pro":       "blackboxai/openai/gpt-5.4-pro",
		"gpt-5.4":           "blackboxai/openai/gpt-5.4",
		"gpt-5.3-codex":     "blackboxai/openai/gpt-5.3-codex",
		"gpt-5.4-nano":      "blackboxai/openai/gpt-5.4-nano",
		"deepseek-v4-flash": "blackboxai/deepseek/deepseek-v4-flash",
		"grok-4.3":          "blackboxai/x-ai/grok-4.3",
	}
	got := map[string]string{}
	for _, m := range cat.Models {
		got[m.ID] = m.UpstreamModelID
	}
	if len(cat.Models) != len(want) {
		t.Errorf("blackbox catalog has %d models, want %d", len(cat.Models), len(want))
	}
	for id, upstream := range want {
		if g, ok := got[id]; !ok {
			t.Errorf("blackbox catalog missing model %q", id)
		} else if g != upstream {
			t.Errorf("blackbox model %q upstreamModelId = %q, want %q", id, g, upstream)
		}
	}
}

// TestKilocodeCatalog ports the 713c5637 kilocode catalog: 8 hardcoded
// upstream OpenRouter models as a fallback (the live 334-model catalog requires
// a resolver not yet ported).
func TestKilocodeCatalog(t *testing.T) {
	cat, ok := Catalog("kilocode")
	if !ok {
		t.Fatalf("Catalog(kilocode) returned false; want a static catalog")
	}
	wantIDs := map[string]bool{
		"anthropic/claude-sonnet-4-20250514": true,
		"anthropic/claude-opus-4-20250514":   true,
		"google/gemini-2.5-pro":              true,
		"google/gemini-2.5-flash":            true,
		"openai/gpt-4.1":                     true,
		"openai/o3":                          true,
		"deepseek/deepseek-chat":             true,
		"deepseek/deepseek-reasoner":         true,
	}
	if len(cat.Models) != len(wantIDs) {
		t.Errorf("kilocode catalog has %d models, want %d", len(cat.Models), len(wantIDs))
	}
	for _, m := range cat.Models {
		if !wantIDs[m.ID] {
			t.Errorf("kilocode catalog unexpected model %q", m.ID)
		}
	}
}

// TestUpstreamModelID ports the getModelUpstreamId resolution
// (providerModels.js): a catalog entry's upstreamModelId is returned when the
// model id matches, with a "(level)" suffix re-appended; providers without a
// catalog or models without an upstreamModelId return the input unchanged.
func TestUpstreamModelID(t *testing.T) {
	cases := []struct {
		provider, model, want string
	}{
		// blackbox remaps to the prefixed upstream id.
		{"blackbox", "claude-opus-4.8", "blackboxai/anthropic/claude-opus-4.8"},
		{"blackbox", "gpt-5.4", "blackboxai/openai/gpt-5.4"},
		// Suffix is stripped for lookup then re-appended.
		{"blackbox", "claude-opus-4.8(high)", "blackboxai/anthropic/claude-opus-4.8(high)"},
		{"blackbox", "gpt-5.4(xhigh)", "blackboxai/openai/gpt-5.4(xhigh)"},
		// nvidia models have no upstreamModelId → unchanged (id + suffix).
		{"nvidia", "z-ai/glm-5.2", "z-ai/glm-5.2"},
		{"nvidia", "z-ai/glm-5.2(high)", "z-ai/glm-5.2(high)"},
		// Model not in catalog → unchanged.
		{"blackbox", "no-such-model", "no-such-model"},
		// Provider with no catalog → unchanged.
		{"openai", "gpt-5", "gpt-5"},
		// Empty model → empty.
		{"blackbox", "", ""},
	}
	for _, c := range cases {
		got := UpstreamModelID(c.provider, c.model)
		if got != c.want {
			t.Errorf("UpstreamModelID(%q, %q) = %q, want %q", c.provider, c.model, got, c.want)
		}
	}
}
