package resolver

import (
	"context"
	"fmt"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// ErrQoderCosyNotPorted signals that the Qoder live model resolver is not yet
// ported: Qoder's /algo/api/v2/model/list endpoint requires a COSY
// (RSA+AES-128-CBC+MD5) signed request — buildCosyHeaders in
// open-sse/shared/qoder/cosy.js. Porting that hybrid signing scheme is a
// follow-up; until then the resolver returns this error and /v1/models falls
// back to the static Qoder catalog.
var ErrQoderCosyNotPorted = fmt.Errorf("qoder live resolver not yet ported (COSY RSA+AES+MD5 signing, T030 follow-up)")

// qoderResolver is a stub: it always returns ErrQoderCosyNotPorted so the
// caller falls back to the static catalog. Registered so /v1/models dispatch
// reaches it and the gap is visible (rather than silently missing).
type qoderResolver struct{}

// NewQoderResolver builds the stub Qoder resolver.
func NewQoderResolver() LiveModelResolver { return &qoderResolver{} }

func (qoderResolver) ProviderID() string { return "qoder" }

func (qoderResolver) Resolve(_ context.Context, _ provider.Credentials, _ ResolveOpts) (*Result, error) {
	return nil, ErrQoderCosyNotPorted
}