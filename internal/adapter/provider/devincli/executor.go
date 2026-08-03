// Package devincli implements the Devin CLI executor: routes completions
// through the official Devin CLI binary via the Agent Client Protocol (ACP)
// JSON-RPC 2.0 over stdio. Ports open-sse/executors/devin-cli.js (upstream 3b14bf4a).
package devincli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Executor struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) *Executor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Executor{logger: logger}
}

func (e *Executor) Execute(ctx context.Context, model string, body json.RawMessage, stream bool) (io.ReadCloser, http.Header, error) {
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, nil, fmt.Errorf("devin: unmarshal body: %w", err)
	}

	messages := extractMessages(b)
	promptText := buildPromptText(messages)
	workspaceCwd := resolveWorkspaceCwd(b)
	devinBin := resolveDevinBin()

	e.logger.Info("devin acp", "model", model, "bin", devinBin, "cwd", workspaceCwd)

	env := os.Environ()
	env = append(env, "DEVIN_PERMISSION_MODE=bypass")

	agentType := strings.TrimSpace(os.Getenv("CLI_DEVIN_AGENT_TYPE"))
	acpArgs := []string{"acp"}
	if agentType != "" {
		acpArgs = append(acpArgs, "--agent-type", agentType)
	}

	cmd := exec.CommandContext(ctx, devinBin, acpArgs...)
	cmd.Env = env
	cmd.Dir = workspaceCwd

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("devin: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("devin: stdout pipe: %w", err)
	}
	cmd.Stderr = &devinLogWriter{logger: e.logger}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("devin: start %s: %w", devinBin, err)
	}

	pr, pw := io.Pipe()
	headers := http.Header{
		"Content-Type":  []string{"text/event-stream"},
		"Cache-Control": []string{"no-cache"},
		"Connection":    []string{"keep-alive"},
	}

	go func() {
		defer pw.Close()
		defer cmd.Wait()

		state := &acpState{
			model:      model,
			promptText: promptText,
			stdin:      stdin,
			pw:         pw,
			logger:     e.logger,
			responseID: fmt.Sprintf("chatcmpl-devin-%d", time.Now().UnixMilli()),
			created:    time.Now().Unix(),
		}

		state.sendRPC("initialize", map[string]any{
			"protocolVersion": "0.3",
			"clientInfo":      map[string]any{"name": "9gouter", "version": "1.0"},
			"capabilities":    map[string]any{},
		}, nil)

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var msg map[string]any
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			if done := state.handleMessage(msg); done {
				break
			}
		}

		if !state.finished {
			state.finish("", "stop")
		}
	}()

	return pr, headers, nil
}

type acpState struct {
	mu         sync.Mutex
	model      string
	promptText string
	stdin      io.WriteCloser
	pw         *io.PipeWriter
	logger     *slog.Logger
	responseID string
	created    int64

	idCounter      int
	initDone       bool
	sessionCreated bool
	sessionID      string
	promptSent     bool
	roleEmitted    bool
	totalText      string
	finished       bool
}

func (s *acpState) sendRPC(method string, params map[string]any, id *int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != nil {
		s.idCounter++
		msg["id"] = s.idCounter
		*id = s.idCounter
	}
	data, _ := json.Marshal(msg)
	s.stdin.Write(append(data, '\n'))
}

func (s *acpState) emit(data string) {
	s.pw.Write([]byte(data))
}

func (s *acpState) emitDelta(delta string) {
	if !s.roleEmitted {
		s.emit("data: " + mustMarshal(map[string]any{
			"id": s.responseID, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
		}) + "\n\n")
		s.roleEmitted = true
	}
	s.totalText += delta
	s.emit("data: " + mustMarshal(map[string]any{
		"id": s.responseID, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil}},
	}) + "\n\n")
}

func (s *acpState) finish(errMsg string, finishReason string) {
	if s.finished {
		return
	}
	s.finished = true

	if errMsg != "" {
		s.emit("data: " + mustMarshal(map[string]any{
			"error": map[string]any{"message": errMsg, "type": "devin_cli_error"},
		}) + "\n\n")
	} else {
		s.emit("data: " + mustMarshal(map[string]any{
			"id": s.responseID, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
			"usage": map[string]any{
				"prompt_tokens": (len(s.promptText) + 3) / 4, "completion_tokens": (len(s.totalText) + 3) / 4,
				"total_tokens": (len(s.promptText) + len(s.totalText) + 3) / 4, "estimated": true,
			},
		}) + "\n\n")
	}
	s.emit("data: [DONE]\n\n")
	s.stdin.Close()
}

func (s *acpState) handleMessage(msg map[string]any) bool {
	if !s.initDone && msg["result"] != nil && msg["method"] == nil {
		s.initDone = true
		s.sendRPC("session/new", map[string]any{"cwd": "", "mcpServers": []any{}, "model": s.model}, nil)
		return false
	}

	if s.initDone && !s.sessionCreated && msg["result"] != nil && msg["method"] == nil {
		res, _ := msg["result"].(map[string]any)
		sessionID, _ := res["sessionId"].(string)
		s.sessionID = sessionID
		if s.sessionID == "" {
			s.finish("Devin ACP: session/new returned no sessionId", "stop")
			return true
		}
		s.sessionCreated = true
		s.promptSent = true
		s.sendRPC("session/prompt", map[string]any{
			"sessionId": s.sessionID,
			"prompt":    []any{map[string]any{"type": "text", "text": s.promptText}},
		}, nil)
		return false
	}

	if s.sessionCreated && s.promptSent && msg["result"] != nil && msg["method"] == nil {
		if !s.roleEmitted {
			res, _ := msg["result"].(map[string]any)
			if content := extractResultText(res); content != "" {
				s.totalText = content
				s.emitDelta(content)
			}
		}
		return false
	}

	if method, _ := msg["method"].(string); method == "session/request_permission" {
		id := msg["id"]
		params, _ := msg["params"].(map[string]any)
		options, _ := params["options"].([]any)
		var allowOption map[string]any
		for _, o := range options {
			if om, ok := o.(map[string]any); ok {
				if kind, _ := om["kind"].(string); strings.Contains(strings.ToLower(kind), "allow") {
					allowOption = om
					break
				}
			}
		}
		if allowOption == nil && len(options) > 0 {
			allowOption, _ = options[0].(map[string]any)
		}
		if allowOption != nil {
			resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": allowOption["optionId"]}}}
			data, _ := json.Marshal(resp)
			s.stdin.Write(append(data, '\n'))
		}
		return false
	}

	if method, _ := msg["method"].(string); method == "_cognition.ai/agent_stopped" || method == "$/agent_stopped" {
		params, _ := msg["params"].(map[string]any)
		cause, _ := params["cause"].(string)
		if cause == "error" {
			errText := "Devin agent error"
			if m, ok := params["errorMessage"].(string); ok && m != "" {
				errText = m
			}
			s.finish(errText, "stop")
		} else {
			s.finish("", "stop")
		}
		return true
	}

	if method, _ := msg["method"].(string); method == "session/update" || method == "$/update" {
		params, _ := msg["params"].(map[string]any)
		if params == nil {
			return false
		}
		update, _ := params["update"].(map[string]any)
		if update == nil {
			update = map[string]any{}
		}
		updateType, _ := update["sessionUpdate"].(string)
		if updateType == "" {
			updateType, _ = params["type"].(string)
		}
		var deltaText string
		if content, ok := update["content"]; ok {
			if s, ok := content.(string); ok {
				deltaText = s
			} else if cm, ok := content.(map[string]any); ok {
				if t, ok := cm["text"].(string); ok {
					deltaText = t
				}
			}
		} else if d, ok := params["delta"].(string); ok {
			deltaText = d
		} else if t, ok := params["text"].(string); ok {
			deltaText = t
		}

		switch updateType {
		case "agent_message_chunk", "message_delta", "text_delta", "content_delta":
			if deltaText != "" {
				s.emitDelta(deltaText)
			}
		case "agent_thought_chunk":
		case "message_stop", "stop", "done":
			s.finish("", "stop")
			return true
		case "error":
			errText := "Devin ACP error"
			if m, ok := params["message"].(string); ok && m != "" {
				errText = m
			}
			s.finish(errText, "stop")
			return true
		}
		return false
	}

	if msg["error"] != nil {
		errObj, _ := msg["error"].(map[string]any)
		s.finish(fmt.Sprintf("Devin ACP error %v: %s", errObj["code"], toStr(errObj["message"])), "stop")
		return true
	}

	return false
}

func resolveDevinBin() string {
	if envBin := strings.TrimSpace(os.Getenv("CLI_DEVIN_BIN")); envBin != "" {
		return envBin
	}
	home, _ := os.UserHomeDir()
	var candidates []string
	if runtime.GOOS == "windows" {
		localApp := os.Getenv("LOCALAPPDATA")
		if localApp == "" {
			localApp = filepath.Join(home, "AppData", "Local")
		}
		candidates = []string{
			filepath.Join(localApp, "devin", "cli", "bin", "devin.exe"),
			filepath.Join(home, ".local", "bin", "devin.exe"),
		}
	} else {
		candidates = []string{
			filepath.Join(home, ".local", "share", "devin", "bin", "devin"),
			filepath.Join(home, ".devin", "bin", "devin"),
			filepath.Join(home, ".local", "bin", "devin"),
			"/opt/homebrew/bin/devin",
			"/usr/local/bin/devin",
			"/usr/bin/devin",
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if runtime.GOOS == "windows" {
		return "devin.exe"
	}
	return "devin"
}

func extractMessages(body map[string]any) []map[string]any {
	var messages []map[string]any
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				messages = append(messages, mm)
			}
		}
	} else if input, ok := body["input"].([]any); ok {
		for _, m := range input {
			if mm, ok := m.(map[string]any); ok {
				messages = append(messages, mm)
			}
		}
	}
	return messages
}

func buildPromptText(messages []map[string]any) string {
	var lines []string
	for _, m := range messages {
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		text := ""
		switch c := m["content"].(type) {
		case string:
			text = c
		case []any:
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					switch pm["type"] {
					case "text":
						text += toStr(pm["text"])
					case "tool_use":
						text += fmt.Sprintf("\n[Tool call %s id=%s]\n%s\n", toStr(pm["name"]), toStr(pm["id"]), mustMarshal(pm["input"]))
					case "tool_result":
						text += fmt.Sprintf("\n[Tool result id=%s]\n%s\n", toStr(pm["tool_use_id"]), toStr(pm["content"]))
					}
				}
			}
		}
		if role == "assistant" {
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				parts := []string{}
				for _, tc := range tcs {
					if tcm, ok := tc.(map[string]any); ok {
						fn, _ := tcm["function"].(map[string]any)
						parts = append(parts, fmt.Sprintf("[Tool call %s id=%s]\n%s", toStr(fn["name"]), toStr(tcm["id"]), toStr(fn["arguments"])))
					}
				}
				if text != "" {
					parts = append([]string{text}, parts...)
				}
				text = strings.Join(parts, "\n\n")
			}
		}
		if role == "tool" {
			text = fmt.Sprintf("[Tool result id=%s]\n%s", toStr(m["tool_call_id"]), toStr(m["content"]))
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		switch role {
		case "system":
			lines = append(lines, "[System]\n"+text)
		case "assistant":
			lines = append(lines, "[Assistant]\n"+text)
		case "tool":
			lines = append(lines, "[Tool]\n"+text)
		default:
			lines = append(lines, "[User]\n"+text)
		}
	}
	if len(lines) == 0 {
		return "(empty)"
	}
	return strings.Join(lines, "\n\n")
}

func resolveWorkspaceCwd(body map[string]any) string {
	candidates := []string{}
	add := func(v any) {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			candidates = append(candidates, strings.TrimSpace(s))
		}
	}
	add(body["cwd"])
	add(body["working_directory"])
	add(body["workdir"])
	if meta, ok := body["metadata"].(map[string]any); ok {
		add(meta["cwd"])
	}
	for _, c := range candidates {
		if filepath.IsAbs(c) {
			if info, err := os.Stat(c); err == nil && info.IsDir() {
				return c
			}
		}
	}
	tmp, _ := os.MkdirTemp("", "devin-*")
	return tmp
}

func extractResultText(result map[string]any) string {
	if s, ok := result["content"].(string); ok {
		return s
	}
	if s, ok := result["text"].(string); ok {
		return s
	}
	if msg, ok := result["message"].(map[string]any); ok {
		if s, ok := msg["content"].(string); ok {
			return s
		}
	}
	if msgs, ok := result["messages"].([]any); ok {
		var texts []string
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if role, _ := mm["role"].(string); role == "assistant" {
					texts = append(texts, toStr(mm["content"]))
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func mustMarshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func toStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return mustMarshal(v)
}

type devinLogWriter struct {
	logger *slog.Logger
}

func (w *devinLogWriter) Write(p []byte) (int, error) {
	w.logger.Debug("devin stderr", "msg", strings.TrimSpace(string(p)))
	return len(p), nil
}
