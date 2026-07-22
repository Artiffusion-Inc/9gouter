package cursorexec

// h2stream_test.go pins the duplex AgentService h2 transport against a real
// in-process h2 server (httptest TLS, ALPN-negotiated). No mocks: the server
// is a genuine net/http handler that reads the request body (duplex) and
// writes response frames on the same stream, exercising the half-open
// client→server write path that Go's high-level client normally forbids.

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// wrapConnect wraps payload as a Connect RPC frame (flags=0, BE length).
func wrapConnect(payload []byte) []byte {
	f := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(f[1:5], uint32(len(payload)))
	copy(f[5:], payload)
	return f
}

// newAgentTestServer returns an httptest TLS+h2 server whose handler mimics
// the Cursor AgentService duplex contract:
//  1. read the client run_request frame,
//  2. emit a request_context_args frame,
//  3. read the client's request-context response frame (duplex write),
//  4. emit an interaction_update text frame + a done frame,
//     then end the response.
func newAgentTestServer(t *testing.T, onRun func(runPayload []byte)) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		// 1. read the client run_request (first Connect frame) without closing
		//    the body — it stays open for the client's request-context write.
		header := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, header); err != nil {
			t.Errorf("server read frame header: %v", err)
			http.Error(w, "read header", http.StatusBadRequest)
			return
		}
		length := int(binary.BigEndian.Uint32(header[1:5]))
		payload := make([]byte, length)
		if _, err := io.ReadFull(r.Body, payload); err != nil {
			t.Errorf("server read frame payload: %v", err)
			http.Error(w, "read payload", http.StatusBadRequest)
			return
		}
		if onRun != nil {
			onRun(payload)
		}

		// 2. request_context_args → exec_request field 2 with field 10.
		execReq := agentMessage(10, nil)
		_, _ = w.Write(wrapConnect(agentMessage(2, execReq)))
		if flusher != nil {
			flusher.Flush()
		}

		// 3. read the client's request-context response (duplex write).
		ctxHeader := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, ctxHeader); err != nil {
			t.Errorf("server read ctx response header: %v", err)
			return
		}
		ctxLen := int(binary.BigEndian.Uint32(ctxHeader[1:5]))
		ctxPayload := make([]byte, ctxLen)
		if _, err := io.ReadFull(r.Body, ctxPayload); err != nil {
			t.Errorf("server read ctx response payload: %v", err)
			return
		}
		ctxMsg := decodeMessage(ctxPayload)
		if ctxMsg.first(2) == nil {
			t.Errorf("server: ctx response missing exec_client_message field 2, payload=%x", ctxPayload)
		}

		// 4. interaction_update text delta + done.
		textUpdate := agentString(1, "Hello from agent")
		_, _ = w.Write(wrapConnect(agentMessage(1, textUpdate)))
		if flusher != nil {
			flusher.Flush()
		}
		doneUpdate := agentBool(14, true)
		_, _ = w.Write(wrapConnect(agentMessage(1, doneUpdate)))
		if flusher != nil {
			flusher.Flush()
		}
	})

	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestAgentSession_DuplexRoundTrip(t *testing.T) {
	var seenRun []byte
	srv := newAgentTestServer(t, func(p []byte) { seenRun = p })

	tr := &http2.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}},
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
			return tls.Dial("tcp", net.JoinHostPort(host, port), cfg)
		},
	}
	endpoint, _ := url.Parse(srv.URL)
	headers := http.Header{}
	headers.Set("authorization", "Bearer tok")
	runReq := BuildAgentRunFrame([]AgentMessage{{Role: "user", Content: "hi"}}, "m")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := OpenAgentSession(ctx, tr, endpoint, headers, runReq)
	if err != nil {
		t.Fatalf("OpenAgentSession: %v", err)
	}
	defer sess.Close()

	if sess.Status() != 200 {
		t.Fatalf("status=%d want 200", sess.Status())
	}
	// The server received a valid run_request.
	if decodeMessage(seenRun).first(1) == nil {
		t.Errorf("server saw no run_request field 1: %x", seenRun)
	}

	// Consume frames: drive the duplex write for request_context, accumulate
	// text deltas, stop at done.
	var texts []string
	var answeredContext bool
	for {
		data, rerr := sess.Read()
		if data == nil {
			if rerr == io.EOF || rerr == nil {
				break
			}
			t.Fatalf("Read error: %v", rerr)
		}
		DecodeAgentFrames(data, func(payload []byte) {
			ev, needCtx := DecodeAgentServerMessage(payload)
			if needCtx {
				if werr := sess.Write(CreateRequestContextResponse()); werr != nil {
					t.Errorf("Write ctx response: %v", werr)
				}
				answeredContext = true
				return
			}
			switch ev.Type {
			case "text":
				texts = append(texts, ev.Value)
			case "done":
				return
			}
		})
		if len(texts) > 0 {
			break
		}
	}
	if !answeredContext {
		t.Error("expected to answer a request_context mid-stream (duplex write)")
	}
	if len(texts) == 0 || texts[0] != "Hello from agent" {
		t.Errorf("texts=%v want [Hello from agent]", texts)
	}
}

func TestAgentSession_Non200(t *testing.T) {
	// A server that rejects immediately surfaces a non-200 status.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTooManyRequests)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	tr := &http2.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}},
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
			return tls.Dial("tcp", net.JoinHostPort(host, port), cfg)
		},
	}
	endpoint, _ := url.Parse(srv.URL)
	runReq := BuildAgentRunFrame([]AgentMessage{{Role: "user", Content: "hi"}}, "m")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := OpenAgentSession(ctx, tr, endpoint, http.Header{}, runReq)
	if err != nil {
		// Some h2 transports surface 429 as a response, others as an error
		// (RoundTripErr). Either is acceptable; the executor handles both.
		if sess == nil {
			return
		}
	}
	defer sess.Close()
	if sess.Status() != 0 && sess.Status()/100 != 2 {
		return
	}
	t.Errorf("expected non-2xx status or error, got status=%d err=%v", sess.Status(), err)
}
