package grokcliexec

// session.go ports the per-session bookkeeping the Grok Build Responses API
// requires: the session id (carried as x-grok-session-id / x-grok-conv-id), the
// monotonically-increasing turn index (x-grok-turn-idx), and the stable agent
// id (x-grok-agent-id). Upstream 59b78282 rewrote all three — the JS executor
// resolved them from headers + body fields + a per-session turn store with TTL
// + LRU eviction; the Go port previously hard-coded "session-placeholder" /
// "req-placeholder" / a static turn of 1, so multi-turn Grok Build conversations
// sent the wrong turn index and no session continuity headers at all.
//
// resolveGrokCliSessionId reads the body fields the JS resolveSessionId(scope
// "grok-cli") path consults (prompt_cache_key / session_id / conversation_id /
// metadata) plus the connectionId/workspaceId from providerSpecificData. The
// full sessionManager.js derive/continuation logic is not ported — the Go
// proxychat path does not thread raw request headers into Credentials, so the
// header-derived derivation has no source; the body + psd fields cover the
// dashboard-initiated flow.

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// resolveGrokCliSessionId mirrors resolveGrokCliSessionId in the JS executor:
// prefer an explicit body field, then the connection-bound sessionId from psd,
// finally mint a fresh id so conv+session are stable for the turn store.
func resolveGrokCliSessionID(body map[string]any, psd map[string]any) string {
	for _, k := range []string{"session_id", "prompt_cache_key", "conversation_id"} {
		if v, ok := body[k].(string); ok && v != "" {
			return v
		}
	}
	if md, ok := body["metadata"].(map[string]any); ok {
		for _, k := range []string{"session_id", "sessionId", "conversation_id", "conversationId"} {
			if v, ok := md[k].(string); ok && v != "" {
				return v
			}
		}
	}
	if psd != nil {
		for _, k := range []string{"sessionId", "session_id", "connectionId", "workspaceId"} {
			if v, ok := psd[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return uuid.NewString()
}

// turnEntry is the per-session last turn + last-used timestamp.
type turnEntry struct {
	turn     int
	lastUsed int64 // unix nano
}

// grokCliTurnStore is the package-level per-session turn store (sessionTurnStore
// + requestTurnStore in JS). The requestTurnStore WeakMap has no Go equivalent;
// retries re-resolve from the session store (the prev+1 delta path), which is
// the dominant case — the per-requestKey cache only mattered for in-flight
// retry dedup of the same body object, which the Go chat path does not re-feed.
var grokCliTurnStore = struct {
	sync.Mutex
	m map[string]turnEntry
}{m: make(map[string]turnEntry)}

// countGrokCliUserTurns mirrors countGrokCliUserTurns: 1-based count of user
// message items in the input (items with role "user" and no type / type
// "message"). Minimum 1 so a fresh session starts at turn 1.
func countGrokCliUserTurns(input []any) int {
	n := 0
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := item["role"].(string)
		if role != "user" {
			continue
		}
		t, _ := item["type"].(string)
		if t == "" || t == "message" {
			n++
		}
	}
	if n < 1 {
		return 1
	}
	return n
}

// resolveGrokCliTurnIdx mirrors resolveGrokCliTurnIdx: for a known session,
// take the max of the input-derived turn and (prev + delta) when the session is
// still live; mint a fresh turn otherwise. LRU-evicts when the store reaches
// grokCliTurnStoreMax. requestKey is unused (see grokCliTurnStore note).
func resolveGrokCliTurnIdx(sessionID string, input []any) int {
	fromInput := countGrokCliUserTurns(input)
	if sessionID == "" {
		return fromInput
	}
	now := time.Now().UnixNano()
	grokCliTurnStore.Lock()
	defer grokCliTurnStore.Unlock()
	prev := 0
	if e, ok := grokCliTurnStore.m[sessionID]; ok && now-e.lastUsed <= int64(grokCliSessionTTL) {
		prev = e.turn
		delete(grokCliTurnStore.m, sessionID)
	}
	turn := fromInput
	if prev > 0 {
		delta := prev + 1
		if fromInput > delta {
			delta = fromInput
		}
		turn = delta
	}
	// LRU eviction: drop the oldest entry until under cap.
	for len(grokCliTurnStore.m) >= grokCliTurnStoreMax {
		var oldestKey string
		var oldestTS int64
		for k, e := range grokCliTurnStore.m {
			if oldestTS == 0 || e.lastUsed < oldestTS {
				oldestTS = e.lastUsed
				oldestKey = k
			}
		}
		if oldestKey == "" {
			break
		}
		delete(grokCliTurnStore.m, oldestKey)
	}
	grokCliTurnStore.m[sessionID] = turnEntry{turn: turn, lastUsed: now}
	return turn
}

// resetGrokCliTurnStore is a test helper.
func resetGrokCliTurnStore() {
	grokCliTurnStore.Lock()
	grokCliTurnStore.m = make(map[string]turnEntry)
	grokCliTurnStore.Unlock()
}

// grokCliTurnStoreSize is a test helper.
func grokCliTurnStoreSize() int {
	grokCliTurnStore.Lock()
	defer grokCliTurnStore.Unlock()
	return len(grokCliTurnStore.m)
}

// grokCliAgentID is the stable per-process agent id, formatted as a UUID-ish
// string (mirrors getConsistentMachineId("grok-cli-agent") + the
// [8,4,"5"+3,"a"+3,12-pad] join in the JS execute()). It is the
// x-grok-agent-id header value. Process-stable (not restart-stable) — enough
// for the agent-id continuity the protocol wants within a single gateway run.
var grokCliAgentID = sync.OnceValue(func() string {
	id := uuid.NewString()
	// Force the RFC 4122 version nibble to 5 and the variant to a (10xx) so the
	// string looks like a v5 UUID, matching the JS formatter shape.
	b := []byte(id)
	if len(b) == 36 {
		b[14] = '5'
		b[19] = 'a'
		return string(b)
	}
	return id
})

// resolveGrokCliAgentID prefers a connection-bound deviceId/agentId from psd
// (set by the device-code importer when it persists the connection), falling
// back to the process-stable grokCliAgentID.
func resolveGrokCliAgentID(psd map[string]any) string {
	if psd != nil {
		for _, k := range []string{"deviceId", "agentId"} {
			if v, ok := psd[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return grokCliAgentID()
}
