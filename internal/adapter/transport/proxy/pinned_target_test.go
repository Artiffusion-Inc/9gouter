package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pipeConn is an in-memory net.Conn for recording tests — no real network
// connect happens. It uses net.Pipe (two-sided: a Read on one end consumes a
// Write on the other), so the server side and the HTTP client side each hold
// one end of a single pipe pair.
type pipeConn struct {
	net.Conn
}

// newPipePair returns two conns representing the two ends of an in-memory
// connection. The test keeps one end (server) to feed a response and discard
// the request; the recording dial returns the other end (client) to net/http.
func newPipePair() (client, server net.Conn) {
	c, s := net.Pipe()
	return &pipeConn{Conn: c}, &pipeConn{Conn: s}
}

// feedHTTP writes a canned HTTP response from the server end of a pipe pair so
// the http.Transport holding the client end sees a complete response. The
// request bytes the client writes to its end are drained in the background.
func feedHTTP(server net.Conn, status, body string) {
	go func() {
		// Drain the request the client writes, then send the response.
		go io.Copy(io.Discard, server)
		fmt.Fprintf(server, "HTTP/1.1 %s\r\nContent-Length: %d\r\nContent-Type: text/plain\r\n\r\n%s", status, len(body), body)
	}()
}

// recordingPinnedDial captures the pinned direct/SOCKS destination (must be the
// validated IP:port, never a re-resolved hostname) and returns the client end
// of an in-memory pipe pair so no real connect happens.
type recordingPinnedDial struct {
	mu         sync.Mutex
	called     int32
	gotAddr    string
	gotVT      ValidatedTarget
	serverConn net.Conn
}

func (r *recordingPinnedDial) DialPinned(ctx context.Context, vt ValidatedTarget) (net.Conn, error) {
	atomic.StoreInt32(&r.called, 1)
	r.mu.Lock()
	r.gotAddr = vt.Address()
	r.gotVT = vt
	client, server := newPipePair()
	r.serverConn = server
	r.mu.Unlock()
	return client, nil
}

// snapshot returns a race-free copy of the recorded dial destination.
func (r *recordingPinnedDial) snapshot() (addr string, hostname string, called bool) {
	called = atomic.LoadInt32(&r.called) != 0
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gotAddr, r.gotVT.Hostname, called
}

// serverPipe returns the server end of the dial pipe for the test feeder.
func (r *recordingPinnedDial) serverPipe() net.Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.serverConn
}

// recordingProxyDial captures the proxy address actually dialed (must be the
// operator proxy) and the CONNECT line written (must target the validated
// IP:port). It returns the client end of an in-memory pipe pair wrapped so the
// CONNECT request line the transport writes is captured.
type recordingProxyDial struct {
	mu         sync.Mutex
	called     int32
	gotProxy   string
	gotConnect string
	serverConn net.Conn
}

func (r *recordingProxyDial) DialProxy(ctx context.Context, network, address string) (net.Conn, error) {
	atomic.StoreInt32(&r.called, 1)
	r.mu.Lock()
	r.gotProxy = address
	client, server := newPipePair()
	r.serverConn = server
	r.mu.Unlock()
	// Capture the CONNECT line the transport writes on the client end.
	return &connectCaptureConn{Conn: client, recorder: r}, nil
}

// connectCaptureConn wraps the proxy client conn to capture the CONNECT request
// line pinnedHTTPConnect writes.
type connectCaptureConn struct {
	net.Conn
	recorder *recordingProxyDial
}

func (c *connectCaptureConn) Write(b []byte) (int, error) {
	c.recorder.mu.Lock()
	if c.recorder.gotConnect == "" {
		if idx := strings.Index(string(b), "\r\n"); idx > 0 {
			c.recorder.gotConnect = strings.TrimSpace(string(b[:idx]))
		}
	}
	c.recorder.mu.Unlock()
	return c.Conn.Write(b)
}

// snapshot returns a race-free copy of the recorded proxy dial + CONNECT line.
func (r *recordingProxyDial) snapshot() (proxy, connect string, called bool) {
	called = atomic.LoadInt32(&r.called) != 0
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gotProxy, r.gotConnect, called
}

// serverPipe returns the server end of the proxy pipe for the test feeder.
func (r *recordingProxyDial) serverPipe() net.Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.serverConn
}

// TestPinnedDirectUsesValidatedIPNotRebinding proves the direct route dials the
// validated IP even when a later lookup rebinds the hostname to loopback (DNS
// rebinding defeat). The validated IP is the one that passed SSRF policy; a
// re-resolution must NOT reach the dial.
func TestPinnedDirectUsesValidatedIPNotRebinding(t *testing.T) {
	var lookups int32
	lookup := func() net.IP {
		if atomic.AddInt32(&lookups, 1) == 1 {
			return net.ParseIP("203.0.113.77") // public, validation-passing
		}
		return net.ParseIP("127.0.0.1") // private — would be SSRF, must NOT be used
	}

	validIP := lookup()
	if !validIP.Equal(net.ParseIP("203.0.113.77")) {
		t.Fatalf("setup: first lookup should be public, got %s", validIP)
	}
	_ = lookup() // simulate the rebinding second lookup a naive dial would hit

	rec := &recordingPinnedDial{}
	restore := SetPinnedDialerForTest(rec)
	defer restore()

	vt := ValidatedTarget{Scheme: "http", Hostname: "rebind.example.com", Port: "80", IP: validIP}
	tr := pinnedDirectTransport(testOptions(), vt)
	fakeClient := &http.Client{Timeout: 2 * time.Second, Transport: tr, CheckRedirect: noRedirect}

	// Use an http URL so the transport does NOT attempt a TLS handshake — the
	// recording dial returns a pipe and feedHTTP writes a plain HTTP response,
	// letting Do() return cleanly. The SNI assertion is via the recorded vt.
	req, _ := http.NewRequest(http.MethodGet, "http://rebind.example.com/path", nil)

	// Feed the canned response asynchronously once DialPinned created the pipe.
	go func() {
		// Wait for the dial to produce a conn (poll the recording).
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if c := rec.serverPipe(); c != nil {
				feedHTTP(c, "200 OK", "ok")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	resp, err := fakeClient.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()

	if gotAddr, gotHost, called := rec.snapshot(); !called {
		t.Fatal("pinned direct dial was not invoked")
	} else {
		if gotAddr != "203.0.113.77:80" {
			t.Fatalf("pinned direct dialed %q, want validated 203.0.113.77:80 (DNS rebinding defeated)", gotAddr)
		}
		if gotHost != "rebind.example.com" {
			t.Fatalf("pinned direct SNI hostname = %q, want rebind.example.com", gotHost)
		}
	}
}

// TestPinnedHTTPConnectTargetsValidatedIP proves the HTTP-CONNECT route writes
// the CONNECT line to the validated IP:port while the proxy itself is dialed at
// the operator proxy address (not the pinned target).
func TestPinnedHTTPConnectTargetsValidatedIP(t *testing.T) {
	prec := &recordingProxyDial{}
	restoreProxy := SetProxyDialerForTest(prec)
	defer restoreProxy()

	validIP := net.ParseIP("203.0.113.77")
	vt := ValidatedTarget{Scheme: "https", Hostname: "img.example.com", Port: "443", IP: validIP}
	tr, err := pinnedHTTPProxyTransport(testOptions(), "http://proxy.operator.net:8080", vt)
	if err != nil {
		t.Fatalf("pinnedHTTPProxyTransport: %v", err)
	}
	fakeClient := &http.Client{Timeout: 2 * time.Second, Transport: tr, CheckRedirect: noRedirect}

	req, _ := http.NewRequest(http.MethodGet, "https://img.example.com/path", nil)

	// Feed the CONNECT 200 response once the proxy pipe exists.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if c := prec.serverPipe(); c != nil {
				// Drain the CONNECT request the transport wrote, then reply.
				go io.Copy(io.Discard, c)
				fmt.Fprintf(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				// Then the TLS-handshake bytes would follow; the fake transport
				// has no real TLS so the Do() errors after we have what we need.
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	_, _ = fakeClient.Do(req) // TLS handshake fails on the pipe; we assert below

	gotProxy, gotConnect, called := prec.snapshot()
	if !called {
		t.Fatal("proxy dial was not invoked")
	}
	if gotProxy != "proxy.operator.net:8080" {
		t.Fatalf("proxy dialed at %q, want proxy.operator.net:8080", gotProxy)
	}
	if !strings.HasPrefix(gotConnect, "CONNECT 203.0.113.77:443") {
		t.Fatalf("CONNECT line = %q, want target 203.0.113.77:443 (validated IP, not hostname)", gotConnect)
	}
}

// TestPinnedSocksUsesValidatedIP proves the SOCKS5 route requests the validated
// IP:port from the SOCKS5 server, not the request hostname. A minimal in-process
// SOCKS5 server records the requested destination.
func TestPinnedSocksUsesValidatedIP(t *testing.T) {
	validIP := net.ParseIP("203.0.113.77")
	vt := ValidatedTarget{Scheme: "https", Hostname: "sock.example.com", Port: "443", IP: validIP}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	requestedCh := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Greeting: VER(1) NMETHODS(1) METHODS(NMETHODS). No-auth client sends
		// "05 01 00". Read VER + NMETHODS, then the METHODS bytes.
		head := make([]byte, 2)
		if _, err := io.ReadFull(conn, head); err != nil {
			requestedCh <- ""
			return
		}
		methods := make([]byte, int(head[1]))
		if _, err := io.ReadFull(conn, methods); err != nil {
			requestedCh <- ""
			return
		}
		// Select no-auth (00).
		conn.Write([]byte{0x05, 0x00})
		// Request: VER CMD RSV ATYP DST.ADDR DST.PORT.
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			requestedCh <- ""
			return
		}
		var host string
		switch hdr[3] {
		case 0x01: // IPv4
			ip := make([]byte, 4)
			if _, err := io.ReadFull(conn, ip); err != nil {
				requestedCh <- ""
				return
			}
			host = net.IP(ip).String()
		case 0x03: // domain
			l := make([]byte, 1)
			if _, err := io.ReadFull(conn, l); err != nil {
				requestedCh <- ""
				return
			}
			d := make([]byte, int(l[0]))
			if _, err := io.ReadFull(conn, d); err != nil {
				requestedCh <- ""
				return
			}
			host = string(d)
		case 0x04: // IPv6
			ip := make([]byte, 16)
			if _, err := io.ReadFull(conn, ip); err != nil {
				requestedCh <- ""
				return
			}
			host = net.IP(ip).String()
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(conn, port); err != nil {
			requestedCh <- ""
			return
		}
		requestedCh <- host + ":" + fmt.Sprintf("%d", int(port[0])<<8|int(port[1]))
		// Success reply, bound addr 0.0.0.0:0.
		conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		io.Copy(io.Discard, conn)
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	tr, err := pinnedSocksTransport(context.Background(), testOptions(), &ParsedURL{
		Scheme: "socks5", Host: host, Port: port,
	}, vt)
	if err != nil {
		t.Fatalf("pinnedSocksTransport: %v", err)
	}
	fakeClient := &http.Client{Timeout: 2 * time.Second, Transport: tr, CheckRedirect: noRedirect}
	req, _ := http.NewRequest(http.MethodGet, "https://sock.example.com/path", nil)
	_, _ = fakeClient.Do(req) // TLS handshake fails after SOCKS connect; fine

	select {
	case socksRequestedAddr := <-requestedCh:
		if socksRequestedAddr != "203.0.113.77:443" {
			t.Fatalf("SOCKS5 requested %q, want pinned 203.0.113.77:443", socksRequestedAddr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS5 server never received a request")
	}
}

// TestProxyAwareFetchPinnedBypassesRelayAndFallback proves that a request
// carrying a ValidatedTarget never reaches the relay or fallback machinery —
// it always goes through the pinned path. A non-pinned request on the same
// proxyOpts keeps the standard behavior (relay hits the relay server).
func TestProxyAwareFetchPinnedBypassesRelayAndFallback(t *testing.T) {
	relayHit := int32(0)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&relayHit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer relay.Close()

	// An upstream that must NOT be hit: the pinned dial is faked, so the
	// request never reaches the real upstream URL. We still point req.URL at a
	// real-looking host so buildRelayRequest etc. have a sane URL.
	upstreamHit := int32(0)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rec := &recordingPinnedDial{}
	restore := SetPinnedDialerForTest(rec)
	defer restore()

	// Build the request against the upstream host so the URL is well-formed, but
	// attach a ValidatedTarget with a different (public) IP. The pinned dial is
	// faked and must be the only path taken; relay and the real upstream are
	// never contacted.
	vt := ValidatedTarget{
		Scheme:   "https",
		Hostname: "anything.example",
		Port:     "443",
		IP:       net.ParseIP("203.0.113.77"),
	}

	// Use an https URL so pinnedDirectTransport's TLS dial is consistent with
	// the fake conn (the recording dial returns a pipe, and net/http will run a
	// TLS handshake against it — which fails). We assert on the recording and
	// the bypass counters, not on resp.Body, so a TLS handshake error is fine.
	req, _ := http.NewRequest(http.MethodGet, "https://anything.example/path", nil)
	ctx := WithValidatedTarget(context.Background(), vt)
	req = req.WithContext(ctx)

	client := &http.Client{Timeout: 2 * time.Second}
	// ProxyAwareFetch on a pinned request ignores the passed client's transport
	// (it builds its own pinned transport); we still pass a sane client.
	_, _ = ProxyAwareFetch(req.Context(), client, req, testOptions(), ProxyFetchOptions{
		VercelRelayUrl: relay.URL, // would normally be hit first — pinned must bypass it
	}, nil)

	if atomic.LoadInt32(&relayHit) != 0 {
		t.Fatal("pinned request must bypass the relay server, but relay was hit")
	}
	if atomic.LoadInt32(&upstreamHit) != 0 {
		t.Fatal("pinned request must bypass upstream (fake dial), but upstream was hit")
	}
	if atomic.LoadInt32(&rec.called) == 0 {
		t.Fatal("pinned request should use the pinned dial path")
	}
	if rec.gotAddr != "203.0.113.77:443" {
		t.Fatalf("pinned dial addressed %q, want 203.0.113.77:443", rec.gotAddr)
	}
}

// TestProxyAwareFetchNonPinnedKeepsRelay proves the standard (non-pinned) path
// is unchanged: no ValidatedTarget on the context means the relay is used as
// before. Regression guard for "no-proxy non-image fetch keeps current behavior".
func TestProxyAwareFetchNonPinnedKeepsRelay(t *testing.T) {
	relayHit := int32(0)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&relayHit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer relay.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	// No ValidatedTarget on the context — standard pipeline.
	resp, err := ProxyAwareFetch(req.Context(), client, req, testOptions(), ProxyFetchOptions{
		VercelRelayUrl: relay.URL,
	}, nil)
	if err != nil {
		t.Fatalf("ProxyAwareFetch: %v", err)
	}
	defer resp.Body.Close()
	if atomic.LoadInt32(&relayHit) == 0 {
		t.Fatal("non-pinned request should hit the relay as before")
	}
}
