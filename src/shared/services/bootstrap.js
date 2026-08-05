// Server-side bootstrap is a no-op in the Go-rewrite static export.
// All server-side init (tunnel, MITM, quota auto-ping, watchdog) is handled
// by the Go backend. This stub exists so layout.js's import resolves cleanly
// during the Next.js static export build.
export {};
