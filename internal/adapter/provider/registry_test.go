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
