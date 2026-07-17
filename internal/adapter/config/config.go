package config

import (
	"fmt"
	"strconv"
	"time"

	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	Port   int    `envconfig:"PORT" default:"20127"`
	DBPath string `envconfig:"DB_PATH" default:"./data/9router.db"`

	// Timeouts (ported from runtimeConfig.js, defaults match current compose).
	// NOTE: envconfig parses time.Duration via time.ParseDuration, so defaults
	// MUST be Go duration strings ("60s"), NOT raw ms integers ("60000" → 60µs).
	// Env vars still use the *_MS names for compatibility with the JS compose file;
	// a downstream env value like "60000" will fail ParseDuration — so Load() must
	// accept either a bare integer (treated as ms) or a duration string. Implement a
	// custom envconfig.Setter for the timeout fields that does ms-or-duration parsing.
	FetchConnectTimeout          DurationMs `envconfig:"FETCH_CONNECT_TIMEOUT_MS" default:"60000"`
	StreamStallTimeout           DurationMs `envconfig:"STREAM_STALL_TIMEOUT_MS" default:"180000"`
	StreamStallTimeoutReasoning  DurationMs `envconfig:"STREAM_STALL_TIMEOUT_REASONING_MS" default:"600000"`
	StreamReadinessMaxTimeout    DurationMs `envconfig:"STREAM_READINESS_MAX_TIMEOUT_MS" default:"900000"`
	FetchHeadersTimeout          DurationMs `envconfig:"FETCH_HEADERS_TIMEOUT_MS" default:"60000"`
	FetchBodyTimeout             DurationMs `envconfig:"FETCH_BODY_TIMEOUT_MS" default:"600000"`
	FetchKeepaliveTimeout        DurationMs `envconfig:"FETCH_KEEPALIVE_TIMEOUT_MS" default:"4000"`
	ProxyDispatcherConnections   int        `envconfig:"PROXY_DISPATCHER_CONNECTIONS" default:"1"`
	ProxyFastFailTimeout         DurationMs `envconfig:"PROXY_FAST_FAIL_TIMEOUT_MS" default:"2000"`
	ProxyHealthCacheTTL          DurationMs `envconfig:"PROXY_HEALTH_CACHE_TTL_MS" default:"30000"`
	ProxyHealthUnhealthyTTL      DurationMs `envconfig:"PROXY_HEALTH_UNHEALTHY_CACHE_TTL_MS" default:"2000"`
	ProxyFallbackProbeTimeout    DurationMs `envconfig:"PROXY_FALLBACK_PROBE_TIMEOUT_MS" default:"3000"`
	ProxyAutoSelectEnabled       bool       `envconfig:"PROXY_AUTO_SELECT_ENABLED" default:"false"`
	ProxyClientMaxBodySize       string     `envconfig:"NINEROUTER_PROXY_CLIENT_MAX_BODY_SIZE" default:"128mb"`
	SocksHandshakeTimeout        DurationMs `envconfig:"SOCKS_HANDSHAKE_TIMEOUT_MS" default:"10000"`

	// Token-saver header name. Default matches open-sse/config/runtimeConfig.js.
	TokenSaverHeader string `envconfig:"TOKEN_SAVER_HEADER" default:"x-9router-token-saver"`

	// Auth
	DashboardPasswordHash string `envconfig:"DASHBOARD_PASSWORD_HASH"`
	SessionSecret         string `envconfig:"SESSION_SECRET" default:"change-me"`

	// Add remaining ~40 env vars as the ports that need them are implemented.
}

// TOKEN_SAVER_HEADER is the canonical lower-case request header name used by
// proxychat to gate the token-saver pipeline.
const TOKEN_SAVER_HEADER = "x-9router-token-saver"

// DurationMs is an envconfig.Setter that accepts either a bare integer
// (milliseconds, matching the JS *_MS env names) or a Go duration string
// ("60s"). The raw-ms default keeps the JS compose values valid verbatim.
type DurationMs time.Duration

func (d *DurationMs) Set(val string) error {
	if n, err := strconv.ParseInt(val, 10, 64); err == nil {
		*d = DurationMs(time.Duration(n) * time.Millisecond)
		return nil
	}
	parsed, err := time.ParseDuration(val)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", val, err)
	}
	*d = DurationMs(parsed)
	return nil
}

func (d DurationMs) Duration() time.Duration { return time.Duration(d) }

func Load() (Config, error) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
