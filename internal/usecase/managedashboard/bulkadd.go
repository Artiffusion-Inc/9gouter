package managedashboard

// bulkadd.go ports the backend collision-aware name planner half of
// decolua/9router #2552 (de680e78): src/shared/utils/bulkAdd.js planBulkAdd.
//
// Background: the backend upserts apikey connections BY NAME (connectionsRepo
// createProviderConnection: existing = find(c => c.authType=="apikey" &&
// c.name == data.name)). A colliding name overwrites an existing key instead of
// inserting a new one. The bulk-add UI used to derive "<base> <lineIndex>" from
// the paste position, blind to existing names, so re-adding keys often silently
// replaced earlier ones.
//
// planBulkAdd gap-fills the smallest free "<base> <n>" against both existing
// connection names and names already assigned earlier in the same batch, so a
// generated name is never reused and the backend always inserts. The Go rewrite
// has no bulk-add endpoint yet, but this is the pure backend primitive the JS
// planner delegates to; placing it here makes it reusable (and unit-coverable
// without a mock) when a Go bulk-add handler lands.
//
// Only numeric-suffix collision is handled. A user who manually types an exact
// existing non-numbered custom name (no index) will still hit the backend
// upsert — but bulk auto-naming always appends " <n>", so that path is
// unreachable from the bulk modal.

import "strings"

// BulkAddEntry is one planned bulk-add row: a collision-free connection name,
// the apiKey, and optional providerSpecificData (e.g. cloudflare accountId).
type BulkAddEntry struct {
	Name    string
	APIKey  string
	Skipped bool
	PSD     map[string]any
}

// BulkAddOptions tunes planBulkAdd. IsCloudflareAI switches the parser to the
// 3-part "name|apiKey|accountId" format (apiKey may itself contain pipes).
type BulkAddOptions struct {
	IsCloudflareAI bool
}

// parseBulkLine parses one pipe-separated bulk line into baseName + apiKey +
// optional providerSpecificData. Returns ok=false for a blank line.
func parseBulkLine(line string, opts BulkAddOptions) (baseName, apiKey string, psd map[string]any, ok bool) {
	parts := strings.Split(line, "|")
	if opts.IsCloudflareAI && len(parts) >= 3 {
		// name|apiKey|accountId  (apiKey may itself contain pipes).
		baseName = strings.TrimSpace(parts[0])
		apiKey = strings.TrimSpace(strings.Join(parts[1:len(parts)-1], "|"))
		accountID := strings.TrimSpace(parts[len(parts)-1])
		if baseName == "" {
			baseName = "Key"
		}
		return baseName, apiKey, map[string]any{"accountId": accountID}, true
	}
	if len(parts) >= 2 {
		// name|apiKey  (apiKey may itself contain pipes).
		baseName = strings.TrimSpace(parts[0])
		apiKey = strings.TrimSpace(strings.Join(parts[1:], "|"))
		if baseName == "" {
			baseName = "Key"
		}
		return baseName, apiKey, nil, true
	}
	// apiKey only — auto-named "Key N".
	apiKey = strings.TrimSpace(parts[0])
	return "Key", apiKey, nil, true
}

// PlanBulkAdd parses raw paste lines and assigns collision-free "<base> <n>"
// names. existingNames are the connection names already saved; names assigned
// earlier in the batch are tracked in `used` so a generated name is never reused
// within one batch. Blank lines are skipped. Lines with no apiKey after parse
// are skipped (Skipped=true is reserved for a future skip-if-exists flag).
func PlanBulkAdd(lines []string, existingNames []string, opts BulkAddOptions) []BulkAddEntry {
	used := make(map[string]bool, len(existingNames))
	for _, n := range existingNames {
		used[strings.ToLower(n)] = true
	}

	out := make([]BulkAddEntry, 0, len(lines))
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		base, apiKey, psd, ok := parseBulkLine(line, opts)
		if !ok || apiKey == "" {
			continue
		}
		// Gap-fill from 1: smallest free "<base> <n>" not in `used`.
		idx := 1
		name := ""
		for {
			name = base + " " + itoa(idx)
			if !used[strings.ToLower(name)] {
				break
			}
			idx++
		}
		used[strings.ToLower(name)] = true
		entry := BulkAddEntry{Name: name, APIKey: apiKey, Skipped: false}
		if psd != nil {
			entry.PSD = psd
		}
		out = append(out, entry)
	}
	return out
}
