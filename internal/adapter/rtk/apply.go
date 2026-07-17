package rtk

// safeApply is the Go equivalent of open-sse/rtk/applyFilter.js safeApply.
// On panic/error it returns the original text and logs via the provided logger
// (nil logger is tolerated).
type filterFunc func(string) string

func safeApply(fn filterFunc, text string, log logger) string {
	if fn == nil {
		return text
	}
	defer func() {
		if r := recover(); r != nil {
			if log != nil {
				log.Warnf("[rtk] warning: filter panicked — passing through raw output: %v", r)
			}
		}
	}()
	out := fn(text)
	if out == "" {
		return text
	}
	return out
}
