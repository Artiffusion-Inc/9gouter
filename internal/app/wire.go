// Package app is the composition root for the 9Gouter Go rewrite.
// It wires together the SQLite database, repositories, provider registry,
// proxychat usecase, HTTP transport, and server lifecycle.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	dbschema "github.com/Artiffusion-Inc/9gouter/internal/adapter/db"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/migrations"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/sqlite"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/antigravity"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/projectid"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver/tokenrefresh"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	httptransport "github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http/api"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/tunnel"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/mitm"
	domainprov "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"

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

	// Re-apply the dashboard "Outbound proxy" settings to the process env every
	// time settings change (PATCH /api/settings, backup import), mirroring the
	// legacy JS applyOutboundProxyEnv side-effect that ran on every settings
	// read. The boot-time apply below covers the initial row; this hook covers
	// live edits. Fail-open: a read error leaves operator env untouched.
	repos.Settings.OnUpdate(func(ctx context.Context, merged map[string]any) {
		proxy.ApplyOutboundProxyEnv(outboundProxyConfigFromMap(merged))
	})

	// Apply the dashboard "Outbound proxy" settings (settings.outboundProxyUrl
	// /Enabled/NoProxy) to the process env so the proxy stack's
	// resolveEnvProxyURL — which reads HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/
	// NO_PROXY — honours them on every outbound fetch. Mirrors legacy
	// src/lib/network/initOutboundProxy.js, which applied settings once at
	// boot. Fail-open: a read error leaves operator env untouched.
	proxy.ApplyOutboundProxyEnv(readOutboundProxyConfig(context.Background(), repos.Settings))

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
	imageHandler := newImageProxyHandler(repos, proxyOpts, cfg, logger)
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
	sessionStore = sessionStore.WithForceSecure(cfg.AuthCookieSecure)
	apiDeps := api.Deps{
		APIKeys:            repos.APIKeys,
		Alias:              repos.Aliases,
		Combos:             repos.Combos,
		Connections:        repos.Connections,
		DisabledModels:     repos.DisabledModels,
		Nodes:              repos.Nodes,
		Pricing:            repos.Pricing,
		ProxyPools:         repos.ProxyPools,
		RequestDetails:     repos.RequestDetails,
		Settings:           repos.Settings,
		Usage:              repos.Usage,
		UsageTracker:       usageTracker,
		SessionStore:       sessionStore,
		Logger:             logger,
		DB:                 db,
		Version:            cfg.Version,
		CloudflareTunnel:    tunnel.NewCloudflareManager(),
		TailscaleTunnel:    tunnel.NewTailscaleManager(),
		MITMManager:        mitm.NewManager(filepath.Join(filepath.Dir(cfg.DBPath), "mitm"), fmt.Sprintf("http://localhost:%d", cfg.Port), "", logger),
		ProxyOpts:          proxyOpts,
		ResetComboRotation: httptransport.ResetComboRotation,
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
	// taking precedence. (T018 wiring.) The handler is wrapped in a
	// DashboardGuard that ports src/dashboardGuard.js: enforce requireLogin +
	// tunnelDashboardAccess (block tunnel/tailscale hostname exposure) for
	// /dashboard* — without it the dashboard UI was served to anyone reaching
	// the port, and the dashboard "Require login" / "Block tunnel dashboard
	// access" toggles did nothing.
	mux.Handle("/", httptransport.NewDashboardGuard(httptransport.NewStaticHandler(logger), sessionStore, repos.Settings))

	server := httptransport.NewServer(httptransport.Deps{
		Config:  cfg,
		Logger:  logger,
		Auth:    httptransport.NewAuthFunc(sessionStore, httptransport.NewRequireLoginGate(repos.Settings)),
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
			Registry:             domainProvRegistry,
			UsageRepo:            r.Usage,
			StreamPipe:           pipeAdapter{},
			JSONToSSE:            synthesizerFunc(translator.Synthesize),
			Logger:               &slogLogger{logger},
			Config:               cfg,
			UsageEvents:          events,
			Pricing:              priceResolver,
			RequestDetails:       r.RequestDetails,
			ObservabilityGate:    proxychat.NewObservabilityGate(r.Settings),
			TokenSaverGate:       proxychat.NewTokenSaverGate(r.Settings),
			ProviderThinkingGate: proxychat.NewProviderThinkingGate(r.Settings),
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

func newImageProxyHandler(r repos, opts proxy.Options, cfg config.Config, logger *slog.Logger) *imageProxyHandler {
	return &imageProxyHandler{
		handler: imageproxy.New(imageproxy.Dependencies{
			Executor:            newProductionImageExecutor(r, opts, logger),
			AntigravityExecutor: newAntigravityImageExecutor(opts, logger),
			Logger:              &slogLogger{logger},
			Config:              cfg,
		}),
	}
}

// ImageProxyTestOptions configures the exported test seam
// NewImageProxyHandlerForTest. The zero value leaves the production behaviour
// intact; tests set Fetch to a recording seam that wraps proxy.ProxyAwareFetch
// (or an httptest upstream) so the e2e HTTP path can assert the effective proxy
// options, connection id and lifecycle phase reach the boundary WITHOUT
// stubbing the image executor itself. The AntigravityExecutor and
// LifecycleHostPredicates fields mirror imageproxy.Dependencies so the e2e
// harness can override them for loopback httptest endpoints.
type ImageProxyTestOptions struct {
	// Fetch overrides the productionImageExecutor's proxy-aware fetch seam.
	// When nil the real proxy.ProxyAwareFetch is used. Tests pass a recording
	// function that captures (client, req.URL, proxy options, validated target,
	// phase) and delegates to the real seam (or a stub backed by httptest).
	Fetch fetchFunc
	// DirectClient overrides the no-auth direct-only client (sdwebui/comfyui).
	// Tests pass an httptest server client so the direct-only path reaches the
	// upstream; the production guard (loopback-only target) still applies.
	DirectClient *http.Client
	// NoRedirectClient overrides the connection-backed / pinned client handed
	// to proxy.ProxyAwareFetch. Tests pass an httptest server client so TLS
	// endpoints are trusted.
	NoRedirectClient *http.Client
	// AntigravityExecutor overrides the Antigravity image delegation. Tests
	// pass nil to keep the production adapter, or a stub to exercise the path
	// without the real OAuth/project-id machinery.
	AntigravityExecutor imageproxy.AntigravityImageExecutor
	// LifecycleHostPredicates overrides the async lifecycle host allowlists so
	// httptest loopback endpoints can exercise the poll/result path. The
	// production wiring leaves this nil so the exact documented allowlists
	// remain the trust boundary.
	LifecycleHostPredicates map[string]imageproxy.LifecycleHostPredicate
	// Resolver overrides the SSRF guard resolver (production wiring injects a
	// net.LookupIP-based resolver; tests inject a static loopback resolver so
	// an httptest endpoint passes the IP check).
	Resolver imageproxy.HostResolver
	// SSRFPolicy overrides the default-deny egress policy. Tests inject a
	// permissive policy so an httptest loopback endpoint can exercise the
	// download/redirect path; the production policy is never weakened.
	SSRFPolicy imageproxy.SSRFPolicy
	// PollInterval / PollTimeout override the production poll cadence so the
	// e2e test does not sleep.
	PollInterval time.Duration
	PollTimeout  time.Duration
}

// NewImageProxyHandlerForTest is the exported test seam that lets the transport-
// layer e2e test (package http) construct a REAL productionImageExecutor +
// REAL imageProxyHandler with an injectable fetch boundary, so the full HTTP
// /v1/images/generations path can be driven end-to-end against an httptest
// upstream without stubbing the image executor. It is the only exported
// constructor in wire.go intended for tests; production wiring uses the
// unexported newImageProxyHandler.
//
// The returned handler implements httptransport.ImageHandler. The fetch seam,
// when set, is the observability boundary: it records (client, req, proxy
// options, validated target) and delegates to the real proxy.ProxyAwareFetch
// (or an httptest-backed stub), proving the connection id and phase reach the
// proxy-aware boundary. It is NOT a mock of the image executor — the executor
// is the real productionImageExecutor, and only its outbound fetch is wrapped.
func NewImageProxyHandlerForTest(r ConnectionPools, opts proxy.Options, cfg config.Config, logger *slog.Logger, testOpts ImageProxyTestOptions) httptransport.ImageHandler {
	exec := newProductionImageExecutor(toRepos(r), opts, logger)
	if testOpts.Fetch != nil {
		exec.fetch = testOpts.Fetch
	}
	if testOpts.DirectClient != nil {
		exec.directClient = testOpts.DirectClient
	}
	if testOpts.NoRedirectClient != nil {
		exec.noRedirectClient = testOpts.NoRedirectClient
	}
	deps := imageproxy.Dependencies{
		Executor:                exec,
		AntigravityExecutor:     testOpts.AntigravityExecutor,
		Logger:                  &slogLogger{logger},
		Config:                  cfg,
		LifecycleHostPredicates: testOpts.LifecycleHostPredicates,
		Resolver:                testOpts.Resolver,
		SSRFPolicy:              testOpts.SSRFPolicy,
		PollInterval:            testOpts.PollInterval,
		PollTimeout:             testOpts.PollTimeout,
	}
	if testOpts.AntigravityExecutor == nil {
		deps.AntigravityExecutor = newAntigravityImageExecutor(opts, logger)
	}
	return &imageProxyHandler{handler: imageproxy.New(deps)}
}

// ConnectionPools is the exported, minimal view of repos that
// NewImageProxyHandlerForTest needs: the connection + proxy-pool repositories
// the productionImageExecutor loads to resolve per-connection proxy settings.
// The transport-layer e2e test builds it from an in-memory SQLite DB without
// importing the unexported repos struct.
type ConnectionPools struct {
	Connections *repo.ConnectionRepo
	ProxyPools  *repo.ProxyPoolRepo
}

// toRepos widens the exported ConnectionPools view to the internal repos
// container. Only the connection + proxy-pool fields are used by the image
// executor; the rest stay nil.
func toRepos(r ConnectionPools) repos {
	return repos{Connections: r.Connections, ProxyPools: r.ProxyPools}
}

// antigravityImageExecutor adapts the real Antigravity provider executor to
// the imageproxy.AntigravityImageExecutor boundary. It is the production
// delegation: the same `antigravityexec.New` used by the chat path builds the
// envelope (requestType:image_gen, imageConfig, clean model) and runs the
// non-streaming `POST /v1internal:generateContent` through
// base.BaseExecutor.Execute → proxy.ProxyAwareFetch with the executor-level
// ProxyOpts + Logger + per-connection ProxyFetchOptions resolved from the
// credentials' ProviderSpecificData (the base executor already does this in
// doFetch → proxyFetchOptsFromCreds).
//
// This preserves OAuth bearer auth (BuildHeaders sets `Authorization: Bearer
// <accessToken>`), project-ID resolution (the envelope reads
// credentials.projectId; the chat path's ensureProjectID injects it into the
// PSD before the call), refresh/account behavior (the credentials carry the
// refreshed token + _connectionId the base executor uses for proxy routing),
// and the existing connection-aware proxy route. The credential is never put
// in the URL (`?key=` is forbidden for Antigravity).
type antigravityImageExecutor struct {
	exec *antigravityexec.Executor
}

func newAntigravityImageExecutor(opts proxy.Options, logger *slog.Logger) *antigravityImageExecutor {
	// Reuse the chat-path registry config so BaseURL / Headers / Retry /
	// ComputeRetryDelay stay identical. Lookup never fails for "antigravity"
	// (it is in the registry), but guard anyway.
	p, err := provider.Lookup("antigravity")
	if err != nil || p == nil || p.Executor() == nil {
		// Fall back to a freshly-built executor from the raw registry config.
		// This branch is unreachable in production (antigravity is registered);
		// it exists so a misconfigured registry degrades to 501 at call time
		// rather than panicking at wiring time.
		return &antigravityImageExecutor{exec: nil}
	}
	exec, ok := p.Executor().(*antigravityexec.Executor)
	if !ok {
		return &antigravityImageExecutor{exec: nil}
	}
	be := exec.BaseExecutor
	if be != nil {
		be.SetProxyOptions(opts)
		be.SetLogger(logger)
		be.HTTPClient = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &antigravityImageExecutor{exec: exec}
}

// ExecuteImage implements imageproxy.AntigravityImageExecutor. It hands the
// Gemini-shaped contents body to the real Antigravity executor; the
// executor's Execute applies the `image_gen` envelope (image.go), forces
// non-streaming (envelope.go), and runs the upstream call with OAuth bearer +
// project ID + connection-aware proxy route. The raw upstream body + status
// are returned for the imageproxy adapter to extract inline image data.
func (a *antigravityImageExecutor) ExecuteImage(ctx context.Context, req imageproxy.AntigravityImageRequest) (imageproxy.AntigravityImageResponse, error) {
	if a.exec == nil {
		return imageproxy.AntigravityImageResponse{StatusCode: http.StatusNotImplemented, Err: fmt.Errorf("antigravity executor not wired")}, nil
	}
	resp, err := a.exec.Execute(ctx, domainprov.ExecRequest{
		Model:       req.Model,
		Body:        req.Contents,
		Stream:      false,
		Credentials: req.Credentials,
	})
	if err != nil {
		return imageproxy.AntigravityImageResponse{}, err
	}
	defer resp.Response.Body.Close()
	body, readErr := io.ReadAll(resp.Response.Body)
	if readErr != nil {
		if resp.Done != nil {
			resp.Done()
		}
		return imageproxy.AntigravityImageResponse{}, readErr
	}
	if resp.Done != nil {
		resp.Done()
	}
	return imageproxy.AntigravityImageResponse{Body: body, StatusCode: resp.Response.StatusCode}, nil
}

// productionImageExecutor implements imageproxy.HTTPExecutor in the composition
// root. It is the only place that knows both the imageproxy transport metadata
// API and the proxy/DB packages. For each outbound image lifecycle request it:
//
//   - reads TransportMetadata attached by the usecase;
//   - for a pinned ValidatedHost (untrusted image URL), translates it to a
//     proxy.ValidatedTarget, attaches it to the request context, and runs the
//     request through proxy.ProxyAwareFetch so the policy-aware pinned transport
//     dials the validated IP:port (step 1);
//   - for a connection-backed request (auth provider), loads the
//     ProviderConnection, builds proxy.ProxyFetchOptions from the connection's
//     resolved proxy config (connection-level + assigned proxy pool), and runs
//     through proxy.ProxyAwareFetch. A missing connection fails hard — no
//     silent direct fallback (plan invariant);
//   - for a no-auth direct-only request (sdwebui/comfyui, connectionID == ""),
//     uses a dedicated direct client that does not follow redirects and never
//     invokes the proxy-aware executor. The full local guard (loopback-only
//     target, 403 for external viewers) lands in step 3.
//
// The fetch seam is injectable so app-level tests can assert the validated
// target and effective proxy options reach proxy.ProxyAwareFetch without
// performing a real network connect.
type productionImageExecutor struct {
	connections *repo.ConnectionRepo
	pools       *repo.ProxyPoolRepo
	proxyOpts   proxy.Options
	fallback    *proxy.Fallback
	logger      *slog.Logger
	// fetch is the proxy-aware fetch seam. Defaults to proxy.ProxyAwareFetch;
	// tests override it to record the validated target and proxy options.
	fetch func(ctx context.Context, client *http.Client, req *http.Request, opts proxy.Options, proxyOpts proxy.ProxyFetchOptions, fallback *proxy.Fallback) (*http.Response, error)
	// directClient is used for no-auth direct-only requests (sdwebui/comfyui).
	// It does not follow redirects; the local guard in step 3 rejects external
	// targets before this client is reached.
	directClient *http.Client
	// noRedirectClient is the client handed to proxy.ProxyAwareFetch for
	// connection-backed and pinned lifecycle requests. It does NOT follow
	// redirects automatically — the imageproxy adapter re-validates each 3xx
	// hop (imageproxy.handleRedirect) and rebuilds the request without
	// forwarding credentials to a foreign origin (spec step 4 point 7: the
	// executor-level redirect contract applies to submit, poll, result, input
	// and output downloads). ProxyAwareFetch's pinned fast-path builds its own
	// no-redirect client internally, so this only governs the relay/proxy/
	// direct/fallback branches.
	noRedirectClient *http.Client
}

// fetchFunc is the package-level alias for the proxy-aware fetch signature.
type fetchFunc func(ctx context.Context, client *http.Client, req *http.Request, opts proxy.Options, proxyOpts proxy.ProxyFetchOptions, fallback *proxy.Fallback) (*http.Response, error)

func newProductionImageExecutor(r repos, opts proxy.Options, logger *slog.Logger) *productionImageExecutor {
	return &productionImageExecutor{
		connections: r.Connections,
		pools:       r.ProxyPools,
		proxyOpts:   opts,
		logger:      logger,
		fetch:       proxy.ProxyAwareFetch,
		directClient: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		noRedirectClient: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Do implements imageproxy.HTTPExecutor.
func (e *productionImageExecutor) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	meta, ok := imageproxy.TransportMetadataFromContext(ctx)
	if !ok {
		// No metadata (test fallback / unmounted path): plain direct fetch.
		return http.DefaultClient.Do(req)
	}

	// Pinned target fast path: untrusted image URL with a validated IP. Translate
	// the usecase's ValidatedHost to proxy.ValidatedTarget and attach to the
	// request context; ProxyAwareFetch's pinned transport dials the validated
	// IP:port and keeps the original hostname as TLS SNI / HTTP Host.
	if meta.ValidatedHost.IsPinned() {
		vt := proxy.ValidatedTarget{
			Scheme:   meta.ValidatedHost.Scheme,
			Hostname: meta.ValidatedHost.Hostname,
			Port:     meta.ValidatedHost.Port,
			IP:       meta.ValidatedHost.IP,
		}
		// Attach the ValidatedTarget to the request context AND use that same
		// context for the fetch call: ProxyAwareFetch reads ValidatedTarget from
		// the ctx argument (not req.Context()), so the pinned fast-path would
		// silently miss if we passed the pre-attachment ctx here. This is the
		// DNS-rebinding-defeat contract from step 1 — the validated IP must
		// reach the actual dial, not a re-resolved hostname.
		pinnedCtx := proxy.WithValidatedTarget(ctx, vt)
		req = req.WithContext(pinnedCtx)
		// Pinned path bypasses relay/fallback regardless of connection; build a
		// connection proxy opts only if a connection is present so a connection
		// proxy can still reach the validated target via CONNECT/SOCKS.
		proxyFetchOpts := proxy.ProxyFetchOptions{Logger: e.logger}
		if meta.ConnectionID != "" {
			pfo, err := e.proxyFetchOptionsForConnection(ctx, meta.ConnectionID)
			if err != nil {
				return nil, err
			}
			pfo.Logger = e.logger
			proxyFetchOpts = pfo
		}
		return e.fetch(pinnedCtx, e.noRedirectClient, req, e.proxyOpts, proxyFetchOpts, e.fallback)
	}

	// No-auth direct-only path (sdwebui/comfyui): no connection, no proxy. The
	// full local guard (literal loopback target, 403 for external viewers)
	// lands in step 3; for now the direct client simply does not follow
	// redirects and skips the proxy-aware executor entirely.
	if meta.ConnectionID == "" {
		return e.directClient.Do(req)
	}

	// Connection-backed auth-provider path: load the connection and build
	// proxy options from its resolved proxy config. A missing connection fails
	// hard — no direct fallback (plan invariant: "Production image lifecycle
	// call does not do silent direct fallback when connection is missing").
	proxyFetchOpts, err := e.proxyFetchOptionsForConnection(ctx, meta.ConnectionID)
	if err != nil {
		return nil, err
	}
	proxyFetchOpts.Logger = e.logger
	return e.fetch(ctx, e.noRedirectClient, req, e.proxyOpts, proxyFetchOpts, e.fallback)
}

// proxyFetchOptionsForConnection loads a connection by ID and builds
// proxy.ProxyFetchOptions from its data blob, merging the assigned proxy pool
// the same way v1.go resolveConnectionProxyConfig does. It mirrors
// api.proxyFetchOptionsForConnection so the effective proxy options for the
// image lifecycle match the chat path. A missing connection returns an error
// (no direct fallback).
func (e *productionImageExecutor) proxyFetchOptionsForConnection(ctx context.Context, connectionID string) (proxy.ProxyFetchOptions, error) {
	conn, err := e.connections.GetByID(ctx, connectionID)
	if err != nil {
		return proxy.ProxyFetchOptions{}, fmt.Errorf("image executor: load connection %q: %w", connectionID, err)
	}
	if conn == nil {
		return proxy.ProxyFetchOptions{}, fmt.Errorf("image executor: connection %q not found", connectionID)
	}
	opts := proxy.ProxyFetchOptions{}
	var data map[string]any
	if err := json.Unmarshal(conn.Data, &data); err != nil {
		return opts, fmt.Errorf("image executor: parse connection %q data: %w", connectionID, err)
	}
	opts.ConnectionProxyEnabled = psdBool(data, "connectionProxyEnabled")
	opts.ConnectionProxyUrl = psdString(data, "connectionProxyUrl")
	opts.NoProxy = psdString(data, "connectionNoProxy")
	opts.VercelRelayUrl = psdString(data, "vercelRelayUrl")
	// Merge the assigned proxy pool — pool strictProxy always wins; pool
	// proxyUrl/noProxy fill in only when the connection does not set its own.
	if poolID, _ := data["proxyPoolId"].(string); poolID != "" && e.pools != nil {
		pool, perr := e.pools.GetByID(ctx, poolID)
		if perr == nil && pool != nil && pool.IsActive {
			var poolData map[string]any
			_ = json.Unmarshal(pool.Data, &poolData)
			if v, ok := poolData["strictProxy"].(bool); ok {
				opts.StrictProxy = v
			}
			if opts.ConnectionProxyUrl == "" {
				if v, ok := poolData["proxyUrl"].(string); ok && v != "" {
					opts.ConnectionProxyUrl = v
					if !opts.ConnectionProxyEnabled {
						opts.ConnectionProxyEnabled = true
					}
				}
			}
			if opts.NoProxy == "" {
				if v, ok := poolData["noProxy"].(string); ok && v != "" {
					opts.NoProxy = v
				}
			}
		}
	}
	return opts, nil
}

// psdString / psdBool read typed values from a parsed connection data blob. They
// mirror v1.go's helpers (which read from Credentials.ProviderSpecificData);
// kept local so the app package does not grow a v1->app dependency.
func psdString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func psdBool(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func (h *imageProxyHandler) Handle(ctx context.Context, req httptransport.ImageRequest) (httptransport.ImageResult, error) {
	res := h.handler.Handle(ctx, imageproxy.Request{
		Ctx:                   ctx,
		ProviderID:            req.ProviderID,
		Model:                 req.Model,
		Prompt:                req.Prompt,
		N:                     req.N,
		NSupplied:             req.NSupplied,
		Size:                  req.Size,
		Quality:               req.Quality,
		Style:                 req.Style,
		ResponseFormat:        req.ResponseFormat,
		OutputFormat:          req.OutputFormat,
		Background:            req.Background,
		Credentials:           req.Credentials,
		UserAgent:             req.UserAgent,
		PreferredConnectionID: req.PreferredConnectionID,
		Options:               req.Options,
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

// readOutboundProxyConfig reads the dashboard "Outbound proxy" panel fields
// from the settings repo. On any read/parse error it returns the zero config
// (disabled), so ApplyOutboundProxyEnv leaves operator env untouched — the
// fail-open contract of legacy applyOutboundProxyEnv.
func readOutboundProxyConfig(ctx context.Context, s *repo.SettingsRepo) proxy.OutboundProxyConfig {
	if s == nil {
		return proxy.OutboundProxyConfig{}
	}
	st, err := s.Get(ctx)
	if err != nil || len(st.Data) == 0 {
		return proxy.OutboundProxyConfig{}
	}
	var m map[string]any
	if err := json.Unmarshal(st.Data, &m); err != nil {
		return proxy.OutboundProxyConfig{}
	}
	return outboundProxyConfigFromMap(m)
}

// outboundProxyConfigFromMap pulls the three outbound-proxy keys from a merged
// settings map (defaults already applied by SettingsRepo.mergeWithDefaults).
func outboundProxyConfigFromMap(m map[string]any) proxy.OutboundProxyConfig {
	cfg := proxy.OutboundProxyConfig{}
	if b, ok := m["outboundProxyEnabled"].(bool); ok {
		cfg.Enabled = b
	}
	if s, ok := m["outboundProxyUrl"].(string); ok {
		cfg.ProxyURL = s
	}
	if s, ok := m["outboundNoProxy"].(string); ok {
		cfg.NoProxy = s
	}
	return cfg
}
