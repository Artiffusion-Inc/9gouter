// Package mitm implements the MITM TLS interception proxy for the Go rewrite.
// It ports src/mitm/ (server.js, cert/, dns/, handlers/) into Go:
//
//   - Generates a Root CA (RSA 2048, self-signed, 10-year validity)
//   - Dynamically signs leaf certificates per-domain via SNI callback
//   - Listens on port 443 with a TLS server that intercepts traffic to
//     provider domains (cloudcode-pa.googleapis.com, api.githubcopilot.com, ...)
//   - Routes intercepted requests through the local 9router /v1/* endpoints
//   - Modifies /etc/hosts to redirect target domains to 127.0.0.1
//   - Installs the Root CA into the system trust store
//
// The MITM proxy is the most OS-coupled component: DNS modification requires
// root/sudo, cert installation varies by OS, and the TLS interception uses
// Go's crypto/tls GetCertificate callback for per-domain cert generation.
package mitm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RootCA is the MITM Root CA certificate and key, loaded from or generated to
// disk so it persists across restarts (and can be installed in the system trust
// store once). Mirrors src/mitm/cert/rootCA.js.
type RootCA struct {
	Key  *rsa.PrivateKey
	Cert *x509.Certificate
	DER  []byte // raw cert bytes for tls.Certificate

	keyPath  string
	certPath string
}

// LoadOrGenerateRootCA loads the Root CA from disk, or generates a new one if
// it doesn't exist or is expiring within 30 days. The key/cert are stored at
// <mitmDir>/rootCA.key and <mitmDir>/rootCA.crt (PEM format).
func LoadOrGenerateRootCA(mitmDir string) (*RootCA, error) {
	keyPath := filepath.Join(mitmDir, "rootCA.key")
	certPath := filepath.Join(mitmDir, "rootCA.crt")

	// Try to load existing.
	if ca, err := loadRootCA(keyPath, certPath); err == nil {
		// Check expiry (30 days lead).
		if time.Now().Add(30 * 24 * time.Hour).Before(ca.Cert.NotAfter) {
			return ca, nil
		}
	}
	// Generate new.
	if err := os.MkdirAll(mitmDir, 0700); err != nil {
		return nil, fmt.Errorf("mitm: create dir: %w", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("mitm: generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "9Router MITM Root CA",
			Organization: []string{"9Router"},
			Country:      []string{"US"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("mitm: create CA cert: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse CA cert: %w", err)
	}

	// Save to disk.
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, fmt.Errorf("mitm: write CA key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return nil, fmt.Errorf("mitm: write CA cert: %w", err)
	}

	return &RootCA{
		Key:      key,
		Cert:     cert,
		DER:      der,
		keyPath:  keyPath,
		certPath: certPath,
	}, nil
}

// loadRootCA reads the PEM key/cert from disk and parses them.
func loadRootCA(keyPath, certPath string) (*RootCA, error) {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("mitm: no PEM block in key file")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse key: %w", err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("mitm: no PEM block in cert file")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse cert: %w", err)
	}

	return &RootCA{
		Key:      key,
		Cert:     cert,
		DER:      certBlock.Bytes,
		keyPath:  keyPath,
		certPath: certPath,
	}, nil
}

// CertPath returns the path to the Root CA cert file (for trust store installation).
func (ca *RootCA) CertPath() string { return ca.certPath }

// leafCertCache caches per-domain leaf certificates to avoid re-signing on
// every connection. The cache is in-memory; certs are valid for 1 year.
type leafCertCache struct {
	mu    sync.Mutex
	cache map[string]*tls.Certificate
	ca    *RootCA
}

func newLeafCertCache(ca *RootCA) *leafCertCache {
	return &leafCertCache{cache: make(map[string]*tls.Certificate), ca: ca}
}

// GetCertificate returns a TLS certificate for the given domain, signed by the
// Root CA. Used as the tls.Config.GetCertificate callback for SNI-based cert
// generation. Mirrors getCertForDomain in cert/generate.js.
func (c *leafCertCache) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	domain := hello.ServerName
	if domain == "" {
		return nil, fmt.Errorf("mitm: no SNI server name")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if cert, ok := c.cache[domain]; ok {
		return cert, nil
	}

	cert, err := c.signLeaf(domain)
	if err != nil {
		return nil, err
	}
	c.cache[domain] = cert
	return cert, nil
}

// signLeaf creates and signs a leaf certificate for the given domain.
func (c *leafCertCache) signLeaf(domain string) (*tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate leaf key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("mitm: generate serial: %w", err)
	}

	// Subject Key Identifier = SHA1 of public key (RFC 5280 §4.2.1.2).
	ski := sha1.Sum(x509.MarshalPKCS1PublicKey(&key.PublicKey))

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: domain,
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().AddDate(1, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{domain},
		SubjectKeyId: ski[:],
	}

	der, err := x509.CreateCertificate(rand.Reader, template, c.ca.Cert, &key.PublicKey, c.ca.Key)
	if err != nil {
		return nil, fmt.Errorf("mitm: sign leaf cert: %w", err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{der, c.ca.DER},
		PrivateKey:  key,
		Leaf:        mustParseCert(der),
	}, nil
}

func mustParseCert(der []byte) *x509.Certificate {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		panic(fmt.Sprintf("mitm: parse leaf cert: %v", err))
	}
	return cert
}
