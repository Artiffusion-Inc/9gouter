// Package cursor implements the cursor format translator.
package cursor

import (
	"encoding/json"
	"fmt"
)

type stubTranslator struct{}

func (stubTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	return nil, fmt.Errorf("not yet implemented")
}
func (stubTranslator) TranslateResponse(chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error) {
	return nil, fmt.Errorf("not yet implemented")
}
