package proxy

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// DescribeFetchCause flattens a Go dial/fetch error chain into a single
// diagnostic line with code/syscall/errno/address:port. It ports the intent
// of open-sse/utils/fetchCause.js for Go net.OpError chains.
func DescribeFetchCause(err error) string {
	if err == nil {
		return ""
	}
	var parts []string
	seen := make(map[error]struct{})
	cur := err
	for depth := 0; cur != nil && depth < 5; depth++ {
		if _, ok := seen[cur]; ok {
			break
		}
		seen[cur] = struct{}{}
		seg := errorSegment(cur)
		if seg != "" {
			parts = append(parts, seg)
		}
		// Aggregate errors (e.g., from happy eyeballs).
		if aggr, ok := cur.(interface{ Unwrap() []error }); ok {
			for _, sub := range aggr.Unwrap() {
				if sub == nil {
					continue
				}
				if s := errorSegment(sub); s != "" {
					parts = append(parts, "-> "+s)
				} else if sub.Error() != "" {
					parts = append(parts, "-> "+truncate(sub.Error(), 80))
				}
			}
		}
		cur = errors.Unwrap(cur)
	}
	if len(parts) == 0 {
		return err.Error()
	}
	return strings.Join(parts, " | ")
}

func errorSegment(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		parts := []string{}
		if opErr.Op != "" {
			parts = append(parts, opErr.Op)
		}
		if opErr.Net != "" {
			parts = append(parts, opErr.Net)
		}
		if opErr.Err != nil {
			parts = append(parts, truncate(opErr.Err.Error(), 160))
		}
		if opErr.Source != nil {
			parts = append(parts, "src="+opErr.Source.String())
		}
		if opErr.Addr != nil {
			parts = append(parts, "dst="+opErr.Addr.String())
		}
		return strings.Join(parts, " ")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		parts := []string{"dns"}
		if dnsErr.Name != "" {
			parts = append(parts, dnsErr.Name)
		}
		if dnsErr.Err != "" {
			parts = append(parts, truncate(dnsErr.Err, 160))
		}
		return strings.Join(parts, " ")
	}
	var syscallErr *net.AddrError
	if errors.As(err, &syscallErr) {
		return "addr: " + truncate(syscallErr.Error(), 160)
	}
	msg := err.Error()
	if msg == "" || msg == "fetch failed" {
		return ""
	}
	return truncate(msg, 160)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// FailureSource categorises where an upstream fetch error originated, so
// callers (account-selection / fallback logic) can decide whether a failure
// should lock the account or only mark the route unhealthy. Ports the intent
// of decolua/9router #2703 Fix 5 (Diagnostics) — distinct from the JS build,
// which only ever produced a flattened string. The typed value lets
// checkFallbackError treat a proxy/relay outage (FailureSourceProxy /
// FailureSourceRelay) as shouldFallback:false so a healthy account is not
// locked when its proxy is down.
type FailureSource string

const (
	// FailureSourceUnknown is the zero value; the failure could not be
	// classified (e.g. a non-net error surfaced without a cause chain).
	FailureSourceUnknown FailureSource = "unknown"
	// FailureSourceProxy means the error came from the configured connection
	// or environment proxy (dial/handshake/transport). The account itself is
	// not at fault.
	FailureSourceProxy FailureSource = "proxy"
	// FailureSourceRelay means the error came from the Vercel relay hop.
	FailureSourceRelay FailureSource = "relay"
	// FailureSourceUpstream means the error came from the target provider
	// (non-2xx status, upstream-side network). The account may be at fault.
	FailureSourceUpstream FailureSource = "upstream"
)

// FetchError wraps an upstream fetch error with a diagnostic cause for logging.
// Source categorises the failure origin so account-selection logic can
// distinguish a proxy outage from a provider failure (#2703 Fix 5).
type FetchError struct {
	Err    error
	Cause  string
	Source FailureSource
}

func (e *FetchError) Error() string {
	if e.Cause != "" {
		return fmt.Sprintf("%s: %s", e.Err.Error(), e.Cause)
	}
	return e.Err.Error()
}

func (e *FetchError) Unwrap() error { return e.Err }
