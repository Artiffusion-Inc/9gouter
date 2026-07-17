package api

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/usecase/managedashboard"
)

// RegisterPricing mounts pricing management routes.
func RegisterPricing(mux *http.ServeMux, deps Deps) {
	h := &pricingHandler{
		deps: deps,
		svc:  &managedashboard.PricingService{Repo: deps.Pricing},
	}
	mux.HandleFunc("GET /api/pricing", h.get)
	mux.HandleFunc("PATCH /api/pricing", h.patch)
	mux.HandleFunc("DELETE /api/pricing", h.delete)
}

type pricingHandler struct {
	deps Deps
	svc  *managedashboard.PricingService
}

func (h *pricingHandler) get(w http.ResponseWriter, r *http.Request) {
	pricing, err := h.svc.Get(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch pricing")
		return
	}
	writeJSON(w, http.StatusOK, pricing)
}

func (h *pricingHandler) patch(w http.ResponseWriter, r *http.Request) {
	var body map[string]map[string]json.RawMessage
	if err := parseJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid pricing data format")
		return
	}
	if err := h.svc.Validate(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := h.svc.Update(r.Context(), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update pricing")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *pricingHandler) delete(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	updated, err := h.svc.Reset(r.Context(), q.Get("provider"), q.Get("model"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to reset pricing")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
