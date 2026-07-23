package defaultexec

// cache_control.go ports upstream 9e386665 (filterToOpenAIFormat): when a
// request body is normalized to clean OpenAI format before dispatch, content
// blocks in messages[].content carry two non-OpenAI fields that must be
// stripped — `signature` (Claude thinking-tool signatures, never valid on an
// OpenAI upstream) and `cache_control` (Anthropic prompt-cache markers). DashScope
// providers (alicode / alicode-intl / alims-intl) opt in via the
// PreserveCacheControl quirk so their cache_control blocks reach upstream and
// enable prompt caching; every other provider gets both fields removed.
//
// The JS reference lives in open-sse/translator/formats/openai.js
// filterToOpenAIFormat(body, { preserveCacheControl }). Go has no central
// format-normalization step — passthrough/translated bodies reach the
// DefaultExecutor.TransformRequest verbatim — so the strip is applied there,
// gated by the resolved provider quirk (e.Config.Quirks.PreserveCacheControl).

// stripNonOpenAIBlockFields strips `signature` from a content block and strips
// `cache_control` unless preserveCacheControl is true. The block is mutated in
// place. Non-map blocks are left untouched.
func stripNonOpenAIBlockFields(block any, preserveCacheControl bool) {
	m, ok := block.(map[string]any)
	if !ok {
		return
	}
	delete(m, "signature")
	if !preserveCacheControl {
		delete(m, "cache_control")
	}
}

// stripNonOpenAIMessageFields walks a message's content array (or a single
// string/object content) and strips signature / cache_control from each block
// per stripNonOpenAIBlockField. Messages with string content or no content are
// unchanged. preserveCacheControl gates the cache_control strip.
func stripNonOpenAIMessageFields(msg map[string]any, preserveCacheControl bool) {
	content, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, block := range content {
		stripNonOpenAIBlockFields(block, preserveCacheControl)
	}
}

// stripNonOpenAIBodyFields applies stripNonOpenAIMessageFields to every message
// in body["messages"]. It is the Go analogue of filterToOpenAIFormat's content
// walk: signature is always removed, cache_control is removed unless
// preserveCacheControl is true. The body map is mutated in place.
func stripNonOpenAIBodyFields(body map[string]any, preserveCacheControl bool) {
	messages, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		stripNonOpenAIMessageFields(msg, preserveCacheControl)
	}
}
