package http

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
)

// Deps holds dependencies required to build the HTTP server.
type Deps struct {
	Config config.Config
	Logger *slog.Logger
	Auth   AuthFunc
}

// NewServer builds a *http.Server with a method-pattern *http.ServeMux,
// middleware chain, and graceful shutdown support.
func NewServer(deps Deps) *http.Server {
	cfg := deps.Config
	if cfg.Port <= 0 {
		cfg.Port = 20127
	}

	bodySizeStr := cfg.ProxyClientMaxBodySize
	if bodySizeStr == "" {
		bodySizeStr = "128mb"
	}
	bodySize, err := parseBodySize(bodySizeStr)
	if err != nil {
		bodySize = 128 * 1024 * 1024
		if deps.Logger != nil {
			deps.Logger.Warn("invalid body size, using default", "value", bodySizeStr, "error", err)
		}
	}

	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	chain := Chain(
		RecoverMiddleware(log),
		LogMiddleware(log),
		ClientIPMiddleware(),
		BodySizeMiddleware(bodySize),
		APIMiddleware(deps.Auth),
	)

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           chain(mux),
		ReadHeaderTimeout: 30 * time.Second,
	}
}
