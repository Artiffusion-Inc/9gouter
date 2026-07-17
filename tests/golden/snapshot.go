package golden

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// snapshotString serializes a Go value into the same format Vitest uses for
// object snapshots: 2-space indentation, double-quoted keys, and trailing
// commas after every object property and array element (including the last
// within its container). The root value itself has no trailing comma.
func snapshotString(v any) string {
	var b bytes.Buffer
	writeSnapValue(&b, v, "")
	return b.String()
}

func writeSnapValue(b *bytes.Buffer, v any, indent string) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case float64:
		b.WriteString(formatFloat(x))
	case json.Number:
		b.WriteString(x.String())
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case string:
		b.WriteString(jsonString(x))
	case map[string]any:
		if len(x) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		nextIndent := indent + "  "
		keys := sortedKeys(x)
		for _, k := range keys {
			b.WriteString(nextIndent)
			b.WriteString(jsonString(k))
			b.WriteString(": ")
			writeSnapValue(b, x[k], nextIndent)
			b.WriteString(",\n")
		}
		b.WriteString(indent)
		b.WriteByte('}')
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		nextIndent := indent + "  "
		for _, item := range x {
			b.WriteString(nextIndent)
			writeSnapValue(b, item, nextIndent)
			b.WriteString(",\n")
		}
		b.WriteString(indent)
		b.WriteByte(']')
	default:
		// Fallback for anything else: marshal via encoding/json then add trailing commas.
		raw, _ := json.Marshal(x)
		b.WriteString(string(raw))
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func jsonString(s string) string {
	// Vitest snapshots are template literals: string values are written with
	// real newlines and unescaped double quotes (only backticks, backslashes and
	// ${} are escaped by Vitest). Keep actual newlines and quotes; escape
	// backslashes so the parser can reverse them safely.
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString("\\\\")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func formatFloat(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Sprintf("%v", f)
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
