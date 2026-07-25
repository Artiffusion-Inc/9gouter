package quotafetch

// registry.go maps provider ids → Fetchers and exposes Lookup so the api
// byConnection handler can dispatch per-connection live quota fetches instead
// of the hard-coded "not implemented" stub. The fetchers live in
// per-provider files (claude.go, codex.go, …) to keep each transport's quirks
// isolated.

var registry = map[string]Fetcher{}

// register adds a Fetcher for a provider id. Called from per-provider init()
// blocks; idempotent if the same provider registers twice (last wins).
func register(providerID string, f Fetcher) {
	registry[providerID] = f
}

// Lookup returns the Fetcher for a provider id, or nil when no live quota
// fetch is implemented for it (the api handler then falls back to the
// "not available" payload). Aliases (e.g. minimax-cn, glm-cn) are registered
// explicitly alongside their base id.
func Lookup(providerID string) Fetcher {
	return registry[providerID]
}

// Providers returns every provider id with a registered Fetcher, for
// diagnostics / tests.
func Providers() []string {
	out := make([]string, 0, len(registry))
	for id := range registry {
		out = append(out, id)
	}
	return out
}
