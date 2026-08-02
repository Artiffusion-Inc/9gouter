package mitm

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
)

// DNSManager handles the modification of /etc/hosts (or Windows hosts file)
// to redirect target MITM domains to 127.0.0.1. Mirrors src/mitm/dns/dnsConfig.js.
//
// Requires root/admin privileges. The manager keeps track of which entries it
// added so they can be cleanly removed on disable.
type DNSManager struct {
	mu      sync.Mutex
	added   map[string]bool
	hostsPath string
}

func NewDNSManager() *DNSManager {
	hostsPath := "/etc/hosts"
	if runtime.GOOS == "windows" {
		systemRoot := os.Getenv("SystemRoot")
		if systemRoot == "" {
			systemRoot = `C:\Windows`
		}
		hostsPath = fmt.Sprintf(`%s\System32\drivers\etc\hosts`, systemRoot)
	}
	return &DNSManager{
		added:     make(map[string]bool),
		hostsPath: hostsPath,
	}
}

// AddRedirect adds a hosts entry mapping the domain to 127.0.0.1.
// The entry is marked with a comment so it can be identified for removal.
func (d *DNSManager) AddRedirect(domain string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.added[domain] {
		return nil // already added
	}

	content, err := os.ReadFile(d.hostsPath)
	if err != nil {
		return fmt.Errorf("mitm: read hosts file: %w", err)
	}

	entry := fmt.Sprintf("\n127.0.0.1 %s # 9router-mitm\n", domain)
	newContent := string(content) + entry

	if err := d.writeHosts(newContent); err != nil {
		return fmt.Errorf("mitm: write hosts file: %w", err)
	}

	d.added[domain] = true
	return nil
}

// RemoveRedirect removes a previously added hosts entry.
func (d *DNSManager) RemoveRedirect(domain string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.added[domain] {
		return nil
	}

	content, err := os.ReadFile(d.hostsPath)
	if err != nil {
		return fmt.Errorf("mitm: read hosts file: %w", err)
	}

	entry := fmt.Sprintf("127.0.0.1 %s # 9router-mitm", domain)
	newContent := strings.ReplaceAll(string(content), entry+"\n", "")
	newContent = strings.ReplaceAll(newContent, entry, "")

	if err := d.writeHosts(newContent); err != nil {
		return fmt.Errorf("mitm: write hosts file: %w", err)
	}

	delete(d.added, domain)
	return nil
}

// RemoveAll removes all 9router-mitm entries from the hosts file.
func (d *DNSManager) RemoveAll() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	content, err := os.ReadFile(d.hostsPath)
	if err != nil {
		return nil // file may not exist
	}

	lines := strings.Split(string(content), "\n")
	var kept []string
	for _, line := range lines {
		if strings.Contains(line, "# 9router-mitm") {
			continue
		}
		kept = append(kept, line)
	}

	newContent := strings.Join(kept, "\n")
	if err := d.writeHosts(newContent); err != nil {
		return fmt.Errorf("mitm: write hosts file: %w", err)
	}

	d.added = make(map[string]bool)
	return nil
}

// writeHosts writes content to the hosts file. On Unix this requires root;
// the caller must have already escalated privileges.
func (d *DNSManager) writeHosts(content string) error {
	// On non-Windows, use the elevated write path (sudo tee).
	if runtime.GOOS != "windows" {
		return d.writeHostsElevated(content)
	}
	return os.WriteFile(d.hostsPath, []byte(content), 0644)
}

// writeHostsElevated writes the hosts file via sudo on Unix.
func (d *DNSManager) writeHostsElevated(content string) error {
	// Try direct write first (works if running as root).
	if err := os.WriteFile(d.hostsPath, []byte(content), 0644); err == nil {
		return nil
	}
	// Fall back to sudo tee.
	// This requires that sudo is available and the user has passwordless sudo
	// or has already authenticated. The JS implementation prompts for a password;
	// the Go implementation delegates this to the dashboard, which can capture
	// the password and use a shell helper. For now, we return an error.
	return fmt.Errorf("mitm: cannot write hosts file (need root or passwordless sudo)")
}

// HostsPath returns the path to the hosts file.
func (d *DNSManager) HostsPath() string { return d.hostsPath }
