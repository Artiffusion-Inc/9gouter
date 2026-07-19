package resolver

// nopLogger drops all log output. Used as a fallback when a caller passes a
// ResolveOpts without a Logger, so resolvers never panic on nil Logger.
type nopLogger struct{}

// NopLogger returns a Logger that discards everything.
func NopLogger() Logger { return nopLogger{} }

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Debug(string, ...any) {}

// loggerOr returns opts.Logger if set, else a NopLogger.
func loggerOr(l Logger) Logger {
	if l == nil {
		return NopLogger()
	}
	return l
}