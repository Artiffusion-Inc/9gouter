// Package cursorexec — cursorModels parse port.
//
// models.go ports the response-parsing half of open-sse/services/cursorModels.js
// (upstream v0.5.40): ParseCursorUsableModels decodes Cursor's
// agent.v1.GetUsableModelsResponse protobuf (repeated agent.v1.ModelDetails in
// field 1) into { id, name } entries. The HTTP/2 fetch + cache live in the
// resolver package (resolver/cursor.go); this package owns the wire decode so
// it is unit-testable without a server.
package cursorexec

import "strings"

// agent.v1.ModelDetails protobuf field numbers (from cursorModels.js).
const (
	modelIDField          = 1
	displayModelIDField   = 3
	displayNameField      = 4
	displayNameShortField = 5
	responseModelsField   = 1
)

// CursorModel is one entry in the account-specific usable model catalog.
type CursorModel struct {
	ID   string
	Name string
}

// firstString returns the first LEN-payload string for fieldNum, or "".
func firstString(fields decodedMessage, fieldNum int) string {
	v := fields.first(fieldNum)
	if v == nil {
		return ""
	}
	return string(v)
}

// BuildGetUsableModelsResponse encodes a GetUsableModelsResponse protobuf from
// the given model entries (id → model_id/display_model_id, name → display_name),
// mirroring the wire shape ParseCursorUsableModels decodes. Exported so the
// resolver package can build realistic response bodies in its no-mock tests
// without reaching into this package's internal encoder.
func BuildGetUsableModelsResponse(models []CursorModel) []byte {
	var resp []byte
	for _, m := range models {
		detail := concat(
			encodeField(modelIDField, wireLen, m.ID),
			encodeField(displayModelIDField, wireLen, m.ID),
			encodeField(displayNameField, wireLen, m.Name),
		)
		resp = concat(resp, encodeField(responseModelsField, wireLen, detail))
	}
	return resp
}

// ParseCursorUsableModels decodes an agent.v1.GetUsableModelsResponse payload
// into deduplicated { id, name } entries, mirroring the JS
// parseCursorUsableModels. The model id (field 1) is the unique key; the name
// falls back through display_name, display_name_short, display_model_id, id.
func ParseCursorUsableModels(payload []byte) []CursorModel {
	resp := decodeMessage(payload)
	seen := map[string]bool{}
	var models []CursorModel
	for _, entry := range resp[responseModelsField] {
		if entry.value == nil {
			continue
		}
		detail := decodeMessage(entry.value)
		id := strings.TrimSpace(firstString(detail, modelIDField))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(firstString(detail, displayNameField))
		if name == "" {
			name = strings.TrimSpace(firstString(detail, displayNameShortField))
		}
		if name == "" {
			name = strings.TrimSpace(firstString(detail, displayModelIDField))
		}
		if name == "" {
			name = id
		}
		models = append(models, CursorModel{ID: id, Name: name})
	}
	return models
}
