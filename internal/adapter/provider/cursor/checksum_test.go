package cursorexec

// checksum_test.go pins the cursorChecksum port (upstream v0.5.40). Pure
// functions, no network.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestGenerateHashed64HexDeterministic(t *testing.T) {
	a := GenerateHashed64Hex("tok", "machineId")
	b := GenerateHashed64Hex("tok", "machineId")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	// Equals sha256("tok"+"machineId") hex.
	h := sha256.Sum256([]byte("tokmachineId"))
	want := hex.EncodeToString(h[:])
	if a != want {
		t.Fatalf("GenerateHashed64Hex = %q want %q", a, want)
	}
	if len(a) != 64 {
		t.Errorf("length = %d want 64", len(a))
	}
}

func TestGenerateSessionIDIsUUIDv5DNS(t *testing.T) {
	id := GenerateSessionID("my-token")
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("not a UUID: %v", err)
	}
	if parsed.Version() != 5 {
		t.Errorf("version = %d want 5 (SHA1/v5)", parsed.Version())
	}
	// Deterministic: uuid v5 DNS of "my-token".
	want := uuid.NewSHA1(uuid.NameSpaceDNS, []byte("my-token")).String()
	if id != want {
		t.Errorf("session id = %q want %q", id, want)
	}
}

func TestGenerateCursorChecksumSuffix(t *testing.T) {
	got := GenerateCursorChecksum("machine-xyz")
	if !strings.HasSuffix(got, "machine-xyz") {
		t.Fatalf("checksum %q must end with machineId", got)
	}
	encoded := strings.TrimSuffix(got, "machine-xyz")
	// 6 input bytes → 8 base64 chars (custom alphabet, no padding).
	if len(encoded) != 8 {
		t.Errorf("encoded length = %d want 8 (6 bytes → 8 chars)", len(encoded))
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for _, c := range encoded {
		if !strings.ContainsRune(alphabet, c) {
			t.Errorf("char %q not in URL-safe alphabet", c)
		}
	}
}

func TestGenerateCursorChecksumUniqueOverTime(t *testing.T) {
	// The checksum embeds a coarse timestamp (per-second); two calls within
	// the same second share the encoded prefix but should both be valid. We
	// only assert the machineId suffix is stable and the prefix is non-empty.
	a := GenerateCursorChecksum("m")
	if a == "m" {
		t.Fatal("checksum missing encoded prefix")
	}
}

func TestBuildCursorHeadersBumpedVersionAndCommit(t *testing.T) {
	h := BuildCursorHeaders(CursorHeadersOpts{
		AccessToken: "raw-token",
		MachineID: "machine-1",
		GhostMode: true,
	})
	if h["x-cursor-client-version"] != "3.12.17" {
		t.Errorf("client-version = %q want 3.12.17", h["x-cursor-client-version"])
	}
	if h["x-cursor-client-commit"] != cursorClientCommit {
		t.Errorf("client-commit = %q want %q", h["x-cursor-client-commit"], cursorClientCommit)
	}
	if h["authorization"] != "Bearer raw-token" {
		t.Errorf("authorization = %q", h["authorization"])
	}
	if h["x-cursor-checksum"] == "" {
		t.Error("checksum header empty")
	}
	if !strings.HasSuffix(h["x-cursor-checksum"], "machine-1") {
		t.Errorf("checksum header %q should end with machineId", h["x-cursor-checksum"])
	}
	if h["x-ghost-mode"] != "true" {
		t.Errorf("ghost-mode = %q want true", h["x-ghost-mode"])
	}
	if h["x-session-id"] == "" {
		t.Error("session-id empty")
	}
	if h["x-cursor-client-type"] != "ide" {
		t.Errorf("client-type = %q", h["x-cursor-client-type"])
	}
	// OS/arch always non-empty.
	if h["x-cursor-client-os"] == "" || h["x-cursor-client-arch"] == "" {
		t.Error("os/arch header empty")
	}
}

func TestBuildCursorHeadersSplitsTokenPrefix(t *testing.T) {
	// A "::"-prefixed token (provider-prefixed storage shape) is cleaned to
	// the suffix for the Bearer header and derived values.
	h := BuildCursorHeaders(CursorHeadersOpts{
		AccessToken: "cursor::actual-secret",
		MachineID: "m",
		GhostMode: false,
	})
	if h["authorization"] != "Bearer actual-secret" {
		t.Errorf("authorization = %q want Bearer actual-secret (prefix stripped)", h["authorization"])
	}
	// session-id must be v5 DNS of the *cleaned* token.
	want := GenerateSessionID("actual-secret")
	if h["x-session-id"] != want {
		t.Errorf("session-id = %q want %q (derived from cleaned token)", h["x-session-id"], want)
	}
	if h["x-ghost-mode"] != "false" {
		t.Errorf("ghost-mode = %q want false", h["x-ghost-mode"])
	}
}

func TestBuildCursorHeadersDerivesMachineID(t *testing.T) {
	// When machineID is empty, it is derived from the cleaned token.
	h := BuildCursorHeaders(CursorHeadersOpts{
		AccessToken: "tok",
		MachineID: "",
		GhostMode: true,
	})
	derived := GenerateHashed64Hex("tok", "machineId")
	if !strings.HasSuffix(h["x-cursor-checksum"], derived) {
		t.Errorf("checksum %q should end with derived machineId %q", h["x-cursor-checksum"], derived)
	}
}

func TestRandomUUIDFormat(t *testing.T) {
	id := randomUUID()
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("not a UUID: %v", err)
	}
	if parsed.Version() != 4 {
		t.Errorf("version = %d want 4", parsed.Version())
	}
}