package proxy

import (
	"net/url"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
)

// Family directive values.
type Family string

const (
	FamilyAuto Family = "auto"
	FamilyIPv4 Family = "ipv4"
	FamilyIPv6 Family = "ipv6"
)

// Options carries the proxy-related timeout and behaviour configuration.
// Options are populated from config.Config by callers; fields intentionally
// mirror the JS env names documented in CLAUDE.md and the Go config package.
type Options struct {
	FetchConnectTimeout          time.Duration
	FetchHeadersTimeout          time.Duration
	FetchBodyTimeout             time.Duration
	FetchKeepaliveTimeout        time.Duration
	SocksHandshakeTimeout        time.Duration
	ProxyDispatcherConnections   int
	ProxyFastFailTimeout         time.Duration
	ProxyHealthCacheTTL          time.Duration
	ProxyHealthUnhealthyTTL      time.Duration
	ProxyFallbackProbeTimeout    time.Duration
	ProxyAutoSelectEnabled       bool

	// ConnectionProxy is a per-connection dashboard proxy override.
	ConnectionProxy *url.URL
}

// OptionsFromConfig builds Options from the application config.
func OptionsFromConfig(cfg config.Config) Options {
	return Options{
		FetchConnectTimeout:        cfg.FetchConnectTimeout.Duration(),
		FetchHeadersTimeout:        cfg.FetchHeadersTimeout.Duration(),
		FetchBodyTimeout:           cfg.FetchBodyTimeout.Duration(),
		FetchKeepaliveTimeout:      cfg.FetchKeepaliveTimeout.Duration(),
		SocksHandshakeTimeout:      cfg.SocksHandshakeTimeout.Duration(),
		ProxyDispatcherConnections: cfg.ProxyDispatcherConnections,
		ProxyFastFailTimeout:       cfg.ProxyFastFailTimeout.Duration(),
		ProxyHealthCacheTTL:        cfg.ProxyHealthCacheTTL.Duration(),
		ProxyHealthUnhealthyTTL:    cfg.ProxyHealthUnhealthyTTL.Duration(),
		ProxyFallbackProbeTimeout:  cfg.ProxyFallbackProbeTimeout.Duration(),
		ProxyAutoSelectEnabled:     cfg.ProxyAutoSelectEnabled,
	}
}
