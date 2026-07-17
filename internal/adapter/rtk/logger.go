package rtk

// logger is a minimal log sink used by the RTK adapter for structured HEADROOM
// and warning lines. The production wiring will inject a real logger; tests pass nil.
type logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}
