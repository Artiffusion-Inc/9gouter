package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// mediaproviders_voices_fetchers.go implements the per-provider voice-list
// fetchers. Each mirrors the legacy JS route:
//   - elevenlabs: GET https://api.elevenlabs.io/v1/voices (xi-api-key), grouped
//     by labels.language (primary) + every verified_languages[].language.
//   - deepgram:   GET https://api.deepgram.com/v1/models (Authorization: Token),
//     each tts model is a voice, language from model.languages or the
//     canonical_name suffix.
//   - inworld:    GET https://api.inworld.ai/tts/v1/voices (Authorization: Basic),
//     grouped by each voice's languages[].
//   - minimax:    POST https://api.minimax.io/v1/get_voice (Bearer) or
//     https://api.minimaxi.com/v1/get_voice for minimax-cn; body {voice_type};
//     voices grouped by inferred language prefix.

// ── ElevenLabs ──────────────────────────────────────────────────────────────

// elevenLabsVoicesURL is a var so tests can point it at an httptest.Server.
var elevenLabsVoicesURL = "https://api.elevenlabs.io/v1/voices"

func fetchElevenLabsVoices(ctx context.Context, pools *repo.ProxyPoolRepo, opts proxy.Options, conn *settings.ProviderConnection, apiKey string) (*voiceGroups, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, elevenLabsVoicesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", apiKey)
	req.Header.Set("Accept", "application/json")

	body, status, err := doVoicesRequest(ctx, pools, opts, conn, req)
	if err != nil {
		return nil, fmt.Errorf("ElevenLabs voices fetch failed: %v", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("ElevenLabs voices fetch failed: %d", status)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("ElevenLabs voices fetch failed: invalid response")
	}
	voices, _ := raw["voices"].([]any)
	groups := newVoiceGroups()
	for _, e := range voices {
		v, ok := e.(map[string]any)
		if !ok {
			continue
		}
		voiceID, _ := v["voice_id"].(string)
		if voiceID == "" {
			continue
		}
		name, _ := v["name"].(string)
		if name == "" {
			name = voiceID
		}
		category, _ := v["category"].(string)
		isOwner, _ := v["is_owner"].(bool)
		voice := map[string]any{
			"id":                 voiceID,
			"name":               name,
			"gender":             nestedStr(v, "labels", "gender"),
			"free_users_allowed": category == "premade" || isOwner,
		}

		labels, _ := v["labels"].(map[string]any)
		primary := "en"
		if labels != nil {
			if l, ok := labels["language"].(string); ok && l != "" {
				primary = l
			}
		}
		groups.addVoice(primary, withLang(voice, primary))

		for _, ve := range verifiedLangs(v) {
			if ve != primary && ve != "" {
				groups.addVoice(ve, withLang(voice, ve))
			}
		}
	}
	if len(groups.byLang) == 0 {
		return nil, nil
	}
	return groups, nil
}

// verifiedLangs returns the languages array of an ElevenLabs voice
// (verified_languages[].language).
func verifiedLangs(v map[string]any) []string {
	raw, _ := v["verified_languages"].([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			if l, ok := m["language"].(string); ok && l != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

// ── Deepgram ────────────────────────────────────────────────────────────────

// deepgramModelsURL is a var so tests can point it at an httptest.Server.
var deepgramModelsURL = "https://api.deepgram.com/v1/models"

func fetchDeepgramVoices(ctx context.Context, pools *repo.ProxyPoolRepo, opts proxy.Options, conn *settings.ProviderConnection, apiKey string) (*voiceGroups, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, deepgramModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+apiKey)
	req.Header.Set("Accept", "application/json")

	body, status, err := doVoicesRequest(ctx, pools, opts, conn, req)
	if err != nil {
		return nil, fmt.Errorf("Deepgram API: %v", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("Deepgram API %d: %s", status, string(body))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("Deepgram API: invalid response")
	}
	ttsModels, _ := raw["tts"].([]any)
	groups := newVoiceGroups()
	for _, e := range ttsModels {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		voiceID, _ := m["canonical_name"].(string)
		if voiceID == "" {
			voiceID, _ = m["name"].(string)
		}
		if voiceID == "" {
			continue
		}
		display, _ := m["name"].(string)
		if display == "" {
			display = voiceID
		}
		voice := map[string]any{
			"id":     voiceID,
			"name":   display,
			"gender": deepgramGender(m),
		}
		for _, code := range deepgramLangs(m, voiceID) {
			groups.addVoice(code, withLang(voice, code))
		}
	}
	if len(groups.byLang) == 0 {
		return nil, nil
	}
	return groups, nil
}

func deepgramLangs(m map[string]any, voiceID string) []string {
	if arr, ok := m["languages"].([]any); ok && len(arr) > 0 {
		out := make([]string, 0, len(arr))
		for _, l := range arr {
			if s, ok := l.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// Fall back to the canonical_name suffix (e.g. aura-2-thalia-en → en).
	if idx := strings.LastIndex(voiceID, "-"); idx >= 0 {
		if suf := voiceID[idx+1:]; suf != "" {
			return []string{suf}
		}
	}
	return []string{"en"}
}

func deepgramGender(m map[string]any) string {
	meta, _ := m["metadata"].(map[string]any)
	if meta == nil {
		return ""
	}
	tags, _ := meta["tags"].([]any)
	for _, t := range tags {
		if s, ok := t.(string); ok && (s == "masculine" || s == "feminine") {
			return s
		}
	}
	return ""
}

// ── Inworld ─────────────────────────────────────────────────────────────────

// inworldVoicesURL is a var so tests can point it at an httptest.Server.
var inworldVoicesURL = "https://api.inworld.ai/tts/v1/voices"

func fetchInworldVoices(ctx context.Context, pools *repo.ProxyPoolRepo, opts proxy.Options, conn *settings.ProviderConnection, apiKey string) (*voiceGroups, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, inworldVoicesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Basic "+apiKey)
	req.Header.Set("Accept", "application/json")

	body, status, err := doVoicesRequest(ctx, pools, opts, conn, req)
	if err != nil {
		return nil, fmt.Errorf("Inworld API: %v", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("Inworld API %d: %s", status, string(body))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("Inworld API: invalid response")
	}
	voices, _ := raw["voices"].([]any)
	groups := newVoiceGroups()
	for _, e := range voices {
		v, ok := e.(map[string]any)
		if !ok {
			continue
		}
		voiceID, _ := v["voiceId"].(string)
		if voiceID == "" {
			continue
		}
		name, _ := v["displayName"].(string)
		if name == "" {
			name = voiceID
		}
		voice := map[string]any{
			"id":     voiceID,
			"name":   name,
			"gender": strFieldAny(v, "gender"),
		}
		langs, _ := v["languages"].([]any)
		codes := make([]string, 0, len(langs))
		for _, l := range langs {
			if s, ok := l.(string); ok && s != "" {
				codes = append(codes, s)
			}
		}
		if len(codes) == 0 {
			codes = []string{"en"}
		}
		for _, code := range codes {
			groups.addVoice(code, withLang(voice, code))
		}
	}
	if len(groups.byLang) == 0 {
		return nil, nil
	}
	return groups, nil
}

// ── MiniMax ─────────────────────────────────────────────────────────────────

// minimaxVoiceEndpoints maps provider → voice-list URL. Already a var so tests
// can swap entries to point at an httptest.Server.
var minimaxVoiceEndpoints = map[string]string{
	"minimax":    "https://api.minimax.io/v1/get_voice",
	"minimax-cn": "https://api.minimaxi.com/v1/get_voice",
}

var minimaxVoiceGroups = []struct{ key, label string }{
	{"system_voice", "System"},
	{"voice_cloning", "Cloned"},
	{"voice_generation", "Generated"},
	{"music_generation", "Music"},
}

func fetchMiniMaxVoices(ctx context.Context, pools *repo.ProxyPoolRepo, opts proxy.Options, conn *settings.ProviderConnection, provider, apiKey, voiceType string) (*voiceGroups, error) {
	endpoint := minimaxVoiceEndpoints[provider]
	if endpoint == "" {
		endpoint = minimaxVoiceEndpoints["minimax"]
	}
	payload, _ := json.Marshal(map[string]any{"voice_type": voiceType})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	body, status, err := doVoicesRequest(ctx, pools, opts, conn, req)
	if err != nil {
		return nil, fmt.Errorf("MiniMax API: %v", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("MiniMax API %d: %s", status, string(body))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("MiniMax API: invalid response")
	}
	// base_resp.status_code != 0 → upstream error (mirrors the JS handler).
	if code := minimaxStatusCode(raw); code != 0 {
		msg := minimaxStatusMessage(raw)
		if msg == "" {
			msg = "MiniMax voice API error"
		}
		return nil, errors.New(msg)
	}
	groups := newVoiceGroups()
	for _, g := range minimaxVoiceGroups {
		voices, _ := raw[g.key].([]any)
		for _, e := range voices {
			item, ok := e.(map[string]any)
			if !ok {
				continue
			}
			voiceID := strFieldAny(item, "voice_id", "voiceId")
			if voiceID == "" {
				continue
			}
			voiceName := strFieldAny(item, "voice_name", "voiceName")
			if voiceName == "" {
				voiceName = voiceID
			}
			lang := "Custom"
			if g.key == "system_voice" {
				lang = minimaxInferLang(voiceID)
			}
			display := voiceName
			if g.key != "system_voice" {
				display = voiceName + " · " + g.label
			}
			groups.addVoice(lang, map[string]any{
				"id":       voiceID,
				"name":     display,
				"lang":     lang,
				"category": g.key,
			})
		}
	}
	if len(groups.byLang) == 0 {
		return nil, nil
	}
	// Sort each language's voices by name (JS does lang.voices.sort by name).
	for _, grp := range groups.byLang {
		voices := grp["voices"].([]any)
		sort.Slice(voices, func(i, j int) bool {
			an, _ := voices[i].(map[string]any)
			bn, _ := voices[j].(map[string]any)
			return anyStr(an, "name") < anyStr(bn, "name")
		})
		grp["voices"] = voices
	}
	return groups, nil
}

func minimaxInferLang(voiceID string) string {
	if !strings.Contains(voiceID, "_") {
		return "Custom"
	}
	parts := strings.SplitN(voiceID, "_", 2)
	if parts[0] == "" {
		return "Custom"
	}
	return parts[0]
}

func minimaxStatusCode(raw map[string]any) int {
	br, _ := raw["base_resp"].(map[string]any)
	if br == nil {
		br, _ = raw["baseResp"].(map[string]any)
	}
	return int(anyNum(br, "status_code", "statusCode"))
}

func minimaxStatusMessage(raw map[string]any) string {
	br, _ := raw["base_resp"].(map[string]any)
	if br == nil {
		br, _ = raw["baseResp"].(map[string]any)
	}
	return strFieldAny(br, "status_msg", "statusMsg")
}

// ── shared helpers ──────────────────────────────────────────────────────────

func withLang(voice map[string]any, lang string) map[string]any {
	out := make(map[string]any, len(voice)+1)
	for k, v := range voice {
		out[k] = v
	}
	out["lang"] = lang
	return out
}

func nestedStr(v map[string]any, parent, child string) string {
	m, _ := v[parent].(map[string]any)
	if m == nil {
		return ""
	}
	if s, ok := m[child].(string); ok {
		return s
	}
	return ""
}

// strFieldAny returns the first present string field among keys (snake/camel
// tolerance), mirroring the quotafetch.strField helper.
func strFieldAny(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// anyStr returns the string value of m[key], "" otherwise.
func anyStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// anyNum returns the numeric value of the first present field among keys,
// parsing string values via strconv (mirrors the quotafetch.toFinite numerics).
func anyNum(m map[string]any, keys ...string) float64 {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return n
			case int:
				return float64(n)
			case int64:
				return float64(n)
			case string:
				if f, err := strconv.ParseFloat(n, 64); err == nil {
					return f
				}
			}
		}
	}
	return 0
}
