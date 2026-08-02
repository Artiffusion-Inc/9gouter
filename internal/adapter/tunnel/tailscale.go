package tunnel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// TailscaleManager manages the tailscale subprocess for funnel access.
// Unlike cloudflared, tailscale is typically installed system-wide (apt/brew/
// snap). The Go rewrite uses the system-installed tailscale binary rather than
// downloading one.
type TailscaleManager struct {
	mu      sync.Mutex
	running bool
	url     string
	port    int
}

func NewTailscaleManager() *TailscaleManager {
	return &TailscaleManager{}
}

// tailscaleBinaryPath returns the path to the tailscale binary, checking:
// 1. PATH lookup
// 2. Common system install paths
// 3. <dataDir>/bin/tailscale (downloaded)
func tailscaleBinaryPath(dataDir string) string {
	if path, err := exec.LookPath("tailscale"); err == nil {
		return path
	}
	if runtime.GOOS == "windows" {
		winPath := `C:\Program Files\Tailscale\tailscale.exe`
		if _, err := os.Stat(winPath); err == nil {
			return winPath
		}
	} else {
		for _, p := range []string{
			"/usr/local/bin/tailscale",
			"/opt/homebrew/bin/tailscale",
			"/usr/sbin/tailscale",
			"/usr/bin/tailscale",
			"/snap/bin/tailscale",
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return filepath.Join(dataDir, "bin", "tailscale")
}

// IsInstalled checks if tailscale is available on the system.
func (m *TailscaleManager) IsInstalled() bool {
	_, err := exec.LookPath("tailscale")
	if err == nil {
		return true
	}
	if runtime.GOOS == "windows" {
		_, err = os.Stat(`C:\Program Files\Tailscale\tailscale.exe`)
	} else {
		for _, p := range []string{
			"/usr/local/bin/tailscale",
			"/opt/homebrew/bin/tailscale",
			"/usr/bin/tailscale",
			"/snap/bin/tailscale",
		} {
			if _, err = os.Stat(p); err == nil {
				return true
			}
		}
	}
	return false
}

// IsRunning checks if the tailscale daemon is running.
func (m *TailscaleManager) IsRunning() bool {
	bin := tailscaleBinaryPath("")
	cmd := exec.Command(bin, "status")
	cmd.Env = append(os.Environ(), "TS_NO_LOGS=true")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// StartFunnel enables a Tailscale funnel on the local port. Returns the funnel URL.
func (m *TailscaleManager) StartFunnel(ctx context.Context, localPort int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	bin := tailscaleBinaryPath("")
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("tunnel: tailscale not installed")
	}

	// Start funnel: tailscale serve --bg --https=443 tcp://localhost:PORT
	cmd := exec.CommandContext(ctx, bin, "serve", "--bg", "--https=443",
		fmt.Sprintf("tcp://localhost:%d", localPort))
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tunnel: tailscale serve: %w", err)
	}

	// Get the funnel URL: tailscale status --json
	statusCmd := exec.CommandContext(ctx, bin, "status", "--json")
	output, err := statusCmd.Output()
	if err != nil {
		// Funnel started but can't get URL — report success without URL.
		m.running = true
		m.port = localPort
		return "", nil
	}

	// Parse the Tailscale URL from status output.
	url := parseTailscaleURL(output)
	m.running = true
	m.url = url
	m.port = localPort
	return url, nil
}

// StopFunnel disables the Tailscale funnel.
func (m *TailscaleManager) StopFunnel() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	bin := tailscaleBinaryPath("")
	cmd := exec.Command(bin, "serve", "--reset")
	if err := cmd.Run(); err != nil {
		// Best-effort: reset may fail if no funnel is configured.
		return nil
	}
	m.running = false
	m.url = ""
	return nil
}

// Status returns the current Tailscale funnel state.
func (m *TailscaleManager) Status() TailscaleStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return TailscaleStatus{
		Enabled:   m.running,
		TunnelURL: m.url,
		Port:      m.port,
	}
}

// TailscaleStatus is the Tailscale funnel state reported to the dashboard.
type TailscaleStatus struct {
	Enabled   bool   `json:"enabled"`
	TunnelURL string `json:"tunnelUrl"`
	Port      int    `json:"port"`
}

// parseTailscaleURL extracts the HTTPS funnel URL from tailscale status --json output.
func parseTailscaleURL(output []byte) string {
	// Look for the "MagicDNSSuffix" or self URL in the JSON output.
	// The funnel URL is typically https://<hostname>.<tailnet>.ts.net
	s := string(output)
	// Simple heuristic: find .ts.net in the output
	idx := strings.Index(s, ".ts.net")
	if idx < 0 {
		return ""
	}
	// Walk backwards to find the start of the hostname.
	start := idx
	for start > 0 && s[start-1] != '"' && s[start-1] != ' ' && s[start-1] != '\n' {
		start--
	}
	end := idx + len(".ts.net")
	for end < len(s) && s[end] != '"' && s[end] != ' ' && s[end] != '\n' {
		end++
	}
	host := s[start:end]
	if host == "" {
		return ""
	}
	return "https://" + host
}
