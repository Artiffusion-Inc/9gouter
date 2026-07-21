package api

import (
	"net/http"
	"strconv"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/managedashboard"
)

// RegisterUsage mounts usage/read-only analytics routes.
func RegisterUsage(mux *http.ServeMux, deps Deps) {
	h := &usageHandler{
		deps: deps,
		svc:  &managedashboard.UsageService{Repo: deps.Usage, DetailRepo: deps.RequestDetails},
	}
	mux.HandleFunc("GET /api/usage/history", h.history)
	mux.HandleFunc("GET /api/usage/stats", h.stats)
	mux.HandleFunc("GET /api/usage/chart", h.chart)
	mux.HandleFunc("GET /api/usage/logs", h.logs)
	mux.HandleFunc("GET /api/usage/request-details", h.requestDetails)
	mux.HandleFunc("GET /api/usage/providers", h.providers)
}

type usageHandler struct {
	deps Deps
	svc  *managedashboard.UsageService
}

func (h *usageHandler) history(w http.ResponseWriter, r *http.Request) {
	stats, err := h.svc.Stats(r.Context(), "all")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch usage stats")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *usageHandler) stats(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "7d"
	}
	stats, err := h.svc.Stats(r.Context(), period)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch usage stats")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *usageHandler) chart(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "7d"
	}
	data, err := h.svc.Chart(r.Context(), period)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch chart data")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (h *usageHandler) logs(w http.ResponseWriter, r *http.Request) {
	logs, err := h.svc.RecentLogs(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch logs")
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

func (h *usageHandler) requestDetails(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("pageSize"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	result, err := h.svc.RequestDetails(r.Context(), repo.RequestDetailFilter{
		Provider:     q.Get("provider"),
		Model:        q.Get("model"),
		ConnectionID: q.Get("connectionId"),
		Status:       q.Get("status"),
		StartDate:    q.Get("startDate"),
		EndDate:      q.Get("endDate"),
		Page:         page,
		PageSize:     pageSize,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch request details")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *usageHandler) providers(w http.ResponseWriter, r *http.Request) {
	providers, err := h.svc.DistinctProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch providers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

var _ usage.Aggregates
