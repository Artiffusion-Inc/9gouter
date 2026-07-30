package api

// grokbuildconfig.go ports src/lib/grokBuildConfig.js (decolua/9router) into
// the Go dashboard backend. It is a regex-based TOML *editor*, not a full
// parser: it reads/writes only the [model.<slot>], [models], and
// [subagents.models] sections the Grok Build config uses, preserving every
// other line verbatim. This mirrors the JS regex-forregex so a config the
// dashboard wrote on one backend round-trips identically on the other.
//
// The slot name is "9gouter" (our rename of the upstream "9router" slot); the
// frontend GrokBuildToolCard.js sends MODEL_SLOT = "9gouter", api_key default
// "sk_9gouter", and name "9Gouter", and the GET response reports has9Gouter —
// so the Go port keeps those renames rather than the upstream "9router" /
// "sk_9router" / "9Router" / has9Router literals.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// grokSectionHeaderRe returns a regex matching the exact "[<section>]" header
// line at the start of a line (multiline). Used to locate a section; the body
// is then carved out by grokFindSection without a lookahead.
func grokSectionHeaderRe(section string) *regexp.Regexp {
	pat := `(?m)^\[` + escapeRegExp(section) + `\][ \t]*\r?\n`
	return regexp.MustCompile(pat)
}

// grokSectionLoc finds a section, returning [sectionStart, bodyStart, sectionEnd]
// byte indices:
//
//   - toml[sectionStart:bodyStart]   is the "[section]\n" header
//   - toml[bodyStart:sectionEnd]     is the section body (lines until the next
//     line that starts with "[" or end-of-string)
//
// Returns nil when the section is absent. This replaces the JS sectionRegExp's
// `(?!\\[)` negative-lookahead body capture, which Go's RE2 regexp cannot
// express; the body is carved out by scanning line-by-line instead.
func grokSectionLoc(toml, section string) []int {
	hre := grokSectionHeaderRe(section)
	hloc := hre.FindStringSubmatchIndex(toml)
	if hloc == nil {
		return nil
	}
	sectionStart, bodyStart := hloc[0], hloc[1]
	sectionEnd := len(toml)
	rest := toml[bodyStart:]
	for i := 0; i < len(rest); {
		if rest[i] == '[' {
			sectionEnd = bodyStart + i
			break
		}
		nl := strings.IndexByte(rest[i:], '\n')
		if nl < 0 {
			break
		}
		i += nl + 1
	}
	return []int{sectionStart, bodyStart, sectionEnd}
}

const (
	grokMainModelSlot   = "9gouter"
	grokBuiltinDefault  = "grok-build"
	grokModelsSection   = "models"
	grokSubagentSection = "subagents.models"
	grokUnsetSentinel   = "__9gouter_unset__"
)

// grokSubagentTypes mirrors GROK_SUBAGENT_TYPES.
var grokSubagentTypes = []string{"general-purpose", "explore", "plan"}

// grokModelSlot mirrors modelSlot(type): "<mainSlot>-<type>".
func grokModelSlot(typ string) string {
	return grokMainModelSlot + "-" + typ
}

// escapeRegExp mirrors the JS escapeRegExp (escape regex metacharacters).
func escapeRegExp(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '^', '$', '\\', '|':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// tomlString mirrors the JS tomlString: JSON-stringify a string for a TOML
// quoted value.
func tomlString(v string) string {
	return strconv.Quote(v)
}

var grokPrevDefaultRe = regexp.MustCompile(`(?m)^# 9gouter-prev-default = "([^"]*)"[ \t]*\r?\n?`)

func grokPrevSubagentRe(typ string) *regexp.Regexp {
	pat := `(?m)^# 9gouter-prev-subagent-` + escapeRegExp(typ) + ` = "([^"]*)"[ \t]*\r?\n?`
	return regexp.MustCompile(pat)
}

// grokGetSectionField mirrors getSectionField: read a quoted field from a
// section. Returns (value, found); found distinguishes "absent" from "present
// but empty", which the JS null vs "" distinction relies on.
func grokGetSectionField(toml, section, key string) (string, bool) {
	loc := grokSectionLoc(toml, section)
	if loc == nil {
		return "", false
	}
	body := toml[loc[1]:loc[2]]
	fre := regexp.MustCompile(`(?m)^[ \t]*` + escapeRegExp(key) + `[ \t]*=[ \t]*"([^"]*)"`)
	if fm := fre.FindStringSubmatch(body); fm != nil {
		return fm[1], true
	}
	return "", false
}

// grokGetSectionNumber mirrors getSectionNumber: read a numeric field from a
// section, or -1 (JS null) when absent / unparseable.
func grokGetSectionNumber(toml, section, key string) int {
	loc := grokSectionLoc(toml, section)
	if loc == nil {
		return -1
	}
	body := toml[loc[1]:loc[2]]
	fre := regexp.MustCompile(`(?m)^[ \t]*` + escapeRegExp(key) + `[ \t]*=[ \t]*([0-9]+(?:\.[0-9]+)?)`)
	fm := fre.FindStringSubmatch(body)
	if fm == nil {
		return -1
	}
	n, err := strconv.ParseFloat(fm[1], 64)
	if err != nil {
		return -1
	}
	return int(n)
}

// grokSetSectionField mirrors setSectionField: upsert a quoted field into a
// section, creating the section header if absent. Replacement strings are
// QuoteMeta'd so a value containing '$' is inserted literally (Go's regexp
// replacement would otherwise interpret $0/$1).
func grokSetSectionField(toml, section, key, value string) string {
	line := key + " = " + tomlString(value)
	loc := grokSectionLoc(toml, section)
	if loc == nil {
		prefix := toml
		if len(toml) > 0 && !strings.HasSuffix(toml, "\n") {
			prefix = toml + "\n"
		}
		return prefix + "\n[" + section + "]\n" + line + "\n"
	}
	body := toml[loc[1]:loc[2]]
	fre := regexp.MustCompile(`(?m)^[ \t]*` + escapeRegExp(key) + `[ \t]*=[ \t]*"[^"]*"`)
	var nextBody string
	if fre.MatchString(body) {
		nextBody = fre.ReplaceAllString(body, regexp.QuoteMeta(line))
	} else {
		nextBody = line + "\n" + body
	}
	return toml[:loc[0]] + "[" + section + "]\n" + nextBody + toml[loc[2]:]
}

// grokDeleteSectionField mirrors deleteSectionField: remove a field; drop the
// whole section header if the body becomes empty (collapsing runs of blank
// lines).
func grokDeleteSectionField(toml, section, key string) string {
	loc := grokSectionLoc(toml, section)
	if loc == nil {
		return toml
	}
	body := toml[loc[1]:loc[2]]
	fre := regexp.MustCompile(`(?m)^[ \t]*` + escapeRegExp(key) + `[ \t]*=[^\r\n]*\r?\n?`)
	nextBody := fre.ReplaceAllString(body, "")
	if strings.TrimSpace(nextBody) == "" {
		next := toml[:loc[0]] + toml[loc[2]:]
		return regexp.MustCompile(`\n{3,}`).ReplaceAllString(next, "\n\n")
	}
	return toml[:loc[0]] + "[" + section + "]\n" + nextBody + toml[loc[2]:]
}

// grokModelSection is the parsed [model.<slot>] body.
type grokModelSection struct {
	Model         string `json:"model"`
	BaseURL       string `json:"base_url"`
	Name          string `json:"name"`
	APIKey        string `json:"api_key"`
	APIBackend    string `json:"api_backend"`
	ContextWindow int    `json:"context_window"`
	Raw           string `json:"raw"`
}

// grokParseModelSection mirrors parseModelSection. context_window is clamped
// to 0 (JSON falsy, like the JS null) when absent or non-positive.
func grokParseModelSection(toml, slot string) *grokModelSection {
	section := "model." + slot
	loc := grokSectionLoc(toml, section)
	if loc == nil {
		return nil
	}
	body := toml[loc[1]:loc[2]]
	cw := grokGetSectionNumber(toml, section, "context_window")
	if cw <= 0 {
		cw = 0
	}
	model, _ := grokGetSectionField(toml, section, "model")
	baseURL, _ := grokGetSectionField(toml, section, "base_url")
	name, _ := grokGetSectionField(toml, section, "name")
	apiKey, _ := grokGetSectionField(toml, section, "api_key")
	apiBackend, _ := grokGetSectionField(toml, section, "api_backend")
	return &grokModelSection{
		Model:         model,
		BaseURL:       baseURL,
		Name:          name,
		APIKey:        apiKey,
		APIBackend:    apiBackend,
		ContextWindow: cw,
		Raw:           body,
	}
}

// grokBuildModelSection mirrors buildModelSection: serialize a [model.<slot>]
// block.
func grokBuildModelSection(slot, model, baseURL, apiKey, name string, contextWindow int) string {
	lines := []string{
		"[model." + slot + "]",
		"model = " + tomlString(model),
		"base_url = " + tomlString(baseURL),
		"name = " + tomlString(name),
		"description = " + tomlString("Routed via 9Gouter gateway"),
		`api_backend = "chat_completions"`,
	}
	if apiKey != "" {
		lines = append(lines, "api_key = "+tomlString(apiKey))
	}
	if contextWindow > 0 {
		lines = append(lines, fmt.Sprintf("context_window = %d", contextWindow))
	}
	return strings.Join(lines, "\n") + "\n"
}

// grokUpsertModelSection mirrors upsertModelSection: replace the existing
// [model.<slot>] block wholesale, or append a new one when absent.
func grokUpsertModelSection(toml, slot, model, baseURL, apiKey, name string, contextWindow int) string {
	loc := grokSectionLoc(toml, "model."+slot)
	block := grokBuildModelSection(slot, model, baseURL, apiKey, name, contextWindow)
	if loc == nil {
		prefix := toml
		if len(toml) > 0 && !strings.HasSuffix(toml, "\n") {
			prefix = toml + "\n"
		}
		return prefix + "\n" + block
	}
	return toml[:loc[0]] + block + toml[loc[2]:]
}

// grokRemoveModelSection mirrors removeModelSection: drop the [model.<slot>]
// block and collapse any resulting run of blank lines.
func grokRemoveModelSection(toml, slot string) string {
	loc := grokSectionLoc(toml, "model."+slot)
	if loc == nil {
		return toml
	}
	next := toml[:loc[0]] + toml[loc[2]:]
	return regexp.MustCompile(`\n{3,}`).ReplaceAllString(next, "\n\n")
}

// grokInsertMarker mirrors insertMarker: place a comment marker immediately
// before the main model section (or at the end if absent). The marker is
// inserted via byte slicing so its text is taken literally (no regexp
// replacement-template expansion).
func grokInsertMarker(toml, marker string) string {
	loc := grokSectionLoc(toml, "model."+grokMainModelSlot)
	if loc == nil {
		prefix := toml
		if len(toml) > 0 && !strings.HasSuffix(toml, "\n") {
			prefix = toml + "\n"
		}
		return prefix + marker
	}
	return toml[:loc[0]] + marker + toml[loc[0]:]
}

// grokRememberPreviousDefault mirrors rememberPreviousDefault.
func grokRememberPreviousDefault(toml string) string {
	if grokPrevDefaultRe.MatchString(toml) {
		return toml
	}
	current, _ := grokGetSectionField(toml, grokModelsSection, "default")
	if current == "" || current == grokMainModelSlot {
		return toml
	}
	return grokInsertMarker(toml, "# 9gouter-prev-default = "+tomlString(current)+"\n")
}

// grokRestorePreviousDefault mirrors restorePreviousDefault.
func grokRestorePreviousDefault(toml string) string {
	previous := grokBuiltinDefault
	if m := grokPrevDefaultRe.FindStringSubmatch(toml); m != nil && m[1] != "" {
		previous = m[1]
	}
	next := grokPrevDefaultRe.ReplaceAllString(toml, "")
	if cur, _ := grokGetSectionField(next, grokModelsSection, "default"); cur == grokMainModelSlot {
		next = grokSetSectionField(next, grokModelsSection, "default", previous)
	}
	return next
}

// grokRememberPreviousSubagent mirrors rememberPreviousSubagent.
func grokRememberPreviousSubagent(toml, typ string) string {
	re := grokPrevSubagentRe(typ)
	if re.MatchString(toml) {
		return toml
	}
	current, ok := grokGetSectionField(toml, grokSubagentSection, typ)
	previous := grokUnsetSentinel
	if ok {
		previous = current
	}
	return grokInsertMarker(toml, "# 9gouter-prev-subagent-"+typ+" = "+tomlString(previous)+"\n")
}

// grokRestorePreviousSubagent mirrors restorePreviousSubagent.
func grokRestorePreviousSubagent(toml, typ string) string {
	re := grokPrevSubagentRe(typ)
	previous := grokUnsetSentinel
	if m := re.FindStringSubmatch(toml); m != nil && m[1] != "" {
		previous = m[1]
	}
	next := re.ReplaceAllString(toml, "")
	cur, _ := grokGetSectionField(next, grokSubagentSection, typ)
	if cur != grokModelSlot(typ) {
		return next
	}
	if previous == grokUnsetSentinel {
		return grokDeleteSectionField(next, grokSubagentSection, typ)
	}
	return grokSetSectionField(next, grokSubagentSection, typ, previous)
}

// GrokBuildParsedConfig is the GET response payload (parseGrokBuildConfig).
type GrokBuildParsedConfig struct {
	Model            map[string]any `json:"model"`
	Default          string         `json:"default"`
	SubagentModels   map[string]any `json:"subagentModels"`
	SubagentMappings map[string]any `json:"subagentMappings"`
}

// parseGrokBuildConfig mirrors parseGrokBuildConfig.
func parseGrokBuildConfig(toml string) GrokBuildParsedConfig {
	subagentModels := map[string]any{}
	subagentMappings := map[string]any{}
	for _, typ := range grokSubagentTypes {
		mapping, _ := grokGetSectionField(toml, grokSubagentSection, typ)
		subagentMappings[typ] = mapping
		if mapping == grokModelSlot(typ) {
			if sec := grokParseModelSection(toml, mapping); sec != nil {
				subagentModels[typ] = sec
			} else {
				subagentModels[typ] = nil
			}
		} else {
			subagentModels[typ] = nil
		}
	}

	var modelMap map[string]any
	if sec := grokParseModelSection(toml, grokMainModelSlot); sec != nil {
		modelMap = map[string]any{
			"model":          sec.Model,
			"base_url":       sec.BaseURL,
			"name":           sec.Name,
			"api_key":        sec.APIKey,
			"api_backend":    sec.APIBackend,
			"context_window": sec.ContextWindow,
			"raw":            sec.Raw,
		}
	}
	def, _ := grokGetSectionField(toml, grokModelsSection, "default")
	return GrokBuildParsedConfig{
		Model:            modelMap,
		Default:          def,
		SubagentModels:   subagentModels,
		SubagentMappings: subagentMappings,
	}
}

// grokSubagentOverride is one normalized entry in the applyGrokBuildConfig
// subagentModels map (model + contextWindow).
type grokSubagentOverride struct {
	Model         string
	ContextWindow int
}

// applyGrokBuildConfig mirrors applyGrokBuildConfig. subagentModels == nil
// (absent) leaves existing subagent config untouched; an empty non-nil map
// clears every override. This mirrors the JS `subagentModels === undefined`
// (skip) vs `{}` (clear) distinction.
func applyGrokBuildConfig(toml, baseURL, apiKey, model string, contextWindow int, subagentModels map[string]*grokSubagentOverride) string {
	next := grokRememberPreviousDefault(toml)
	next = grokUpsertModelSection(next, grokMainModelSlot, model, baseURL, apiKey, "9Gouter", contextWindow)
	next = grokSetSectionField(next, grokModelsSection, "default", grokMainModelSlot)

	if subagentModels != nil {
		for _, typ := range grokSubagentTypes {
			selected := subagentModels[typ]
			slot := grokModelSlot(typ)
			if selected != nil && selected.Model != "" {
				next = grokRememberPreviousSubagent(next, typ)
				name := "9Gouter " + typ
				next = grokUpsertModelSection(next, slot, selected.Model, baseURL, apiKey, name, selected.ContextWindow)
				next = grokSetSectionField(next, grokSubagentSection, typ, slot)
			} else {
				next = grokRestorePreviousSubagent(next, typ)
				next = grokRemoveModelSection(next, slot)
			}
		}
	}
	return next
}

// resetGrokBuildConfig mirrors resetGrokBuildConfig.
func resetGrokBuildConfig(toml string) string {
	next := toml
	for _, typ := range grokSubagentTypes {
		next = grokRestorePreviousSubagent(next, typ)
		next = grokRemoveModelSection(next, grokModelSlot(typ))
	}
	next = grokRemoveModelSection(next, grokMainModelSlot)
	next = grokRestorePreviousDefault(next)
	return regexp.MustCompile(`\n{3,}`).ReplaceAllString(next, "\n\n")
}

// hasGrokBuildConfig mirrors has9RouterConfig: the main slot is configured with
// a base_url, so the GET response can flag whether a reset is meaningful.
func hasGrokBuildConfig(settings GrokBuildParsedConfig) bool {
	if settings.Model == nil {
		return false
	}
	bu, _ := settings.Model["base_url"].(string)
	return bu != ""
}

// getGrokSubagentSlot mirrors getGrokSubagentSlot.
func getGrokSubagentSlot(typ string) string {
	for _, t := range grokSubagentTypes {
		if t == typ {
			return grokModelSlot(typ)
		}
	}
	return ""
}
