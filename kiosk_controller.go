// kiosk_controller — WPE/Cog browser supervisor and HTTP control API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultURL   = "about:blank"
	defaultCache = "/tmp/kiosk-wpe-cache"

	// stopTimeout is how long we wait for Cog to exit after SIGTERM before SIGKILL.
	// 10 s gives WPE subprocesses time to wind down GL contexts and release DRM.
	stopTimeout = 10 * time.Second

	// pollInterval is the fallback Supervise poll cadence; crashes are also detected
	// immediately via the process exit channel.
	pollInterval = 5 * time.Second

	backoffMaxS = 30.0

	// drmSettleDelay is the pause between stopping old Cog and starting new Cog,
	// giving the kernel time to release the DRM master lock after process group exit.
	drmSettleDelay = 500 * time.Millisecond

	// crashResetStableFor: crash counter resets only after Cog has been up this long.
	crashResetStableFor = 30 * time.Second

	// healthyCrashThreshold: /health returns 503 when crash_count exceeds this.
	healthyCrashThreshold = 5

	maxBodyBytes = 4096

	recoveryPollInterval = 5 * time.Second
	recoveryProbeTimeout = 2 * time.Second
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envWith(key, value string) []string {
	prefix := key + "="
	environment := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, prefix) {
			environment = append(environment, item)
		}
	}
	return append(environment, prefix+value)
}

// proc wraps a running exec.Cmd; exited is closed when the process terminates.
type proc struct {
	cmd      *exec.Cmd
	exitCode int
	exited   chan struct{}
	pgid     int
}

func launch(args []string, cacheDir string) (*proc, error) {
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = envWith("XDG_CACHE_HOME", cacheDir)
	// Own process group so SIGTERM reaches all WPE child processes (WPEWebProcess,
	// WPENetworkProcess) and they release DRM/GL resources before we start a new Cog.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proc{
		cmd:    cmd,
		exited: make(chan struct{}),
		pgid:   cmd.Process.Pid,
	}
	go func() {
		_ = cmd.Wait()
		if cmd.ProcessState != nil {
			p.exitCode = cmd.ProcessState.ExitCode()
			log.Printf("Cog exited (code %d)", p.exitCode)
		}
		close(p.exited)
	}()
	return p, nil
}

func (p *proc) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

func (p *proc) stop() {
	if p.pgid <= 0 || !processGroupRunning(p.pgid) {
		return
	}

	if err := syscall.Kill(-p.pgid, syscall.SIGTERM); err != nil &&
		!errors.Is(err, syscall.ESRCH) {
		log.Printf("Failed to terminate Cog process group %d: %v", p.pgid, err)
	}

	deadline := time.NewTimer(stopTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for processGroupRunning(p.pgid) {
		select {
		case <-deadline.C:
			log.Printf("Cog process group %d did not stop; killing it", p.pgid)
			_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
			if p.running() {
				<-p.exited
			}
			return
		case <-ticker.C:
		}
	}

	if p.running() {
		<-p.exited
	}
}

func processGroupRunning(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

type recoveryAvailability struct {
	initialized bool
	reachable   bool
}

// observe reports whether a configured recovery target has returned after an
// outage. The first observation establishes the baseline. A healthy initial
// observation never restarts Cog; recovery after an initial outage does.
func (state *recoveryAvailability) observe(reachable bool) bool {
	if !state.initialized {
		state.initialized = true
		state.reachable = reachable
		return false
	}

	recovered := !state.reachable && reachable
	state.reachable = reachable
	return recovered
}

func recoveryURLReachable(recoveryURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), recoveryProbeTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, recoveryURL, nil)
	if err != nil {
		return false
	}
	request.Header.Set("Cache-Control", "no-cache, no-store")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusBadRequest
}

// Kiosk manages the Cog subprocess and the active URL.
type Kiosk struct {
	mu           sync.Mutex
	process      *proc
	currentURL   string
	cacheRoot    string
	cacheDir     string
	stopping     bool
	restarting   int // counts in-flight intentional restarts; only 0 when all callers finished
	crashCount   int
	startedAt    time.Time
	cogStartedAt time.Time // zero value = Cog not yet started
	lastCrashAt  time.Time // zero value = no crash yet
	ready        bool
	cogVersion   string
	stopCh       chan struct{}
	stopOnce     sync.Once
}

// reapplyTouchCalibration re-triggers udev so libinput picks up the hwdb
// calibration matrix on any input device that was opened since the last trigger.
// A no-op when TOUCH_DEVICE is not set.
func reapplyTouchCalibration() {
	if os.Getenv("TOUCH_DEVICE") == "" {
		return
	}
	_ = exec.Command("udevadm", "trigger", "--action=change", "--type=devices", "--subsystem-match=input").Run() //nolint:gosec
	_ = exec.Command("udevadm", "settle", "--timeout=3").Run()                                                   //nolint:gosec
}

// cogNavigate asks the running Cog instance to navigate to url via D-Bus,
// using GApplication's standard Open method (org.gtk.Application.Open).
// Requires DBUS_SESSION_BUS_ADDRESS to be set (done by start.sh).
func cogNavigate(url string) error {
	// GVariant text format: array-of-strings, hint string, empty platform-data dict.
	// Escape backslashes before single quotes so neither can break the string literal.
	escaped := strings.ReplaceAll(url, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, "'", `\'`)
	uris := fmt.Sprintf("['%s']", escaped)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gdbus", "call", //nolint:gosec
		"--session",
		"--dest=com.igalia.Cog",
		"--object-path=/com/igalia/Cog",
		"--method=org.gtk.Application.Open",
		uris, "", "{}",
	)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) > 0 {
		log.Printf("gdbus: %s", strings.TrimSpace(string(out)))
	}
	return err
}

func getCogVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "cog", "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func newKiosk() *Kiosk {
	k := &Kiosk{
		startedAt:  time.Now(),
		cogVersion: getCogVersion(),
		stopCh:     make(chan struct{}),
		cacheRoot:  envOr("KIOSK_CACHE_ROOT", defaultCache),
	}
	k.currentURL = k.loadURL()
	return k
}

func (k *Kiosk) loadURL() string {
	return envOr("LAUNCH_URL", defaultURL)
}

func (k *Kiosk) buildArgs() []string {
	args := strings.Fields(envOr("COG_COMMAND", "cog"))
	if extra := os.Getenv("COG_EXTRA_ARGS"); extra != "" {
		args = append(args, strings.Fields(extra)...)
	}
	switch strings.ToLower(os.Getenv("IGNORE_TLS_ERRORS")) {
	case "1", "true", "yes":
		args = append(args, "--ignore-tls-errors")
	}
	args = append(args, "--platform", "drm")
	if p := os.Getenv("COG_PLATFORM_PARAMS"); p != "" {
		args = append(args, "--platform-params", p)
	}
	if !hasArgument(args, "--webprocess-failure") {
		args = append(args, "--webprocess-failure=exit")
	}
	return append(args, k.currentURL)
}

func hasArgument(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func (k *Kiosk) start() {
	k.mu.Lock()
	if k.stopping || (k.process != nil && k.process.running()) {
		k.mu.Unlock()
		return
	}
	k.mu.Unlock()

	// Run calibration outside the lock: udevadm settle can block for seconds.
	reapplyTouchCalibration()

	k.mu.Lock()
	defer k.mu.Unlock()
	// Re-check after calibration in case Stop() or a concurrent start() raced.
	if k.stopping || (k.process != nil && k.process.running()) {
		return
	}
	if k.cacheDir == "" {
		if err := os.MkdirAll(k.cacheRoot, 0o700); err != nil {
			log.Printf("Failed to prepare Cog cache root: %v", err)
			return
		}
		cacheDir, err := os.MkdirTemp(k.cacheRoot, "generation-")
		if err != nil {
			log.Printf("Failed to prepare Cog cache directory: %v", err)
			return
		}
		k.cacheDir = filepath.Clean(cacheDir)
	}
	args := k.buildArgs()
	log.Printf("Starting Cog at %s", k.currentURL)
	p, err := launch(args, k.cacheDir)
	if err != nil {
		log.Printf("Failed to start Cog: %v", err)
		return
	}
	k.cogStartedAt = time.Now()
	k.ready = false
	k.process = p
}

func (k *Kiosk) stop() {
	k.mu.Lock()
	p := k.process
	k.process = nil
	k.mu.Unlock()
	if p != nil {
		p.stop()
	}
}

// Restart intentionally restarts Cog, resetting the crash counter.
// It is safe to call concurrently; last caller wins.
func (k *Kiosk) Restart() {
	k.mu.Lock()
	k.crashCount = 0
	k.restarting++
	k.mu.Unlock()

	k.stop()
	// Allow the kernel to fully release the DRM master lock before the next
	// Cog process tries to claim it; without this the gles renderer gets EPERM.
	time.Sleep(drmSettleDelay)

	k.mu.Lock()
	staleCacheDir := k.cacheDir
	k.cacheDir = ""
	k.mu.Unlock()
	if staleCacheDir != "" {
		if err := os.RemoveAll(staleCacheDir); err != nil {
			log.Printf("Failed to clear stale Cog cache %s: %v", staleCacheDir, err)
		}
	}

	k.start()

	k.mu.Lock()
	k.restarting--
	k.mu.Unlock()
}

// SetURL updates the URL for the current container runtime and navigates Cog
// to it via D-Bus. After a container restart, LAUNCH_URL is authoritative.
// Falls back to a full restart when D-Bus is unavailable.
func (k *Kiosk) SetURL(url string) {
	k.mu.Lock()
	k.currentURL = url
	k.mu.Unlock()

	if err := cogNavigate(url); err != nil {
		log.Printf("D-Bus navigate failed (%v); falling back to restart", err)
		k.Restart()
		return
	}
	// WPEWebProcess opens the input device shortly after navigation starts;
	// re-trigger udev so libinput picks up the hwdb calibration matrix.
	go func() {
		time.Sleep(500 * time.Millisecond)
		reapplyTouchCalibration()
	}()
}

// Reload re-navigates Cog to the current URL via D-Bus without restarting the process.
// Falls back to a full restart when D-Bus is unavailable.
func (k *Kiosk) Reload() {
	k.mu.Lock()
	url := k.currentURL
	k.mu.Unlock()

	if err := cogNavigate(url); err != nil {
		log.Printf("D-Bus reload failed (%v); falling back to restart", err)
		k.Restart()
		return
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		reapplyTouchCalibration()
	}()
}

// CurrentURL returns the active URL.
func (k *Kiosk) CurrentURL() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.currentURL
}

// Supervise watches Cog in a loop; it reacts immediately when the process exits
// and restarts with exponential backoff on unexpected crashes.
func (k *Kiosk) Supervise() {
	for {
		// Capture the current process so we can select on its exit channel.
		k.mu.Lock()
		p := k.process
		k.mu.Unlock()

		if p != nil {
			select {
			case <-p.exited: // react immediately on crash
			case <-time.After(pollInterval):
			case <-k.stopCh:
				return
			}
		} else {
			select {
			case <-time.After(pollInterval):
			case <-k.stopCh:
				return
			}
		}

		k.mu.Lock()
		if k.stopping {
			k.mu.Unlock()
			return
		}

		running := k.process != nil && k.process.running()
		if running {
			if !k.ready {
				k.ready = true
			}
			// Reset crash counter only after sustained stability to properly
			// apply exponential backoff against rapid crash loops.
			if !k.cogStartedAt.IsZero() && time.Since(k.cogStartedAt) > crashResetStableFor {
				k.crashCount = 0
			}
			k.mu.Unlock()
			continue
		}

		// Process stopped — not a crash if an intentional restart is in progress.
		if k.restarting > 0 {
			k.mu.Unlock()
			continue
		}

		k.lastCrashAt = time.Now()
		k.crashCount++
		count := k.crashCount
		k.mu.Unlock()

		backoff := time.Duration(math.Min(math.Pow(2, float64(count-1)), backoffMaxS)) * time.Second
		if count > healthyCrashThreshold {
			log.Printf("Cog crash loop detected (%d crashes); restarting in %v — check container logs for root cause", count, backoff)
		} else {
			log.Printf("Cog not running (crash #%d); restarting in %v", count, backoff)
		}

		// Cancellable backoff — exits immediately on Stop().
		select {
		case <-time.After(backoff):
		case <-k.stopCh:
			return
		}

		k.start()
	}
}

// Stop shuts down Cog cleanly and exits the Supervise loop.
func (k *Kiosk) Stop() {
	k.mu.Lock()
	k.stopping = true
	k.mu.Unlock()
	k.stopOnce.Do(func() { close(k.stopCh) })
	k.stop()
}

// MonitorRecovery hard-restarts Cog once when an optional HTTP recovery target
// becomes reachable again. This recovers native browser error pages that cannot
// reload themselves after their upstream was temporarily unavailable.
func (k *Kiosk) MonitorRecovery(recoveryURL string) {
	var availability recoveryAvailability
	ticker := time.NewTicker(recoveryPollInterval)
	defer ticker.Stop()

	for {
		reachable := recoveryURLReachable(recoveryURL)
		if availability.observe(reachable) {
			log.Printf("Kiosk recovery target is reachable again; restarting Cog")
			k.Restart()
		}

		select {
		case <-ticker.C:
		case <-k.stopCh:
			return
		}
	}
}

// --- HTTP handler ---

type handler struct{ kiosk *Kiosk }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/url":
		h.handleURL(w, r)
	case "/refresh":
		h.handleRefresh(w, r)
	case "/restart":
		h.handleRestart(w, r)
	case "/status":
		h.handleStatus(w, r)
	case "/health":
		h.handleHealth(w, r)
	default:
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

// validURL only allows safe URL schemes to prevent file:// or javascript: injection.
func validURL(u string) bool {
	s := strings.ToLower(u)
	return strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "about:")
}

func (h *handler) handleURL(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(h.kiosk.CurrentURL()))
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
			return
		}
		url := strings.TrimSpace(body.URL)
		if url == "" {
			sendJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_url"})
			return
		}
		if !validURL(url) {
			sendJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_url_scheme"})
			return
		}
		// Run asynchronously: D-Bus call or fallback restart both block briefly.
		go h.kiosk.SetURL(url)
		sendJSON(w, http.StatusOK, map[string]string{"url": url})
	default:
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
	}
}

// handleRefresh re-navigates Cog to the current URL without restarting the process.
func (h *handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	go h.kiosk.Reload()
	sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRestart fully stops and restarts Cog, re-applying touch calibration.
func (h *handler) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	go h.kiosk.Restart()
	sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	h.kiosk.mu.Lock()
	now := time.Now()
	var lastCrash *string
	if !h.kiosk.lastCrashAt.IsZero() {
		s := h.kiosk.lastCrashAt.UTC().Format(time.RFC3339)
		lastCrash = &s
	}
	var cogStarted *string
	if !h.kiosk.cogStartedAt.IsZero() {
		s := h.kiosk.cogStartedAt.UTC().Format(time.RFC3339)
		cogStarted = &s
	}
	status := map[string]any{
		"url":            h.kiosk.currentURL,
		"running":        h.kiosk.process != nil && h.kiosk.process.running(),
		"crash_count":    h.kiosk.crashCount,
		"ready":          h.kiosk.ready,
		"started_at":     h.kiosk.startedAt.UTC().Format(time.RFC3339),
		"uptime_seconds": int(now.Sub(h.kiosk.startedAt).Seconds()),
		"cog_started_at": cogStarted,
		"last_crash_at":  lastCrash,
		"cog_version":    h.kiosk.cogVersion,
	}
	h.kiosk.mu.Unlock()
	sendJSON(w, http.StatusOK, status)
}

func (h *handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	h.kiosk.mu.Lock()
	crashCount := h.kiosk.crashCount
	ready := h.kiosk.ready
	running := h.kiosk.process != nil && h.kiosk.process.running()
	h.kiosk.mu.Unlock()

	if !running || !ready || crashCount > healthyCrashThreshold {
		sendJSON(w, http.StatusServiceUnavailable, map[string]bool{"ok": false})
		return
	}
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func sendJSON(w http.ResponseWriter, status int, v any) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func main() {
	k := newKiosk()
	k.start()
	go k.Supervise()
	if recoveryURL := strings.TrimSpace(os.Getenv("KIOSK_RECOVERY_URL")); recoveryURL != "" {
		if !strings.HasPrefix(recoveryURL, "http://") && !strings.HasPrefix(recoveryURL, "https://") {
			log.Fatal("KIOSK_RECOVERY_URL must use http:// or https://")
		}
		go k.MonitorRecovery(recoveryURL)
	}

	port := envOr("KIOSK_API_PORT", "5011")
	addr := "0.0.0.0:" + port
	log.Printf("Kiosk API listening on %s", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           &handler{kiosk: k},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("Received %v, shutting down", sig)
		k.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server error: %v", err)
	}
}
