package rtk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// kiroCompressHandler returns an httptest handler that echoes back each
// incoming message with a compressed marker prefix on the content, preserving
// role and order. It is a real HTTP server, not a dependency mock.
func kiroCompressHandler(t *testing.T, prefix string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode kiro compress payload: %v", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		out := map[string]any{"messages": []map[string]any{}}
		for _, m := range payload.Messages {
			content := m["content"]
			if s, ok := content.(string); ok {
				content = prefix + s
			}
			out["messages"] = append(out["messages"].([]map[string]any), map[string]any{
				"role":    m["role"],
				"content": content,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
}

// kiroBody builds a Kiro conversationState body with one user + one assistant
// turn for use across the projection/apply tests.
func kiroBody() map[string]any {
	return map[string]any{
		"model": "kiro-1",
		"conversationState": map[string]any{
			"history": []any{
				map[string]any{
					"userInputMessage": map[string]any{
						"content": "please write a function",
					},
				},
				map[string]any{
					"assistantResponseMessage": map[string]any{
						"content": "here is a function",
						"toolUses": []any{
							map[string]any{
								"toolUseId": "tu_1",
								"name":      "write_file",
								"input":     map[string]any{"path": "main.go"},
							},
						},
					},
				},
			},
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{
					"content":           "now add tests",
					"systemInstruction": "be concise",
				},
			},
		},
	}
}

// TestCollectKiroHeadroomMessages verifies the projection: history + current
// messages are projected in order, system instructions map to "system", tool
// results to "tool", and assistant toolUses become tool_calls.
func TestCollectKiroHeadroomMessages(t *testing.T) {
	projection := collectKiroHeadroomMessages(kiroBody())
	if projection == nil {
		t.Fatal("expected non-nil projection")
	}
	// Expected order: user(please write), assistant(here is a function, +tool_calls),
	// system(be concise), user(now add tests).
	wantRoles := []string{"user", "assistant", "system", "user"}
	if len(projection.messages) != len(wantRoles) {
		t.Fatalf("got %d messages, want %d: %+v", len(projection.messages), len(wantRoles), projection.messages)
	}
	for i, want := range wantRoles {
		if got, _ := projection.messages[i]["role"].(string); got != want {
			t.Errorf("message[%d].role = %q, want %q", i, got, want)
		}
	}
	// Assistant message must carry tool_calls.
	assistant := projection.messages[1]
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Errorf("assistant tool_calls = %v, want 1 call", assistant["tool_calls"])
	} else {
		call := calls[0].(map[string]any)
		fn := call["function"].(map[string]any)
		if fn["name"] != "write_file" {
			t.Errorf("tool_call name = %v, want write_file", fn["name"])
		}
		if fn["arguments"] != `{"path":"main.go"}` {
			t.Errorf("tool_call arguments = %v, want json input", fn["arguments"])
		}
	}
	// Targets must point at the original fields.
	if len(projection.targets) != len(projection.messages) {
		t.Fatalf("targets count %d != messages count %d", len(projection.targets), len(projection.messages))
	}
}

// TestCollectKiroHeadroomMessagesToolResult projects a user turn with
// toolResults; each text part becomes a "tool" message with tool_call_id.
func TestCollectKiroHeadroomMessagesToolResult(t *testing.T) {
	body := map[string]any{
		"conversationState": map[string]any{
			"history": []any{
				map[string]any{
					"userInputMessage": map[string]any{
						"userInputMessageContext": map[string]any{
							"toolResults": []any{
								map[string]any{
									"toolUseId": "tu_1",
									"content": []any{
										map[string]any{"text": "file written ok"},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	projection := collectKiroHeadroomMessages(body)
	if projection == nil {
		t.Fatal("expected non-nil projection")
	}
	if len(projection.messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(projection.messages))
	}
	msg := projection.messages[0]
	if msg["role"] != "tool" {
		t.Errorf("role = %v, want tool", msg["role"])
	}
	if msg["tool_call_id"] != "tu_1" {
		t.Errorf("tool_call_id = %v, want tu_1", msg["tool_call_id"])
	}
	if msg["content"] != "file written ok" {
		t.Errorf("content = %v, want file written ok", msg["content"])
	}
}

// TestCollectKiroHeadroomMessagesEmpty returns nil when there is no
// conversationState or no text-bearing messages.
func TestCollectKiroHeadroomMessagesEmpty(t *testing.T) {
	if got := collectKiroHeadroomMessages(map[string]any{}); got != nil {
		t.Errorf("expected nil for body without conversationState, got %+v", got)
	}
	if got := collectKiroHeadroomMessages(map[string]any{"conversationState": map[string]any{}}); got != nil {
		t.Errorf("expected nil for empty conversationState, got %+v", got)
	}
}

// TestCompressKiroViaHeadroomRoundTrip verifies #2488: the projected messages
// are compressed and the compressed text is written back into the original Kiro
// fields (userInputMessage.content, assistantResponseMessage.content, system).
func TestCompressKiroViaHeadroomRoundTrip(t *testing.T) {
	srv := httptest.NewServer(kiroCompressHandler(t, "[c] "))
	defer srv.Close()

	body := kiroBody()
	stats, err := compressKiroViaHeadroomImpl(body, HeadroomConfig{
		Enabled: true, URL: srv.URL, Model: "kiro-1",
	}, srv.Client(), 3000)
	if err != nil {
		t.Fatalf("compress kiro: %v", err)
	}
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}

	state := body["conversationState"].(map[string]any)
	hist := state["history"].([]any)
	// history[0].userInputMessage.content compressed.
	user0 := hist[0].(map[string]any)["userInputMessage"].(map[string]any)
	if !strings.HasPrefix(user0["content"].(string), "[c] ") {
		t.Errorf("user0 content = %v, want compressed marker", user0["content"])
	}
	// history[1].assistantResponseMessage.content compressed.
	assistant := hist[1].(map[string]any)["assistantResponseMessage"].(map[string]any)
	if !strings.HasPrefix(assistant["content"].(string), "[c] ") {
		t.Errorf("assistant content = %v, want compressed marker", assistant["content"])
	}
	// currentMessage.userInputMessage.systemInstruction + content compressed.
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if !strings.HasPrefix(current["systemInstruction"].(string), "[c] ") {
		t.Errorf("systemInstruction = %v, want compressed marker", current["systemInstruction"])
	}
	if !strings.HasPrefix(current["content"].(string), "[c] ") {
		t.Errorf("current content = %v, want compressed marker", current["content"])
	}
}

// TestCompressKiroViaHeadroomMismatchedCount verifies fail-open: when the proxy
// returns a different message count, the body is left untouched and stats nil.
func TestCompressKiroViaHeadroomMismatchedCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return fewer messages than projected.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]any{
			{"role": "user", "content": "only one"},
		}})
	}))
	defer srv.Close()

	body := kiroBody()
	origBytes, _ := json.Marshal(body)
	stats, err := compressKiroViaHeadroomImpl(body, HeadroomConfig{
		Enabled: true, URL: srv.URL, Model: "kiro-1",
	}, srv.Client(), 3000)
	if err != nil {
		t.Fatalf("expected fail-open nil err, got %v", err)
	}
	if stats != nil {
		t.Errorf("expected nil stats on mismatch, got %+v", stats)
	}
	afterBytes, _ := json.Marshal(body)
	if string(origBytes) != string(afterBytes) {
		t.Error("body was mutated despite mismatched proxy response")
	}
}

// TestCompressKiroViaHeadroomRoleOrder verifies fail-open when the proxy
// reorders/changes roles: body left untouched.
func TestCompressKiroViaHeadroomRoleOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return the right count but wrong roles.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]any{
			{"role": "system", "content": "x"},
			{"role": "system", "content": "x"},
			{"role": "system", "content": "x"},
			{"role": "system", "content": "x"},
		}})
	}))
	defer srv.Close()

	body := kiroBody()
	origBytes, _ := json.Marshal(body)
	stats, _ := compressKiroViaHeadroomImpl(body, HeadroomConfig{
		Enabled: true, URL: srv.URL, Model: "kiro-1",
	}, srv.Client(), 3000)
	if stats != nil {
		t.Errorf("expected nil stats on role mismatch, got %+v", stats)
	}
	afterBytes, _ := json.Marshal(body)
	if string(origBytes) != string(afterBytes) {
		t.Error("body was mutated despite role-order mismatch")
	}
}

// TestCompressKiroViaHeadroomNoConversationState verifies the no-projection
// path: a body without conversationState is skipped with a diagnostic.
func TestCompressKiroViaHeadroomNoConversationState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("proxy should not be called without conversationState")
	}))
	defer srv.Close()

	diag := &HeadroomDiagnostics{}
	stats, err := compressKiroViaHeadroomImpl(map[string]any{"model": "kiro-1"}, HeadroomConfig{
		Enabled: true, URL: srv.URL, Model: "kiro-1", Diagnostics: diag,
	}, srv.Client(), 3000)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if stats != nil {
		t.Errorf("expected nil stats, got %+v", stats)
	}
	if !strings.Contains(diag.Reason, "did not project") {
		t.Errorf("diagnostic reason = %q, want 'did not project'", diag.Reason)
	}
}
