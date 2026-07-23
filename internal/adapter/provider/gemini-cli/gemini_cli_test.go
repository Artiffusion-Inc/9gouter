package geminicliexec

import (
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// TestTransformRequestAddsValidatedToolConfig ports 7610f28f (#2486): when the
// translated Gemini body carries tools, the request envelope gets
// toolConfig.functionCallingConfig.mode=VALIDATED.
func TestTransformRequestAddsValidatedToolConfig(t *testing.T) {
	e := New(base.Config{})
	body := map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "use the tool"}}}},
		"tools": []any{map[string]any{
			"functionDeclarations": []any{map[string]any{"name": "get_weather"}},
		}},
	}
	raw, _ := json.Marshal(body)
	out, err := e.TransformRequest("gemini-2.5-pro", raw, true, provider.Credentials{
		ProviderSpecificData: map[string]any{"projectId": "proj-1"},
	})
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	req, ok := envelope["request"].(map[string]any)
	if !ok {
		t.Fatalf("envelope.request missing: %+v", envelope)
	}
	tc, ok := req["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("request.toolConfig missing: %+v", req)
	}
	fcc, ok := tc["functionCallingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("functionCallingConfig missing: %+v", tc)
	}
	if fcc["mode"] != "VALIDATED" {
		t.Errorf("mode = %v, want VALIDATED", fcc["mode"])
	}
}

// TestTransformRequestNoToolConfigWithoutTools verifies toolConfig is NOT
// added when the body has no tools array.
func TestTransformRequestNoToolConfigWithoutTools(t *testing.T) {
	e := New(base.Config{})
	body := map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "plain text"}}}},
	}
	raw, _ := json.Marshal(body)
	out, err := e.TransformRequest("gemini-2.5-pro", raw, true, provider.Credentials{
		ProviderSpecificData: map[string]any{"projectId": "proj-1"},
	})
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	var envelope map[string]any
	json.Unmarshal(out, &envelope)
	req := envelope["request"].(map[string]any)
	if _, has := req["toolConfig"]; has {
		t.Errorf("toolConfig should be absent without tools, got %v", req["toolConfig"])
	}
}

// TestTransformRequestEmptyToolsNoToolConfig verifies an empty tools array does
// not trigger toolConfig (mirrors `tools?.length > 0`).
func TestTransformRequestEmptyToolsNoToolConfig(t *testing.T) {
	e := New(base.Config{})
	body := map[string]any{"tools": []any{}}
	raw, _ := json.Marshal(body)
	out, _ := e.TransformRequest("gemini-2.5-pro", raw, true, provider.Credentials{})
	var envelope map[string]any
	json.Unmarshal(out, &envelope)
	req := envelope["request"].(map[string]any)
	if _, has := req["toolConfig"]; has {
		t.Errorf("toolConfig should be absent for empty tools, got %v", req["toolConfig"])
	}
}

// TestHasGeminiTools covers the guard directly.
func TestHasGeminiTools(t *testing.T) {
	cases := map[string]struct {
		m    map[string]any
		want bool
	}{
		"non-empty tools": {map[string]any{"tools": []any{map[string]any{"x": 1}}}, true},
		"empty tools":     {map[string]any{"tools": []any{}}, false},
		"no tools key":    {map[string]any{"contents": 1}, false},
		"tools not array": {map[string]any{"tools": "nope"}, false},
		"nil tools":       {map[string]any{"tools": nil}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := hasGeminiTools(c.m); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestTransformRequestEnvelopeShape is a regression guard: the envelope stays
// {project, model, request} and request carries the translated body.
func TestTransformRequestEnvelopeShape(t *testing.T) {
	e := New(base.Config{})
	body := map[string]any{
		"contents": []any{
			map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": "hi"}},
			},
		},
	}
	raw, _ := json.Marshal(body)
	out, _ := e.TransformRequest("gemini-2.5-pro", raw, false, provider.Credentials{
		ProviderSpecificData: map[string]any{"projectId": "proj-1"},
	})
	var envelope map[string]any
	json.Unmarshal(out, &envelope)
	if envelope["project"] != "proj-1" {
		t.Errorf("project = %v, want proj-1", envelope["project"])
	}
	if envelope["model"] != "gemini-2.5-pro" {
		t.Errorf("model = %v, want gemini-2.5-pro", envelope["model"])
	}
	if _, ok := envelope["request"].(map[string]any); !ok {
		t.Errorf("request missing or wrong type: %T", envelope["request"])
	}
	// Ensure no stray top-level toolConfig (must live inside request).
	if _, has := envelope["toolConfig"]; has {
		t.Error("envelope should not carry top-level toolConfig")
	}
}
