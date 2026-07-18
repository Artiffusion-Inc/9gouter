// shadowdiff mirrors traffic to a primary (legacy JS) backend and a shadow (Go)
// backend, then diffs status, headers, SSE event sequence, and usage rows.
//
// Configuration via environment variables:
//   SHADOWDIFF_LISTEN      - listen address (default :8080)
//   SHADOWDIFF_PRIMARY     - primary backend URL (e.g. http://127.0.0.1:20128)
//   SHADOWDIFF_SHADOW      - shadow backend URL (e.g. http://127.0.0.1:20127)
//   SHADOWDIFF_MISMATCH_LOG - optional JSONL file path for mismatch records
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxBodySize = 64 << 20 // 64 MiB
	shadowTimeout      = 5 * time.Minute
)

var ignoredHeaders = map[string]bool{
	"connection":        true,
	"content-length":    true,
	"date":              true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"request-id":        true,
	"set-cookie":        true,
	"te":                true,
	"trailer":           true,
	"transfer-encoding": true,
	"upgrade":           true,
	"x-request-id":      true,
}

type cfg struct {
	Listen      string
	Primary     string
	Shadow      string
	MismatchLog string
}

func loadConfig() cfg {
	c := cfg{
		Listen:      os.Getenv("SHADOWDIFF_LISTEN"),
		Primary:     os.Getenv("SHADOWDIFF_PRIMARY"),
		Shadow:      os.Getenv("SHADOWDIFF_SHADOW"),
		MismatchLog: os.Getenv("SHADOWDIFF_MISMATCH_LOG"),
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	return c
}

type captured struct {
	status  int
	headers http.Header
	body    []byte
	err     error
}

type record struct {
	TS          string       `json:"ts"`
	Method      string       `json:"method"`
	Path        string       `json:"path"`
	StatusDiff  *statusDiff  `json:"status_diff,omitempty"`
	HeaderDiff  *headerDiff  `json:"header_diff,omitempty"`
	SSEDiff     *sseDiff     `json:"sse_diff,omitempty"`
	UsageDiff   *usageDiff   `json:"usage_diff,omitempty"`
	ShadowError string       `json:"shadow_error,omitempty"`
}

type statusDiff struct {
	Primary int `json:"primary"`
	Shadow  int `json:"shadow"`
}

type headerDiff struct {
	PrimaryOnly   []string `json:"primary_only,omitempty"`
	ShadowOnly    []string `json:"shadow_only,omitempty"`
	ValueMismatch []string `json:"value_mismatch,omitempty"`
}

type sseDiff struct {
	PrimaryEvents   int             `json:"primary_events"`
	ShadowEvents    int             `json:"shadow_events"`
	EventMismatches []eventMismatch `json:"event_mismatches,omitempty"`
}

type eventMismatch struct {
	Index   int    `json:"index"`
	Primary string `json:"primary,omitempty"`
	Shadow  string `json:"shadow,omitempty"`
}

type usageDiff struct {
	Primary any  `json:"primary"`
	Shadow  any  `json:"shadow"`
	Match   bool `json:"match"`
}

type event struct {
	Name string
	Data []string
}

func main() {
	config := loadConfig()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if config.Primary == "" || config.Shadow == "" {
		logger.Error("SHADOWDIFF_PRIMARY and SHADOWDIFF_SHADOW are required")
		os.Exit(1)
	}

	primaryURL, err := url.Parse(config.Primary)
	if err != nil {
		logger.Error("invalid primary URL", "err", err)
		os.Exit(1)
	}
	shadowURL, err := url.Parse(config.Shadow)
	if err != nil {
		logger.Error("invalid shadow URL", "err", err)
		os.Exit(1)
	}

	var logMu sync.Mutex
	var mismatchFile *os.File
	if config.MismatchLog != "" {
		f, err := os.OpenFile(config.MismatchLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			logger.Error("cannot open mismatch log", "path", config.MismatchLog, "err", err)
			os.Exit(1)
		}
		defer f.Close()
		mismatchFile = f
	}

	writeMismatch := func(rec record) {
		b, err := json.Marshal(rec)
		if err != nil {
			return
		}
		fmt.Fprintln(os.Stdout, string(b))
		if mismatchFile != nil {
			logMu.Lock()
			_, _ = mismatchFile.Write(b)
			_, _ = mismatchFile.WriteString("\n")
			logMu.Unlock()
		}
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handle(logger, w, r, primaryURL, shadowURL, writeMismatch)
	})

	logger.Info("shadowdiff listening",
		"addr", config.Listen,
		"primary", config.Primary,
		"shadow", config.Shadow,
		"mismatch_log", config.MismatchLog,
	)
	if err := http.ListenAndServe(config.Listen, nil); err != nil {
		logger.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func handle(logger *slog.Logger, w http.ResponseWriter, r *http.Request, primaryURL, shadowURL *url.URL, writeMismatch func(record)) {
	body, err := io.ReadAll(io.LimitReader(r.Body, defaultMaxBodySize+1))
	if err != nil {
		http.Error(w, "reading request body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tooLarge := len(body) > defaultMaxBodySize

	// Primary: proxy the response back to the client as it streams.
	primaryReq := cloneRequest(r, primaryURL, body)
	primaryResp, err := noRedirectClient().Do(primaryReq)
	if err != nil {
		http.Error(w, "primary error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer primaryResp.Body.Close()

	for k, vv := range primaryResp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(primaryResp.StatusCode)

	var primaryBuf bytes.Buffer
	_, copyErr := io.Copy(w, io.TeeReader(primaryResp.Body, &primaryBuf))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if copyErr != nil {
		logger.Warn("primary response stream error", "path", r.URL.Path, "err", copyErr)
	}

	// Shadow: send the same request in the background, then compare.
	shadow := captured{}
	if tooLarge {
		shadow.err = fmt.Errorf("request body exceeds %d bytes; shadow skipped", defaultMaxBodySize)
	} else {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), shadowTimeout)
			defer cancel()
			req := cloneRequest(r.WithContext(ctx), shadowURL, body)
			resp, err := noRedirectClientWithTimeout(shadowTimeout).Do(req)
			if err != nil {
				shadow.err = err
				return
			}
			defer resp.Body.Close()
			shadow.status = resp.StatusCode
			shadow.headers = resp.Header.Clone()
			shadow.body, _ = io.ReadAll(resp.Body)
		}()
		wg.Wait()
	}

	primary := captured{
		status:  primaryResp.StatusCode,
		headers: primaryResp.Header.Clone(),
		body:    primaryBuf.Bytes(),
	}

	if rec := compare(r.Method, r.URL.Path, &primary, &shadow); rec != nil {
		writeMismatch(*rec)
	}
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func noRedirectClientWithTimeout(timeout time.Duration) *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: timeout,
	}
}

func cloneRequest(r *http.Request, target *url.URL, body []byte) *http.Request {
	u := *target
	u.Path = singleJoiningSlash(u.Path, r.URL.Path)
	if r.URL.RawPath != "" {
		u.RawPath = singleJoiningSlash(u.RawPath, r.URL.RawPath)
	}
	u.RawQuery = r.URL.RawQuery

	out := r.Clone(r.Context())
	out.URL = &u
	out.Host = r.Host
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))

	if out.Header == nil {
		out.Header = make(http.Header)
	}
	for _, h := range []string{"Connection", "Proxy-Connection", "Transfer-Encoding", "Upgrade", "Keep-Alive", "TE", "Trailer"} {
		out.Header.Del(h)
	}
	out.Header.Del("Host")
	return out
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case aslash || bslash:
		return a + b
	}
	return a + "/" + b
}

func compare(method, path string, primary, shadow *captured) *record {
	rec := &record{
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		Method: method,
		Path:   path,
	}
	hasDiff := false

	if shadow.err != nil {
		rec.ShadowError = shadow.err.Error()
		hasDiff = true
	} else if primary.status != shadow.status {
		rec.StatusDiff = &statusDiff{Primary: primary.status, Shadow: shadow.status}
		hasDiff = true
	}

	ph := headerMap(primary.headers)
	sh := headerMap(shadow.headers)
	if hd := diffHeaders(ph, sh); hd != nil {
		rec.HeaderDiff = hd
		hasDiff = true
	}

	isSSE := strings.Contains(primary.headers.Get("Content-Type"), "text/event-stream") ||
		(shadow.headers != nil && strings.Contains(shadow.headers.Get("Content-Type"), "text/event-stream"))

	var pEvents, sEvents []event
	if isSSE {
		pEvents = parseSSE(primary.body)
		sEvents = parseSSE(shadow.body)
		if sd := diffSSE(pEvents, sEvents); sd != nil {
			rec.SSEDiff = sd
			hasDiff = true
		}
	}

	pUsage, pHas := extractUsage(primary.body, pEvents)
	sUsage, sHas := extractUsage(shadow.body, sEvents)
	if pHas || sHas {
		match := jsonEqual(pUsage, sUsage) && pHas == sHas
		rec.UsageDiff = &usageDiff{Primary: pUsage, Shadow: sUsage, Match: match}
		if !match {
			hasDiff = true
		}
	}

	if !hasDiff {
		return nil
	}
	return rec
}

func headerMap(h http.Header) map[string][]string {
	if h == nil {
		return nil
	}
	m := make(map[string][]string)
	for k, vv := range h {
		kl := strings.ToLower(k)
		if ignoredHeaders[kl] {
			continue
		}
		sorted := append([]string(nil), vv...)
		sort.Strings(sorted)
		m[kl] = sorted
	}
	return m
}

func diffHeaders(primary, shadow map[string][]string) *headerDiff {
	d := &headerDiff{}
	for k, pv := range primary {
		sv, ok := shadow[k]
		if !ok {
			d.PrimaryOnly = append(d.PrimaryOnly, k)
		} else if !slicesEqual(pv, sv) {
			d.ValueMismatch = append(d.ValueMismatch, k)
		}
	}
	for k := range shadow {
		if _, ok := primary[k]; !ok {
			d.ShadowOnly = append(d.ShadowOnly, k)
		}
	}
	sort.Strings(d.PrimaryOnly)
	sort.Strings(d.ShadowOnly)
	sort.Strings(d.ValueMismatch)
	if len(d.PrimaryOnly)+len(d.ShadowOnly)+len(d.ValueMismatch) == 0 {
		return nil
	}
	return d
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func parseSSE(data []byte) []event {
	var events []event
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Split(scanSSEEvent)
	for sc.Scan() {
		ev := parseEvent(sc.Bytes())
		if ev.Name != "" || len(ev.Data) > 0 {
			events = append(events, ev)
		}
	}
	return events
}

func scanSSEEvent(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func parseEvent(blk []byte) event {
	ev := event{}
	sc := bufio.NewScanner(bytes.NewReader(blk))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			ev.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			ev.Data = append(ev.Data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return ev
}

func normalizeEventData(data []string) []string {
	out := make([]string, 0, len(data))
	for _, d := range data {
		var m map[string]any
		if err := json.Unmarshal([]byte(d), &m); err == nil {
			delete(m, "created")
			b, _ := json.Marshal(m)
			out = append(out, string(b))
		} else {
			out = append(out, d)
		}
	}
	return out
}

func eventKey(ev event) string {
	parts := []string{ev.Name}
	parts = append(parts, normalizeEventData(ev.Data)...)
	return strings.Join(parts, "|")
}

func diffSSE(primary, shadow []event) *sseDiff {
	d := &sseDiff{
		PrimaryEvents: len(primary),
		ShadowEvents:  len(shadow),
	}
	max := len(primary)
	if len(shadow) > max {
		max = len(shadow)
	}
	for i := 0; i < max; i++ {
		var pk, sk string
		if i < len(primary) {
			pk = eventKey(primary[i])
		}
		if i < len(shadow) {
			sk = eventKey(shadow[i])
		}
		if pk != sk {
			d.EventMismatches = append(d.EventMismatches, eventMismatch{Index: i, Primary: pk, Shadow: sk})
		}
	}
	if len(d.EventMismatches) > 0 || len(primary) != len(shadow) {
		return d
	}
	return nil
}

func extractUsage(body []byte, events []event) (any, bool) {
	if len(events) > 0 {
		for _, ev := range events {
			for _, d := range ev.Data {
				var m map[string]any
				if err := json.Unmarshal([]byte(d), &m); err != nil {
					continue
				}
				if u, ok := m["usage"]; ok {
					return u, true
				}
			}
		}
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false
	}
	if u, ok := m["usage"]; ok {
		return u, true
	}
	return nil, false
}

func jsonEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}
