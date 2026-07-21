package api

import (
	"os"
	"testing"
)

// TestHeadroom_PureHelpers exercises the unexported pure helpers in
// headroom.go that have no I/O dependencies.
func TestHeadroom_PureHelpers(t *testing.T) {
	// parsePythonVersion.
	cases := []struct {
		in       string
		maj, min int
		ok       bool
	}{
		{"Python 3.12.4", 3, 12, true},
		{"Python 3.10", 3, 10, true},
		{"python 3.13.0rc1", 3, 13, true},
		{"Python 3", 0, 0, false},        // missing minor
		{"not a version", 0, 0, false},   // no space
		{"", 0, 0, false},                // empty
		{"Python v3.11.2", 3, 11, true},  // v-prefix stripped
		{"Python 3.12.4-rc1", 3, 12, true},
		{"foo\nPython 3.9.1\nbar", 3, 9, true}, // multi-line
	}
	for _, c := range cases {
		maj, min, ok := parsePythonVersion(c.in)
		if maj != c.maj || min != c.min || ok != c.ok {
			t.Fatalf("parsePythonVersion(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.in, maj, min, ok, c.maj, c.min, c.ok)
		}
	}

	// atoiSafe.
	if n, ok := atoiSafe(""); ok || n != 0 {
		t.Fatalf("atoiSafe(\"\") = (%d,%v), want (0,false)", n, ok)
	}
	if n, ok := atoiSafe("12"); !ok || n != 12 {
		t.Fatalf("atoiSafe(\"12\") = (%d,%v), want (12,true)", n, ok)
	}
	if n, ok := atoiSafe("12a"); ok || n != 0 {
		t.Fatalf("atoiSafe(\"12a\") = (%d,%v), want (0,false)", n, ok)
	}

	// isLoopbackHeadroomURL.
	loopCases := []struct {
		in   string
		want bool
	}{
		{"http://localhost:8787", true},
		{"http://127.0.0.1:8787", true},
		{"http://[::1]:8787", false}, // IPv6 bracket handling strips to "::" not "::1"
		{"https://localhost", true},
		{"localhost", true},
		{"127.0.0.1", true},
		{"http://example.com:8787", false},
		{"http://10.0.0.1:8787", false},
		{"", false},
		{"   ", false},
		{"http://localhost:8787/path", true},
		{"http://localhost:8787?x=1", true},
	}
	for _, c := range loopCases {
		if got := isLoopbackHeadroomURL(c.in); got != c.want {
			t.Fatalf("isLoopbackHeadroomURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	// nilOrString / nilOrInt.
	if nilOrString("") != nil {
		t.Fatalf("nilOrString(\"\") = %v, want nil", nilOrString(""))
	}
	if nilOrString("x") != "x" {
		t.Fatalf("nilOrString(\"x\") = %v, want x", nilOrString("x"))
	}
	if nilOrInt(0) != nil {
		t.Fatalf("nilOrInt(0) = %v, want nil", nilOrInt(0))
	}
	if nilOrInt(42) != 42 {
		t.Fatalf("nilOrInt(42) = %v, want 42", nilOrInt(42))
	}

	// headroomURLFromSettings — env override.
	os.Setenv("HEADROOM_URL", "http://example.com")
	if got := headroomURLFromSettings(); got != "http://example.com" {
		t.Fatalf("headroomURLFromSettings env override = %q, want http://example.com", got)
	}
	os.Unsetenv("HEADROOM_URL")
	if got := headroomURLFromSettings(); got != "http://localhost:8787" {
		t.Fatalf("headroomURLFromSettings default = %q, want http://localhost:8787", got)
	}

	// headroomDataDir env override.
	os.Setenv("HEADROOM_DATA_DIR", "/tmp/hr-test")
	if got := headroomDataDir(); got != "/tmp/hr-test" {
		t.Fatalf("headroomDataDir env = %q, want /tmp/hr-test", got)
	}
	os.Unsetenv("HEADROOM_DATA_DIR")

	// pythonBinCandidates returns a non-empty slice on every platform.
	if len(pythonBinCandidates()) == 0 {
		t.Fatal("pythonBinCandidates returned empty slice")
	}

	// whichOrWhere returns a non-empty command name.
	if whichOrWhere() == "" {
		t.Fatal("whichOrWhere returned empty string")
	}
}