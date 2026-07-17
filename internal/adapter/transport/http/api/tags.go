package api

import (
	"encoding/json"
	"net/http"
)

// RegisterTags mounts the public Ollama model tags route.
func RegisterTags(mux *http.ServeMux) {
	mux.HandleFunc("OPTIONS /api/tags", tagsOptions)
	mux.HandleFunc("GET /api/tags", tags)
}

func tagsOptions(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func tags(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}

var _ = json.Marshal
