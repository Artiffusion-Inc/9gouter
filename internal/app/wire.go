// Package app is the composition root for the 9Gouter Go rewrite.
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

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	dbschema "github.com/Artiffusion-Inc/9gouter/internal/adapter/db"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/migrations"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/sqlite"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/projectid"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver/tokenrefresh"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	httptransport "github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http/api"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"

	// Side-effect import: triggers RegisterRequest/RegisterResponse in every
	// translator subpackage so the registry is populated in the final binary.
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/register"
	// Side-effect import: registers live-model resolvers (kiro, ...) in the
	// resolver registry so /v1/models can fetch live catalogs. Each resolver's
	// init() calls resolver.Register. Wire overrides the kiro registration
	// below with a real KiroRefresher (the init() default uses the stub).
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/imageproxy"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/managedashboard"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/proxychat"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/proxyembeddings"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/proxyfetch"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/searchproxy"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/sttproxy"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/ttsproxy"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/videoproxy"
)

// App is the wired application. It exposes the HTTP server and the underlying
// database connection for graceful shutdown.
type App struct {
	Config config.Config
	Logger *slog.Logger
	DB     *sql.DB
	Server *http.Server
	// projectIDFetcher runs the antigravity/gemini-cli Cloud Code project-id
	// background cleanup sweep; stopped in Close (#2703 Fix 2e).
	projectIDFetcher *projectid.Fetcher
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

	// Register live-model resolvers with their real token refreshers so a
	// 401 from the upstream /models endpoint triggers an actual refresh +
	// retry instead of the stub-refresher fallback. kiro, grok-cli (xAI),
	// and copilot (GitHub token exchange) refresh on 401; clinepass and
	// kimchi have no refresh (long-lived tokens) so they pass nil.
	resolver.Register(resolver.NewKiroResolver(nil, tokenrefresh.NewKiroRefresher()))
	resolver.Register(resolver.NewGrokCliResolver(nil, tokenrefresh.NewXaiRefresher()))
	resolver.Register(resolver.NewCopilotResolver(nil, tokenrefresh.NewCopilotRefresher(), cfg.Version))
	resolver.Register(resolver.NewClinepassResolver(nil, cfg.Version))
	resolver.Register(resolver.NewKimchiResolver(nil))
	resolver.Register(resolver.NewQoderResolver()) // stub: COSY signing not yet ported
	resolver.Register(resolver.NewCodexResolver(nil, tokenrefresh.NewCodexRefresher()))
	// cursor has no token refresher (the upstream cursorModels.js returns null
	// on auth failure so callers fall back to the static catalog), so unlike
	// codex/grok-cli it takes only a cache. #92 (v0.5.40): AgentService
	// GetUsableModels live resolver with bumped clientVersion 3.12.17.
	resolver.Register(resolver.NewCursorResolver(nil))
	// kilocode gateway catalog (713c5637): unauthenticated OpenRouter-shaped
	// /api/gateway/models read, narrowed by the openrouter-free filter (free +
	// context_length >= 200000). Replaces the 8-model static fallback for
	// active kilocode connections; auth lives on the chat path, not the catalog.
	resolver.Register(resolver.NewKilocodeResolver(nil))

	proxyOpts := proxy.OptionsFromConfig(cfg)

	// usageTracker is the process-live real-time analytics surface (#83):
	// proxychat publishes Start/Stop/Save events into it; the dashboard
	// /api/usage/stream SSE handler subscribes and pushes live frames. One
	// instance is shared by both the chat path and the API handlers so a
	// request flowing through chat is visible to the dashboard immediately.
	usageTracker := managedashboard.NewEventTracker()

	// Pricing resolver merges user overrides (kv) on top of the hard-coded
	// MODEL_PRICING/PATTERN_PRICING fallback chain so saveUsage can compute the
	// USD cost of each request (the legacy saveRequestUsage → calculateCost path).
	pricingResolver := pricing.NewResolver(&pricing.RepoOverrides{Store: repos.Pricing})

	chatHandler := newProxyChatHandler(repos, proxyOpts, cfg, logger, usageTracker, pricingResolver)
	embeddingsHandler := newProxyEmbeddingsHandler(repos, proxyOpts, cfg, logger)
	webFetchHandler := newProxyWebFetchHandler(cfg, logger)
	videoHandler := newVideoProxyHandler(cfg, logger)
	sttHandler := newSttHandler(cfg, logger)
	ttsHandler := newTtsHandler(cfg, logger)
	imageHandler := newImageProxyHandler(cfg, logger)
	searchHandler := newSearchHandler(cfg, logger)

	// Cloud Code project-id fetcher for antigravity/gemini-cli (#2703 Fix 2e).
	// Shares the default HTTP client (60s timeout); the background cleanup
	// sweep is stopped when the App shuts down.
	projectIDFetcher := projectid.New(nil)

	mux := http.NewServeMux()
	httptransport.RegisterV1(mux, httptransport.V1Deps{
		APIKeysRepo:      repos.APIKeys,
		SettingsRepo:     repos.Settings,
		ConnectionRepo:   repos.Connections,
		ComboRepo:        repos.Combos,
		AliasRepo:        repos.Aliases,
		NodeRepo:         repos.Nodes,
		ProxyPoolRepo:    repos.ProxyPools,
		DisabledModels:   repos.DisabledModels,
		ProxyOpts:        proxyOpts,
		Logger:           logger,
		Config:           cfg,
		ProjectIDFetcher: projectIDFetcher,
		Chat:             chatHandler,
		Embeddings:       embeddingsHandler,
		WebFetch:         webFetchHandler,
		Video:            videoHandler,
		Stt:              sttHandler,
		Tts:              ttsHandler,
		Image:            imageHandler,
		Search:           searchHandler,
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
		UsageTracker:   usageTracker,
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
	// V1Dispatch delegates /api/v1/* passthrough requests to the /v1/*
	// routes registered above by httptransport.RegisterV1 on the same mux.
	apiDeps.V1Dispatch = mux.ServeHTTP
	api.RegisterV1Dashboard(mux, apiDeps)
	api.RegisterProvidersExtra(mux, apiDeps)
	api.RegisterUsageExtra(mux, apiDeps)
	api.RegisterSettingsExtra(mux, apiDeps)
	api.RegisterProxyPoolsExtra(mux, apiDeps)

	// Static dashboard catch-all: serves the embedded Next.js static export
	// for any path NOT claimed by /v1, /api, or /health above. Must be
	// registered last so the ServeMux longest-prefix match keeps API routes
	// taking precedence. (T018 wiring.)
	mux.Handle("/", httptransport.NewStaticHandler(logger))

	server := httptransport.NewServer(httptransport.Deps{
		Config:  cfg,
		Logger:  logger,
		Auth:    httptransport.NewAuthFunc(sessionStore),
		Handler: mux,
	})

	return &App{
		Config:           cfg,
		Logger:           logger,
		DB:               db,
		Server:           server,
		projectIDFetcher: projectIDFetcher,
	}, nil
}

// Close shuts down the database connection and background sweeps.
func (a *App) Close() error {
	if a.projectIDFetcher != nil {
		a.projectIDFetcher.Stop()
	}
	if a.DB != nil {
		return a.DB.Close()
	}
	return nil
}

func openDB(dbPath string, logger *slog.Logger) (*sql.DB, error) {
	if dbPath == "" {
		dbPath = "./data/9gouter.db"
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

func newProxyChatHandler(r repos, opts proxy.Options, cfg config.Config, logger *slog.Logger, events proxychat.UsageEventPublisher, priceResolver *pricing.Resolver) *proxyChatHandler {
	return &proxyChatHandler{
		logger: logger,
		handler: proxychat.New(proxychat.Dependencies{
			Registry:    domainProvRegistry,
			UsageRepo:   r.Usage,
			StreamPipe:  pipeAdapter{},
			JSONToSSE:   synthesizerFunc(translator.Synthesize),
			Logger:      &slogLogger{logger},
			Config:      cfg,
			UsageEvents: events,
			Pricing:     priceResolver,
		}),
	}
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

// proxyEmbeddingsHandler adapts proxyembeddings.Handler to the
// httptransport.EmbeddingsHandler interface. Lives in the composition root
// (the only place allowed to know both packages).
type proxyEmbeddingsHandler struct {
	handler *proxyembeddings.Handler
}

func newProxyEmbeddingsHandler(r repos, opts proxy.Options, cfg config.Config, logger *slog.Logger) *proxyEmbeddingsHandler {
	return &proxyEmbeddingsHandler{
		handler: proxyembeddings.New(proxyembeddings.Dependencies{
			UsageRepo: r.Usage,
			ProxyOpts: opts,
			Logger:    &slogLogger{logger},
			Config:    cfg,
		}),
	}
}

func (h *proxyEmbeddingsHandler) Handle(ctx context.Context, req httptransport.EmbeddingsRequest) (httptransport.EmbeddingsResult, error) {
	res := h.handler.Handle(ctx, proxyembeddings.Request{
		Ctx:          ctx,
		Body:         req.Body,
		Endpoint:     req.Endpoint,
		Headers:      req.Headers,
		ProviderID:   req.ProviderID,
		Model:        req.Model,
		Credentials:  req.Credentials,
		APIKey:       req.APIKey,
		ConnectionID: req.ConnectionID,
		UserAgent:    req.UserAgent,
	})
	return httptransport.EmbeddingsResult{
		StatusCode: res.StatusCode,
		Err:        res.Err,
		Body:       res.Body,
	}, nil
}

// domainProvRegistry wraps the provider adapter registry for proxychat.
func domainProvRegistry(id string) (proxychat.DomainProvider, error) { return provider.Lookup(id) }

// proxyWebFetchHandler adapts proxyfetch.Handler to the
// httptransport.WebFetchHandler interface. Lives in the composition root
// (the only place allowed to know both packages). Unlike embeddings, web-fetch
// does not persist usage rows (the legacy JS fetch path never called
// saveRequestUsage), so it only needs config + logger.
type proxyWebFetchHandler struct {
	handler *proxyfetch.Handler
}

func newProxyWebFetchHandler(cfg config.Config, logger *slog.Logger) *proxyWebFetchHandler {
	return &proxyWebFetchHandler{
		handler: proxyfetch.New(proxyfetch.Dependencies{
			Logger: &slogLogger{logger},
			Config: cfg,
		}),
	}
}

func (h *proxyWebFetchHandler) Handle(ctx context.Context, req httptransport.WebFetchRequest) (httptransport.WebFetchResult, error) {
	res := h.handler.Handle(ctx, proxyfetch.Request{
		Ctx:          ctx,
		ProviderID:   req.ProviderID,
		Credentials:  req.Credentials,
		APIKey:       req.APIKey,
		ConnectionID: req.ConnectionID,
		Endpoint:     req.Endpoint,
		UserAgent:    req.UserAgent,
		Params:       req.Params,
	})
	return httptransport.WebFetchResult{
		StatusCode: res.StatusCode,
		Err:        res.Err,
		Body:       res.Body,
	}, nil
}

// videoProxyHandler adapts videoproxy.Handler to the
// httptransport.VideoProxyHandler interface. Lives in the composition root.
type videoProxyHandler struct {
	handler *videoproxy.Handler
}

func newVideoProxyHandler(cfg config.Config, logger *slog.Logger) *videoProxyHandler {
	return &videoProxyHandler{
		handler: videoproxy.New(videoproxy.Dependencies{
			Logger: &slogLogger{logger},
			Config: cfg,
		}),
	}
}

func (h *videoProxyHandler) Handle(ctx context.Context, req httptransport.VideoProxyRequest) (httptransport.VideoProxyResult, error) {
	res := h.handler.Handle(ctx, videoproxy.Request{
		Ctx:            ctx,
		Action:         videoproxy.Action(req.Action),
		RequestID:      req.RequestID,
		Body:           req.Body,
		ContentType:    req.ContentType,
		IdempotencyKey: req.IdempotencyKey,
		ProviderID:     req.ProviderID,
		Model:          req.Model,
		Credentials:    req.Credentials,
		ConnectionID:   req.ConnectionID,
		UserAgent:      req.UserAgent,
	})
	return httptransport.VideoProxyResult{
		StatusCode:   res.StatusCode,
		Err:          res.Err,
		Body:         res.Body,
		ContentType:  res.ContentType,
		ConnectionID: res.ConnectionID,
	}, nil
}

// sttHandler adapts sttproxy.Handler to the httptransport.SttHandler
// interface. Lives in the composition root (the only place allowed to know
// both packages). Like web-fetch, STT does not persist usage rows (the legacy
// JS STT path never called saveRequestUsage), so it only needs config + logger.
type sttHandler struct {
	handler *sttproxy.Handler
}

func newSttHandler(cfg config.Config, logger *slog.Logger) *sttHandler {
	return &sttHandler{
		handler: sttproxy.New(sttproxy.Dependencies{
			Logger: &slogLogger{logger},
			Config: cfg,
		}),
	}
}

func (h *sttHandler) Handle(ctx context.Context, req httptransport.SttRequest) (httptransport.SttResult, error) {
	res := h.handler.Handle(ctx, sttproxy.Request{
		Ctx:         ctx,
		ProviderID:  req.ProviderID,
		Model:       req.Model,
		File:        req.File,
		Filename:    req.Filename,
		FileMIME:    req.FileMIME,
		FormFields:  req.FormFields,
		Credentials: req.Credentials,
		UserAgent:   req.UserAgent,
	})
	return httptransport.SttResult{
		StatusCode:  res.StatusCode,
		Err:         res.Err,
		Body:        res.Body,
		ContentType: res.ContentType,
	}, nil
}

// ttsHandler adapts ttsproxy.Handler to the httptransport.TtsHandler interface.
// Lives in the composition root (the only place allowed to know both packages).
// Like STT, TTS does not persist usage rows (the legacy JS TTS path never
// called saveRequestUsage), so it only needs config + logger.
type ttsHandler struct {
	handler *ttsproxy.Handler
}

func newTtsHandler(cfg config.Config, logger *slog.Logger) *ttsHandler {
	return &ttsHandler{
		handler: ttsproxy.New(ttsproxy.Dependencies{
			Logger: &slogLogger{logger},
			Config: cfg,
		}),
	}
}

func (h *ttsHandler) Handle(ctx context.Context, req httptransport.TtsRequest) (httptransport.TtsResult, error) {
	res := h.handler.Handle(ctx, ttsproxy.Request{
		Ctx:            ctx,
		ProviderID:     req.ProviderID,
		Model:          req.Model,
		Input:          req.Input,
		Language:       req.Language,
		ResponseFormat: req.ResponseFormat,
		Credentials:    req.Credentials,
		UserAgent:      req.UserAgent,
	})
	return httptransport.TtsResult{
		StatusCode:  res.StatusCode,
		Err:         res.Err,
		Body:        res.Body,
		ContentType: res.ContentType,
	}, nil
}

// imageProxyHandler adapts imageproxy.Handler to the httptransport.ImageHandler
// interface. Lives in the composition root (the only place allowed to know
// both packages). Like TTS/STT, image generation does not persist usage rows
// (the legacy JS image path never called saveRequestUsage), so it only needs
// config + logger.
type imageProxyHandler struct {
	handler *imageproxy.Handler
}

func newImageProxyHandler(cfg config.Config, logger *slog.Logger) *imageProxyHandler {
	return &imageProxyHandler{
		handler: imageproxy.New(imageproxy.Dependencies{
			Logger: &slogLogger{logger},
			Config: cfg,
		}),
	}
}

func (h *imageProxyHandler) Handle(ctx context.Context, req httptransport.ImageRequest) (httptransport.ImageResult, error) {
	res := h.handler.Handle(ctx, imageproxy.Request{
		Ctx:            ctx,
		ProviderID:     req.ProviderID,
		Model:          req.Model,
		Prompt:         req.Prompt,
		N:              req.N,
		Size:           req.Size,
		Quality:        req.Quality,
		Style:          req.Style,
		ResponseFormat: req.ResponseFormat,
		OutputFormat:   req.OutputFormat,
		Background:     req.Background,
		Credentials:    req.Credentials,
		UserAgent:      req.UserAgent,
	})
	return httptransport.ImageResult{
		StatusCode:  res.StatusCode,
		Err:         res.Err,
		Body:        res.Body,
		ContentType: res.ContentType,
	}, nil
}

// searchHandler adapts searchproxy.Handler to the httptransport.SearchHandler
// interface. Lives in the composition root (the only place allowed to know
// both packages). Like image/TTS, web search does not persist usage rows (the
// legacy JS search path never called saveRequestUsage), so it only needs
// config + logger.
type searchHandler struct {
	handler *searchproxy.Handler
}

func newSearchHandler(cfg config.Config, logger *slog.Logger) *searchHandler {
	return &searchHandler{
		handler: searchproxy.New(searchproxy.Dependencies{
			Logger: &slogLogger{logger},
			Config: cfg,
		}),
	}
}

func (h *searchHandler) Handle(ctx context.Context, req httptransport.SearchRequest) (httptransport.SearchResult, error) {
	res := h.handler.Handle(ctx, searchproxy.Request{
		Ctx:         ctx,
		ProviderID:  req.ProviderID,
		Query:       req.Query,
		Model:       req.Model,
		MaxResults:  req.MaxResults,
		SearchType:  req.SearchType,
		Country:     req.Country,
		Language:    req.Language,
		TimeRange:   req.TimeRange,
		Offset:      req.Offset,
		Credentials: req.Credentials,
		UserAgent:   req.UserAgent,
	})
	return httptransport.SearchResult{
		StatusCode:  res.StatusCode,
		Err:         res.Err,
		Body:        res.Body,
		ContentType: res.ContentType,
	}, nil
}
