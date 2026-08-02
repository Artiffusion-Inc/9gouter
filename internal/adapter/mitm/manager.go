package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Manager coordinates the MITM proxy lifecycle: Root CA generation, DNS
// redirects, TLS server start/stop, and cert installation. Mirrors
// src/mitm/manager.js.
type Manager struct {
	mu         sync.Mutex
	ca         *RootCA
	server     *Server
	dns        *DNSManager
	mitmDir    string
	routerBase string
	apiKey     string
	logger     *slog.Logger
	running    bool
}

// NewManager creates a MITM manager. mitmDir is where the Root CA key/cert
// are stored (typically <dataDir>/mitm).
func NewManager(mitmDir, routerBase, apiKey string, logger *slog.Logger) *Manager {
	return &Manager{
		mitmDir:    mitmDir,
		routerBase: routerBase,
		apiKey:     apiKey,
		dns:        NewDNSManager(),
		logger:     logger,
	}
}

// Enable starts the MITM proxy: generates/loads the Root CA, installs it,
// adds DNS redirects for all target hosts, and starts the TLS server.
func (m *Manager) Enable(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return fmt.Errorf("mitm: already running")
	}

	ca, err := LoadOrGenerateRootCA(m.mitmDir)
	if err != nil {
		return fmt.Errorf("mitm: CA setup: %w", err)
	}
	m.ca = ca

	if err := InstallCert(ca.CertPath()); err != nil {
		if m.logger != nil {
			m.logger.Warn("mitm: failed to install cert (may need root/admin)", "err", err)
		}
	}

	for host := range TargetHosts {
		if err := m.dns.AddRedirect(host); err != nil {
			if m.logger != nil {
				m.logger.Warn("mitm: DNS redirect failed (may need root/sudo)", "host", host, "err", err)
			}
		}
	}

	m.server = NewServer(ca, m.routerBase, m.apiKey, m.logger)
	if err := m.server.Start(ctx); err != nil {
		_ = m.dns.RemoveAll()
		return fmt.Errorf("mitm: start server: %w", err)
	}

	m.running = true
	if m.logger != nil {
		m.logger.Info("mitm enabled", "routerBase", m.routerBase)
	}
	return nil
}

// Disable stops the MITM proxy: stops the TLS server, removes DNS redirects.
func (m *Manager) Disable() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.server != nil {
		m.server.Stop()
		m.server = nil
	}

	_ = m.dns.RemoveAll()
	m.running = false
	if m.logger != nil {
		m.logger.Info("mitm disabled")
	}
}

// IsRunning reports whether the MITM proxy is active.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Status returns the current MITM state for the dashboard.
func (m *Manager) Status() MITMStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MITMStatus{
		Enabled:    m.running,
		CertPath:   m.caPath(),
		RouterBase: m.routerBase,
	}
}

// MITMStatus is the state reported to the dashboard.
type MITMStatus struct {
	Enabled    bool   `json:"enabled"`
	CertPath   string `json:"certPath"`
	RouterBase string `json:"routerBase"`
}

func (m *Manager) caPath() string {
	if m.ca != nil {
		return m.ca.CertPath()
	}
	return filepath.Join(m.mitmDir, "rootCA.crt")
}

func (m *Manager) MITMDir() string { return m.mitmDir }

func (m *Manager) SetRouterBase(base string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routerBase = base
}

func (m *Manager) SetAPIKey(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apiKey = key
}

// EnsureMITMDir creates the MITM directory if it doesn't exist.
func EnsureMITMDir(dataDir string) (string, error) {
	mitmDir := filepath.Join(dataDir, "mitm")
	if err := os.MkdirAll(mitmDir, 0700); err != nil {
		return "", fmt.Errorf("mitm: create dir: %w", err)
	}
	return mitmDir, nil
}
