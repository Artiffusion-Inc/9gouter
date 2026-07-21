package http

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
)

// Deps holds dependencies required to build the HTTP server.
type Deps struct {
	Config  config.Config
	Logger  *slog.Logger
	Auth    AuthFunc
	Handler http.Handler
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

	chain := Chain(
		RequestIDMiddleware(),
		RecoverMiddleware(log),
		LogMiddleware(log),
		ClientIPMiddleware(),
		BodySizeMiddleware(bodySize),
		APIMiddleware(deps.Auth),
	)

	handler := deps.Handler
	if handler == nil {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		handler = mux
	}

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           chain(handler),
		ReadHeaderTimeout: 30 * time.Second,
	}
}
