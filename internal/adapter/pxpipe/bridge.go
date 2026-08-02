// Package pxpipe implements a Node subprocess bridge for the pxpipe-proxy
// library. The pxpipe transform renders bulky Claude-format context as dense
// PNGs to save tokens (Anthropic bills images by pixels, not text tokens).
//
// Since the transform is a JavaScript library (pxpipe-proxy on npm), this
// bridge invokes it via a short-lived Node subprocess: it writes the request
// body to stdin, runs a tiny inline Node script that imports the installed
// pxpipe-proxy module and calls transformAnthropicMessages, and reads the
// transformed body from stdout. Fail-open: any error returns (nil, err) and
// the chat path leaves the body untouched.
package pxpipe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// nodeScript is the inline JS that loads pxpipe-proxy, reads stdin, transforms,
// and writes the result to stdout as JSON.
const nodeScript = `
import { createRequire } from "module";
const require = createRequire(import.meta.url);
let mod;
try {
  mod = require("pxpipe-proxy");
} catch (e) {
  console.log(JSON.stringify({ ok: false, error: "pxpipe-proxy not installed" }));
  process.exit(0);
}
if (typeof mod.transformAnthropicMessages !== "function") {
  console.log(JSON.stringify({ ok: false, error: "pxpipe-proxy missing transformAnthropicMessages" }));
  process.exit(0);
}
let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (d) => input += d);
process.stdin.on("end", async () => {
  try {
    const parsed = JSON.parse(input);
    const result = await mod.transformAnthropicMessages({
      body: Buffer.from(parsed.body, "base64"),
      model: parsed.model,
      options: { minCompressChars: parsed.minChars }
    });
    if (!result || !result.applied) {
      console.log(JSON.stringify({ ok: true, applied: false, reason: result?.reason || "passthrough" }));
    } else {
      console.log(JSON.stringify({
        ok: true, applied: true,
        body: Buffer.from(result.body).toString("base64"),
        info: { imageCount: result.info?.imageCount || 0, imageBytes: result.info?.imageBytes || 0, compressedChars: result.info?.compressedChars || 0 }
      }));
    }
  } catch (e) {
    console.log(JSON.stringify({ ok: false, error: e.message }));
  }
});
`

// Transform calls the pxpipe-proxy library via a Node subprocess to transform
// a Claude-format request body. Returns the transformed body bytes when the
// transform was applied, or (nil, nil) when it was skipped (below threshold,
// passthrough). The caller (runPxpipe) treats nil as "no change".
//
// nodeBin is the path to the node binary (empty → "node" from PATH).
// minChars is the minimum body size (in chars) to consider compressing.
func Transform(body []byte, model string, minChars int, nodeBin string, timeoutMs int) ([]byte, error) {
	if len(body) == 0 {
		return nil, nil
	}

	// Encode the body as base64 for safe JSON transport over stdin.
	encoded := base64.StdEncoding.EncodeToString(body)
	input := map[string]any{
		"body":     encoded,
		"model":    model,
		"minChars": minChars,
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("pxpipe: marshal input: %w", err)
	}

	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	bin := nodeBin
	if bin == "" {
		bin = "node"
	}

	cmd := exec.CommandContext(ctx, bin, "--input-type=module", "-e", nodeScript)
	cmd.Stdin = bytes.NewReader(inputJSON)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("pxpipe: timeout after %dms", timeoutMs)
		}
		return nil, fmt.Errorf("pxpipe: node subprocess: %w", err)
	}

	var result struct {
		OK      bool   `json:"ok"`
		Applied bool   `json:"applied"`
		Body    string `json:"body"`
		Error   string `json:"error"`
		Reason  string `json:"reason"`
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("pxpipe: parse node output: %w (stdout=%s)", err, stdout.String())
	}
	if !result.OK {
		return nil, fmt.Errorf("pxpipe: %s", result.Error)
	}
	if !result.Applied {
		return nil, nil
	}

	// Decode the base64 body back to bytes.
	decoded, err := base64.StdEncoding.DecodeString(result.Body)
	if err != nil {
		return nil, fmt.Errorf("pxpipe: decode body: %w", err)
	}
	return decoded, nil
}
