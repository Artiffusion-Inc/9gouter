// Package app is the composition root for the 9Router Go rewrite.
// It wires together the SQLite database, repositories, provider registry,
// proxychat usecase, HTTP transport, and server lifecycle.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Artiffusion-Inc/9router/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	dbschema "github.com/Artiffusion-Inc/9router/internal/adapter/db"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/migrations"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/sqlite"
	"github.com/Artiffusion-Inc/9router/internal/adapter/provider"
	httptransport "github.com/Artiffusion-Inc/9router/internal/adapter/transport/http"
	"github.com/Artiffusion-Inc/9router/internal/adapter/transport/http/api"
	"github.com/Artiffusion-Inc/9router/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9router/internal/adapter/translator"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
	"github.com/Artiffusion-Inc/9router/internal/usecase/proxychat"
)

// App is the wired application. It exposes the HTTP server and the underlying
// database connection for graceful shutdown.
type App struct {
	Config config.Config
	Logger *slog.Logger
	DB     *sql.DB
	Server *http.Server
}

// Wire builds the application from configuration. It opens the database,
// applies migrations/schema sync, constructs repositories, the provider
// registry, the proxychat usecase, and the HTTP server with /v1 routes.
func Wire(cfg config.Config, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}

	db, err := openDB(cfg.DBPath, logger)
	if err != nil {
		return nil, err
	}

	repos := buildRepos(db)

	proxyOpts := proxy.OptionsFromConfig(cfg)

	chatHandler := newProxyChatHandler(repos, proxyOpts, cfg, logger)

	mux := http.NewServeMux()
	httptransport.RegisterV1(mux, httptransport.V1Deps{
		APIKeysRepo:    repos.APIKeys,
		SettingsRepo:   repos.Settings,
		ConnectionRepo: repos.Connections,
		ComboRepo:      repos.Combos,
		AliasRepo:      repos.Aliases,
		NodeRepo:       repos.Nodes,
		ProxyPoolRepo:  repos.ProxyPools,
		ProxyOpts:      proxyOpts,
		Logger:         logger,
		Config:         cfg,
		Chat:           chatHandler,
	})

	sessionStore, err := auth.NewCookieStore(cfg.DashboardSessionSecret)
	if err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}
	apiDeps := api.Deps{
		APIKeys:        repos.APIKeys,
		Alias:          repos.Aliases,
		Combos:         repos.Combos,
		Connections:    repos.Connections,
		DisabledModels: repos.DisabledModels,
		Nodes:          repos.Nodes,
		Pricing:        repos.Pricing,
		ProxyPools:     repos.ProxyPools,
		RequestDetails: repos.RequestDetails,
		Settings:       repos.Settings,
		Usage:          repos.Usage,
		SessionStore:   sessionStore,
		Logger:         logger,
		DB:             db,
		Version:        cfg.Version,
	}
	api.RegisterHealth(mux)
	api.RegisterVersion(mux, cfg.Version)
	api.RegisterAuth(mux, apiDeps, cfg)
	api.RegisterKeys(mux, apiDeps)
	api.RegisterCombos(mux, apiDeps)
	api.RegisterModels(mux, apiDeps)
	api.RegisterProxyPools(mux, apiDeps)
	api.RegisterProviders(mux, apiDeps)
	api.RegisterSettings(mux, apiDeps)
	api.RegisterPricing(mux, apiDeps)
	api.RegisterUsage(mux, apiDeps)
	api.RegisterProviderNodes(mux, apiDeps)
	api.RegisterLocale(mux)
	api.RegisterTags(mux)
	api.RegisterShutdown(mux, apiDeps)
	api.RegisterCliTools(mux, apiDeps)
	api.RegisterHeadroom(mux, apiDeps)
	api.RegisterMcp(mux, apiDeps)
	api.RegisterMediaProviders(mux, apiDeps)
	api.RegisterOAuth(mux, apiDeps)
	api.RegisterPxPipe(mux, apiDeps)
	api.RegisterTunnel(mux, apiDeps)
	api.RegisterTranslator(mux, apiDeps)
	api.RegisterV1Beta(mux, apiDeps)
	api.RegisterV1Dashboard(mux, apiDeps)
	api.RegisterProvidersExtra(mux, apiDeps)
	api.RegisterUsageExtra(mux, apiDeps)
	api.RegisterSettingsExtra(mux, apiDeps)
	api.RegisterProxyPoolsExtra(mux, apiDeps)

	server := httptransport.NewServer(httptransport.Deps{
		Config:  cfg,
		Logger:  logger,
		Auth:    httptransport.NewAuthFunc(sessionStore),
		Handler: mux,
	})

	return &App{
		Config: cfg,
		Logger: logger,
		DB:     db,
		Server: server,
	}, nil
}

// Close shuts down the database connection.
func (a *App) Close() error {
	if a.DB != nil {
		return a.DB.Close()
	}
	return nil
}

func openDB(dbPath string, logger *slog.Logger) (*sql.DB, error) {
	if dbPath == "" {
		dbPath = "./data/9router.db"
	}
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	db, err := sqlite.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := migrations.Run(db, dbPath); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	if err := dbschema.SyncSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sync schema: %w", err)
	}

	logger.Info("database opened", "path", dbPath)
	return db, nil
}

// repos is a container for all SQLite-backed repositories.
type repos struct {
	APIKeys        *repo.APIKeyRepo
	Settings       *repo.SettingsRepo
	Connections    *repo.ConnectionRepo
	Combos         *repo.ComboRepo
	Aliases        *repo.AliasRepo
	Nodes          *repo.NodeRepo
	ProxyPools     *repo.ProxyPoolRepo
	Usage          *repo.UsageRepo
	RequestDetails *repo.RequestDetailRepo
	DisabledModels *repo.DisabledModelsRepo
	Pricing        *repo.PricingRepo
}

func buildRepos(db *sql.DB) repos {
	return repos{
		APIKeys:        repo.NewAPIKeyRepo(db),
		Settings:       repo.NewSettingsRepo(db),
		Connections:    repo.NewConnectionRepo(db),
		Combos:         repo.NewComboRepo(db),
		Aliases:        repo.NewAliasRepo(db),
		Nodes:          repo.NewNodeRepo(db),
		ProxyPools:     repo.NewProxyPoolRepo(db),
		Usage:          repo.NewUsageRepo(db),
		RequestDetails: repo.NewRequestDetailRepo(db),
		DisabledModels: repo.NewDisabledModelsRepo(db),
		Pricing:        repo.NewPricingRepo(db),
	}
}

// proxyChatHandler adapts proxychat.Handler to the httptransport.ChatHandler
// interface declared in the transport layer. It lives in the composition root,
// which is the only place allowed to know both packages.
type proxyChatHandler struct {
	handler *proxychat.Handler
	logger  *slog.Logger
}

func newProxyChatHandler(r repos, opts proxy.Options, cfg config.Config, logger *slog.Logger) *proxyChatHandler {
	return &proxyChatHandler{
		logger: logger,
		handler: proxychat.New(proxychat.Dependencies{
			Registry:  domainProvRegistry,
			UsageRepo: r.Usage,
			StreamPipe: pipeAdapter{},
			JSONToSSE:  synthesizerFunc(translator.Synthesize),
			Logger:     &slogLogger{logger},
			Config:     cfg,
		}),
	}
}

// proxychatDomainProvider narrows the registry result to what proxychat expects.
type proxychatDomainProvider interface {
	ID() string
	Executor() domainProv.Executor
}

// Handle implements httptransport.ChatHandler by mapping transport-level
// ChatRequest into proxychat.Request and invoking the usecase.
func (h *proxyChatHandler) Handle(ctx context.Context, req httptransport.ChatRequest, w http.ResponseWriter, sse *httptransport.Writer) (httptransport.ChatResult, error) {
	pcReq := proxychat.Request{
		Ctx:            ctx,
		Body:           req.Body,
		Endpoint:       req.Endpoint,
		Headers:        req.Headers,
		ProviderID:     req.ProviderID,
		Model:          req.Model,
		Credentials:    req.Credentials,
		Stream:         req.Stream,
		APIKey:         req.APIKey,
		ConnectionID:   req.ConnectionID,
		UserAgent:      req.UserAgent,
		ResponseWriter: w,
		SSEWriter:      sse,
	}

	res, err := h.handler.Handle(ctx, pcReq)
	return httptransport.ChatResult{
		StatusCode: res.StatusCode,
		Streamed:   res.Streamed,
		Err:        res.Err,
	}, err
}

// pipeAdapter adapts httptransport.Pipe to the proxychat streamPiper interface.
type pipeAdapter struct{}

func (pipeAdapter) Pipe(ctx context.Context, upstream io.Reader, w *httptransport.Writer, opts httptransport.PipeOpts) error {
	return httptransport.Pipe(ctx, upstream, w, opts)
}

// synthesizerFunc adapts translator.Synthesize to the proxychat jsonToSSETranslator interface.
type synthesizerFunc func([]byte) (string, error)

func (f synthesizerFunc) Synthesize(body []byte) (string, error) { return f(body) }

// slogLogger adapts *slog.Logger to proxychat's logger interface.
type slogLogger struct {
	log *slog.Logger
}

func (l slogLogger) Infof(format string, args ...any)  { l.log.Info(fmt.Sprintf(format, args...)) }
func (l slogLogger) Warnf(format string, args ...any)  { l.log.Warn(fmt.Sprintf(format, args...)) }
func (l slogLogger) Debugf(format string, args ...any) { l.log.Debug(fmt.Sprintf(format, args...)) }

// domainProvRegistry wraps the provider adapter registry for proxychat.
func domainProvRegistry(id string) (proxychat.DomainProvider, error) { return provider.Lookup(id) }
