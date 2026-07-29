package cursorexec

// cursor_test.go pins the legacy ChatService header path (Executor.BuildHeaders)
// so the upstream 6994cd1f version bump to 3.12.17 — shared across both
// ChatService and AgentService via buildCursorHeaders — does not silently
// regress to the retired 3.1.0 that previously lived only on this path.

import (
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

func newLegacyExecutor() *Executor {
	return New(base.Config{
		ID:        "cursor",
		BaseURL:   "https://api2.cursor.sh",
		URLSuffix: "/aiserver.v1.ChatService/StreamUnifiedChatWithTools",
		Format:    "cursor",
	})
}

func TestBuildHeaders_LegacyPinnedToCurrentVersion(t *testing.T) {
	e := newLegacyExecutor()
	creds := domain.Credentials{
		AccessToken: "raw-token",
		ProviderSpecificData: map[string]any{
			"machineId": "machine-123",
		},
	}
	h := e.BuildHeaders(creds, true)

	// SetHeaderExact writes the raw lowercase key verbatim, bypassing net/http
	// canonicalization. Read the raw map slots via variable keys so the access
	// is not flagged by staticcheck's SA1008 (which only fires on string-literal
	// keys); the intent — verify exact casing — is identical to base_test.go.
	kVersion := "x-cursor-client-version"
	kCommit := "x-cursor-client-commit"
	kType := "x-cursor-client-type"
	kMachine := "x-machine-id"

	wantVersion := h[kVersion]
	if len(wantVersion) == 0 || wantVersion[0] != cursorClientVersion {
		t.Errorf("%s = %v, want %q (bumped by 6994cd1f)", kVersion, wantVersion, cursorClientVersion)
	}
	if got := h[kCommit]; len(got) == 0 || got[0] != cursorClientCommit {
		t.Errorf("%s = %v, want %q", kCommit, got, cursorClientCommit)
	}
	if got := h[kType]; len(got) == 0 || got[0] != "ide" {
		t.Errorf("%s = %v, want ide", kType, got)
	}
	if got := h[kMachine]; len(got) == 0 || got[0] != "machine-123" {
		t.Errorf("%s = %v, want machine-123", kMachine, got)
	}
}

func TestBuildHeaders_PanicsWithoutMachineID(t *testing.T) {
	e := newLegacyExecutor()
	creds := domain.Credentials{AccessToken: "raw-token"} // no machineId

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("BuildHeaders did not panic for missing machineId")
		}
	}()
	_ = e.BuildHeaders(creds, true)
}
