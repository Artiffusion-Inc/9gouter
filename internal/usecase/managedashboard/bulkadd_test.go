package managedashboard

import (
	"reflect"
	"testing"
)

// bulkadd_test.go ports the regression coverage for the collision-aware name
// planner half of decolua/9router #2552 (de680e78): PlanBulkAdd gap-fills the
// smallest free "<base> <n>" against existing + same-batch names so a generated
// name is never reused and the backend always inserts.

func names(entries []BulkAddEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func TestPlanBulkAdd_ApiKeyOnlyAutoNamed(t *testing.T) {
	entries := PlanBulkAdd([]string{"sk-1", "sk-2"}, nil, BulkAddOptions{})
	if got := names(entries); !reflect.DeepEqual(got, []string{"Key 1", "Key 2"}) {
		t.Errorf("names = %v, want [Key 1 Key 2]", got)
	}
	if entries[0].APIKey != "sk-1" || entries[1].APIKey != "sk-2" {
		t.Errorf("apiKeys = %v", entries)
	}
}

func TestPlanBulkAdd_NameApiKeyFormat(t *testing.T) {
	entries := PlanBulkAdd([]string{"Primary|sk-aaa", "Backup|sk-bbb"}, nil, BulkAddOptions{})
	if got := names(entries); !reflect.DeepEqual(got, []string{"Primary 1", "Backup 1"}) {
		t.Errorf("names = %v, want [Primary 1 Backup 1]", got)
	}
	if entries[0].APIKey != "sk-aaa" || entries[1].APIKey != "sk-bbb" {
		t.Errorf("apiKeys = %v", entries)
	}
}

func TestPlanBulkAdd_ApiKeyWithPipes(t *testing.T) {
	// apiKey may itself contain pipes: "name|a|b|c" → name, apiKey="a|b|c".
	entries := PlanBulkAdd([]string{"My Key|sk|with|pipes"}, nil, BulkAddOptions{})
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if entries[0].Name != "My Key 1" {
		t.Errorf("name = %q, want My Key 1", entries[0].Name)
	}
	if entries[0].APIKey != "sk|with|pipes" {
		t.Errorf("apiKey = %q, want sk|with|pipes", entries[0].APIKey)
	}
}

func TestPlanBulkAdd_CloudflareThreePart(t *testing.T) {
	entries := PlanBulkAdd([]string{"Edge|sk-cf|acct-9"}, nil, BulkAddOptions{IsCloudflareAI: true})
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if entries[0].Name != "Edge 1" {
		t.Errorf("name = %q, want Edge 1", entries[0].Name)
	}
	if entries[0].APIKey != "sk-cf" {
		t.Errorf("apiKey = %q, want sk-cf", entries[0].APIKey)
	}
	if entries[0].PSD["accountId"] != "acct-9" {
		t.Errorf("psd.accountId = %v, want acct-9", entries[0].PSD["accountId"])
	}
}

func TestPlanBulkAdd_CloudflareApiKeyWithPipes(t *testing.T) {
	// cloudflare-ai: "name|apiKey|accountId", apiKey may contain pipes.
	entries := PlanBulkAdd([]string{"Edge|sk|a|b|acct-7"}, nil, BulkAddOptions{IsCloudflareAI: true})
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if entries[0].APIKey != "sk|a|b" {
		t.Errorf("apiKey = %q, want sk|a|b", entries[0].APIKey)
	}
	if entries[0].PSD["accountId"] != "acct-7" {
		t.Errorf("psd.accountId = %v, want acct-7", entries[0].PSD["accountId"])
	}
}

func TestPlanBulkAdd_GapFillsAroundExisting(t *testing.T) {
	// Existing "Key 1" + "Key 3" → new keys take "Key 2" then "Key 4".
	entries := PlanBulkAdd([]string{"sk-new1", "sk-new2"}, []string{"Key 1", "Key 3"}, BulkAddOptions{})
	if got := names(entries); !reflect.DeepEqual(got, []string{"Key 2", "Key 4"}) {
		t.Errorf("gap-fill names = %v, want [Key 2 Key 4]", got)
	}
}

func TestPlanBulkAdd_NoCollisionWithExistingSameBase(t *testing.T) {
	// Existing "Key 1", "Key 2" → first new key takes "Key 3".
	entries := PlanBulkAdd([]string{"sk-1"}, []string{"Key 1", "Key 2"}, BulkAddOptions{})
	if entries[0].Name != "Key 3" {
		t.Errorf("name = %q, want Key 3", entries[0].Name)
	}
}

func TestPlanBulkAdd_IntraBatchNoReuse(t *testing.T) {
	// Two bare-key lines in one batch must not reuse the same generated name.
	entries := PlanBulkAdd([]string{"sk-1", "sk-2", "sk-3"}, nil, BulkAddOptions{})
	if got := names(entries); !reflect.DeepEqual(got, []string{"Key 1", "Key 2", "Key 3"}) {
		t.Errorf("intra-batch names = %v, want [Key 1 Key 2 Key 3]", got)
	}
}

func TestPlanBulkAdd_IntraBatchCollisionAcrossBases(t *testing.T) {
	// "Primary" and "Primary" again → second must take "Primary 2".
	entries := PlanBulkAdd([]string{"Primary|sk-1", "Primary|sk-2"}, nil, BulkAddOptions{})
	if got := names(entries); !reflect.DeepEqual(got, []string{"Primary 1", "Primary 2"}) {
		t.Errorf("cross-base intra-batch = %v, want [Primary 1 Primary 2]", got)
	}
}

func TestPlanBulkAdd_CaseInsensitiveCollision(t *testing.T) {
	// Existing "key 1" (lowercase) must collide with generated "Key 1".
	entries := PlanBulkAdd([]string{"sk-1"}, []string{"key 1"}, BulkAddOptions{})
	if entries[0].Name != "Key 2" {
		t.Errorf("case-insensitive: name = %q, want Key 2", entries[0].Name)
	}
}

func TestPlanBulkAdd_SkipsBlankLines(t *testing.T) {
	entries := PlanBulkAdd([]string{"sk-1", "", "  ", "sk-2"}, nil, BulkAddOptions{})
	if got := names(entries); !reflect.DeepEqual(got, []string{"Key 1", "Key 2"}) {
		t.Errorf("blank-skip names = %v, want [Key 1 Key 2]", got)
	}
}

func TestPlanBulkAdd_TrimsWhitespace(t *testing.T) {
	entries := PlanBulkAdd([]string{"  Primary  |  sk-aaa  "}, nil, BulkAddOptions{})
	if entries[0].Name != "Primary 1" {
		t.Errorf("name = %q, want Primary 1 (trimmed)", entries[0].Name)
	}
	if entries[0].APIKey != "sk-aaa" {
		t.Errorf("apiKey = %q, want sk-aaa (trimmed)", entries[0].APIKey)
	}
}

func TestPlanBulkAdd_EmptyBaseFallsBackToKey(t *testing.T) {
	// "|sk-x" → baseName empty → "Key".
	entries := PlanBulkAdd([]string{"|sk-x"}, nil, BulkAddOptions{})
	if entries[0].Name != "Key 1" {
		t.Errorf("name = %q, want Key 1 (empty base fallback)", entries[0].Name)
	}
}

func TestPlanBulkAdd_EmptyInput(t *testing.T) {
	if got := PlanBulkAdd(nil, nil, BulkAddOptions{}); len(got) != 0 {
		t.Errorf("nil input = %v, want empty", got)
	}
	if got := PlanBulkAdd([]string{}, []string{}, BulkAddOptions{}); len(got) != 0 {
		t.Errorf("empty input = %v, want empty", got)
	}
}

func TestPlanBulkAdd_ExistingNamesNilSafe(t *testing.T) {
	entries := PlanBulkAdd([]string{"sk-1"}, nil, BulkAddOptions{})
	if entries[0].Name != "Key 1" {
		t.Errorf("name = %q, want Key 1 (nil existingNames)", entries[0].Name)
	}
}
