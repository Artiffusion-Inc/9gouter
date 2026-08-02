package mitm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"bufio"
	"sync"
	"time"
)

// TargetHosts maps intercepted domain patterns to their handler type.
// Mirrors src/shared/constants/mitmToolHosts.js.
var TargetHosts = map[string]string{
	"cloudcode-pa.googleapis.com":         "antigravity",
	"daily-cloudcode-pa.googleapis.com":   "antigravity",
	"api.githubcopilot.com":               "copilot",
	"copilot-proxy.githubusercontent.com": "copilot",
	"codewhisperer.us-east-1.amazonaws.com": "kiro",
}

// RouterBase is the local 9router endpoint that intercepted requests are
// forwarded to. Defaults to http://localhost:20128 (the Go backend port).
const DefaultRouterBase = "http://localhost:20128"

// Server is the MITM TLS interception proxy.
type Server struct {
	ca         *RootCA
	certCache  *leafCertCache
	routerBase string
	apiKey     string
	port       int
	logger     *slog.Logger
	mu         sync.Mutex
	listener   net.Listener
	running    bool
}

// NewServer creates a MITM server with the given Root CA.
func NewServer(ca *RootCA, routerBase string, apiKey string, logger *slog.Logger) *Server {
	if routerBase == "" {
		routerBase = DefaultRouterBase
	}
	return &Server{
		ca:         ca,
		certCache:  newLeafCertCache(ca),
		routerBase: strings.TrimRight(routerBase, "/"),
		apiKey:     apiKey,
		port:       443,
		logger:     logger,
	}
}

// Start begins listening for TLS connections on port 443. The caller must
// have already modified DNS (/etc/hosts) to redirect target domains to
// 127.0.0.1, and installed the Root CA in the system trust store.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("mitm: already running")
	}

	tlsConfig := &tls.Config{
		GetCertificate: s.certCache.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}

	ln, err := tls.Listen("tcp", fmt.Sprintf(":%d", s.port), tlsConfig)
	if err != nil {
		return fmt.Errorf("mitm: listen on :%d: %w", s.port, err)
	}

	s.listener = ln
	s.running = true

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				s.mu.Lock()
				if !s.running {
					s.mu.Unlock()
					return
				}
				s.mu.Unlock()
				if s.logger != nil {
					s.logger.Warn("mitm: accept error", "err", err)
				}
				continue
			}
			go s.handleConn(ctx, conn)
		}
	}()

	if s.logger != nil {
		s.logger.Info("mitm server started", "port", s.port)
	}
	return nil
}

// Stop shuts down the MITM server.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.running = false
	if s.logger != nil {
		s.logger.Info("mitm server stopped")
	}
}

// IsRunning reports whether the MITM server is active.
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// handleConn reads an HTTP request from the intercepted TLS connection and
// forwards it to the local 9router, then pipes the response back.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Read the HTTP request from the client.
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		if s.logger != nil && err != io.EOF {
			s.logger.Debug("mitm: read request", "err", err)
		}
		return
	}

	// Determine the handler type from the Host header.
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	// Strip port if present.
	host = strings.Split(host, ":")[0]

	handlerType := resolveHandlerType(host)
	if handlerType == "" {
		// Unknown host — pass through to the real upstream.
		s.passthrough(conn, req, host)
		return
	}

	// Forward to 9router.
	s.forwardToRouter(ctx, conn, req, handlerType)
}

// forwardToRouter sends the intercepted request to the local 9router
// and pipes the response back to the client.
func (s *Server) forwardToRouter(ctx context.Context, conn net.Conn, req *http.Request, handlerType string) {
	// Read the request body.
	body, err := io.ReadAll(req.Body)
	if err != nil {
		s.writeError(conn, 502, "Failed to read request body")
		return
	}
	defer req.Body.Close()

	// Determine the router path based on handler type.
	routerPath := resolveRouterPath(handlerType, req.URL.Path)

	// Build the router request.
	routerURL := s.routerBase + routerPath
	routerReq, err := http.NewRequestWithContext(ctx, req.Method, routerURL, bytes.NewReader(body))
	if err != nil {
		s.writeError(conn, 500, "Failed to build router request")
		return
	}

	// Forward non-sensitive headers.
	for k, vs := range req.Header {
		if isStripHeader(k) {
			continue
		}
		for _, v := range vs {
			routerReq.Header.Add(k, v)
		}
	}
	routerReq.Header.Set("Content-Type", "application/json")
	if s.apiKey != "" {
		routerReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	}

	// Send to router.
	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := client.Do(routerReq)
	if err != nil {
		s.writeError(conn, 502, "Router request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Write the response back to the client.
	respHeaders := ""
	for k, vs := range resp.Header {
		for _, v := range vs {
			respHeaders += fmt.Sprintf("%s: %s\r\n", k, v)
		}
	}
	statusLine := fmt.Sprintf("HTTP/1.1 %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	conn.Write([]byte(statusLine))
	conn.Write([]byte(respHeaders))
	conn.Write([]byte("\r\n"))
	io.Copy(conn, resp.Body)
}

// passthrough forwards the request to the real upstream server (for hosts
// not in the TargetHosts map).
func (s *Server) passthrough(conn net.Conn, req *http.Request, host string) {
	// Build upstream URL.
	scheme := "https"
	upstreamURL := fmt.Sprintf("%s://%s%s", scheme, host, req.URL.Path)
	if req.URL.RawQuery != "" {
		upstreamURL += "?" + req.URL.RawQuery
	}

	body, _ := io.ReadAll(req.Body)
	defer req.Body.Close()

	upstreamReq, err := http.NewRequest(req.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		s.writeError(conn, 500, "Failed to build upstream request")
		return
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			upstreamReq.Header.Add(k, v)
		}
	}
	upstreamReq.Host = host

	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		s.writeError(conn, 502, "Upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respHeaders := ""
	for k, vs := range resp.Header {
		for _, v := range vs {
			respHeaders += fmt.Sprintf("%s: %s\r\n", k, v)
		}
	}
	statusLine := fmt.Sprintf("HTTP/1.1 %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	conn.Write([]byte(statusLine))
	conn.Write([]byte(respHeaders))
	conn.Write([]byte("\r\n"))
	io.Copy(conn, resp.Body)
}

// writeError sends an HTTP error response to the intercepted connection.
func (s *Server) writeError(conn net.Conn, status int, msg string) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "mitm_error",
		},
	})
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
	conn.Write([]byte(resp))
}

// resolveHandlerType maps a host to its MITM handler type.
func resolveHandlerType(host string) string {
	for pattern, handlerType := range TargetHosts {
		if strings.Contains(host, pattern) {
			return handlerType
		}
	}
	return ""
}

// resolveRouterPath maps a handler type + original URL path to the 9router
// endpoint path. Mirrors the URL_MAP in handlers/*.js.
func resolveRouterPath(handlerType, originalPath string) string {
	switch handlerType {
	case "antigravity":
		return "/v1/chat/completions"
	case "copilot":
		if strings.Contains(originalPath, "/v1/messages") {
			return "/v1/messages"
		}
		if strings.Contains(originalPath, "/responses") {
			return "/v1/responses"
		}
		return "/v1/chat/completions"
	case "kiro":
		return "/v1/chat/completions"
	default:
		return "/v1/chat/completions"
	}
}

// isStripHeader reports whether a header should not be forwarded to 9router.
func isStripHeader(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "connection", "transfer-encoding",
		"content-type", "authorization":
		return true
	}
	return false
}

// bufio is needed by handleConn.
var _ = bufio.NewReader

// Ensure bufio import is present.
type _bufio struct{}
