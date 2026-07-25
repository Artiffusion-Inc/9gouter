package imageproxy

// poll.go implements the shared polling lifecycle helper used by the async
// image providers (fal-ai, black-forest-labs, runwayml, nanobanana — steps 6–7).
// The helper owns ONLY deadline/sleep/context mechanics. Provider-specific
// request shape (URL, auth headers, method, body), host validation, and the
// status parser remain local to each adapter — the abstraction deliberately
// does not hide provider semantics (spec risk table).
//
// Contract (spec "Polling lifecycle"):
//   - interval: production default 1500ms (Dependencies.PollInterval)
//   - overall timeout: production default 120s (Dependencies.PollTimeout)
//   - terminate immediately on context cancellation/deadline (no false success)
//   - no retry after a terminal state (completed/failed/malformed)
//   - poll non-2xx / malformed / unexpected host / download failure → 502
//   - timeout → 504 (ErrPollTimeout)
//   - cancelled context → returns ctx.Err() (not a synthetic success)

import (
	"context"
	"fmt"
	"net/http"
	"time"

	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// PollStatus is the provider-local parser's verdict for one poll response.
type PollStatus string

const (
	// PollCompleted: the task finished successfully; the adapter extracts the
	// result from the response body and stops polling.
	PollCompleted PollStatus = "completed"
	// PollPending: the task is still running; the helper waits one interval and
	// polls again (subject to the overall timeout).
	PollPending PollStatus = "pending"
	// PollFailed: the task reached a terminal failure state; the helper stops
	// and returns ErrPollFailed (mapped to 502).
	PollFailed PollStatus = "failed"
	// PollMalformed: the response body could not be parsed into a known state;
	// the helper stops and returns ErrMalformedState (mapped to 502).
	PollMalformed PollStatus = "malformed"
)

// PollResult carries the parsed status, the raw response body (for the adapter
// to extract the result URLs/sample on completion), and the final poll URL.
type PollResult struct {
	Status PollStatus
	Body   []byte
	// FinalURL is the last poll URL attempted (redacted by the caller in logs).
	FinalURL string
}

// PollRequestFactory builds the *http.Request for one poll attempt at pollURL.
// The adapter attaches auth headers, the provider's host-allowlist predicate
// (via the request context if needed), and the connection metadata. The
// factory is called for every attempt so a redirect re-validation can rebuild
// the request without carrying credentials to a foreign origin.
type PollRequestFactory func(ctx context.Context, pollURL string) (*http.Request, error)

// PollStatusParser inspects one poll response body and returns the
// provider-local status. It MUST NOT retry; it only classifies. A non-2xx
// response is handled by the helper before the parser is invoked (the parser
// only sees 2xx bodies).
type PollStatusParser func(body []byte) (PollStatus, error)

// pollError is a typed provider diagnostic error carrying the HTTP status the
// caller should return. It implements the unified error-mapper approach
// (spec point 4: "Введи typed errors ... с методом HTTPStatus() int ИЛИ
// mapper-функцию").
type pollError struct {
	httpStatus int
	msg        string
	cause      error
}

func (e *pollError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.msg, e.cause)
	}
	return e.msg
}

func (e *pollError) Unwrap() error { return e.cause }

// HTTPStatus returns the HTTP status the handler should return for this error.
func (e *pollError) HTTPStatus() int { return e.httpStatus }

// ErrPollTimeout is returned when the overall poll deadline expires before the
// task reaches a terminal state. Maps to 504.
var ErrPollTimeout = &pollError{httpStatus: http.StatusGatewayTimeout, msg: "image poll timeout"}

// ErrPollFailed is returned when the provider reports a terminal failure state.
// Maps to 502.
func NewPollFailed(msg string) error { return &pollError{httpStatus: http.StatusBadGateway, msg: msg} }

// ErrMalformedState is returned when the poll response body cannot be parsed
// into a known state. Maps to 502.
func NewMalformedState(msg string) error {
	return &pollError{httpStatus: http.StatusBadGateway, msg: msg}
}

// ErrUnexpectedHost is returned when a poll/result URL host does not match the
// provider's allowlist. Maps to 502.
func NewUnexpectedHost(host string) error {
	return &pollError{httpStatus: http.StatusBadGateway, msg: fmt.Sprintf("provider returned unexpected polling host: %s", host)}
}

// ErrDownloadFailedPoll is the download-failure diagnostic used inside the poll
// path (alias of ErrDownloadFailed in image_security.go, kept exported here so
// tests can assert against it by identity). Maps to 502.
var ErrDownloadFailedPoll = ErrDownloadFailed

// poll executes the polling loop. It is exported as Handler.poll so the async
// adapters (steps 6–7) call it through the receiver; tests exercise it
// directly with a short PollInterval/PollTimeout.
//
// pollURL is the initial poll URL (taken from the submit response — never
// reconstructed from a model/base URL, spec invariant). The factory rebuilds
// the request for each attempt; the parser classifies 2xx bodies.
//
// The helper:
//  1. waits PollInterval before each attempt (no wait before the first attempt
//     is intentional — providers' submit response usually implies "ready later",
//     but the first poll is immediate so a fast-completing task does not pay a
//     1.5s tax; this matches the legacy JS behaviour);
//  2. honours ctx.Done() immediately between the wait and the fetch;
//  3. does not retry after a terminal state (completed/failed/malformed);
//  4. maps poll non-2xx to 502, timeout to 504, cancellation to ctx.Err().
func (h *Handler) poll(ctx context.Context, pollURL string, factory PollRequestFactory, parser PollStatusParser) (PollResult, error) {
	interval := h.deps.PollInterval
	if interval <= 0 {
		interval = 1500 * time.Millisecond
	}
	deadline := h.deps.PollTimeout
	if deadline <= 0 {
		deadline = 120 * time.Second
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	currentURL := pollURL
	for {
		if err := deadlineCtx.Err(); err != nil {
			if err == context.DeadlineExceeded {
				return PollResult{FinalURL: currentURL}, ErrPollTimeout
			}
			return PollResult{FinalURL: currentURL}, err
		}
		req, err := factory(deadlineCtx, currentURL)
		if err != nil {
			return PollResult{FinalURL: currentURL}, &pollError{httpStatus: http.StatusBadGateway, msg: "poll request build", cause: err}
		}
		// h.do attaches transport metadata; the adapter's factory already set
		// auth headers on the request. host is left zero here — the adapter
		// may attach a ValidatedHost via the factory if it pre-resolves the
		// poll host; otherwise the production executor treats poll URLs as
		// operator-trusted provider hosts (no SSRF pinning).
		resp, err := h.do(deadlineCtx, req, req.Header.Get("x-9gouter-provider"), "poll", domainProv.Credentials{}, "", ValidatedHost{})
		if err != nil {
			if deadlineCtx.Err() != nil {
				if deadlineCtx.Err() == context.DeadlineExceeded {
					return PollResult{FinalURL: currentURL}, ErrPollTimeout
				}
				return PollResult{FinalURL: currentURL}, deadlineCtx.Err()
			}
			return PollResult{FinalURL: currentURL}, &pollError{httpStatus: http.StatusBadGateway, msg: "poll fetch", cause: err}
		}
		body, readErr := readPollBody(resp)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return PollResult{FinalURL: currentURL, Body: body}, &pollError{
				httpStatus: http.StatusBadGateway,
				msg:        fmt.Sprintf("poll upstream status %d", resp.StatusCode),
			}
		}
		if readErr != nil {
			return PollResult{FinalURL: currentURL}, &pollError{httpStatus: http.StatusBadGateway, msg: "poll body read", cause: readErr}
		}
		status, perr := parser(body)
		if perr != nil {
			return PollResult{FinalURL: currentURL, Body: body}, NewMalformedState(perr.Error())
		}
		switch status {
		case PollCompleted:
			return PollResult{Status: PollCompleted, Body: body, FinalURL: currentURL}, nil
		case PollFailed:
			return PollResult{Status: PollFailed, Body: body, FinalURL: currentURL}, NewPollFailed("provider reported task failure")
		case PollMalformed:
			return PollResult{Status: PollMalformed, Body: body, FinalURL: currentURL}, NewMalformedState("provider returned malformed poll state")
		case PollPending:
			// fall through to the wait
		default:
			return PollResult{FinalURL: currentURL, Body: body}, NewMalformedState(fmt.Sprintf("unknown poll status %q", status))
		}
		// Wait one interval, honouring cancellation.
		select {
		case <-deadlineCtx.Done():
			if deadlineCtx.Err() == context.DeadlineExceeded {
				return PollResult{FinalURL: currentURL}, ErrPollTimeout
			}
			return PollResult{FinalURL: currentURL}, deadlineCtx.Err()
		case <-time.After(interval):
		}
	}
}

// readPollBody reads the poll response body with a sane cap (poll bodies are
// small status JSON, not image payloads).
func readPollBody(resp *http.Response) ([]byte, error) {
	const cap = 4 << 20 // 4 MiB
	r := resp.Body
	if resp.ContentLength > cap {
		return nil, fmt.Errorf("poll body too large: %d", resp.ContentLength)
	}
	buf := make([]byte, 0, 4096)
	for {
		chunk := make([]byte, 4096)
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > cap {
				return nil, fmt.Errorf("poll body exceeds %d bytes", cap)
			}
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
