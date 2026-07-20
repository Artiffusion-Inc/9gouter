package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// RegisterHeadroom mounts headroom proxy management routes.
func RegisterHeadroom(mux *http.ServeMux, deps Deps) {
	h := &headroomHandler{deps: deps}
	mux.HandleFunc("GET /api/headroom/status", h.status)
	mux.HandleFunc("GET /api/headroom/extras", h.extras)
	mux.HandleFunc("POST /api/headroom/extras", h.extrasAction)
	mux.HandleFunc("DELETE /api/headroom/extras", h.extrasAction)
	mux.HandleFunc("POST /api/headroom/start", h.start)
	mux.HandleFunc("POST /api/headroom/stop", h.stop)
	mux.HandleFunc("POST /api/headroom/restart", h.restart)
	mux.HandleFunc("GET /api/headroom/proxy/{path...}", h.proxy)
	mux.HandleFunc("POST /api/headroom/proxy/{path...}", h.proxy)
	mux.HandleFunc("PUT /api/headroom/proxy/{path...}", h.proxy)
	mux.HandleFunc("DELETE /api/headroom/proxy/{path...}", h.proxy)
	mux.HandleFunc("PATCH /api/headroom/proxy/{path...}", h.proxy)

	// The JS route was headroom/proxy/[...path]; the Go equivalent is
	// "{path...}" covering any depth. No additional registration needed.
}

type headroomHandler struct {
	deps Deps
}

// headroomCompressionExtras mirrors the JS whitelist in
// src/lib/headroom/detect.js. Only these values are accepted by POST/DELETE
// /api/headroom/extras to keep the pip-install surface predictable.
var headroomCompressionExtras = []string{"code", "ml"}

// extraMarkers mirrors detect.js EXTRA_MARKERS. The marker packages are used
// both for reporting the GET /api/headroom/extras `extras` field and for
// building the `pip uninstall` argv on DELETE.
var extraMarkers = map[string][]string{
	"code": {"tree-sitter", "tree-sitter-language-pack"},
	"ml":   {"torch", "huggingface-hub"},
}

// headroomDataDir matches JS src/lib/headroom/process.js (DATA_DIR/headroom).
// HEADROOM_DATA_DIR is the Go-side escape hatch for non-default layouts.
func headroomDataDir() string {
	if dir := os.Getenv("HEADROOM_DATA_DIR"); dir != "" {
		return dir
	}
	return filepath.Join("data", "headroom")
}

func headroomPIDFile() string    { return filepath.Join(headroomDataDir(), "proxy.pid") }
func headroomLogFile() string    { return filepath.Join(headroomDataDir(), "proxy.log") }
func headroomInstallLog() string { return filepath.Join(headroomDataDir(), "install.log") }

const (
	headroomDefaultPort    = 8787
	headroomPipTimeout     = 30 * time.Second
	headroomMinPythonMajor = 3
	headroomMinPythonMinor = 10
)

// pythonBinCandidates enumerates interpreters the frontend would consider,
// mirroring detect.js PYTHON_CANDIDATES.
func pythonBinCandidates() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"python3.13.exe", "python3.12.exe", "python3.11.exe", "python3.10.exe",
			"python3.exe", "python.exe",
		}
	}
	return []string{"python3.13", "python3.12", "python3.11", "python3.10", "python3", "python"}
}

// whichOrWhere picks `which` on unix and `where` on windows, matching
// detect.js WHICH_CMD.
func whichOrWhere() string {
	if runtime.GOOS == "windows" {
		return "where"
	}
	return "which"
}

// findHeadroomBinary returns the absolute path of the `headroom` binary if
// found on PATH, or "" if not installed. Mirrors detect.js findHeadroomBinary.
func findHeadroomBinary() string {
	cmd := exec.Command(whichOrWhere(), "headroom")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	return strings.TrimSpace(strings.SplitN(line, "\r", 2)[0])
}

// findPython310 returns the first candidate that satisfies the version check.
// Mirrors detect.js findPython310 (without the headroom-ai weighting — we
// only need any python3.10+ to drive pip).
func findPython310() string {
	for _, c := range pythonBinCandidates() {
		if pythonVersionOK(c) {
			return c
		}
	}
	return ""
}

func pythonVersionOK(bin string) bool {
	cmd := exec.Command(bin, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	major, minor, ok := parsePythonVersion(string(out))
	if !ok {
		return false
	}
	if major > headroomMinPythonMajor {
		return true
	}
	return major == headroomMinPythonMajor && minor >= headroomMinPythonMinor
}

func parsePythonVersion(s string) (int, int, bool) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		// "Python 3.12.4" or "python 3.10".
		idx := strings.Index(line, " ")
		if idx < 0 {
			continue
		}
		rest := strings.TrimPrefix(line[idx+1:], "v")
		if dash := strings.Index(rest, "-"); dash >= 0 {
			rest = rest[:dash]
		}
		parts := strings.SplitN(rest, ".", 3)
		if len(parts) < 2 {
			continue
		}
		major, ok1 := atoiSafe(parts[0])
		minor, ok2 := atoiSafe(parts[1])
		if ok1 && ok2 {
			return major, minor, true
		}
	}
	return 0, 0, false
}

func atoiSafe(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// isLoopbackHeadroomURL mirrors detect.js isLoopbackHeadroomUrl.
var loopbackHosts = map[string]struct{}{
	"localhost": {}, "127.0.0.1": {}, "::1": {},
}

func isLoopbackHeadroomURL(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return false
	}
	host := u
	if i := strings.Index(u, "://"); i >= 0 {
		host = u[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	// Strip bracketed IPv6 + optional port.
	host = strings.TrimPrefix(host, "[")
	if i := strings.Index(host, "]"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	// Drop :port if all-digits.
	if i := strings.LastIndex(host, ":"); i >= 0 {
		tail := host[i+1:]
		allDigits := tail != ""
		for _, c := range tail {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			host = host[:i]
		}
	}
	_, ok := loopbackHosts[host]
	return ok
}

// getInstalledHeadroomExtras runs `pip list --format=json` on the given
// interpreter and reports whether `headroom-ai` is installed, its version,
// and which compression-extras marker packages are present. Mirrors
// detect.js getInstalledHeadroomExtras.
func getInstalledHeadroomExtras(python string) (installed bool, version string, extras map[string]bool) {
	extras = map[string]bool{"code": false, "ml": false}
	if python == "" {
		return false, "", extras
	}
	ctx, cancel := context.WithTimeout(context.Background(), headroomPipTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-m", "pip", "list", "--format=json", "--disable-pip-version-check")
	out, err := cmd.Output()
	if err != nil {
		return false, "", extras
	}
	var pkgs []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &pkgs); err != nil {
		return false, "", extras
	}
	names := make(map[string]struct{}, len(pkgs))
	for _, p := range pkgs {
		names[strings.ToLower(p.Name)] = struct{}{}
	}
	if _, ok := names["headroom-ai"]; !ok {
		return false, "", extras
	}
	for _, p := range pkgs {
		if strings.EqualFold(p.Name, "headroom-ai") {
			version = p.Version
			break
		}
	}
	for _, extra := range headroomCompressionExtras {
		for _, marker := range extraMarkers[extra] {
			if _, ok := names[marker]; ok {
				extras[extra] = true
				break
			}
		}
	}
	return true, version, extras
}

// headroomStatusResponse mirrors the JS getHeadroomStatus() shape from
// src/lib/headroom/detect.js. The TokenSaverClient renders this directly, so
// field names matter.
type headroomStatusResponse struct {
	Installed  bool            `json:"installed"`
	Path       string          `json:"path,omitempty"`
	Running    bool            `json:"running"`
	Python     any             `json:"python"` // string|null
	LocalURL   bool            `json:"localUrl"`
	CanStart   bool            `json:"canStart"`
	ManagedPID any             `json:"managedPid"` // int|null
	URL        string          `json:"url,omitempty"`
	Version    any             `json:"version"`    // string|null
	Extras     map[string]bool `json:"extras,omitempty"`
}

func (h *headroomHandler) status(w http.ResponseWriter, r *http.Request) {
	bin := findHeadroomBinary()
	py := findPython310()
	installed := bin != ""
	settingsURL := headroomURLFromSettings()
	localURL := isLoopbackHeadroomURL(settingsURL)
	running := false
	if installed && localURL {
		// Only attempt liveness when we manage a local daemon; external URLs
		// are user-managed and probing them blindly is a needless SSRF risk.
		running = probeLocalHeadroom(settingsURL)
	}
	managedPID := managedPID()
	canStart := installed && localURL
	resp := headroomStatusResponse{
		Installed:  installed,
		Path:       bin,
		Running:    running,
		Python:     nilOrString(py),
		LocalURL:   localURL,
		CanStart:   canStart,
		ManagedPID: nilOrInt(managedPID),
		URL:        settingsURL,
	}
	if installed {
		_, version, extras := getInstalledHeadroomExtras(py)
		if version != "" {
			resp.Version = version
		}
		resp.Extras = extras
	}
	writeJSON(w, http.StatusOK, resp)
}

// headroomURLFromSettings returns the headroom URL the UI is configured
// against. The JS server reads this from settingsRepo; for the Go rewrite we
// fall back to HEADROOM_URL env (matches detect.js DEFAULT_HEADROOM_URL) and
// otherwise the loopback default. This keeps the status response stable
// when settings storage is unavailable.
func headroomURLFromSettings() string {
	if v := os.Getenv("HEADROOM_URL"); v != "" {
		return v
	}
	return "http://localhost:8787"
}

func nilOrString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilOrInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// extrasResponse mirrors {version, extras, available} from the JS contract
// (TokenSaverClient reads ed.version, ed.extras, ed.available). The install
// action endpoint also returns this shape.
type extrasResponse struct {
	Version   any             `json:"version"`   // string|null
	Extras    map[string]bool `json:"extras"`    // {code, ml}
	Available []string        `json:"available"` // fixed list, ["code", "ml"]
}

// extrasRunning tracks in-flight pip actions. The JS UI uses a single poll
// stream, so a single in-flight slot is plenty.
var (
	extrasMu      sync.Mutex
	extrasRunning bool
)

func (h *headroomHandler) extras(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("log") == "1" {
		tail := getInstallLogTail(200)
		writeJSON(w, http.StatusOK, map[string]any{"log": tail})
		return
	}
	bin := findHeadroomBinary()
	resp := extrasResponse{
		Version:   nil,
		Extras:    map[string]bool{"code": false, "ml": false},
		Available: append([]string(nil), headroomCompressionExtras...),
	}
	if bin != "" {
		py := findPython310()
		installed, version, extras := getInstalledHeadroomExtras(py)
		if installed {
			resp.Version = nilOrString(version)
			if extras != nil {
				resp.Extras = extras
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type extrasActionRequest struct {
	Extras []string `json:"extras"`
}

// extrasAction handles POST (install) and DELETE (uninstall) for compression
// extras. The frontend always sends a JSON body {extras:[...]} on both verbs.
func (h *headroomHandler) extrasAction(w http.ResponseWriter, r *http.Request) {
	var req extrasActionRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	// Whitelist.
	cleaned := make([]string, 0, len(req.Extras))
	for _, e := range req.Extras {
		for _, allowed := range headroomCompressionExtras {
			if e == allowed {
				cleaned = append(cleaned, e)
				break
			}
		}
	}
	if len(cleaned) == 0 {
		writeError(w, http.StatusBadRequest, "no valid extras specified")
		return
	}
	py := findPython310()
	if py == "" {
		writeError(w, http.StatusServiceUnavailable, "Python >= 3.10 not found")
		return
	}
	bin := findHeadroomBinary()
	if r.Method == http.MethodPost && bin == "" {
		writeError(w, http.StatusPreconditionFailed, "headroom-ai not installed (run `pip install headroom-ai[proxy]` first)")
		return
	}

	extrasMu.Lock()
	if extrasRunning {
		extrasMu.Unlock()
		writeError(w, http.StatusConflict, "another install/uninstall is already in progress")
		return
	}
	extrasRunning = true
	extrasMu.Unlock()
	defer func() {
		extrasMu.Lock()
		extrasRunning = false
		extrasMu.Unlock()
	}()

	if err := ensureDir(headroomDataDir()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to prepare data dir: "+err.Error())
		return
	}

	// Truncate the install log so the UI sees only the current action.
	logPath := headroomInstallLog()
	if err := writeEmptyFile(logPath); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open install log: "+err.Error())
		return
	}

	var args []string
	if r.Method == http.MethodPost {
		// pip install "headroom-ai[proxy,<extras>]"
		extrasList := "proxy"
		for _, e := range cleaned {
			extrasList += "," + e
		}
		spec := fmt.Sprintf("headroom-ai[%s]", extrasList)
		args = []string{"-m", "pip", "install", "--upgrade", spec}
	} else {
		// DELETE: pip uninstall -y <marker pkgs>
		markerSet := map[string]struct{}{}
		for _, e := range cleaned {
			for _, m := range extraMarkers[e] {
				markerSet[m] = struct{}{}
			}
		}
		if len(markerSet) == 0 {
			writeError(w, http.StatusBadRequest, "no marker packages to remove")
			return
		}
		pkgs := make([]string, 0, len(markerSet))
		for m := range markerSet {
			pkgs = append(pkgs, m)
		}
		args = append([]string{"-m", "pip", "uninstall", "-y"}, pkgs...)
	}

	logF, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open install log: "+err.Error())
		return
	}
	defer logF.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, args...)
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Run(); err != nil {
		code := "INSTALL_FAILED"
		if r.Method == http.MethodDelete {
			code = "UNINSTALL_FAILED"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": fmt.Sprintf("pip exited with error: %v — see headroom/install.log", err),
			"code":  code,
		})
		return
	}

	// Report the new state back to the UI.
	_, version, extras := getInstalledHeadroomExtras(py)
	if extras == nil {
		extras = map[string]bool{"code": false, "ml": false}
	}
	writeJSON(w, http.StatusOK, extrasResponse{
		Version:   nilOrString(version),
		Extras:    extras,
		Available: append([]string(nil), headroomCompressionExtras...),
	})
}

func ensureDir(p string) error { return os.MkdirAll(p, 0o755) }

func writeEmptyFile(p string) error {
	if err := ensureDir(filepath.Dir(p)); err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	return f.Close()
}

func (h *headroomHandler) start(w http.ResponseWriter, r *http.Request) {
	bin := findHeadroomBinary()
	if bin == "" {
		writeError(w, http.StatusPreconditionFailed, "Headroom CLI not installed")
		return
	}
	settingsURL := headroomURLFromSettings()
	if !isLoopbackHeadroomURL(settingsURL) {
		writeError(w, http.StatusBadRequest, "headroomUrl is not a loopback URL; cannot start a managed daemon for an external endpoint")
		return
	}
	if pid := managedPID(); pid != 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":        true,
			"running":        true,
			"alreadyRunning": true,
			"pid":            pid,
		})
		return
	}
	if err := ensureDir(headroomDataDir()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logF, err := os.OpenFile(headroomLogFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer logF.Close()
	cmd := exec.Command(bin, "proxy", "--port", fmt.Sprintf("%d", headroomDefaultPort))
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"running": false,
			"code":    "SPAWN_FAILED",
			"error":   err.Error(),
		})
		return
	}
	if err := writePID(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		writeError(w, http.StatusInternalServerError, "failed to record pid: "+err.Error())
		return
	}
	// Reap the child asynchronously so the OS doesn't keep a zombie around.
	go func() { _ = cmd.Wait() }()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"running": true,
		"pid":     cmd.Process.Pid,
	})
}

func (h *headroomHandler) stop(w http.ResponseWriter, r *http.Request) {
	pid := managedPID()
	if pid == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"running": false,
		})
		return
	}
	_ = exec.Command("kill", "-TERM", fmt.Sprintf("%d", pid)).Run()
	_ = clearPID()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"running": false,
		"pid":     pid,
	})
}

func (h *headroomHandler) restart(w http.ResponseWriter, r *http.Request) {
	bin := findHeadroomBinary()
	if bin == "" {
		writeError(w, http.StatusPreconditionFailed, "Headroom CLI not installed")
		return
	}
	settingsURL := headroomURLFromSettings()
	if !isLoopbackHeadroomURL(settingsURL) {
		writeError(w, http.StatusBadRequest, "headroomUrl is not a loopback URL; cannot restart a managed daemon for an external endpoint")
		return
	}
	if pid := managedPID(); pid != 0 {
		_ = exec.Command("kill", "-TERM", fmt.Sprintf("%d", pid)).Run()
		// Best-effort wait for graceful exit; force-kill if still alive.
		time.Sleep(2 * time.Second)
		_ = exec.Command("kill", "-KILL", fmt.Sprintf("%d", pid)).Run()
		_ = clearPID()
	}
	h.start(w, r)
}

func (h *headroomHandler) proxy(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	writeJSON(w, http.StatusOK, map[string]any{
		"success": false,
		"message": "Headroom proxy passthrough not available in Go build",
		"path":    strings.TrimPrefix(path, "/"),
	})
}

// managedPID reads headroom's pid file and verifies the process is alive.
func managedPID() int {
	data, err := os.ReadFile(headroomPIDFile())
	if err != nil {
		return 0
	}
	pid, ok := atoiSafe(strings.TrimSpace(string(data)))
	if !ok || pid == 0 {
		return 0
	}
	if !pidAlive(pid) {
		_ = os.Remove(headroomPIDFile())
		return 0
	}
	return pid
}

func writePID(pid int) error {
	if err := ensureDir(headroomDataDir()); err != nil {
		return err
	}
	return os.WriteFile(headroomPIDFile(), []byte(fmt.Sprintf("%d\n", pid)), 0o644)
}

func clearPID() error { return os.Remove(headroomPIDFile()) }

// pidAlive is a portable liveness probe: `kill -0` works on every unix and
// on modern Windows Go runtimes. We deliberately avoid pulling in
// golang.org/x/sys just for this.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
		if err != nil {
			return false
		}
		s := strings.ToLower(string(out))
		// tasklist /NH prints "<imagename>    <pid>   ..." or "INFO: No tasks..."
		// when the pid is missing. If the line does not contain "INFO", the pid
		// is alive.
		return !strings.Contains(s, "info:")
	}
	return exec.Command("kill", "-0", fmt.Sprintf("%d", pid)).Run() == nil
}

// probeLocalHeadroom hits <url>/health with a short timeout. Mirrors
// detect.js probeProxyRunning — only used when the URL is a loopback we
// manage, so SSRF surface is contained.
func probeLocalHeadroom(u string) bool {
	if u == "" {
		return false
	}
	base := strings.TrimRight(u, "/")
	cli := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := cli.Get(base + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// getInstallLogTail mirrors process.js getInstallLogTail: read the file,
// keep only non-empty lines, return the last N joined with "\n".
func getInstallLogTail(maxLines int) string {
	f, err := os.Open(headroomInstallLog())
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// Pip output can have long lines; raise the buffer cap to avoid Scan errors.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := make([]string, 0, maxLines+1)
	for scanner.Scan() {
		if t := strings.TrimSpace(scanner.Text()); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}
