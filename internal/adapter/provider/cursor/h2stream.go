// h2stream.go ports openAgentHttp2Stream from open-sse/executors/cursor.js
// (upstream v0.5.40, commit 6994cd1f). The Cursor AgentService at
// agent.api5.cursor.sh is HTTP/2-only AND bidirectional: after the client sends
// its run_request, the server streams interaction_update frames back on the
// SAME stream and may send a request_context_args that the client must answer
// (CreateRequestContextResponse) by writing back on the same stream mid-flight.
//
// Go's net/http client does NOT support writing to a request body after the
// response HEADERS arrive — http.Request.Body is consumed before the response
// is returned. golang.org/x/net/http2.ClientConn.RoundTrip, however, streams
// request Body as DATA frames concurrently with reading the response, so an
// io.Pipe body lets us keep writing frames to the same stream after RoundTrip
// returns the response. This file implements that duplex session.
package cursorexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// AgentSession is the bidirectional AgentService stream surface the executor
// consumes. Defined as an interface so the executor loop (#99) is unit-testable
// with a fake session; the production implementation is h2AgentSession below.
type AgentSession interface {
	// Write sends a Connect-RPC frame back to the server on the open stream
	// (used for CreateRequestContextResponse mid-stream).
	Write(frame []byte) error
	// Read returns the next raw frame payload the server sent. Returns
	// io.EOF when the stream ended. The caller decodes Connect frames via
	// DecodeAgentFrames.
	Read() ([]byte, error)
	// Status is the HTTP response status code (200 on success).
	Status() int
	// Close releases the stream and the underlying h2 connection.
	Close() error
}

// agentSessionTimeout is the hard ceiling for a single AgentService run —
// prevents a hung stream from leaking a goroutine forever. Mirrors the JS
// HTTP2_TIMEOUT_MS (60s) hang timeout; the proxychat stall timeout still
// governs client-facing liveness separately.
const agentSessionTimeout = 60 * time.Second

// h2AgentSession is a duplex AgentService stream over golang.org/x/net/http2.
//
// The request body is an io.Pipe: Open writes the initial run_request frame and
// keeps the pipe open so Write can append more frames (request-context
// responses) on the same h2 stream. RoundTrip streams the pipe as DATA frames
// while concurrently delivering the response; resp.Body yields the server
// frames. The read goroutine copies resp.Body into a channel so Read is
// non-blocking and survives after the pipe closes.
type h2AgentSession struct {
	pw   *io.PipeWriter
	pr   *io.PipeReader
	resp *http.Response
	// cancel, when non-nil, cancels the session's derived context (the
	// agentSessionTimeout ceiling). Invoked from Close.
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	readCh   chan readResult
	readDone chan struct{}
}

type readResult struct {
	data []byte
	err  error
}

// OpenAgentSession dials agent.api5.cursor.sh over h2, POSTs the initial
// runRequest frame, and returns a duplex session. The caller must Close it.
//
// transport may be nil (a default direct h2 Transport is used). Injecting a
// transport is how tests target an in-process h2 server and how a future
// proxy-aware dial would route the AgentService call through a SOCKS5/HTTP
// proxy (DialTLSContext on the transport).
func OpenAgentSession(ctx context.Context, transport *http2.Transport, endpoint *url.URL, headers http.Header, runRequest []byte) (AgentSession, error) {
	if transport == nil {
		transport = &http2.Transport{}
	}
	var deferCancel context.CancelFunc
	// Guard against a caller that hands us a context with no deadline: a hung
	// AgentService stream would otherwise leak the pump goroutine forever.
	// The proxychat stall timeout governs client-facing liveness separately;
	// this is a hard ceiling on the duplex session itself (mirrors JS
	// HTTP2_TIMEOUT_MS = 60s).
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, agentSessionTimeout)
		// The derived ctx owns the request/stream lifetime, so cancel must outlive
		// OpenAgentSession — a defer here would cancel the stream the moment Open
		// returns. The session Close invokes it instead.
		deferCancel = cancel
	}
	pr, pw := io.Pipe()
	reqURL := *endpoint
	reqURL.Scheme = "https"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL.String(), pr)
	if err != nil {
		pw.Close()
		return nil, err
	}
	// h2 pseudo-headers; net/http2 sets :method/:scheme/:path/:authority from
	// the URL, but the Cursor headers (authorization, x-cursor-*) must be plain.
	for k, vv := range headers {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	// AgentService Connect unary uses application/connect+proto for the
	// streaming Run RPC (the gateway rejects application/proto here).
	req.Header.Set("content-type", "application/connect+proto")
	req.Header.Set("te", "trailers")

	// Write the initial run_request frame from a goroutine: an io.Pipe Write
	// blocks until a reader drains it, and RoundTrip only starts reading the
	// body once it has sent the request HEADERS — so writing synchronously
	// here would deadlock. The pipe stays open after the write so Write() can
	// append request-context responses on the same h2 stream mid-flight.
	writeErr := make(chan error, 1)
	go func() {
		_, werr := pw.Write(runRequest)
		writeErr <- werr
	}()

	// RoundTrip blocks until response HEADERS arrive. The pipe stays open so
	// the stream is half-open (client→server) for later Write calls.
	resp, err := transport.RoundTrip(req)
	if err != nil {
		pw.Close()
		return nil, fmt.Errorf("cursor agent h2 roundtrip: %w", err)
	}
	if werr := <-writeErr; werr != nil {
		resp.Body.Close()
		pw.Close()
		return nil, fmt.Errorf("cursor agent write run_request: %w", werr)
	}

	s := &h2AgentSession{
		pw:       pw,
		pr:       pr,
		resp:     resp,
		cancel:   deferCancel,
		readCh:   make(chan readResult, 1),
		readDone: make(chan struct{}),
	}
	// Pump resp.Body frames into readCh so Read is channel-driven and survives
	// after the write side closes. A 1-buffered channel plus a fresh goroutine
	// read keeps the body draining (h2 flow control requires the client to
	// consume DATA frames or the server stalls).
	go s.pumpBody()
	return s, nil
}

func (s *h2AgentSession) pumpBody() {
	defer close(s.readDone)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.resp.Body.Read(buf)
		if n > 0 {
			out := make([]byte, n)
			copy(out, buf[:n])
			select {
			case s.readCh <- readResult{data: out, err: nil}:
			case <-s.readDone:
				return
			}
		}
		if err != nil {
			// Map h2 stream errors to io.EOF for the consumer; non-EOF errors
			// are surfaced too so the executor can emit a terminal error.
			select {
			case s.readCh <- readResult{data: nil, err: err}:
			case <-s.readDone:
			}
			return
		}
	}
}

func (s *h2AgentSession) Status() int {
	if s.resp == nil {
		return 0
	}
	return s.resp.StatusCode
}

func (s *h2AgentSession) Write(frame []byte) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("cursor agent session closed")
	}
	if _, err := s.pw.Write(frame); err != nil {
		return err
	}
	return nil
}

func (s *h2AgentSession) Read() ([]byte, error) {
	select {
	case r, ok := <-s.readCh:
		if !ok {
			return nil, io.EOF
		}
		return r.data, r.err
	case <-s.readDone:
		return nil, io.EOF
	}
}

func (s *h2AgentSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	// Close the write side: signals end-of-stream to the server.
	_ = s.pw.Close()
	// Close the response body / h2 stream.
	if s.resp != nil && s.resp.Body != nil {
		_ = s.resp.Body.Close()
	}
	// Cancel the derived session context (the agentSessionTimeout ceiling),
	// if one was created. Safe to call on a caller-supplied context's cancel
	// only when Open created it (cancel == nil otherwise).
	if s.cancel != nil {
		s.cancel()
	}
	<-s.readDone
	return nil
}
