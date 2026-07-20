package managedashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

var httpValidateTimeout = 10 * time.Second

// NodeService exposes provider node CRUD and cascading connection updates.
type NodeService struct {
	Repo interface {
		List(ctx context.Context, filter repo.NodeFilter) ([]settings.ProviderNode, error)
		GetByID(ctx context.Context, id string) (*settings.ProviderNode, error)
		Create(ctx context.Context, n settings.ProviderNode) error
		Update(ctx context.Context, n settings.ProviderNode) error
		Delete(ctx context.Context, id string) error
	}
	ConnRepo interface {
		List(ctx context.Context, filter repo.ConnectionFilter) ([]settings.ProviderConnection, error)
		Update(ctx context.Context, c settings.ProviderConnection) error
		DeleteByProvider(ctx context.Context, provider string) (int64, error)
	}
}

// List returns all provider nodes.
func (s *NodeService) List(ctx context.Context) ([]settings.ProviderNode, error) {
	return s.Repo.List(ctx, repo.NodeFilter{})
}

// NodeCreateRequest is the validated shape for creating a node.
type NodeCreateRequest struct {
	Name    string `json:"name"`
	Prefix  string `json:"prefix"`
	APIType string `json:"apiType"`
	BaseURL string `json:"baseUrl"`
	Type    string `json:"type"`
}

// Create builds a provider node from the request and persists it.
func (s *NodeService) Create(ctx context.Context, r NodeCreateRequest) (*settings.ProviderNode, error) {
	name := strings.TrimSpace(r.Name)
	prefix := strings.TrimSpace(r.Prefix)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if prefix == "" {
		return nil, fmt.Errorf("prefix is required")
	}
	nodeType := strings.TrimSpace(r.Type)
	if nodeType == "" {
		nodeType = "openai-compatible"
	}

	data := map[string]any{
		"prefix":  prefix,
		"baseUrl": "",
	}
	var id string

	switch nodeType {
	case "openai-compatible":
		apiType := r.APIType
		if apiType != "chat" && apiType != "responses" {
			return nil, fmt.Errorf("invalid OpenAI compatible API type")
		}
		id = fmt.Sprintf("9gouter-openai-compatible-%s-%s", apiType, generateShortID())
		data["apiType"] = apiType
		data["baseUrl"] = strings.TrimSpace(stringsOr(r.BaseURL, "https://api.openai.com/v1"))
	case "custom-embedding":
		id = fmt.Sprintf("9gouter-custom-embedding-%s", generateShortID())
		data["baseUrl"] = sanitizeEmbeddingBaseURL(r.BaseURL)
	case "anthropic-compatible":
		id = fmt.Sprintf("9gouter-anthropic-compatible-%s", generateShortID())
		data["baseUrl"] = sanitizeAnthropicBaseURL(r.BaseURL)
	default:
		return nil, fmt.Errorf("invalid provider node type")
	}

	node := settings.ProviderNode{
		ID:   id,
		Type: nodeType,
		Name: name,
		Data: jsonBytes(data),
	}
	if err := s.Repo.Create(ctx, node); err != nil {
		return nil, err
	}
	return &node, nil
}

// NodeUpdateRequest is the validated shape for updating a node.
type NodeUpdateRequest struct {
	Name    string `json:"name"`
	Prefix  string `json:"prefix"`
	APIType string `json:"apiType"`
	BaseURL string `json:"baseUrl"`
}

// Update modifies an existing provider node and propagates prefix/baseUrl/apiType/nodeName to linked connections.
func (s *NodeService) Update(ctx context.Context, id string, r NodeUpdateRequest) (*settings.ProviderNode, error) {
	node, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, fmt.Errorf("provider node not found")
	}

	name := strings.TrimSpace(r.Name)
	prefix := strings.TrimSpace(r.Prefix)
	baseURL := strings.TrimSpace(r.BaseURL)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if prefix == "" {
		return nil, fmt.Errorf("prefix is required")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}

	data := map[string]any{}
	_ = json.Unmarshal(node.Data, &data)
	data["prefix"] = prefix

	switch node.Type {
	case "openai-compatible":
		apiType := r.APIType
		if apiType != "chat" && apiType != "responses" {
			return nil, fmt.Errorf("invalid OpenAI compatible API type")
		}
		data["apiType"] = apiType
		data["baseUrl"] = baseURL
	case "custom-embedding":
		data["baseUrl"] = sanitizeEmbeddingBaseURL(baseURL)
	case "anthropic-compatible":
		data["baseUrl"] = sanitizeAnthropicBaseURL(baseURL)
	default:
		data["baseUrl"] = baseURL
	}

	node.Name = name
	node.Data = jsonBytes(data)
	if err := s.Repo.Update(ctx, *node); err != nil {
		return nil, err
	}

	conns, err := s.ConnRepo.List(ctx, repo.ConnectionFilter{Provider: id})
	if err != nil {
		return nil, err
	}
	for _, c := range conns {
		cdata := map[string]any{}
		_ = json.Unmarshal(c.Data, &cdata)
		psd := map[string]any{}
		if v, ok := cdata["providerSpecificData"].(map[string]any); ok {
			psd = v
		}
		psd["prefix"] = prefix
		psd["baseUrl"] = data["baseUrl"]
		if node.Type == "openai-compatible" {
			psd["apiType"] = data["apiType"]
		} else {
			delete(psd, "apiType")
		}
		psd["nodeName"] = name
		cdata["providerSpecificData"] = psd
		c.Data = jsonBytes(cdata)
		if err := s.ConnRepo.Update(ctx, c); err != nil {
			return nil, err
		}
	}
	return node, nil
}

// Delete removes a node and all connections associated with it.
func (s *NodeService) Delete(ctx context.Context, id string) error {
	node, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("provider node not found")
	}
	if _, err := s.ConnRepo.DeleteByProvider(ctx, id); err != nil {
		return err
	}
	return s.Repo.Delete(ctx, id)
}

// ValidateRequest holds the fields for validating a node base URL + API key.
type ValidateRequest struct {
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
	Type    string `json:"type"`
	ModelID string `json:"modelId"`
}

// ValidateResult reports whether a base URL + API key combination is usable.
type ValidateResult struct {
	Valid    bool   `json:"valid"`
	Method   string `json:"method,omitempty"`
	Error    string `json:"error,omitempty"`
	Dimensions *int  `json:"dimensions,omitempty"`
}

// Validate tests an upstream endpoint. For local callers, private/internal
// URLs are allowed; otherwise only public endpoints are permitted.
func (s *NodeService) Validate(ctx context.Context, r ValidateRequest, isLocal bool) (ValidateResult, error) {
	baseURL := strings.TrimSpace(r.BaseURL)
	apiKey := strings.TrimSpace(r.APIKey)
	if baseURL == "" || apiKey == "" {
		return ValidateResult{Valid: false, Error: "Base URL and API key required"}, nil
	}
	if _, err := url.Parse(baseURL); err != nil {
		return ValidateResult{Valid: false, Error: "Invalid URL format"}, nil
	}
	if !isLocal && !isPublicURL(baseURL) {
		return ValidateResult{Valid: false, Error: "URL not allowed"}, nil
	}

	client := &http.Client{Timeout: httpValidateTimeout}

	switch strings.TrimSpace(r.Type) {
	case "custom-embedding":
		return validateEmbedding(ctx, client, baseURL, apiKey, strings.TrimSpace(r.ModelID))
	case "anthropic-compatible":
		return validateAnthropic(ctx, client, baseURL, apiKey, strings.TrimSpace(r.ModelID))
	default:
		return validateOpenAI(ctx, client, baseURL, apiKey, strings.TrimSpace(r.ModelID))
	}
}

func validateEmbedding(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) (ValidateResult, error) {
	if modelID == "" {
		return ValidateResult{Valid: false, Error: "Model ID required for embedding validation"}, nil
	}
	u := strings.TrimRight(baseURL, "/") + "/embeddings"
	body := mustJSON(map[string]any{"model": modelID, "input": "ping"})
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(body))
	if err != nil {
		return ValidateResult{Valid: false, Error: err.Error()}, nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return ValidateResult{Valid: false, Error: networkError(err)}, nil
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		var data struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		_ = json.NewDecoder(res.Body).Decode(&data)
		dims := 0
		if len(data.Data) > 0 && data.Data[0].Embedding != nil {
			dims = len(data.Data[0].Embedding)
		}
		return ValidateResult{Valid: true, Method: "embeddings", Dimensions: &dims}, nil
	}
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return ValidateResult{Valid: false, Error: "API key unauthorized", Method: "embeddings"}, nil
	}
	return ValidateResult{Valid: false, Error: fmt.Sprintf("Embeddings request failed (%d)", res.StatusCode), Method: "embeddings"}, nil
}

func validateAnthropic(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) (ValidateResult, error) {
	baseURL = strings.TrimRight(sanitizeAnthropicBaseURL(baseURL), "/")
	u := baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return ValidateResult{Valid: false, Error: err.Error()}, nil
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err := client.Do(req)
	if err != nil {
		return ValidateResult{Valid: false, Error: networkError(err)}, nil
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return ValidateResult{Valid: true}, nil
	}
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return ValidateResult{Valid: false, Error: "API key unauthorized"}, nil
	}
	if modelID != "" {
		body := mustJSON(map[string]any{
			"model":    modelID,
			"messages": []map[string]any{{"role": "user", "content": "ping"}},
			"max_tokens": 1,
		})
		u = baseURL + "/chat/completions"
		req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(body))
		if err != nil {
			return ValidateResult{Valid: false, Error: err.Error()}, nil
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		res2, err := client.Do(req)
		if err != nil {
			return ValidateResult{Valid: false, Error: networkError(err), Method: "chat"}, nil
		}
		defer res2.Body.Close()
		if res2.StatusCode >= 200 && res2.StatusCode < 300 {
			return ValidateResult{Valid: true, Method: "chat"}, nil
		}
		return ValidateResult{Valid: false, Error: chatErrorMessage(res2.StatusCode), Method: "chat"}, nil
	}
	return ValidateResult{Valid: false, Error: modelsErrorMessage(res.StatusCode)}, nil
}

func validateOpenAI(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) (ValidateResult, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	u := baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return ValidateResult{Valid: false, Error: err.Error()}, nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err := client.Do(req)
	if err != nil {
		return ValidateResult{Valid: false, Error: networkError(err)}, nil
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return ValidateResult{Valid: true}, nil
	}
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return ValidateResult{Valid: false, Error: "API key unauthorized"}, nil
	}
	if modelID != "" {
		body := mustJSON(map[string]any{
			"model":      modelID,
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
			"max_tokens": 1,
		})
		u = baseURL + "/chat/completions"
		req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(body))
		if err != nil {
			return ValidateResult{Valid: false, Error: err.Error()}, nil
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		res2, err := client.Do(req)
		if err != nil {
			return ValidateResult{Valid: false, Error: networkError(err), Method: "chat"}, nil
		}
		defer res2.Body.Close()
		if res2.StatusCode >= 200 && res2.StatusCode < 300 {
			return ValidateResult{Valid: true, Method: "chat"}, nil
		}
		return ValidateResult{Valid: false, Error: chatErrorMessage(res2.StatusCode), Method: "chat"}, nil
	}
	return ValidateResult{Valid: false, Error: modelsErrorMessage(res.StatusCode)}, nil
}

func sanitizeAnthropicBaseURL(u string) string {
	u = strings.TrimSpace(stringsOr(u, "https://api.anthropic.com/v1"))
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, "/messages") {
		u = u[:len(u)-len("/messages")]
	}
	return u
}

func sanitizeEmbeddingBaseURL(u string) string {
	u = strings.TrimSpace(stringsOr(u, "https://api.openai.com/v1"))
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, "/embeddings") {
		u = u[:len(u)-len("/embeddings")]
	}
	return u
}

func isPublicURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return false
	}
	if matched, _ := regexp.MatchString(`^(10\.|172\.(1[6-9]|2[0-9]|3[0-1])\.|192\.168\.|169\.254\.|127\.|0\.0\.0\.0|::1|fc00:|fd00:)`, host); matched {
		return false
	}
	return true
}

func networkError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "connection refused") {
		return "Connection refused - provider node offline or unreachable"
	}
	if strings.Contains(msg, "no such host") || strings.Contains(msg, "DNS") {
		return "DNS lookup failed - invalid domain or network issue"
	}
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "exceeded") {
		return "Request timeout (>10s) - provider node not responding"
	}
	if strings.Contains(msg, "certificate") {
		return "SSL certificate verification failed"
	}
	return "Network connection failed - check URL and network connectivity"
}

func chatErrorMessage(status int) string {
	switch {
	case status == 401 || status == 403:
		return "API key unauthorized"
	case status == 400:
		return "Invalid model or bad request"
	case status == 404:
		return "Chat endpoint not found"
	case status >= 500:
		return "Server error - try again later"
	default:
		return fmt.Sprintf("Chat request failed (%d)", status)
	}
}

func modelsErrorMessage(status int) string {
	switch {
	case status == 401 || status == 403:
		return "API key unauthorized"
	case status == 404:
		return "/models endpoint not found - try chat validation with model ID"
	case status >= 500:
		return "Server error - try again later"
	default:
		return fmt.Sprintf("Unexpected response (%d)", status)
	}
}

func stringsOr(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonBytes(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func generateShortID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
