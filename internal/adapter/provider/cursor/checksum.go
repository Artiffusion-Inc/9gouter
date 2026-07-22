// Package cursorexec — cursorChecksum port.
//
// checksum.go ports open-sse/utils/cursorChecksum.js (upstream v0.5.40): the
// x-cursor-checksum header generator (Jyh cipher) and BuildCursorHeaders, the
// full Cursor request header set the AgentService gateway identifies as a
// current Cursor IDE release. This is shared by the live model resolver
// (GetUsableModels) and the executeAgent executor.
package cursorexec

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

// cursorClientVersion is the x-cursor-client-version sent to the AgentService
// gateway. Bumped from 3.1.0 to 3.12.17 by upstream 6994cd1f so the gateway
// stops returning HTTP 429 "Update Required" for the retired ChatService
// headers.
const cursorClientVersion = "3.12.17"

// cursorClientCommit is the x-cursor-client-commit header — a recent Cursor
// IDE release git sha the gateway fingerprints as a current client.
const cursorClientCommit = "0fb762053c34788bb7760d5673f8a6d4c8589d50"

// GenerateHashed64Hex returns a SHA-256 hex digest of input+salt, matching the
// JS GenerateHashed64Hex.
func GenerateHashed64Hex(input, salt string) string {
	h := sha256.Sum256([]byte(input + salt))
	return hex.EncodeToString(h[:])
}

// GenerateSessionID returns a UUID v5 (SHA1, DNS namespace) of the auth token,
// matching the JS generateSessionId (uuid v5 DNS).
func GenerateSessionID(authToken string) string {
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(authToken)).String()
}

// GenerateCursorChecksum implements the Jyh cipher obfuscation used by Cursor:
//
//  1. timestamp = floor(now / 1e6) → 6-byte big-endian array
//  2. XOR each byte with a rolling key (starts 165), then key = result byte
//  3. URL-safe base64-encode (no padding), custom alphabet
//  4. return base64 + machineId
//
// The custom URL-safe alphabet matches the JS `alphabet` constant exactly.
func GenerateCursorChecksum(machineID string) string {
	ts := time.Now().UnixMilli() / 1_000_000
	ba := [6]byte{
		byte(ts >> 40),
		byte(ts >> 32),
		byte(ts >> 24),
		byte(ts >> 16),
		byte(ts >> 8),
		byte(ts),
	}
	t := byte(165)
	for i := 0; i < len(ba); i++ {
		ba[i] = byte((int(ba[i]^t) + i%256)) & 0xFF
		t = ba[i]
	}

	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	encode := func(a, b, c byte, hasB, hasC bool) string {
		out := []byte{alphabet[a>>2], alphabet[((a&3)<<4)|(b>>4)]}
		if hasB {
			out = append(out, alphabet[((b&15)<<2)|(c>>6)])
		}
		if hasC {
			out = append(out, alphabet[c&63])
		}
		return string(out)
	}
	encoded := ""
	for i := 0; i < len(ba); i += 3 {
		a := ba[i]
		var b, c byte
		hasB := i+1 < len(ba)
		hasC := i+2 < len(ba)
		if hasB {
			b = ba[i+1]
		}
		if hasC {
			c = ba[i+2]
		}
		encoded += encode(a, b, c, hasB, hasC)
	}
	return encoded + machineID
}

// cursorOS detects the x-cursor-client-os value. Defaults to linux; the JS
// source keys off process.platform. The Go port keys off runtime.GOOS so a
// Windows/macOS-built binary reports the matching value, mirroring the IDE.
func cursorOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

// cursorArch detects the x-cursor-client-arch value (x64 / aarch64).
func cursorArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x64"
}

// CursorHeadersOpts controls BuildCursorHeaders behavior.
type CursorHeadersOpts struct {
	AccessToken string
	MachineID   string
	GhostMode   bool
}

// BuildCursorHeaders builds the full Cursor AgentService header set. Mirrors
// the JS BuildCursorHeaders. A "::"-prefixed token is split (Bearer tokens are
// stored with a provider prefix); a missing machineID is derived from the
// token. The base64 raw-URL encoding for x-amzn-trace-id mirrors
// crypto.randomUUID().
func BuildCursorHeaders(opts CursorHeadersOpts) map[string]string {
	cleanToken := opts.AccessToken
	if strings.Contains(cleanToken, "::") {
		cleanToken = strings.SplitN(cleanToken, "::", 2)[1]
	}
	machineID := opts.MachineID
	if machineID == "" {
		machineID = GenerateHashed64Hex(cleanToken, "machineId")
	}
	sessionID := GenerateSessionID(cleanToken)
	clientKey := GenerateHashed64Hex(cleanToken, "")
	checksum := GenerateCursorChecksum(machineID)
	ghost := "true"
	if !opts.GhostMode {
		ghost = "false"
	}

	return map[string]string{
		"authorization":                "Bearer " + cleanToken,
		"connect-accept-encoding":      "gzip",
		"connect-protocol-version":     "1",
		"content-type":                 "application/connect+proto",
		"user-agent":                   "connect-es/1.6.1",
		"x-amzn-trace-id":              "Root=" + randomUUID(),
		"x-client-key":                 clientKey,
		"x-cursor-checksum":            checksum,
		"x-cursor-client-version":      cursorClientVersion,
		"x-cursor-client-commit":       cursorClientCommit,
		"x-cursor-client-type":         "ide",
		"x-cursor-client-os":           cursorOS(),
		"x-cursor-client-arch":         cursorArch(),
		"x-cursor-client-device-type":  "desktop",
		"x-cursor-config-version":      randomUUID(),
		"x-cursor-timezone":            "UTC",
		"x-ghost-mode":                 ghost,
		"x-request-id":                 randomUUID(),
		"x-session-id":                 sessionID,
	}
}

// randomUUID returns a random v4 UUID string. crypto/rand is used so the
// generated x-request-id / x-amzn-trace-id / x-cursor-config-version values
// are fresh per request, mirroring crypto.randomUUID() in the JS source.
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	// Set version (4) and variant (RFC 4122) bits.
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	return formatUUID(b)
}

// formatUUID renders 16 bytes as a canonical 8-4-4-4-12 UUID string.
func formatUUID(b []byte) string {
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// base64URLNoPad is retained for parity with the JS URL-safe base64 path; the
// Jyh cipher uses a custom alphabet, but other Cursor checksum variants (not
// currently exercised) use standard URL-safe base64 without padding.
var _ = base64.URLEncoding