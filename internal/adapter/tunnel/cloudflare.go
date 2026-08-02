// Package tunnel implements the Cloudflare quick-tunnel and Tailscale funnel
// orchestration for the Go rewrite. It ports src/lib/tunnel/cloudflare/
// cloudflared.js and src/lib/tunnel/tailscale/tailscale.js: download/locate
// the cloudflared binary, spawn a quick tunnel, parse the trycloudflare.com
// URL, manage the process lifecycle, and report status.
//
// Fail-open: when cloudflared is not installed and cannot be downloaded, the
// tunnel enable handler returns a clear error to the dashboard rather than
// silently failing.
package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// CloudflareManager manages the cloudflared subprocess for quick tunnels.
type CloudflareManager struct {
	mu      sync.Mutex
	process *os.Process
	url     string
	port    int
}

// NewCloudflareManager creates a manager.
func NewCloudflareManager() *CloudflareManager {
	return &CloudflareManager{}
}

// cloudflaredBinaryPath returns the path where cloudflared is expected or
// downloaded to: <dataDir>/bin/cloudflared (or .exe on Windows).
func cloudflaredBinaryPath(dataDir string) string {
	binName := "cloudflared"
	if runtime.GOOS == "windows" {
		binName = "cloudflared.exe"
	}
	return filepath.Join(dataDir, "bin", binName)
}

// cloudflaredDownloadURL returns the platform-specific download URL for the
// latest cloudflared release.
func cloudflaredDownloadURL() string {
	base := "https://github.com/cloudflare/cloudflared/releases/latest/download"
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return base + "/cloudflared-darwin-arm64.tgz"
		}
		return base + "/cloudflared-darwin-amd64.tgz"
	case "windows":
		return base + "/cloudflared-windows-amd64.exe"
	default: // linux
		if runtime.GOARCH == "arm64" {
			return base + "/cloudflared-linux-arm64"
		}
		return base + "/cloudflared-linux-amd64"
	}
}

// EnsureCloudflared checks if cloudflared is available (in PATH or dataDir/bin)
// and downloads it if not. Returns the binary path.
func EnsureCloudflared(ctx context.Context, dataDir string) (string, error) {
	// Check PATH first.
	if path, err := exec.LookPath("cloudflared"); err == nil {
		return path, nil
	}

	binPath := cloudflaredBinaryPath(dataDir)
	if info, err := os.Stat(binPath); err == nil && info.Size() > 1<<20 {
		if runtime.GOOS != "windows" {
			_ = os.Chmod(binPath, 0755)
		}
		return binPath, nil
	}

	// Download.
	binDir := filepath.Dir(binPath)
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", fmt.Errorf("tunnel: create bin dir: %w", err)
	}

	url := cloudflaredDownloadURL()
	tmpPath := binPath + ".tmp"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("tunnel: download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("tunnel: download cloudflared: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("tunnel: download failed: HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("tunnel: create tmp file: %w", err)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("tunnel: write cloudflared: %w", err)
	}
	out.Close()

	if strings.HasSuffix(url, ".tgz") {
		// Extract tarball.
		if err := exec.CommandContext(ctx, "tar", "-xzf", tmpPath, "-C", binDir).Run(); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("tunnel: extract cloudflared: %w", err)
		}
		os.Remove(tmpPath)
	} else {
		if err := os.Rename(tmpPath, binPath); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("tunnel: rename cloudflared: %w", err)
		}
	}

	if runtime.GOOS != "windows" {
		_ = os.Chmod(binPath, 0755)
	}
	return binPath, nil
}

// tunnelURLRegex matches https://<subdomain>.trycloudflare.com from logs.
var tunnelURLRegex = regexp.MustCompile(`https://([a-z0-9-]+)\.trycloudflare\.com`)

// StartQuickTunnel spawns cloudflared in quick-tunnel mode pointing at the
// local port, parses the generated trycloudflare.com URL from stdout/stderr,
// and returns it. The process is kept alive; call Stop to kill it.
func (m *CloudflareManager) StartQuickTunnel(ctx context.Context, binPath string, localPort int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Kill any existing process.
	if m.process != nil {
		_ = m.process.Kill()
		m.process = nil
	}

	cmd := exec.CommandContext(ctx, binPath,
		"tunnel",
		"--url", fmt.Sprintf("http://127.0.0.1:%d", localPort),
		"--no-autoupdate",
		"--retries", "99",
	)
	cmd.Env = append(os.Environ(), "TUNNEL_TRANSPORT_PROTOCOL=http2")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("tunnel: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("tunnel: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("tunnel: start cloudflared: %w", err)
	}
	m.process = cmd.Process
	m.port = localPort

	// Scan stdout+stderr for the tunnel URL.
	urlCh := make(chan string, 1)
	errCh := make(chan error, 1)
	scanDone := make(chan struct{})

	scan := func(rd io.Reader) {
		scanner := bufio.NewScanner(rd)
		for scanner.Scan() {
			line := scanner.Text()
			if matches := tunnelURLRegex.FindStringSubmatch(line); len(matches) > 0 {
				if matches[1] != "api" {
					select {
					case urlCh <- "https://" + matches[1] + ".trycloudflare.com":
					default:
					}
				}
			}
		}
	}
	go scan(stdout)
	go scan(stderr)

	go func() {
		<-scanDone
	}()

	select {
	case url := <-urlCh:
		close(scanDone)
		m.url = url
		return url, nil
	case err := <-errCh:
		close(scanDone)
		return "", err
	case <-time.After(90 * time.Second):
		close(scanDone)
		_ = cmd.Process.Kill()
		m.process = nil
		return "", fmt.Errorf("tunnel: timed out waiting for cloudflared URL")
	case <-ctx.Done():
		close(scanDone)
		_ = cmd.Process.Kill()
		m.process = nil
		return "", ctx.Err()
	}
}

// Stop kills the cloudflared subprocess.
func (m *CloudflareManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.process != nil {
		_ = m.process.Kill()
		m.process = nil
	}
	m.url = ""
}

// Status returns the current tunnel state.
func (m *CloudflareManager) Status() TunnelStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return TunnelStatus{
		Enabled:   m.process != nil,
		TunnelURL: m.url,
		Port:      m.port,
	}
}

// TunnelStatus is the tunnel state reported to the dashboard.
type TunnelStatus struct {
	Enabled   bool   `json:"enabled"`
	TunnelURL string `json:"tunnelUrl"`
	Port      int    `json:"port"`
}
