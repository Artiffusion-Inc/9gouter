package managedashboard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// fakeSettingsRepo implements SettingsService.Repo.
type fakeSettingsRepo struct {
	current  *settings.Settings
	updates  []json.RawMessage
	updateFn func(json.RawMessage) (*settings.Settings, error)
	getErr   error
}

func (r *fakeSettingsRepo) Get(ctx context.Context) (*settings.Settings, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.current == nil {
		return &settings.Settings{ID: 1, Data: json.RawMessage(`{}`)}, nil
	}
	cp := *r.current
	return &cp, nil
}

func (r *fakeSettingsRepo) Update(ctx context.Context, updates json.RawMessage) (*settings.Settings, error) {
	r.updates = append(r.updates, updates)
	if r.updateFn != nil {
		return r.updateFn(updates)
	}
	return &settings.Settings{ID: 1, Data: updates}, nil
}

func TestSettingsService_Get(t *testing.T) {
	t.Parallel()
	cur := &settings.Settings{ID: 7, Data: json.RawMessage(`{"a":1}`)}
	s := &SettingsService{Repo: &fakeSettingsRepo{current: cur}}

	got, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != 7 {
		t.Errorf("ID = %d, want 7", got.ID)
	}
}

func TestSettingsService_Get_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("db down")
	s := &SettingsService{Repo: &fakeSettingsRepo{getErr: sentinel}}
	if _, err := s.Get(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestSettingsService_Merge_StripsProtectedKeys(t *testing.T) {
	t.Parallel()
	repo := &fakeSettingsRepo{}
	s := &SettingsService{Repo: repo}

	body := []byte(`{"password":"hash","mitmSudoEncrypted":"x","oidcClientSecret":"s","keep":"me"}`)
	if _, err := s.Merge(context.Background(), body); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(repo.updates) != 1 {
		t.Fatalf("updates = %v", repo.updates)
	}
	var patch map[string]any
	if err := json.Unmarshal(repo.updates[0], &patch); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"password", "mitmSudoEncrypted", "oidcClientSecret"} {
		if _, ok := patch[k]; ok {
			t.Errorf("protected key %q survived merge", k)
		}
	}
	if v, _ := patch["keep"].(string); v != "me" {
		t.Errorf("keep = %v, want me", patch["keep"])
	}
}

func TestSettingsService_Merge_NewPassword_FlowsToPassword(t *testing.T) {
	t.Parallel()
	repo := &fakeSettingsRepo{}
	s := &SettingsService{Repo: repo}

	body := []byte(`{"newPassword":"secret","currentPassword":"old","other":"v"}`)
	if _, err := s.Merge(context.Background(), body); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var patch map[string]any
	_ = json.Unmarshal(repo.updates[0], &patch)
	if v, _ := patch["password"].(string); v != "secret" {
		t.Errorf("password = %q, want secret", v)
	}
	for _, k := range []string{"newPassword", "currentPassword"} {
		if _, ok := patch[k]; ok {
			t.Errorf("password transient key %q survived", k)
		}
	}
}

func TestSettingsService_Merge_NewPasswordBlank_DoesNotOverwrite(t *testing.T) {
	t.Parallel()
	repo := &fakeSettingsRepo{}
	s := &SettingsService{Repo: repo}

	// Blank newPassword ("") should not populate password (source uses != "" check).
	body := []byte(`{"newPassword":"","currentPassword":"old","flag":true}`)
	if _, err := s.Merge(context.Background(), body); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var patch map[string]any
	_ = json.Unmarshal(repo.updates[0], &patch)
	if _, ok := patch["password"]; ok {
		t.Errorf("password should not be set when newPassword is blank")
	}
	if _, ok := patch["flag"]; !ok {
		t.Errorf("flag should survive")
	}
}

func TestSettingsService_Merge_BlankOIDCSecret_Removed(t *testing.T) {
	t.Parallel()
	repo := &fakeSettingsRepo{}
	s := &SettingsService{Repo: repo}

	body := []byte(`{"oidcClientSecret":"   ","oidcIssuerUrl":"u"}`)
	if _, err := s.Merge(context.Background(), body); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var patch map[string]any
	_ = json.Unmarshal(repo.updates[0], &patch)
	if _, ok := patch["oidcClientSecret"]; ok {
		t.Errorf("blank oidcClientSecret should have been deleted")
	}
	if v, _ := patch["oidcIssuerUrl"].(string); v != "u" {
		t.Errorf("oidcIssuerUrl = %v, want u", patch["oidcIssuerUrl"])
	}
}

func TestSettingsService_Merge_EmptyBody_ReturnsCurrent(t *testing.T) {
	t.Parallel()
	repo := &fakeSettingsRepo{current: &settings.Settings{ID: 3, Data: json.RawMessage(`{"x":1}`)}}
	s := &SettingsService{Repo: repo}

	if _, err := s.Merge(context.Background(), nil); err != nil {
		t.Fatalf("Merge nil: %v", err)
	}
	if len(repo.updates) != 0 {
		t.Errorf("expected no Update calls, got %d", len(repo.updates))
	}
	// Empty (all-stripped) body also returns current.
	if _, err := s.Merge(context.Background(), []byte(`{"password":"x"}`)); err != nil {
		t.Fatalf("Merge all-stripped: %v", err)
	}
	if len(repo.updates) != 0 {
		t.Errorf("expected no Update calls when patch ends empty, got %d", len(repo.updates))
	}
}

func TestSettingsService_Merge_InvalidJSON(t *testing.T) {
	t.Parallel()
	repo := &fakeSettingsRepo{}
	s := &SettingsService{Repo: repo}
	if _, err := s.Merge(context.Background(), []byte(`{not json`)); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
	if len(repo.updates) != 0 {
		t.Errorf("expected no Update calls on parse failure, got %d", len(repo.updates))
	}
}

func TestSettingsService_Merge_UpdatePropagatesError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("update failed")
	repo := &fakeSettingsRepo{updateFn: func(json.RawMessage) (*settings.Settings, error) {
		return nil, sentinel
	}}
	s := &SettingsService{Repo: repo}
	if _, err := s.Merge(context.Background(), []byte(`{"a":1}`)); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestSettingsService_OidcConfigured(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"all present", `{"oidcIssuerUrl":"u","oidcClientId":"c","oidcClientSecret":"s"}`, true},
		{"missing secret", `{"oidcIssuerUrl":"u","oidcClientId":"c"}`, false},
		{"missing client id", `{"oidcIssuerUrl":"u","oidcClientSecret":"s"}`, false},
		{"missing issuer", `{"oidcClientId":"c","oidcClientSecret":"s"}`, false},
		{"empty values", `{"oidcIssuerUrl":"","oidcClientId":"c","oidcClientSecret":"s"}`, false},
		{"invalid json", `{not json`, false},
		{"empty body", ``, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &SettingsService{}
			if got := s.OidcConfigured(json.RawMessage(tc.data)); got != tc.want {
				t.Fatalf("OidcConfigured = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSettingsService_HasPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"has password", `{"password":"bcrypt$hash"}`, true},
		{"empty password", `{"password":""}`, false},
		{"no password field", `{"other":1}`, false},
		{"password non-string", `{"password":123}`, false},
		{"invalid json", `{not json`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &SettingsService{}
			if got := s.HasPassword(json.RawMessage(tc.data)); got != tc.want {
				t.Fatalf("HasPassword = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStringsTrim(t *testing.T) {
	t.Parallel()
	if got := stringsTrim("  hi  "); got != "hi" {
		t.Errorf("stringsTrim = %q, want hi", got)
	}
	if !strings.HasPrefix(stringsTrim("x"), "x") {
		t.Error("stringsTrim stripped non-space content")
	}
}