package proxychat

import (
	"strings"
	"testing"
)

// TestExtractHTMLTitle covers the <title> extractor used by the cb0135b6
// non-SSE crash guard.
func TestExtractHTMLTitle(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"simple title", `<html><head><title>502 Bad Gateway</title></head><body>...</body></html>`, "502 Bad Gateway"},
		{"uppercase tag", `<TITLE>Cloudflare Error</TITLE>`, "Cloudflare Error"},
		{"title with attrs", `<html><head><title id="x">Error 503</title>`, "Error 503"},
		{"no title", `<html><body>no title here</body></html>`, ""},
		{"title spanning newlines", "<title>Just\na moment...</title>", "Just\na moment..."},
		{"empty title", `<title></title>`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractHTMLTitle(c.body); got != c.want {
				t.Errorf("extractHTMLTitle = %q, want %q", got, c.want)
			}
		})
	}
}

// TestStripHTMLTags verifies coarse tag stripping + whitespace collapse.
func TestStripHTMLTags(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"<html><body>Hello</body></html>", "Hello"},
		{"<p>One</p><p>Two</p>", "One Two"},
		{"line1\nline2\r\nline3", "line1 line2 line3"},
		{"no tags at all", "no tags at all"},
		{"<div>nested <span>text</span></div>", "nested text"},
	}
	for _, c := range cases {
		if got := stripHTMLTags(c.in); got != c.want {
			t.Errorf("stripHTMLTags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeNonSSEBody ports the cb0135b6 behavior: extract a short sanitized
// message from a non-SSE upstream body, never leaking raw HTML to the client.
func TestSanitizeNonSSEBody(t *testing.T) {
	// Cloudflare-style HTML error page → title extracted, clamped, tags stripped.
	cf := `<!DOCTYPE html><html><head><title>Just a moment...</title></head>
<body><div>Checking your browser before accessing the site.</div></body></html>`
	got := sanitizeNonSSEBody(cf, "text/html; charset=utf-8")
	if got != "Just a moment..." {
		t.Fatalf("sanitize(cloudflare) = %q, want %q", got, "Just a moment...")
	}

	// Long title is clamped to 160 runes.
	long := "<title>" + strings.Repeat("x", 300) + "</title>"
	got = sanitizeNonSSEBody(long, "text/html")
	if len([]rune(got)) > 160 {
		t.Fatalf("sanitize(long title) = %d runes, want <= 160", len([]rune(got)))
	}
	if len([]rune(got)) != 160 {
		t.Fatalf("sanitize(long title) = %d runes, want exactly 160 (clamped)", len([]rune(got)))
	}

	// Short body without title → use stripped body.
	got = sanitizeNonSSEBody("rate limit exceeded", "text/plain")
	if got != "rate limit exceeded" {
		t.Fatalf("sanitize(plain) = %q, want %q", got, "rate limit exceeded")
	}

	// Long body without title → generic content-type notice.
	got = sanitizeNonSSEBody(strings.Repeat("y", 500), "text/plain")
	want := "Upstream returned non-SSE response (text/plain)"
	if got != want {
		t.Fatalf("sanitize(long plain) = %q, want %q", got, want)
	}

	// Body with tags inside title is stripped.
	got = sanitizeNonSSEBody("<title>Error: <b>502</b> Bad</title>", "text/html")
	if got != "Error: 502 Bad" {
		t.Fatalf("sanitize(tagged title) = %q, want %q", got, "Error: 502 Bad")
	}

	// Empty body → generic notice.
	got = sanitizeNonSSEBody("", "text/html")
	if got != "Upstream returned non-SSE response (text/html)" {
		t.Fatalf("sanitize(empty) = %q, want generic notice", got)
	}
}