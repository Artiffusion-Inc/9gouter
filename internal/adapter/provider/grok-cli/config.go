package grokcliexec

// config.go holds the Grok Build protocol constants used only by the executor
// package: the cli-chat-proxy base URL, the per-session turn-store cap + TTL,
// and the ordered effort levels. The client fingerprint (version /
// client-identifier / User-Agent) and the model id ("grok-build") live in the
// registry static headers + the resolver / quotafetch packages, not here — a
// shared constants package would import the executor and create a cycle, so
// each consumer carries its own copy (kept in sync by the 59b78282 bump that
// touched all four at once). See the registry grok-cli entry + resolver/grokcli.go
// + quotafetch/grok_cli.go + api/grokclioauth.go for the fingerprint constants.

const (
	// grokCliBaseURL is the cli-chat-proxy Responses API base.
	grokCliBaseURL = "https://cli-chat-proxy.grok.com/v1"

	// grokCliTurnStoreMax is the LRU cap on the per-session turn-index store
	// (upstream GROK_CLI_TURN_STORE_MAX). Prevents unbounded growth across
	// long-lived processes with many sessions.
	grokCliTurnStoreMax = 5000
	// grokCliSessionTTL bounds how long a session's last turn index stays
	// valid for delta increments. Mirrors MEMORY_CONFIG.sessionTtlMs (30 min
	// in the JS runtime config); the Go build has no runtimeConfig, so the
	// constant encodes the same default.
	grokCliSessionTTL = 30 * 60 * 1_000_000_000 // 30 minutes in nanoseconds
)

// grokCliEffortLevels is the ordered set of reasoning effort levels the
// Responses API accepts; "xhigh" was added by 59b78282 (replacing the
// "max"-as-alias mapping done client-side).
var grokCliEffortLevels = []string{"low", "medium", "high", "xhigh"}
