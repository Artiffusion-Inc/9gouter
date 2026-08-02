package tokenrefresh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver"
)

func generateTestSAKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	pemKey = strings.ReplaceAll(pemKey, "\n", "\\n")
	sa := serviceAccountJSON{
		Type:         "service_account",
		ProjectID:    "test-project",
		PrivateKeyID: "key123",
		PrivateKey:   pemKey,
		ClientEmail:  "test@test-project.iam.gserviceaccount.com",
		ClientID:     "123456789",
		TokenURI:     "https://oauth2.googleapis.com/token",
	}
	raw, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseVertexSAJSON_Valid(t *testing.T) {
	saJSON := generateTestSAKey(t)
	sa, err := parseVertexSAJSON(saJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sa == nil {
		t.Fatal("expected non-nil SA")
	}
	if sa.ClientEmail != "test@test-project.iam.gserviceaccount.com" {
		t.Errorf("email = %q", sa.ClientEmail)
	}
}

func TestParseVertexSAJSON_NotSA(t *testing.T) {
	sa, err := parseVertexSAJSON(`{"type":"authorized_user"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sa != nil {
		t.Fatal("expected nil for non-SA")
	}
}

func TestParseVertexSAJSON_Empty(t *testing.T) {
	sa, err := parseVertexSAJSON("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sa != nil {
		t.Fatal("expected nil for empty input")
	}
}

func TestVertexRefresh_NoAPIKey(t *testing.T) {
	r := NewVertexRefresher()
	result, err := r.Refresh(context.Background(), "", map[string]any{}, resolver.ProxyOptions{}, resolver.NopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatal("expected nil result with no apiKey")
	}
}

func TestVertexRefresh_NotJSON(t *testing.T) {
	r := NewVertexRefresher()
	psd := map[string]any{"apiKey": "not-json"}
	result, _ := r.Refresh(context.Background(), "", psd, resolver.ProxyOptions{}, resolver.NopLogger())
	if result != nil {
		t.Fatal("expected nil for non-JSON")
	}
}

func TestParsePKCS8PrivateKey_Valid(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	parsed, err := parsePKCS8PrivateKey(pemKey)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if parsed == nil {
		t.Fatal("expected non-nil key")
	}
}

func TestParsePKCS8PrivateKey_EscapedNewlines(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	escaped := strings.ReplaceAll(pemKey, "\n", "\\n")
	parsed, err := parsePKCS8PrivateKey(escaped)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if parsed == nil {
		t.Fatal("expected non-nil key")
	}
}
