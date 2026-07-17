package chat

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
	}{
		{
			name: "string content",
			msg: Message{
				Role:    "user",
				Content: json.RawMessage(`"hello"`),
			},
		},
		{
			name: "array content",
			msg: Message{
				Role:    "user",
				Content: json.RawMessage(`[{"type":"text","text":"hi"}]`),
				Name:    "alice",
			},
		},
		{
			name: "empty content preserved",
			msg: Message{
				Role:    "system",
				Content: json.RawMessage(`null`),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.msg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got Message
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, c.msg) {
				t.Errorf("round-trip = %+v, want %+v", got, c.msg)
			}
		})
	}
}

func TestChatRequestRawBody(t *testing.T) {
	raw := json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	req := ChatRequest{RawBody: raw}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// RawBody is tagged with "-", so it must not appear in the JSON.
	if string(b) == string(raw) {
		t.Errorf("RawBody leaked into JSON: %s", b)
	}
}
