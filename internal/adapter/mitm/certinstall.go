package mitm

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// InstallCert installs the Root CA cert into the system trust store so the
// intercepted TLS certs are trusted by the IDE/CLI. Mirrors
// src/mitm/cert/install.js. The installation commands vary by OS:
//
//   macOS:   security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain <cert>
//   Linux:   cp <cert> /usr/local/share/ca-certificates/9router-mitm.crt && update-ca-certificates
//   Windows: certutil -addstore -f Root <cert>
//
// All commands require elevated privileges (root/admin).
func InstallCert(certPath string) error {
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("security", "add-trusted-cert", "-d", "-r", "trustRoot",
			"-k", "/Library/Keychains/System.keychain", certPath)
		return cmd.Run()
	case "linux":
		// Debian/Ubuntu
		dest := "/usr/local/share/ca-certificates/9router-mitm.crt"
		if err := copyFile(certPath, dest); err != nil {
			return fmt.Errorf("mitm: copy cert: %w", err)
		}
		cmd := exec.Command("update-ca-certificates")
		return cmd.Run()
	case "windows":
		cmd := exec.Command("certutil", "-addstore", "-f", "Root", certPath)
		return cmd.Run()
	default:
		return fmt.Errorf("mitm: unsupported OS: %s", runtime.GOOS)
	}
}

// UninstallCert removes the Root CA cert from the system trust store.
func UninstallCert(certPath string) error {
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("security", "delete-certificate", "-c", "9Router MITM Root CA")
		_ = cmd.Run()
		return nil
	case "linux":
		_ = os.Remove("/usr/local/share/ca-certificates/9router-mitm.crt")
		cmd := exec.Command("update-ca-certificates", "--fresh")
		_ = cmd.Run()
		return nil
	case "windows":
		cmd := exec.Command("certutil", "-delstore", "Root", "9Router MITM Root CA")
		_ = cmd.Run()
		return nil
	default:
		return fmt.Errorf("mitm: unsupported OS: %s", runtime.GOOS)
	}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
