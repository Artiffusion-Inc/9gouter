package repo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// connections_dedup.go ports the createProviderConnection dedup half of
// decolua/9router #2477 (c73c419d): only collapse an incoming OAuth connection
// onto an existing row when both rows describe the SAME identity, so a second
// Codex login (different ChatGPT account id) no longer overwrites the first
// account's rotated token pair and surfaces as token_invalid.
//
// The JS rule (connectionsRepo.js createProviderConnection):
//
//	oauth + email:
//	  codex:        match only if incoming.chatgptAccountId == existing.chatgptAccountId (both present)
//	  workspace:    match if both have chatgptAccountId and they are equal; one-sided ⇒ no match
//	  non-workspace:match if both have username and they are equal; one-sided ⇒ no match; neither ⇒ bare-email match
//	apikey + name:  match on (authType=apikey && name == existing.name)
//
// FindExistingForImport evaluates that rule against the live rows and returns
// the matching existing connection (nil + nil error when no match), so the
// caller can update-in-place instead of creating a duplicate.

// FindExistingForImport returns the existing connection an incoming record
// should merge into, mirroring the createProviderConnection dedup rule. Returns
// nil, nil when no candidate matches (caller creates a new row).
func (r *ConnectionRepo) FindExistingForImport(ctx context.Context, incoming settings.ProviderConnection) (*settings.ProviderConnection, error) {
	if incoming.AuthType == "oauth" && incoming.Email != "" {
		return r.findExistingOAuth(ctx, incoming)
	}
	if incoming.AuthType == "apikey" && incoming.Name != "" {
		return r.findExistingAPIKey(ctx, incoming)
	}
	// access_token: never dedup — user manages duplicates manually (JS rule).
	return nil, nil
}

func (r *ConnectionRepo) findExistingOAuth(ctx context.Context, incoming settings.ProviderConnection) (*settings.ProviderConnection, error) {
	all, err := r.List(ctx, ConnectionFilter{Provider: incoming.Provider})
	if err != nil {
		return nil, fmt.Errorf("connections dedup list: %w", err)
	}
	incomingPSD := psdMap(incoming.Data)
	incomingWs := psdString(incomingPSD, "chatgptAccountId")
	incomingUser := psdString(incomingPSD, "username")
	for i := range all {
		c := &all[i]
		if c.AuthType != "oauth" || c.Email != incoming.Email {
			continue
		}
		existingPSD := psdMap(c.Data)
		existingWs := psdString(existingPSD, "chatgptAccountId")
		existingUser := psdString(existingPSD, "username")

		if incoming.Provider == "codex" {
			// Codex/OpenAI can issue multiple OAuth grants for the same email.
			// Refresh tokens are rotated single-use; collapsing a new login
			// onto a bare-email row overwrites the first account's token pair.
			// Only update an existing Codex row when both rows expose the same
			// ChatGPT account id.
			if incomingWs != "" && existingWs != "" && incomingWs == existingWs {
				return c, nil
			}
			continue
		}

		// Workspace providers use chatgptAccountId when both sides have it.
		if incomingWs != "" && existingWs != "" {
			if incomingWs == existingWs {
				return c, nil
			}
			continue
		}
		if incomingWs != "" || existingWs != "" {
			// One-sided workspace id ⇒ distinct identity, do not collapse.
			continue
		}

		// Non-workspace providers: match on (email + username) so cross-IdP
		// accounts don't overwrite each other. Require username on both sides.
		if incomingUser != "" && existingUser != "" {
			if incomingUser == existingUser {
				return c, nil
			}
			continue
		}
		if incomingUser != "" || existingUser != "" {
			// One-sided username ⇒ distinct identity.
			continue
		}
		// Neither side has a distinguishing id — bare-email fallback match.
		return c, nil
	}
	return nil, nil
}

func (r *ConnectionRepo) findExistingAPIKey(ctx context.Context, incoming settings.ProviderConnection) (*settings.ProviderConnection, error) {
	all, err := r.List(ctx, ConnectionFilter{Provider: incoming.Provider})
	if err != nil {
		return nil, fmt.Errorf("connections dedup list: %w", err)
	}
	for i := range all {
		c := &all[i]
		if c.AuthType == "apikey" && c.Name == incoming.Name {
			return c, nil
		}
	}
	return nil, nil
}

// psdMap extracts the providerSpecificData object from a connection's Data blob.
func psdMap(data []byte) map[string]any {
	if len(data) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	if psd, ok := m["providerSpecificData"].(map[string]any); ok {
		return psd
	}
	return nil
}

// psdString reads a string field from the providerSpecificData map.
func psdString(psd map[string]any, key string) string {
	if psd == nil {
		return ""
	}
	if v, ok := psd[key].(string); ok {
		return v
	}
	return ""
}

// decodeIDTokenClaims extracts the JWT payload claims from an OAuth id_token
// (the second base64url segment). Codex's id_token carries the ChatGPT account
// id under the "https://api.openai.com/auth" namespace claim as
// chatgpt_account_id / chatgpt_id, or a top-level chatgptAccountId. The token
// signature is NOT verified — the dashboard already trusts it from the OAuth
// handshake; we only need the claims for the dedup key.
func decodeIDTokenClaims(idToken string) (map[string]any, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("id_token: expected 3 segments, got %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some encoders pad; fall back to padded StdEncoding.
		payload, err = base64.URLEncoding.DecodeString(addB64Pad(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("id_token: decode payload: %w", err)
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("id_token: unmarshal claims: %w", err)
	}
	return claims, nil
}

// ChatGPTAccountIDFromIDToken extracts the ChatGPT account id from an OAuth
// id_token, checking the OpenAI auth-namespace claim first, then the top-level
// alias. Returns "" when no account id is present (the caller treats a missing
// id as a distinct identity rather than a bare-email collapse for codex).
func ChatGPTAccountIDFromIDToken(idToken string) string {
	claims, err := decodeIDTokenClaims(idToken)
	if err != nil || claims == nil {
		return ""
	}
	if ns, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		for _, key := range []string{"chatgpt_account_id", "chatgpt_id", "chatgptAccountId"} {
			if v, ok := ns[key].(string); ok && v != "" {
				return v
			}
		}
	}
	for _, key := range []string{"chatgpt_account_id", "chatgpt_id", "chatgptAccountId"} {
		if v, ok := claims[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// addB64Pad restores the padding a non-Raw base64 encoder expects.
func addB64Pad(s string) string {
	if mod := len(s) % 4; mod != 0 {
		s += strings.Repeat("=", 4-mod)
	}
	return s
}
