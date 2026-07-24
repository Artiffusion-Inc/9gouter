package repo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// connections_dedup_test.go ports the regression coverage for decolua/9router
// #2477 (c73c419d): the createProviderConnection OAuth/API-key dedup rule and
// the id_token → chatgptAccountId extraction. Tests drive the real
// ConnectionRepo over sqlite (no mock).

func connWith(email string, psd map[string]any) settings.ProviderConnection {
	return settings.ProviderConnection{
		ID:       "c-" + email,
		Provider: "codex",
		AuthType: "oauth",
		Email:    email,
		Data:     dataWithPSD(psd),
	}
}

func dataWithPSD(psd map[string]any) json.RawMessage {
	m := map[string]any{}
	if psd != nil {
		m["providerSpecificData"] = psd
	}
	b, _ := json.Marshal(m)
	return b
}

func TestFindExistingForImport_CodexSameAccountIDMatches(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	existing := connWith("u@x.com", map[string]any{"chatgptAccountId": "acct-1"})
	if _, err := r.Create(ctx, existing); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Second login, same email + same chatgptAccountId → collapse (update-in-place).
	incoming := connWith("u@x.com", map[string]any{"chatgptAccountId": "acct-1"})
	incoming.ID = "c-incoming"
	got, err := r.FindExistingForImport(ctx, incoming)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil {
		t.Fatal("expected to match the existing codex row, got nil")
	}
	if got.ID != existing.ID {
		t.Errorf("matched id = %q, want %q", got.ID, existing.ID)
	}
}

func TestFindExistingForImport_CodexDifferentAccountIDNoMatch(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	// First account, bare-ish email with chatgptAccountId=acct-1.
	if _, err := r.Create(ctx, connWith("u@x.com", map[string]any{"chatgptAccountId": "acct-1"})); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Second login, same email but DIFFERENT chatgptAccountId → must NOT collapse
	// (otherwise the first account's rotated token pair is overwritten and looks
	// "invalid" after adding a second account — the exact regression #2477 fixes).
	incoming := connWith("u@x.com", map[string]any{"chatgptAccountId": "acct-2"})
	incoming.ID = "c-incoming"
	got, err := r.FindExistingForImport(ctx, incoming)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != nil {
		t.Fatalf("different chatgptAccountId must not collapse, matched %q", got.ID)
	}
}

func TestFindExistingForImport_CodexOneSidedAccountIDNoMatch(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	// Existing codex row has chatgptAccountId; incoming does not → no collapse.
	if _, err := r.Create(ctx, connWith("u@x.com", map[string]any{"chatgptAccountId": "acct-1"})); err != nil {
		t.Fatalf("create: %v", err)
	}
	incoming := connWith("u@x.com", nil)
	incoming.ID = "c-incoming"
	got, err := r.FindExistingForImport(ctx, incoming)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != nil {
		t.Fatalf("one-sided codex chatgptAccountId must not collapse, matched %q", got.ID)
	}
}

func TestFindExistingForImport_WorkspaceSameIDMatches(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	existing := settings.ProviderConnection{
		ID:       "c-1",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(map[string]any{"chatgptAccountId": "ws-9"}),
	}
	if _, err := r.Create(ctx, existing); err != nil {
		t.Fatalf("create: %v", err)
	}
	incoming := settings.ProviderConnection{
		ID:       "c-incoming",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(map[string]any{"chatgptAccountId": "ws-9"}),
	}
	got, err := r.FindExistingForImport(ctx, incoming)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.ID != existing.ID {
		t.Fatalf("workspace same id must match, got %v", got)
	}
}

func TestFindExistingForImport_WorkspaceOneSidedNoMatch(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	if _, err := r.Create(ctx, settings.ProviderConnection{
		ID:       "c-1",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(map[string]any{"chatgptAccountId": "ws-9"}),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Incoming has no chatgptAccountId → distinct, no collapse.
	incoming := settings.ProviderConnection{
		ID:       "c-incoming",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(nil),
	}
	got, _ := r.FindExistingForImport(ctx, incoming)
	if got != nil {
		t.Fatalf("one-sided workspace id must not collapse, matched %q", got.ID)
	}
}

func TestFindExistingForImport_UsernameBothMatch(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	if _, err := r.Create(ctx, settings.ProviderConnection{
		ID:       "c-1",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(map[string]any{"username": "alice"}),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	incoming := settings.ProviderConnection{
		ID:       "c-incoming",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(map[string]any{"username": "alice"}),
	}
	got, _ := r.FindExistingForImport(ctx, incoming)
	if got == nil || got.ID != "c-1" {
		t.Fatalf("same username must match, got %v", got)
	}
}

func TestFindExistingForImport_UsernameDiffersNoMatch(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	if _, err := r.Create(ctx, settings.ProviderConnection{
		ID:       "c-1",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(map[string]any{"username": "alice"}),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Same email, different username (cross-IdP) → distinct identity.
	incoming := settings.ProviderConnection{
		ID:       "c-incoming",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(map[string]any{"username": "bob"}),
	}
	got, _ := r.FindExistingForImport(ctx, incoming)
	if got != nil {
		t.Fatalf("different username must not collapse, matched %q", got.ID)
	}
}

func TestFindExistingForImport_BareEmailMatchesWhenNoDistinguishingID(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	// Non-codex, neither side has chatgptAccountId or username → bare-email match.
	if _, err := r.Create(ctx, settings.ProviderConnection{
		ID:       "c-1",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(nil),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	incoming := settings.ProviderConnection{
		ID:       "c-incoming",
		Provider: "claude",
		AuthType: "oauth",
		Email:    "u@x.com",
		Data:     dataWithPSD(nil),
	}
	got, _ := r.FindExistingForImport(ctx, incoming)
	if got == nil || got.ID != "c-1" {
		t.Fatalf("bare-email must match, got %v", got)
	}
}

func TestFindExistingForImport_DifferentEmailNoMatch(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	if _, err := r.Create(ctx, connWith("a@x.com", map[string]any{"chatgptAccountId": "acct-1"})); err != nil {
		t.Fatalf("create: %v", err)
	}
	incoming := connWith("b@x.com", map[string]any{"chatgptAccountId": "acct-1"})
	incoming.ID = "c-incoming"
	got, _ := r.FindExistingForImport(ctx, incoming)
	if got != nil {
		t.Fatalf("different email must not match, got %q", got.ID)
	}
}

func TestFindExistingForImport_APIKeyByNameMatches(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	if _, err := r.Create(ctx, settings.ProviderConnection{
		ID:       "c-1",
		Provider: "openai",
		AuthType: "apikey",
		Name:     "Prod",
		Data:     json.RawMessage(`{"apiKey":"sk-1"}`),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	incoming := settings.ProviderConnection{
		ID:       "c-incoming",
		Provider: "openai",
		AuthType: "apikey",
		Name:     "Prod",
		Data:     json.RawMessage(`{"apiKey":"sk-2"}`),
	}
	got, _ := r.FindExistingForImport(ctx, incoming)
	if got == nil || got.ID != "c-1" {
		t.Fatalf("apikey same name must match, got %v", got)
	}
}

func TestFindExistingForImport_APIKeyDifferentNameNoMatch(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	if _, err := r.Create(ctx, settings.ProviderConnection{
		ID:       "c-1",
		Provider: "openai",
		AuthType: "apikey",
		Name:     "Prod",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	incoming := settings.ProviderConnection{
		ID:       "c-incoming",
		Provider: "openai",
		AuthType: "apikey",
		Name:     "Backup",
	}
	got, _ := r.FindExistingForImport(ctx, incoming)
	if got != nil {
		t.Fatalf("different apikey name must not match, got %q", got.ID)
	}
}

func TestFindExistingForImport_AccessTokenNeverDedups(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	if _, err := r.Create(ctx, settings.ProviderConnection{
		ID:       "c-1",
		Provider: "grok-web",
		AuthType: "access_token",
		Name:     "same",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	incoming := settings.ProviderConnection{
		ID:       "c-incoming",
		Provider: "grok-web",
		AuthType: "access_token",
		Name:     "same",
	}
	got, _ := r.FindExistingForImport(ctx, incoming)
	if got != nil {
		t.Fatalf("access_token must never dedup, matched %q", got.ID)
	}
}

// jwt helper: build an unsigned JWT with a given claims payload.
func jwtWithClaims(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + "."
}

func TestChatGPTAccountIDFromIDToken_NamespaceClaim(t *testing.T) {
	tok := jwtWithClaims(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-42"},
	})
	if got := ChatGPTAccountIDFromIDToken(tok); got != "acct-42" {
		t.Errorf("namespace claim: got %q, want acct-42", got)
	}
}

func TestChatGPTAccountIDFromIDToken_NamespaceAlias(t *testing.T) {
	tok := jwtWithClaims(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_id": "acct-7"},
	})
	if got := ChatGPTAccountIDFromIDToken(tok); got != "acct-7" {
		t.Errorf("namespace alias: got %q, want acct-7", got)
	}
}

func TestChatGPTAccountIDFromIDToken_TopLevelClaim(t *testing.T) {
	tok := jwtWithClaims(map[string]any{"chatgpt_account_id": "acct-9"})
	if got := ChatGPTAccountIDFromIDToken(tok); got != "acct-9" {
		t.Errorf("top-level claim: got %q, want acct-9", got)
	}
}

func TestChatGPTAccountIDFromIDToken_NoAccountID(t *testing.T) {
	tok := jwtWithClaims(map[string]any{"sub": "user-1", "email": "u@x.com"})
	if got := ChatGPTAccountIDFromIDToken(tok); got != "" {
		t.Errorf("no account id: got %q, want empty", got)
	}
}

func TestChatGPTAccountIDFromIDToken_Malformed(t *testing.T) {
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.b.c.d"} {
		if got := ChatGPTAccountIDFromIDToken(bad); got != "" {
			t.Errorf("malformed token %q: got %q, want empty", bad, got)
		}
	}
}
