package rtk

import (
	"fmt"
)

// package-level enabled flag mirroring open-sse/rtk/index.js rtkEnabled.
var rtkEnabled = false

// SetEnabled toggles the package-level RTK enable flag.
func SetEnabled(v bool) { rtkEnabled = v }

// IsEnabled returns the package-level RTK enable flag.
func IsEnabled() bool { return rtkEnabled }

// Hit describes one in-place compression result.
type Hit struct {
	Shape  string `json:"shape"`
	Filter string `json:"filter"`
	Saved  int    `json:"saved"`
}

// Stats is the result of compressMessages.
type Stats struct {
	BytesBefore int   `json:"bytesBefore"`
	BytesAfter  int   `json:"bytesAfter"`
	Hits        []Hit `json:"hits"`
}

// Saved returns the bytes saved.
func (s *Stats) Saved() int {
	if s == nil {
		return 0
	}
	return s.BytesBefore - s.BytesAfter
}

// FormatRtkLog formats a log line from stats, matching index.js formatRtkLog.
func FormatRtkLog(stats *Stats) string {
	if stats == nil || len(stats.Hits) == 0 {
		return ""
	}
	saved := stats.Saved()
	var pct string
	if stats.BytesBefore > 0 {
		pct = fmt.Sprintf("%.1f", float64(saved)/float64(stats.BytesBefore)*100)
	} else {
		pct = "0"
	}
	filters := map[string]struct{}{}
	for _, h := range stats.Hits {
		filters[h.Filter] = struct{}{}
	}
	var names []string
	for f := range filters {
		names = append(names, f)
	}
	return fmt.Sprintf("[RTK] saved %dB / %dB (%s%%) via [%s] hits=%d", saved, stats.BytesBefore, pct, joinNames(names), len(stats.Hits))
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return out
}

// CompressMessages compresses tool_result content in-place in body.
// It mirrors open-sse/rtk/index.js compressMessages.
func CompressMessages(body map[string]any, enabled bool, log logger) *Stats {
	if !enabled {
		return nil
	}
	if body == nil {
		return nil
	}

	if _, ok := body["conversationState"]; ok {
		return compressKiroFormat(body, log)
	}

	var items []any
	if msgs, ok := body["messages"].([]any); ok {
		items = msgs
	} else if input, ok := body["input"].([]any); ok {
		items = input
	}
	if items == nil {
		return nil
	}

	stats := &Stats{}
	defer func() {
		if r := recover(); r != nil {
			if log != nil {
				log.Warnf("[RTK] compressMessages error: %v", r)
			}
		}
	}()

	for _, itemRaw := range items {
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := item["type"].(string)
		role, _ := item["role"].(string)

		if t == "function_call_output" {
			if output, ok := item["output"].(string); ok {
				item["output"] = compressText(output, stats, "openai-responses-string", log)
			} else if outputArr, ok := item["output"].([]any); ok {
				for _, partRaw := range outputArr {
					part, ok := partRaw.(map[string]any)
					if !ok {
						continue
					}
					if pt, _ := part["type"].(string); pt == "input_text" {
						if text, ok := part["text"].(string); ok {
							part["text"] = compressText(text, stats, "openai-responses-array", log)
						}
					}
				}
			}
			continue
		}

		if role == "tool" {
			if content, ok := item["content"].(string); ok {
				item["content"] = compressText(content, stats, "openai-tool", log)
				continue
			}
			contentArr, ok := item["content"].([]any)
			if !ok {
				continue
			}
			for _, partRaw := range contentArr {
				part, ok := partRaw.(map[string]any)
				if !ok {
					continue
				}
				if pt, _ := part["type"].(string); pt == "text" {
					if text, ok := part["text"].(string); ok {
						part["text"] = compressText(text, stats, "openai-tool-array", log)
					}
				}
			}
			continue
		}

		contentArr, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, blockRaw := range contentArr {
			block, ok := blockRaw.(map[string]any)
			if !ok {
				continue
			}
			bt, _ := block["type"].(string)
			if bt != "tool_result" {
				continue
			}
			if isErr, ok := block["is_error"].(bool); ok && isErr {
				continue
			}
			if content, ok := block["content"].(string); ok {
				block["content"] = compressText(content, stats, "claude-string", log)
			} else if parts, ok := block["content"].([]any); ok {
				for _, partRaw := range parts {
					part, ok := partRaw.(map[string]any)
					if !ok {
						continue
					}
					if pt, _ := part["type"].(string); pt == "text" {
						if text, ok := part["text"].(string); ok {
							part["text"] = compressText(text, stats, "claude-array", log)
						}
					}
				}
			}
		}
	}

	return stats
}

func compressKiroFormat(body map[string]any, log logger) *Stats {
	stats := &Stats{}
	defer func() {
		if r := recover(); r != nil {
			if log != nil {
				log.Warnf("[RTK] compressKiroFormat error: %v", r)
			}
		}
	}()

	stateRaw, ok := body["conversationState"].(map[string]any)
	if !ok {
		return nil
	}
	var allMessages []any
	if hist, ok := stateRaw["history"].([]any); ok {
		allMessages = append(allMessages, hist...)
	}
	if cur, ok := stateRaw["currentMessage"].(map[string]any); ok {
		allMessages = append(allMessages, cur)
	}

	for _, msgRaw := range allMessages {
		msg, ok := msgRaw.(map[string]any)
		if !ok {
			continue
		}
		user, ok := msg["userInputMessage"].(map[string]any)
		if !ok {
			continue
		}
		ctxRaw, ok := user["userInputMessageContext"].(map[string]any)
		if !ok {
			continue
		}
		trs, ok := ctxRaw["toolResults"].([]any)
		if !ok {
			continue
		}
		for _, trRaw := range trs {
			tr, ok := trRaw.(map[string]any)
			if !ok {
				continue
			}
			if status, _ := tr["status"].(string); status == "error" {
				continue
			}
			content, ok := tr["content"].([]any)
			if !ok {
				continue
			}
			for _, partRaw := range content {
				part, ok := partRaw.(map[string]any)
				if !ok {
					continue
				}
				if text, ok := part["text"].(string); ok {
					part["text"] = compressText(text, stats, "kiro-tool-result", log)
				}
			}
		}
	}
	return stats
}

func compressText(text string, stats *Stats, shape string, log logger) string {
	bytesIn := len(text)
	stats.BytesBefore += bytesIn

	if bytesIn < MinCompressSize || bytesIn > RawCap {
		stats.BytesAfter += bytesIn
		return text
	}

	fn := autoDetectFilter(text)
	if fn == nil {
		stats.BytesAfter += bytesIn
		return text
	}

	out := safeApply(filterFunc(fn), text, log)
	if out == "" || len(out) >= bytesIn {
		stats.BytesAfter += bytesIn
		return text
	}

	stats.BytesAfter += len(out)
	stats.Hits = append(stats.Hits, Hit{Shape: shape, Filter: filterName(fn), Saved: bytesIn - len(out)})
	return out
}

func filterName(fn filter) string {
	if fn == nil {
		return ""
	}
	if fn == nil {
		return ""
	}
	if sameFunc(fn, gitDiffFilter) {
		return FilterGitDiff
	}
	if sameFunc(fn, gitStatus) {
		return FilterGitStatus
	}
	if sameFunc(fn, gitLog) {
		return FilterGitLog
	}
	if sameFunc(fn, buildOutput) {
		return FilterBuildOutput
	}
	if sameFunc(fn, grep) {
		return FilterGrep
	}
	if sameFunc(fn, find) {
		return FilterFind
	}
	if sameFunc(fn, ls) {
		return FilterLs
	}
	if sameFunc(fn, tree) {
		return FilterTree
	}
	if sameFunc(fn, dedupLog) {
		return FilterDedupLog
	}
	if sameFunc(fn, smartTruncate) {
		return FilterSmartTruncate
	}
	if sameFunc(fn, readNumbered) {
		return FilterReadNumbered
	}
	if sameFunc(fn, searchList) {
		return FilterSearchList
	}
	return "unknown"
}

func sameFunc(a, b filter) bool {
	return fmt.Sprintf("%p", a) == fmt.Sprintf("%p", b)
}
