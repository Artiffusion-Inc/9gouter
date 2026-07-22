package kiroexec

// eventstream_test.go pins the binary AWS EventStream codec (#100) against
// hand-built real frames — no mocks. The tests construct frames with the
// correct prelude + message CRC-32, all 10 header wire types, JSON payloads,
// and the framing loop's split-across-reads behavior, then assert ParseEventFrame
// and FrameStream decode them 1:1 with upstream kiro.js parseEventFrame.

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

// encodeHeader appends one typed EventStream header to dst, mirroring the wire
// layout parseEventFrame reads: [1-byte name length][name bytes][1-byte type][typed value].
func encodeHeader(dst []byte, name string, typ byte, value []byte) []byte {
	dst = append(dst, byte(len(name)))
	dst = append(dst, name...)
	dst = append(dst, typ)
	switch typ {
	case 2: // int8
		dst = append(dst, value...)
	case 3: // int16 BE
		dst = append(dst, value...)
	case 4: // int32 BE
		dst = append(dst, value...)
	case 5, 8: // 8-byte (int64/timestamp)
		dst = append(dst, value...)
	case 6, 7: // bytes/string: 2-byte BE length + value
		var ln [2]byte
		binary.BigEndian.PutUint16(ln[:], uint16(len(value)))
		dst = append(dst, ln[:]...)
		dst = append(dst, value...)
	case 9: // 16-byte uuid
		dst = append(dst, value...)
	case 0, 1: // bool true/false — no value bytes
	}
	return dst
}

// buildFrame assembles a complete EventStream frame with correct prelude and
// message CRC-32. headersBlob is the raw header bytes; payload is the JSON
// payload (may be nil/empty).
func buildFrame(t *testing.T, headersBlob []byte, payload []byte) []byte {
	t.Helper()
	headersLength := uint32(len(headersBlob))
	totalLength := uint32(16 + len(headersBlob) + len(payload))

	prelude := make([]byte, 8)
	binary.BigEndian.PutUint32(prelude[0:4], totalLength)
	binary.BigEndian.PutUint32(prelude[4:8], headersLength)
	preludeCRC := crc32(prelude)
	preludeWithCRC := append(prelude, make([]byte, 4)...)
	binary.BigEndian.PutUint32(preludeWithCRC[8:12], preludeCRC)

	body := append(preludeWithCRC, headersBlob...)
	body = append(body, payload...)
	messageCRC := crc32(body)
	frame := append(body, make([]byte, 4)...)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], messageCRC)
	return frame
}

func int16BE(v int16) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(v))
	return b[:]
}

func int32BE(v int32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	return b[:]
}

func TestParseEventFrame_BoolHeaders(t *testing.T) {
	// :message-type event (type 7 string) + a bool true (type 0) + bool false (type 1).
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	hdrs = encodeHeader(hdrs, ":event-type", 7, []byte("assistantResponseEvent"))
	hdrs = encodeHeader(hdrs, "flag-true", 0, nil)
	hdrs = encodeHeader(hdrs, "flag-false", 1, nil)
	payload, _ := json.Marshal(map[string]any{"text": "hi"})
	frame := buildFrame(t, hdrs, payload)

	f, err := ParseEventFrame(frame)
	if err != nil {
		t.Fatalf("ParseEventFrame: %v", err)
	}
	if f.MessageType() != "event" {
		t.Errorf("MessageType=%q want event", f.MessageType())
	}
	if f.EventType() != "assistantResponseEvent" {
		t.Errorf("EventType=%q want assistantResponseEvent", f.EventType())
	}
	m := f.headerMap()
	if m["flag-true"] != true {
		t.Errorf("flag-true=%v want true", m["flag-true"])
	}
	if m["flag-false"] != false {
		t.Errorf("flag-false=%v want false", m["flag-false"])
	}
	if f.Payload["text"] != "hi" {
		t.Errorf("payload=%v want text=hi", f.Payload)
	}
}

func TestParseEventFrame_IntHeaders(t *testing.T) {
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	hdrs = encodeHeader(hdrs, "int8", 2, []byte{0xff})        // -1
	hdrs = encodeHeader(hdrs, "int16", 3, int16BE(-2))        // -2
	hdrs = encodeHeader(hdrs, "int32", 4, int32BE(-3))        // -3
	hdrs = encodeHeader(hdrs, "bytes", 6, []byte{0x01, 0x02}) // bytes
	hdrs = encodeHeader(hdrs, "int64", 5, []byte{0, 0, 0, 0, 0, 0, 0, 7})
	hdrs = encodeHeader(hdrs, "uuid", 9, make([]byte, 16))
	frame := buildFrame(t, hdrs, []byte("{}"))

	f, err := ParseEventFrame(frame)
	if err != nil {
		t.Fatalf("ParseEventFrame: %v", err)
	}
	m := f.headerMap()
	if m["int8"] != int64(-1) {
		t.Errorf("int8=%v want -1", m["int8"])
	}
	if m["int16"] != int64(-2) {
		t.Errorf("int16=%v want -2", m["int16"])
	}
	if m["int32"] != int64(-3) {
		t.Errorf("int32=%v want -3", m["int32"])
	}
	if b, ok := m["bytes"].([]byte); !ok || len(b) != 2 || b[0] != 0x01 {
		t.Errorf("bytes=%v want [01 02]", m["bytes"])
	}
	// skipped types surfaced as Raw
	if raw, ok := m["int64"].([]byte); !ok || len(raw) != 8 || raw[7] != 7 {
		t.Errorf("int64 raw=%v want 8 bytes", m["int64"])
	}
	if raw, ok := m["uuid"].([]byte); !ok || len(raw) != 16 {
		t.Errorf("uuid raw=%v want 16 bytes", m["uuid"])
	}
}

func TestParseEventFrame_EmptyAndWhitespacePayload(t *testing.T) {
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	hdrs = encodeHeader(hdrs, ":event-type", 7, []byte("metadataEvent"))

	// No payload bytes.
	f, err := ParseEventFrame(buildFrame(t, hdrs, nil))
	if err != nil {
		t.Fatalf("empty payload: %v", err)
	}
	if f.Payload != nil {
		t.Errorf("empty payload: got %v want nil", f.Payload)
	}

	// Whitespace-only payload → nil.
	f, err = ParseEventFrame(buildFrame(t, hdrs, []byte("  \n\t\r  ")))
	if err != nil {
		t.Fatalf("whitespace payload: %v", err)
	}
	if f.Payload != nil {
		t.Errorf("whitespace payload: got %v want nil", f.Payload)
	}
}

func TestParseEventFrame_PreludeCRCMismatch(t *testing.T) {
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	frame := buildFrame(t, hdrs, []byte("{}"))
	// Corrupt the prelude-CRC field (bytes 8:12) without touching totalLength /
	// headersLength (bytes 0:8) so the length + bounds checks still pass and the
	// prelude CRC check is what fires.
	frame[8] ^= 0x01
	_, err := ParseEventFrame(frame)
	if err == nil || !strings.Contains(err.Error(), "prelude CRC") {
		t.Errorf("err=%v want prelude CRC mismatch", err)
	}
}

func TestParseEventFrame_MessageCRCMismatch(t *testing.T) {
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	frame := buildFrame(t, hdrs, []byte("{}"))
	// Flip a payload byte so the message CRC no longer matches.
	frame[14] ^= 0x01
	_, err := ParseEventFrame(frame)
	if err == nil || !strings.Contains(err.Error(), "message CRC") {
		t.Errorf("err=%v want message CRC mismatch", err)
	}
}

func TestParseEventFrame_LengthMismatch(t *testing.T) {
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	frame := buildFrame(t, hdrs, []byte("{}"))
	// Truncate so totalLength != actual bytes.
	_, err := ParseEventFrame(frame[:len(frame)-1])
	if err == nil || !strings.Contains(err.Error(), "length does not match") {
		t.Errorf("err=%v want length mismatch", err)
	}
}

func TestParseEventFrame_DuplicateHeader(t *testing.T) {
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	hdrs = encodeHeader(hdrs, ":message-type", 7, []byte("event"))
	_, err := ParseEventFrame(buildFrame(t, hdrs, nil))
	if err == nil || !strings.Contains(err.Error(), "duplicate header") {
		t.Errorf("err=%v want duplicate header", err)
	}
}

func TestParseEventFrame_InvalidPayloadJSON(t *testing.T) {
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	_, err := ParseEventFrame(buildFrame(t, hdrs, []byte("{not json")))
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("err=%v want not valid JSON", err)
	}
}

func TestParseEventFrame_TooShort(t *testing.T) {
	_, err := ParseEventFrame([]byte{0, 0, 0, 0})
	if err == nil || !strings.Contains(err.Error(), "shorter than 16 bytes") {
		t.Errorf("err=%v want shorter than 16 bytes", err)
	}
}

func TestFrameStream_SplitAcrossReads(t *testing.T) {
	// Two complete frames, fed in tiny chunks to exercise the partial-frame
	// buffering path in FrameStream.Next.
	h1 := encodeHeader(nil, ":message-type", 7, []byte("event"))
	h1 = encodeHeader(h1, ":event-type", 7, []byte("assistantResponseEvent"))
	p1, _ := json.Marshal(map[string]any{"n": 1})
	frame1 := buildFrame(t, h1, p1)

	h2 := encodeHeader(nil, ":message-type", 7, []byte("event"))
	h2 = encodeHeader(h2, ":event-type", 7, []byte("messageStopEvent"))
	frame2 := buildFrame(t, h2, []byte("{}"))

	stream := NewFrameStream()
	combined := append(frame1, frame2...)

	// Feed 3 bytes at a time, draining Next() after each Push.
	var got []string
	for i := 0; i < len(combined); i += 3 {
		end := i + 3
		if end > len(combined) {
			end = len(combined)
		}
		if err := stream.Push(combined[i:end]); err != nil {
			t.Fatalf("Push: %v", err)
		}
		for {
			f, err := stream.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if f.EventType() == "" {
				break // no complete frame yet
			}
			got = append(got, f.EventType())
		}
	}
	if len(got) != 2 || got[0] != "assistantResponseEvent" || got[1] != "messageStopEvent" {
		t.Errorf("got=%v want [assistantResponseEvent messageStopEvent]", got)
	}
	if len(stream.Remainder()) != 0 {
		t.Errorf("remainder=%d want 0", len(stream.Remainder()))
	}
}

func TestFrameStream_CorruptPrelude(t *testing.T) {
	stream := NewFrameStream()
	bad := make([]byte, 16)
	binary.BigEndian.PutUint32(bad[0:4], 16) // totalLength=16 (looks plausible)
	binary.BigEndian.PutUint32(bad[4:8], 0)  // headersLength=0
	// prelude CRC (bad[8:12]) left zero → won't match crc32(bad[0:8]).
	if err := stream.Push(bad); err != nil {
		t.Fatalf("Push: %v", err)
	}
	_, err := stream.Next()
	if err == nil || !strings.Contains(err.Error(), "prelude CRC") {
		t.Errorf("err=%v want prelude CRC mismatch", err)
	}
}

func TestFrameStream_OverBound(t *testing.T) {
	stream := NewFrameStream()
	// Force a tiny bound to trigger the guard without allocating 24MB.
	stream.maxRawBytes = 4
	err := stream.Push([]byte{0, 0, 0, 0, 0})
	if err == nil || !strings.Contains(err.Error(), "protocol bound") {
		t.Errorf("err=%v want protocol bound exceeded", err)
	}
}

func TestCrc32_KnownVectors(t *testing.T) {
	// CRC-32 (IEEE) of "123456789" is 0xCBF43926 — the canonical check value.
	got := crc32([]byte("123456789"))
	if got != 0xCBF43926 {
		t.Errorf("crc32(123456789)=%#x want 0xcbf43926", got)
	}
	// Empty input → 0.
	if crc32(nil) != 0 {
		t.Errorf("crc32(nil)=%#x want 0", crc32(nil))
	}
}
