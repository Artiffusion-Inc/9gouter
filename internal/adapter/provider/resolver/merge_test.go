package resolver

import (
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// TestShouldRefreshCredentials_NotNearExpiry returns false when the access
// token is comfortably inside its refresh lead.
func TestShouldRefreshCredentials_NotNearExpiry(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"refreshToken": "rt",
		"expiresAt":    now.Add(1 * time.Hour).Format(time.RFC3339Nano),
	}
	if ShouldRefreshCredentials("claude", data, now) {
		t.Fatal("expected no refresh when token is 1h from expiry (lead 5m)")
	}
}

// TestShouldRefreshCredentials_NearExpiry returns true inside the refresh lead.
func TestShouldRefreshCredentials_NearExpiry(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"refreshToken": "rt",
		"expiresAt":    now.Add(2 * time.Minute).Format(time.RFC3339Nano),
	}
	if !ShouldRefreshCredentials("claude", data, now) {
		t.Fatal("expected refresh when token expires within 5m lead")
	}
}

// TestShouldRefreshCredentials_NoRefreshToken returns false — cannot refresh.
func TestShouldRefreshCredentials_NoRefreshToken(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"expiresAt": now.Add(2 * time.Minute).Format(time.RFC3339Nano),
	}
	if ShouldRefreshCredentials("claude", data, now) {
		t.Fatal("no refresh token → cannot proactively refresh")
	}
}

// TestShouldRefreshCredentials_CodexStale triggers on maxRefreshAge even when
// the access token is far from expiry.
func TestShouldRefreshCredentials_CodexStale(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	// Token is 20d from expiry (well past the 5-day lead, so staleness is the
	// only trigger), but the last refresh was 10 days ago → beyond the 8-day
	// maxRefreshAge.
	data := map[string]any{
		"refreshToken":  "rt",
		"expiresAt":     now.Add(20 * 24 * time.Hour).Format(time.RFC3339Nano),
		"lastRefreshAt": now.Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	if !ShouldRefreshCredentials("codex", data, now) {
		t.Fatal("codex token stale beyond maxRefreshAge should refresh")
	}
}

// TestShouldRefreshCredentials_CodexFresh does NOT trigger when within age.
func TestShouldRefreshCredentials_CodexFresh(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"refreshToken":  "rt",
		"expiresAt":     now.Add(20 * 24 * time.Hour).Format(time.RFC3339Nano), // 20d >> 5d lead
		"lastRefreshAt": now.Add(-1 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	if ShouldRefreshCredentials("codex", data, now) {
		t.Fatal("codex token within maxRefreshAge should not refresh")
	}
}

// TestShouldRefreshCredentials_EpochMsExpiresAt accepts a JS epoch-ms number.
func TestShouldRefreshCredentials_EpochMsExpiresAt(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"refreshToken": "rt",
		"expiresAt":    float64(now.Add(2 * time.Minute).UnixMilli()), // epoch ms
	}
	if !ShouldRefreshCredentials("claude", data, now) {
		t.Fatal("epoch-ms expiresAt within lead should refresh")
	}
}

// TestMergeRefreshedCredentials_RotatesAccessToken writes the new token and an
// ISO expiresAt derived from expiresIn, preserving the unrotated refresh token.
func TestMergeRefreshedCredentials_RotatesAccessToken(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	current := map[string]any{"refreshToken": "rt-old", "idToken": "idt-old"}
	refreshed := &RefreshedCredentials{AccessToken: "at-new", ExpiresIn: 3600}
	patch := MergeRefreshedCredentials("claude", current, refreshed, now)
	if patch["accessToken"] != "at-new" {
		t.Errorf("accessToken=%v want at-new", patch["accessToken"])
	}
	if patch["refreshToken"] != "rt-old" {
		t.Errorf("refreshToken preserved=%v want rt-old", patch["refreshToken"])
	}
	if patch["idToken"] != "idt-old" {
		t.Errorf("idToken preserved=%v want idt-old", patch["idToken"])
	}
	if patch["expiresIn"] != 3600 {
		t.Errorf("expiresIn=%v want 3600", patch["expiresIn"])
	}
	if _, ok := patch["lastRefreshAt"]; !ok {
		t.Error("expected lastRefreshAt stamped (access token rotated)")
	}
}

// TestMergeRefreshedCredentials_CodexAlwaysStampsLastRefreshAt stamps even
// when no token was rotated, because codex tracks refresh staleness.
func TestMergeRefreshedCredentials_CodexAlwaysStampsLastRefreshAt(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	refreshed := &RefreshedCredentials{AccessToken: "at-new", ExpiresIn: 3600}
	patch := MergeRefreshedCredentials("codex", map[string]any{}, refreshed, now)
	if _, ok := patch["lastRefreshAt"]; !ok {
		t.Error("codex trackRefreshAt=true must always stamp lastRefreshAt")
	}
}

// TestMergeRefreshedCredentials_NilRefreshed returns nil (nothing to merge).
func TestMergeRefreshedCredentials_NilRefreshed(t *testing.T) {
	if patch := MergeRefreshedCredentials("claude", map[string]any{}, nil, time.Now()); patch != nil {
		t.Fatalf("nil refreshed must return nil patch, got %v", patch)
	}
}

// TestMergeRefreshedCredentials_Unrecoverable returns the marker patch.
func TestMergeRefreshedCredentials_Unrecoverable(t *testing.T) {
	refreshed := &RefreshedCredentials{Unrecoverable: true}
	patch := MergeRefreshedCredentials("claude", map[string]any{}, refreshed, time.Now())
	if !UnrecoverableRefreshPatch(patch) {
		t.Fatalf("unrecoverable refresh must surface the marker, got %v", patch)
	}
}

// TestMergeRefreshedCredentials_MergesProviderSpecificData shallow-merges PSD
// (refreshed values overwrite, existing keys not in refreshed survive).
func TestMergeRefreshedCredentials_MergesProviderSpecificData(t *testing.T) {
	current := map[string]any{
		"providerSpecificData": map[string]any{"profileArn": "arn-old", "region": "us"},
	}
	refreshed := &RefreshedCredentials{
		AccessToken:          "at-new",
		ExpiresIn:            3600,
		ProviderSpecificData: map[string]any{"profileArn": "arn-new", "project": "p1"},
	}
	patch := MergeRefreshedCredentials("kiro", current, refreshed, time.Now())
	psd, ok := patch["providerSpecificData"].(map[string]any)
	if !ok {
		t.Fatalf("providerSpecificData not merged, got %v", patch["providerSpecificData"])
	}
	if psd["profileArn"] != "arn-new" {
		t.Errorf("profileArn=%v want arn-new", psd["profileArn"])
	}
	if psd["region"] != "us" {
		t.Errorf("region=%v want us (existing must survive)", psd["region"])
	}
	if psd["project"] != "p1" {
		t.Errorf("project=%v want p1", psd["project"])
	}
}

// TestMergeRefreshedCredentials_CopilotToken rotates the copilot session token.
func TestMergeRefreshedCredentials_CopilotToken(t *testing.T) {
	refreshed := &RefreshedCredentials{
		AccessToken:          "at-new",
		CopilotToken:         "cop-new",
		CopilotTokenExpiresAt: "2030-01-01T00:00:00Z",
		ExpiresIn:            3600,
	}
	patch := MergeRefreshedCredentials("copilot", map[string]any{}, refreshed, time.Now())
	if patch["copilotToken"] != "cop-new" {
		t.Errorf("copilotToken=%v want cop-new", patch["copilotToken"])
	}
	if patch["copilotTokenExpiresAt"] != "2030-01-01T00:00:00Z" {
		t.Errorf("copilotTokenExpiresAt=%v", patch["copilotTokenExpiresAt"])
	}
}

// TestCredentialsForRefresh_BuildsAccessTokenAndPSD reads accessToken into the
// Credentials.AccessToken and dumps everything else into PSD.
func TestCredentialsForRefresh_BuildsAccessTokenAndPSD(t *testing.T) {
	data := map[string]any{
		"accessToken": "at",
		"refreshToken": "rt",
		"clientId":    "cid",
	}
	creds := CredentialsForRefresh(data)
	if creds.AccessToken != "at" {
		t.Errorf("AccessToken=%q want at", creds.AccessToken)
	}
	if rt, _ := creds.ProviderSpecificData["refreshToken"].(string); rt != "rt" {
		t.Errorf("PSD refreshToken=%v want rt", creds.ProviderSpecificData["refreshToken"])
	}
	if cid, _ := creds.ProviderSpecificData["clientId"].(string); cid != "cid" {
		t.Errorf("PSD clientId=%v want cid", creds.ProviderSpecificData["clientId"])
	}
}

// TestCredentialsForRefresh_NilData returns empty credentials with non-nil PSD.
func TestCredentialsForRefresh_NilData(t *testing.T) {
	creds := CredentialsForRefresh(nil)
	if creds.AccessToken != "" || creds.ProviderSpecificData == nil {
		t.Fatalf("nil data must yield empty creds with non-nil PSD, got %+v", creds)
	}
}

// Compile-time: keep provider referenced.
var _ = provider.Credentials{}