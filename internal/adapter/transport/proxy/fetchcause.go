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

// FetchError wraps an upstream fetch error with a diagnostic cause for logging.
type FetchError struct {
	Err   error
	Cause string
}

func (e *FetchError) Error() string {
	if e.Cause != "" {
		return fmt.Sprintf("%s: %s", e.Err.Error(), e.Cause)
	}
	return e.Err.Error()
}

func (e *FetchError) Unwrap() error { return e.Err }
