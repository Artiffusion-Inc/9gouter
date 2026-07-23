// claude_signature.go ports open-sse/utils/claudeSignature.js: validation of
// Claude thinking-block signatures. Combo mixes models, so non-Claude thinking
// signatures (Gemini, DeepSeek, …) leak into conversation history and Anthropic
// rejects the passthrough. isValidClaudeSignature lets normalizeClaudePassthrough
// drop foreign-signed thinking blocks and re-insert a valid placeholder.
//
// Two accepted forms (after stripping a "cachehash#…" prefix):
//   - E-form: single-layer base64; decoded[0] == 0x12 (Claude marker).
//   - R-form: double-layer base64; outer decoded[0] == 'E' (0x45), then the
//     outer payload is base64-decoded again and its decoded[0] == 0x12.
//
// Anything else (missing signature, foreign prefix, decode failure, oversized)
// is invalid → the block is dropped.
package claude

import (
	"encoding/base64"
	"strings"
)

const (
	maxClaudeSignatureLen = 32 * 1024 * 1024 // 32 MiB, matches JS
	claudeSignatureMarker = 0x12
	claudeSignatureEForm  = "E"
	claudeSignatureRForm  = "R"
	claudeSignatureOuterE = 0x45 // 'E'
)

// stripCachePrefix removes a leading "cachehash#" segment: everything up to and
// including the first '#' is dropped, mirroring stripCachePrefix in JS.
func stripCachePrefix(rawSignature string) string {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" {
		return ""
	}
	if idx := strings.Index(sig, "#"); idx >= 0 {
		return strings.TrimSpace(sig[idx+1:])
	}
	return sig
}

// hasClaudeSignaturePrefix reports whether rawSignature, after cache-prefix
// stripping, begins with 'E' or 'R' — the only two accepted signature forms.
func hasClaudeSignaturePrefix(rawSignature string) bool {
	sig := stripCachePrefix(rawSignature)
	return len(sig) > 0 && (sig[0] == claudeSignatureEForm[0] || sig[0] == claudeSignatureRForm[0])
}

// isValidClaudeSignature validates a Claude thinking-block signature (strict-ish:
// base64 layers + Claude marker byte). Returns false on any decode error or
// unknown form, never panics.
func isValidClaudeSignature(rawSignature string) bool {
	sig := stripCachePrefix(rawSignature)
	if sig == "" || len(sig) > maxClaudeSignatureLen {
		return false
	}
	switch sig[0] {
	case claudeSignatureEForm[0]:
		decoded, err := base64.StdEncoding.DecodeString(sig)
		if err != nil || len(decoded) == 0 {
			return false
		}
		return decoded[0] == claudeSignatureMarker
	case claudeSignatureRForm[0]:
		outer, err := base64.StdEncoding.DecodeString(sig)
		if err != nil || len(outer) == 0 || outer[0] != claudeSignatureOuterE {
			return false
		}
		inner, err := base64.StdEncoding.DecodeString(string(outer))
		if err != nil || len(inner) == 0 {
			return false
		}
		return inner[0] == claudeSignatureMarker
	}
	return false
}
