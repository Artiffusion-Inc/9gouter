// eventstream.go ports the binary AWS EventStream codec half of
// open-sse/executors/kiro.js (upstream v0.5.40, commit 6994cd1f). Kiro's
// CodeWhisperer streaming endpoint returns binary AWS EventStream frames, NOT
// SSE-wrapped JSON: each message is
//
//	[4-byte BE total-length][4-byte BE headers-length]
//	[4-byte prelude CRC-32][headers...][payload...][4-byte message CRC-32]
//
// with typed headers (10 wire types) carrying :message-type / :event-type.
//
// This file owns the pure codec: CRC-32 (0xEDB88320 table), frame parsing with
// prelude+message CRC validation, and header decoding for all 10 types. It is
// unit-testable without a server; the framing loop that slices a byte stream
// into frames lives here too. The event-dispatch + terminal-state machine
// (#101) and the integrity gate + retry (#102) consume this codec.
package kiroexec

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// EventStream protocol bounds (mirror open-sse/executors/kiro.js lines 13-14).
const (
	eventstreamMaxMessageBytes = 24 * 1024 * 1024
	eventstreamMaxHeadersBytes = 128 * 1024
)

// crc32Table is the standard CRC-32 (poly 0xEDB88320, reflected) table, matching
// the JS CRC32_TABLE built in kiro.js:29-35. AWS EventStream uses IEEE CRC-32.
var crc32Table = func() [256]uint32 {
	var t [256]uint32
	for i := uint32(0); i < 256; i++ {
		c := i
		for bit := 0; bit < 8; bit++ {
			if c&1 != 0 {
				c = (c >> 1) ^ 0xEDB88320
			} else {
				c >>= 1
			}
		}
		t[i] = c
	}
	return t
}()

// crc32 computes the IEEE CRC-32 of bytes, matching the JS crc32 (kiro.js:54-58):
//
//	crc = 0xffffffff; for each byte: crc = table[(crc^byte)&0xff] ^ (crc>>>8); return (crc^0xffffffff)
func crc32(bytes []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range bytes {
		crc = crc32Table[(crc^uint32(b))&0xff] ^ (crc >> 8)
	}
	return crc ^ 0xffffffff
}

// EventHeader is one decoded EventStream header value. AWS EventStream headers
// are typed; this mirrors the subset the JS parser keeps (bool, int8, int16,
// int32, byte-slice, string). The unused numeric widths (int64/float64, the
// 8-byte type 8, the 16-byte uuid type 9) are skipped during parse and surfaced
// here as Raw == nil with a note in Type for completeness — Kiro only ever uses
// bool (0/1) and string (7) for :message-type / :event-type, so the skipped
// types rarely appear in practice.
type EventHeader struct {
	Name  string
	Type  byte
	Bool  bool
	Int   int64
	Bytes []byte
	Str   string
	// Raw is the raw typed value for header types the parser skips (5, 8, 9);
	// nil otherwise.
	Raw []byte
}

// EventFrame is one decoded AWS EventStream message: its typed headers and the
// JSON-decoded payload (nil when the payload is empty / whitespace).
type EventFrame struct {
	Headers []EventHeader
	Payload map[string]any
}

// headerMap returns the headers as a name→value map for the dispatch layer. The
// dispatch only reads :message-type / :event-type (string headers), so string
// and bytes values are surfaced; bool/int are surfaced as their Go zero/decoded
// value. Duplicate-header detection already happened in ParseEventFrame.
func (f EventFrame) headerMap() map[string]any {
	m := make(map[string]any, len(f.Headers))
	for _, h := range f.Headers {
		switch h.Type {
		case 0, 1:
			m[h.Name] = h.Bool
		case 2, 3, 4:
			m[h.Name] = h.Int
		case 6:
			m[h.Name] = h.Bytes
		case 7:
			m[h.Name] = h.Str
		default:
			m[h.Name] = h.Raw
		}
	}
	return m
}

// MessageType returns the :message-type header value ("" if absent).
func (f EventFrame) MessageType() string {
	if v, ok := f.headerMap()[":message-type"].(string); ok {
		return v
	}
	return ""
}

// EventType returns the :event-type header value ("" if absent).
func (f EventFrame) EventType() string {
	if v, ok := f.headerMap()[":event-type"].(string); ok {
		return v
	}
	return ""
}

// ParseEventFrame decodes one complete AWS EventStream frame from data, mirroring
// parseEventFrame in kiro.js:1115-1199. It validates the prelude CRC, message
// CRC, frame bounds, header bounds, and duplicate-header names, then parses all
// 10 header wire types and JSON-decodes the payload.
//
// data must be exactly one complete frame (the framing loop in FrameStream
// slices the buffer to the declared totalLength before calling this).
func ParseEventFrame(data []byte) (EventFrame, error) {
	if len(data) < 16 {
		return EventFrame{}, errors.New("AWS EventStream frame is shorter than 16 bytes")
	}
	totalLength := binary.BigEndian.Uint32(data[0:4])
	headersLength := binary.BigEndian.Uint32(data[4:8])
	if int(totalLength) != len(data) {
		return EventFrame{}, errors.New("AWS EventStream frame length does not match its prelude")
	}
	if totalLength > eventstreamMaxMessageBytes ||
		headersLength > eventstreamMaxHeadersBytes ||
		headersLength > totalLength-16 {
		return EventFrame{}, errors.New("AWS EventStream frame bounds are invalid")
	}
	if binary.BigEndian.Uint32(data[8:12]) != crc32(data[0:8]) {
		return EventFrame{}, errors.New("AWS EventStream prelude CRC mismatch")
	}
	if binary.BigEndian.Uint32(data[totalLength-4:totalLength]) != crc32(data[0:totalLength-4]) {
		return EventFrame{}, errors.New("AWS EventStream message CRC mismatch")
	}

	headers := make([]EventHeader, 0, 4)
	seen := make(map[string]bool, 4)
	offset := 12
	headerEnd := offset + int(headersLength)
	requireBytes := func(count int) error {
		if offset+count > headerEnd {
			return errors.New("AWS EventStream header exceeds its declared bounds")
		}
		return nil
	}

	for offset < headerEnd {
		if err := requireBytes(1); err != nil {
			return EventFrame{}, err
		}
		nameLength := int(data[offset])
		offset++
		if err := requireBytes(nameLength + 1); err != nil {
			return EventFrame{}, err
		}
		name := string(data[offset : offset+nameLength])
		offset += nameLength
		if seen[name] {
			return EventFrame{}, fmt.Errorf("AWS EventStream contains duplicate header: %s", name)
		}
		seen[name] = true
		typ := data[offset]
		offset++

		h := EventHeader{Name: name, Type: typ}
		switch typ {
		case 0, 1:
			h.Bool = typ == 0
		case 2:
			if err := requireBytes(1); err != nil {
				return EventFrame{}, err
			}
			h.Int = int64(int8(data[offset]))
			offset++
		case 3:
			if err := requireBytes(2); err != nil {
				return EventFrame{}, err
			}
			h.Int = int64(int16(binary.BigEndian.Uint16(data[offset : offset+2])))
			offset += 2
		case 4:
			if err := requireBytes(4); err != nil {
				return EventFrame{}, err
			}
			h.Int = int64(int32(binary.BigEndian.Uint32(data[offset : offset+4])))
			offset += 4
		case 5, 8:
			if err := requireBytes(8); err != nil {
				return EventFrame{}, err
			}
			h.Raw = data[offset : offset+8]
			offset += 8
		case 6, 7:
			if err := requireBytes(2); err != nil {
				return EventFrame{}, err
			}
			valueLength := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if err := requireBytes(valueLength); err != nil {
				return EventFrame{}, err
			}
			val := data[offset : offset+valueLength]
			if typ == 7 {
				h.Str = string(val)
			} else {
				h.Bytes = append([]byte(nil), val...)
			}
			offset += valueLength
		case 9:
			if err := requireBytes(16); err != nil {
				return EventFrame{}, err
			}
			h.Raw = data[offset : offset+16]
			offset += 16
		default:
			return EventFrame{}, fmt.Errorf("AWS EventStream header %s has unknown type %d", name, typ)
		}
		headers = append(headers, h)
	}

	payloadBytes := data[headerEnd : totalLength-4]
	if len(payloadBytes) == 0 {
		return EventFrame{Headers: headers, Payload: nil}, nil
	}
	// The JS parser trims and checks emptiness before JSON-decode. A whitespace-only
	// payload is treated as nil (no event payload).
	trimmed := trimSpaceASCII(payloadBytes)
	if len(trimmed) == 0 {
		return EventFrame{Headers: headers, Payload: nil}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return EventFrame{}, fmt.Errorf("AWS EventStream payload is not valid JSON (%w)", err)
	}
	return EventFrame{Headers: headers, Payload: payload}, nil
}

// trimSpaceASCII trims leading/trailing ASCII whitespace from the payload bytes,
// mirroring the JS payloadText.trim() check (kiro.js:1191-1192). JSON payloads
// are UTF-8; ASCII whitespace (space, \t, \n, \r) is the only trim set the JS
// .trim() applies that is also safe to compare against len==0.
func trimSpaceASCII(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isASCIISpace(b[start]) {
		start++
	}
	for end > start && isASCIISpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// FrameStream buffers raw EventStream bytes and yields complete frames one at a
// time. It mirrors the framing half of processBytes in kiro.js:852-911, but the
// CRC/bounds validation lives in ParseEventFrame. The caller feeds chunks via
// Push, then drains Complete frames; Remainder is the un-consumed tail (a
// partial frame split across reads) to carry into the next Push.
type FrameStream struct {
	buffer      []byte
	maxRawBytes int
}

// NewFrameStream returns a FrameStream with the protocol default max-raw-bytes
// bound (eventstreamMaxMessageBytes), mirroring processBytes' guard.
func NewFrameStream() *FrameStream {
	return &FrameStream{maxRawBytes: eventstreamMaxMessageBytes}
}

// Push appends a chunk to the buffer. Returns an error if the buffered bytes
// exceed the protocol bound (corrupt_eventstream_frame / kiro_missing_terminal),
// matching the combinedLength guard in processBytes.
func (s *FrameStream) Push(chunk []byte) error {
	s.buffer = append(s.buffer, chunk...)
	if len(s.buffer) > s.maxRawBytes {
		return errors.New("kiro EventStream buffered bytes exceed the protocol bound")
	}
	return nil
}

// Next returns the next complete frame and advances the buffer. Returns
// (nil, nil) when no complete frame is buffered yet (the caller should Push
// more bytes). Returns an error for a corrupt prelude or frame bounds — the
// caller maps that to a fail-closed terminal error, matching processBytes'
// early-return false path.
func (s *FrameStream) Next() (EventFrame, error) {
	if len(s.buffer) < 12 {
		return EventFrame{}, nil
	}
	// Prelude CRC is validated before reading totalLength so a corrupt prelude
	// is caught before bounds checks (mirrors kiro.js:871-875).
	if binary.BigEndian.Uint32(s.buffer[8:12]) != crc32(s.buffer[0:8]) {
		return EventFrame{}, errors.New("kiro EventStream prelude CRC mismatch")
	}
	totalLength := binary.BigEndian.Uint32(s.buffer[0:4])
	headersLength := binary.BigEndian.Uint32(s.buffer[4:8])
	if totalLength < 16 || totalLength > eventstreamMaxMessageBytes ||
		headersLength > eventstreamMaxHeadersBytes || headersLength > totalLength-16 {
		return EventFrame{}, errors.New("kiro EventStream frame bounds are invalid")
	}
	if uint32(len(s.buffer)) < totalLength {
		return EventFrame{}, nil // partial frame — wait for more bytes
	}
	frame := s.buffer[:totalLength]
	s.buffer = append([]byte(nil), s.buffer[totalLength:]...) // shift tail
	return ParseEventFrame(frame)
}

// Remainder returns the un-consumed buffered bytes (the partial frame tail to
// carry across reads). Used by the terminal-state machine to detect a truncated
// frame at EOF.
func (s *FrameStream) Remainder() []byte { return s.buffer }
