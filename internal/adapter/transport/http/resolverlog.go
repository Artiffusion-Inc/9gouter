package http

import (
	"log/slog"
)

// slogAdapter bridges *slog.Logger to the resolver.Logger interface so the
// live-model resolvers can emit diagnostics through the v1 handler's logger
// without depending on slog directly.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Info(msg string, args ...any)  { a.l.Info(msg, args...) }
func (a slogAdapter) Warn(msg string, args ...any)  { a.l.Warn(msg, args...) }
func (a slogAdapter) Debug(msg string, args ...any) { a.l.Debug(msg, args...) }