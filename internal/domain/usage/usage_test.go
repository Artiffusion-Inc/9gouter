package usage

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUsageRecordRoundTrip(t *testing.T) {
	streamMs := 1234
	tps := 42.5
	rec := UsageRecord{
		Timestamp:        time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
		Provider:         "openai",
		Model:            "gpt-4o",
		ConnectionID:     "conn-1",
		APIKey:           "sk-test",
		Endpoint:         "/v1/chat/completions",
		PromptTokens:     10,
		CompletionTokens: 20,
		Cost:             0.0003,
		Status:           "ok",
		StreamMs:         &streamMs,
		TPS:              &tps,
		Meta:             json.RawMessage(`{"key":"value"}`),
		Tokens:           json.RawMessage(`{"prompt_tokens":10,"completion_tokens":20}`),
	}

	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got UsageRecord
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Provider != rec.Provider || got.Model != rec.Model {
		t.Errorf("basic fields changed: %+v", got)
	}
	if got.StreamMs == nil || *got.StreamMs != streamMs {
		t.Errorf("StreamMs mismatch")
	}
	if got.TPS == nil || *got.TPS != tps {
		t.Errorf("TPS mismatch")
	}
	if string(got.Meta) != string(rec.Meta) {
		t.Errorf("Meta bytes changed: %s vs %s", got.Meta, rec.Meta)
	}
	if string(got.Tokens) != string(rec.Tokens) {
		t.Errorf("Tokens bytes changed: %s vs %s", got.Tokens, rec.Tokens)
	}
}
