package golden

import (
	"bytes"
	"embed"
	"io"
	"regexp"
	"strings"
)

var snapEntryRe = regexp.MustCompile(`(?ms)^exports\[` + "`" + `([^` + "`" + `]+)` + "`" + `\] = ` + "`" + `(.*?)` + "`" + `;?\s*$`)

// parseSnapFile reads a Vitest .snap file and returns map[snapshotName]body.
// It handles the escaped backticks (`) and backslashes used by Jest/Vitest
// snapshot serialization inside template-literal bodies.
func parseSnapFile(fs embed.FS, name string) (map[string]string, error) {
	f, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return parseSnapBytes(data), nil
}

func parseSnapBytes(data []byte) map[string]string {
	out := make(map[string]string)
	for _, m := range snapEntryRe.FindAllSubmatch(data, -1) {
		name := string(m[1])
		body := strings.TrimSpace(string(unescapeSnapBody(m[2])))
		out[name] = body
	}
	return out
}

// unescapeSnapBody reverses Vitest's snapshot escaping:
//   \\  -> \
//   \`  -> `
//   \${ -> ${
func unescapeSnapBody(body []byte) []byte {
	// Vitest escapes backslashes first, then backticks, then ${ so the reverse
	// must be careful. We do a simple state-machine scan.
	var b bytes.Buffer
	b.Grow(len(body))
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\\' && i+1 < len(body) {
			next := body[i+1]
			switch next {
			case '\\':
				b.WriteByte('\\')
				i++
			case '`':
				b.WriteByte('`')
				i++
			case '$':
				if i+2 < len(body) && body[i+2] == '{' {
					b.WriteString("${")
					i += 2
				} else {
					b.WriteByte(c)
				}
			default:
				b.WriteByte(c)
			}
		} else {
			b.WriteByte(c)
		}
	}
	return b.Bytes()
}

