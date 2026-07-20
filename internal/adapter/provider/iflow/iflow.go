// Package iflowexec ports the iFlow executor.
package iflowexec

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor with iFlow HMAC signing.
type Executor struct {
	*base.BaseExecutor
}

// New creates an iFlow executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("iflow", cfg)}
}

// BuildURL returns the static iFlow chat completions URL.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	url := e.Config.BaseURL
	if url == "" {
		url = "https://apis.iflow.cn/v1/chat/completions"
	}
	return url
}

// BuildHeaders generates the iFlow signature headers.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	sessionID := "session-" + randHex(16)
	timestamp := time.Now().UnixMilli()
	userAgent := h.Get("User-Agent")
	if userAgent == "" {
		userAgent = "iFlow-Cli"
	}
	apiKey := creds.APIKey
	if apiKey == "" {
		apiKey = creds.AccessToken
	}
	var signature string
	if apiKey != "" {
		signature = hmacSHA256(userAgent+":"+sessionID+":"+fmt.Sprintf("%d", timestamp), apiKey)
	}
	base.SetHeaderExact(h, "session-id", sessionID)
	base.SetHeaderExact(h, "x-iflow-timestamp", fmt.Sprintf("%d", timestamp))
	base.SetHeaderExact(h, "x-iflow-signature", signature)
	return h
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hmacSHA256(payload, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// TransformRequest injects stream_options for usage.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	if !stream {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	if _, ok := m["messages"]; !ok {
		return body, nil
	}
	if _, ok := m["stream_options"]; !ok {
		m["stream_options"] = map[string]any{"include_usage": true}
	}
	out, _ := json.Marshal(m)
	return out, nil
}
