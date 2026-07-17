package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
	"github.com/Artiffusion-Inc/9router/internal/usecase/managedashboard"
)

// RegisterProviders mounts provider connection management routes.
func RegisterProviders(mux *http.ServeMux, deps Deps) {
	h := &providersHandler{
		deps: deps,
		svc:  &managedashboard.ProviderService{Repo: deps.Connections, NodeRepo: deps.Nodes, PoolRepo: deps.ProxyPools},
	}
	mux.HandleFunc("GET /api/providers", h.list)
	mux.HandleFunc("POST /api/providers", h.create)
	mux.HandleFunc("GET /api/providers/client", h.client)
}

type providersHandler struct {
	deps Deps
	svc  *managedashboard.ProviderService
}

func (h *providersHandler) list(w http.ResponseWriter, r *http.Request) {
	conns, err := h.svc.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch providers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": conns})
}

type createProviderRequest struct {
	Provider    string          `json:"provider"`
	APIKey      string          `json:"apiKey"`
	Name        string          `json:"name"`
	DisplayName string          `json:"displayName"`
	Priority    int             `json:"priority"`
	DefaultModel string         `json:"defaultModel"`
	TestStatus  string          `json:"testStatus"`
	ProxyPoolID string          `json:"proxyPoolId"`
	Data        json.RawMessage `json:"providerSpecificData"`
}

func (h *providersHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createProviderRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Provider == "" {
		writeError(w, http.StatusBadRequest, "Invalid provider")
		return
	}
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "API Key is required")
		return
	}
	name := req.Name
	if name == "" {
		name = req.DisplayName
	}
	if name == "" {
		name = req.Provider
	}

	psd := map[string]any{}
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &psd)
	}
	if req.ProxyPoolID != "" {
		psd["proxyPoolId"] = req.ProxyPoolID
	}

	conn := settings.ProviderConnection{
		ID:       generateID(),
		Provider: req.Provider,
		AuthType: "apikey",
		Name:     name,
		Priority: req.Priority,
		IsActive: true,
		Data:     jsonData(psd),
	}
	if req.TestStatus != "" {
		// Test status is surfaced through providerSpecificData for compatibility.
		var d map[string]any
		_ = json.Unmarshal(conn.Data, &d)
		d["testStatus"] = req.TestStatus
		conn.Data = jsonData(d)
	}
	created, err := h.svc.Create(r.Context(), conn, req.APIKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create provider")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"connection": sanitizeConnection(*created)})
}

func (h *providersHandler) client(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	provider := q.Get("provider")
	accountStatus := q.Get("accountStatus")
	sort := q.Get("sort")
	if sort == "" {
		sort = "priority"
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("pageSize"))
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 20
	}

	result, err := h.svc.Client(r.Context(), managedashboard.ClientFilter{
		Provider:      provider,
		AccountStatus: accountStatus,
		Sort:          sort,
		Page:          page,
		PageSize:      pageSize,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch providers")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func sanitizeConnection(c settings.ProviderConnection) map[string]any {
	m := map[string]any{
		"id":       c.ID,
		"provider": c.Provider,
		"authType": c.AuthType,
		"name":     c.Name,
		"priority": c.Priority,
		"isActive": c.IsActive,
		"data":     c.Data,
	}
	var data map[string]any
	_ = json.Unmarshal(c.Data, &data)
	for _, k := range []string{"baseUrl", "proxyPoolId", "connectionProxyEnabled", "connectionProxyUrl", "connectionNoProxy", "nodeName"} {
		if v, ok := data[k]; ok {
			m[k] = v
		}
	}
	delete(m, "apiKey")
	return m
}

func jsonData(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
