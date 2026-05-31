// kiosk_controller — WPE/Cog browser supervisor and HTTP control API.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultURL   = "about:blank"
	stopTimeout  = 5 * time.Second
	pollInterval = 5 * time.Second
	backoffMaxS  = 30.0
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const stateFile = "/tmp/kiosk-url" //nolint:gosec

// proc wraps a running exec.Cmd; exited is closed when the process terminates.
type proc struct {
	cmd      *exec.Cmd
	exitCode int
	exited   chan struct{}
}

func launch(args []string) (*proc, error) {
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, exited: make(chan struct{})}
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
	if !p.running() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(stopTimeout):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}

// Kiosk manages the Cog subprocess and the active URL.
type Kiosk struct {
	mu         sync.Mutex
	process    *proc
	currentURL string
	stopping   bool
	crashCount int
}

func newKiosk() *Kiosk {
	k := &Kiosk{}
	k.currentURL = k.loadURL()
	return k
}

func (k *Kiosk) loadURL() string {
	if data, err := os.ReadFile(stateFile); err == nil {
		if url := strings.TrimSpace(string(data)); url != "" {
			return url
		}
	}
	return envOr("LAUNCH_URL", defaultURL)
}

func (k *Kiosk) saveURL() {
	_ = os.WriteFile(stateFile, []byte(k.currentURL), 0o600)
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
	return append(args, k.currentURL)
}

func (k *Kiosk) start() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopping = false
	if k.process != nil && k.process.running() {
		return
	}
	args := k.buildArgs()
	log.Printf("Starting Cog: %s", strings.Join(args, " "))
	p, err := launch(args)
	if err != nil {
		log.Printf("Failed to start Cog: %v", err)
		return
	}
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
func (k *Kiosk) Restart() {
	k.mu.Lock()
	k.crashCount = 0
	k.mu.Unlock()
	k.stop()
	k.start()
}

// SetURL navigates to a new URL, persists it, and restarts Cog.
func (k *Kiosk) SetURL(url string) {
	k.mu.Lock()
	k.currentURL = url
	k.saveURL()
	k.mu.Unlock()
	k.Restart()
}

// IsRunning reports whether Cog is currently running.
func (k *Kiosk) IsRunning() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.process != nil && k.process.running()
}

// CrashCount returns the number of unexpected exits since the last intentional restart.
func (k *Kiosk) CrashCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.crashCount
}

// CurrentURL returns the active URL.
func (k *Kiosk) CurrentURL() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.currentURL
}

// Supervise watches Cog in a loop; restarts with exponential backoff on crashes.
func (k *Kiosk) Supervise() {
	for {
		time.Sleep(pollInterval)
		k.mu.Lock()
		if k.stopping {
			k.mu.Unlock()
			return
		}
		running := k.process != nil && k.process.running()
		if running {
			k.crashCount = 0
			k.mu.Unlock()
			continue
		}
		k.crashCount++
		count := k.crashCount
		k.mu.Unlock()

		backoff := time.Duration(math.Min(math.Pow(2, float64(count-1)), backoffMaxS)) * time.Second
		log.Printf("Cog not running (crash #%d); restarting in %v", count, backoff)
		time.Sleep(backoff)
		k.start()
	}
}

// Stop shuts down Cog cleanly.
func (k *Kiosk) Stop() {
	k.mu.Lock()
	k.stopping = true
	k.mu.Unlock()
	k.stop()
}

// --- Screenshot ---

// captureScreenshot reads the Linux framebuffer (/dev/fb0) and returns a PNG.
// Requires the DRM driver to expose a legacy framebuffer. Returns an error when
// /dev/fb0 is not available so callers can return 503 instead of panicking.
func captureScreenshot() ([]byte, error) {
	sizeRaw, err := os.ReadFile("/sys/class/graphics/fb0/virtual_size")
	if err != nil {
		return nil, fmt.Errorf("framebuffer not available: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(sizeRaw)), ",", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("unexpected virtual_size format: %q", strings.TrimSpace(string(sizeRaw)))
	}
	width, err1 := strconv.Atoi(parts[0])
	height, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || width <= 0 || height <= 0 {
		return nil, fmt.Errorf("cannot parse framebuffer dimensions from %q", strings.TrimSpace(string(sizeRaw)))
	}

	f, err := os.Open("/dev/fb0") //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("cannot open framebuffer: %w", err)
	}
	defer f.Close()

	stride := width * 4 // 32 bpp
	raw := make([]byte, height*stride)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, fmt.Errorf("cannot read framebuffer (%dx%d): %w", width, height, err)
	}

	// Linux framebuffer is typically BGRA — swap R and B channels for image/png.
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 4
			img.SetRGBA(x, y, color.RGBA{R: raw[i+2], G: raw[i+1], B: raw[i+0], A: raw[i+3]})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("png encode failed: %w", err)
	}
	return buf.Bytes(), nil
}

// --- HTTP handler ---

type handler struct{ kiosk *Kiosk }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/url":
		h.handleURL(w, r)
	case "/refresh":
		h.handleRefresh(w, r)
	case "/status":
		h.handleStatus(w, r)
	case "/health":
		h.handleHealth(w, r)
	case "/screenshot":
		h.handleScreenshot(w, r)
	default:
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func (h *handler) handleURL(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(h.kiosk.CurrentURL()))
	case http.MethodPost:
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
		h.kiosk.SetURL(url)
		sendJSON(w, http.StatusOK, map[string]string{"url": h.kiosk.CurrentURL()})
	default:
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
	}
}

func (h *handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	h.kiosk.Restart()
	sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"url":         h.kiosk.CurrentURL(),
		"running":     h.kiosk.IsRunning(),
		"crash_count": h.kiosk.CrashCount(),
	})
}

func (h *handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *handler) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	data, err := captureScreenshot()
	if err != nil {
		log.Printf("Screenshot failed: %v", err)
		sendJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(data)
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

	port := envOr("KIOSK_API_PORT", "5011")
	addr := "0.0.0.0:" + port
	log.Printf("Kiosk API listening on %s", addr)

	srv := &http.Server{
		Addr:    addr,
		Handler: &handler{kiosk: k},
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("Received %v, shutting down", sig)
		k.Stop()
		_ = srv.Close()
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
