package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestBuildArgsMakesWebProcessFailuresObservable(t *testing.T) {
	t.Setenv("COG_COMMAND", "cog")
	t.Setenv("COG_EXTRA_ARGS", "")

	kiosk := &Kiosk{currentURL: "https://kiosk.example/app"}
	args := kiosk.buildArgs()

	if !hasArgument(args, "--webprocess-failure") {
		t.Fatalf("Cog args do not expose web process failures: %q", args)
	}
}

func TestHealthRequiresRunningReadyCog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		running      bool
		ready        bool
		crashCount   int
		renderFailed bool
		wantStatus   int
	}{
		{name: "running and ready", running: true, ready: true, wantStatus: http.StatusOK},
		{name: "not running", ready: true, wantStatus: http.StatusServiceUnavailable},
		{name: "not ready", running: true, wantStatus: http.StatusServiceUnavailable},
		{name: "running with display failure", running: true, ready: true, renderFailed: true, wantStatus: http.StatusServiceUnavailable},
		{
			name:       "crash loop",
			running:    true,
			ready:      true,
			crashCount: healthyCrashThreshold + 1,
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			kiosk := &Kiosk{ready: test.ready, crashCount: test.crashCount}
			if test.running {
				kiosk.process = &proc{exited: make(chan struct{}), renderFailed: make(chan struct{})}
				if test.renderFailed {
					close(kiosk.process.renderFailed)
				}
			}
			request := httptest.NewRequest(http.MethodGet, "/health", nil)
			response := httptest.NewRecorder()

			(&handler{kiosk: kiosk}).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("health status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestProcStopTerminatesEntireProcessGroup(t *testing.T) {
	temporaryDirectory := t.TempDir()
	childPIDPath := filepath.Join(temporaryDirectory, "child.pid")
	scriptPath := writeExecutable(t, temporaryDirectory, "process-tree.sh", `#!/bin/sh
sleep 30 &
echo "$!" > "$CHILD_PID_FILE"
wait
`)
	t.Setenv("CHILD_PID_FILE", childPIDPath)

	process, err := launch([]string{scriptPath}, filepath.Join(temporaryDirectory, "cache"))
	if err != nil {
		t.Fatalf("launch process tree: %v", err)
	}
	t.Cleanup(process.stop)

	childPID := waitForPID(t, childPIDPath)
	process.stop()

	if process.running() {
		t.Fatal("Cog leader still runs after stopping its process group")
	}
	if processExists(childPID) {
		t.Fatalf("Cog child process %d survived the process-group stop", childPID)
	}
}

func TestRestartUsesFreshCacheAndResolvesChangedUpstream(t *testing.T) {
	temporaryDirectory := t.TempDir()
	upstreamPath := filepath.Join(temporaryDirectory, "upstream")
	cacheRoot := filepath.Join(temporaryDirectory, "cache")
	scriptPath := writeExecutable(t, temporaryDirectory, "fake-cog.sh", `#!/bin/sh
mkdir -p "$XDG_CACHE_HOME"
cat "$UPSTREAM_STATE_FILE" > "$XDG_CACHE_HOME/resolved-upstream"
echo "$XDG_CACHE_HOME" > "$CACHE_DIR_STATE_FILE"
trap 'exit 0' TERM
while :; do sleep 1; done
`)
	cacheDirStatePath := filepath.Join(temporaryDirectory, "cache-dir")
	t.Setenv("COG_COMMAND", scriptPath)
	t.Setenv("UPSTREAM_STATE_FILE", upstreamPath)
	t.Setenv("CACHE_DIR_STATE_FILE", cacheDirStatePath)

	if err := os.WriteFile(upstreamPath, []byte("172.17.0.2"), 0o600); err != nil {
		t.Fatalf("write initial upstream: %v", err)
	}

	kiosk := &Kiosk{
		currentURL: "http://edge-gateway:8082/screens/test",
		cacheRoot:  cacheRoot,
		stopCh:     make(chan struct{}),
	}
	kiosk.start()
	t.Cleanup(kiosk.Stop)

	firstCacheDir := waitForFileContent(t, cacheDirStatePath)
	if got := waitForFileContent(t, filepath.Join(firstCacheDir, "resolved-upstream")); got != "172.17.0.2" {
		t.Fatalf("initial upstream = %q, want 172.17.0.2", got)
	}
	staleCachePath := filepath.Join(firstCacheDir, "stale-cache-entry")
	if err := os.WriteFile(staleCachePath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale cache marker: %v", err)
	}

	if err := os.WriteFile(upstreamPath, []byte("172.17.0.10"), 0o600); err != nil {
		t.Fatalf("write changed upstream: %v", err)
	}
	if err := os.Remove(cacheDirStatePath); err != nil {
		t.Fatalf("reset cache state marker: %v", err)
	}
	kiosk.Restart()

	secondCacheDir := waitForFileContent(t, cacheDirStatePath)
	if firstCacheDir == secondCacheDir {
		t.Fatalf("restart reused cache directory %q", firstCacheDir)
	}
	if _, err := os.Stat(staleCachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale cache survived restart: %v", err)
	}
	if got := waitForFileContent(t, filepath.Join(secondCacheDir, "resolved-upstream")); got != "172.17.0.10" {
		t.Fatalf("resolved upstream after restart = %q, want 172.17.0.10", got)
	}
	if kiosk.CurrentURL() != "http://edge-gateway:8082/screens/test" {
		t.Fatalf("restart changed stable URL to %q", kiosk.CurrentURL())
	}
}

func TestRestartReturnsAfterStopHasBegun(t *testing.T) {
	cacheDirectory := t.TempDir()
	kiosk := &Kiosk{
		cacheDir: cacheDirectory,
		stopping: true,
		stopCh:   make(chan struct{}),
	}

	kiosk.Restart()

	if kiosk.cacheDir != cacheDirectory {
		t.Fatalf("restart changed cache directory during shutdown to %q", kiosk.cacheDir)
	}
}

func TestCrashRecoveryCleansChildrenAndCacheBeforeRelaunch(t *testing.T) {
	directory := t.TempDir()
	cacheState := filepath.Join(directory, "cache-state")
	childMarker := filepath.Join(directory, "child-active")
	script := writeExecutable(t, directory, "crashing-cog.sh", `#!/bin/sh
if [ -f "$CACHE_DIR_STATE_FILE" ]; then
    if [ -f "$CHILD_MARKER" ]; then
        echo stale-child > "$RECOVERY_RESULT"
    elif [ -d "$(cat "$CACHE_DIR_STATE_FILE")" ]; then
        echo stale-cache > "$RECOVERY_RESULT"
    else
        echo recovered > "$RECOVERY_RESULT"
    fi
else
    sh -c 'trap '\''rm -f "$CHILD_MARKER"; exit 0'\'' TERM; touch "$CHILD_MARKER"; while :; do sleep 1; done' &
    while [ ! -f "$CHILD_MARKER" ]; do sleep 0.01; done
fi
echo "$XDG_CACHE_HOME" > "$CACHE_DIR_STATE_FILE"
trap 'exit 0' TERM
while :; do sleep 1; done
`)
	result := filepath.Join(directory, "result")
	t.Setenv("COG_COMMAND", script)
	t.Setenv("CACHE_DIR_STATE_FILE", cacheState)
	t.Setenv("CHILD_MARKER", childMarker)
	t.Setenv("RECOVERY_RESULT", result)
	kiosk := &Kiosk{currentURL: "about:blank", cacheRoot: filepath.Join(directory, "cache"), stopCh: make(chan struct{})}
	kiosk.start()
	kiosk.mu.Lock()
	first := kiosk.process
	kiosk.mu.Unlock()
	t.Cleanup(func() { kiosk.Stop(); first.stop() })
	firstCache := waitForFileContent(t, cacheState)
	go kiosk.Supervise()
	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if got := waitForFileContent(t, result); got != "recovered" {
		t.Fatalf("crash recovery left old display resources: %s", got)
	}
	if nextCache := waitForFileContent(t, cacheState); nextCache == firstCache {
		t.Fatal("crash recovery reused the crashed browser cache")
	}
}

func TestDisplayPermissionFailureRecoversRunningCog(t *testing.T) {
	directory := t.TempDir()
	cacheState := filepath.Join(directory, "cache-state")
	result := filepath.Join(directory, "result")
	script := writeExecutable(t, directory, "display-failure.sh", `#!/bin/sh
if [ -f "$CACHE_DIR_STATE_FILE" ]; then
    if [ -d "$(cat "$CACHE_DIR_STATE_FILE")" ]; then
        echo stale-cache > "$RECOVERY_RESULT"
    else
        echo recovered > "$RECOVERY_RESULT"
    fi
    echo "$XDG_CACHE_HOME" > "$CACHE_DIR_STATE_FILE"
else
    echo "$XDG_CACHE_HOME" > "$CACHE_DIR_STATE_FILE"
    printf 'Cog-DRM-WARNING: failed to schedule a page flip: ' >&2
    printf 'Permission denied\n' >&2
fi
trap 'exit 0' TERM
while :; do sleep 1; done
`)
	t.Setenv("COG_COMMAND", script)
	t.Setenv("CACHE_DIR_STATE_FILE", cacheState)
	t.Setenv("RECOVERY_RESULT", result)
	kiosk := &Kiosk{currentURL: "about:blank", cacheRoot: filepath.Join(directory, "cache"), stopCh: make(chan struct{})}
	kiosk.start()
	t.Cleanup(kiosk.Stop)
	kiosk.mu.Lock()
	first := kiosk.process
	kiosk.mu.Unlock()
	select {
	case <-first.renderFailed:
	case <-time.After(3 * time.Second):
		t.Fatal("live browser display failure was not detected")
	}
	if !first.running() {
		t.Fatal("fixture exited instead of reproducing a live browser with broken output")
	}
	kiosk.mu.Lock()
	kiosk.ready = true
	kiosk.mu.Unlock()
	response := httptest.NewRecorder()
	(&handler{kiosk: kiosk}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed rendering was reported healthy: %d", response.Code)
	}
	go kiosk.Supervise()
	if got := waitForFileContent(t, result); got != "recovered" {
		t.Fatalf("display failure recovery = %s", got)
	}
	if first.running() {
		t.Fatal("failed browser survived automatic display recovery")
	}
	kiosk.mu.Lock()
	crashes := kiosk.crashCount
	kiosk.mu.Unlock()
	if crashes != 1 {
		t.Fatalf("automatic recovery reset crash backoff: crash count = %d", crashes)
	}
}

func TestRenderFailureWriterHandlesSplitDiagnostics(t *testing.T) {
	t.Parallel()
	failed := make(chan struct{})
	writer := &renderFailureWriter{failed: failed}
	for _, part := range []string{"unrelated warning\n", "failed to schedule a page ", "flip: Permission ", "denied\n"} {
		if n, err := writer.Write([]byte(part)); n != len(part) || err != nil {
			t.Fatalf("diagnostic write = %d, %v", n, err)
		}
	}
	select {
	case <-failed:
	default:
		t.Fatal("split DRM failure was missed")
	}
	// Repeated diagnostics must not close the channel twice.
	_, _ = writer.Write([]byte(pageFlipPermissionFailure))
}

func TestRenderFailureWriterIgnoresOtherWarnings(t *testing.T) {
	t.Parallel()
	failed := make(chan struct{})
	writer := &renderFailureWriter{failed: failed}
	_, _ = writer.Write([]byte("Renderer modeset does not support rotation 0\nfailed to schedule a page flip: Device or resource busy\n"))
	select {
	case <-failed:
		t.Fatal("unrelated warning triggered display recovery")
	default:
	}
}

func TestConcurrentRestartsKeepActiveCache(t *testing.T) {
	directory := t.TempDir()
	script := writeExecutable(t, directory, "concurrent-cog.sh", `#!/bin/sh
trap 'exit 0' TERM
while :; do sleep 1; done
`)
	t.Setenv("COG_COMMAND", script)
	kiosk := &Kiosk{currentURL: "about:blank", cacheRoot: filepath.Join(directory, "cache"), stopCh: make(chan struct{})}
	kiosk.start()
	t.Cleanup(kiosk.Stop)
	var callers sync.WaitGroup
	for range 3 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			kiosk.Restart()
		}()
	}
	callers.Wait()
	kiosk.mu.Lock()
	cacheDir, process := kiosk.cacheDir, kiosk.process
	kiosk.mu.Unlock()
	if !process.running() {
		t.Fatal("concurrent restart left the browser stopped")
	}
	if _, err := os.Stat(cacheDir); err != nil {
		t.Fatalf("concurrent restart removed the active browser cache: %v", err)
	}
	entries, err := os.ReadDir(kiosk.cacheRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("concurrent restart left stale cache generations: %v, %v", entries, err)
	}
}

func TestRecoveryAvailabilityRestartsOnlyAfterRecovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		observations []bool
		wantRestarts []bool
	}{
		{
			name:         "healthy baseline",
			observations: []bool{true, true, true},
			wantRestarts: []bool{false, false, false},
		},
		{
			name:         "single restart after outage",
			observations: []bool{true, false, false, true, true},
			wantRestarts: []bool{false, false, false, true, false},
		},
		{
			name:         "initial outage recovers",
			observations: []bool{false, false, true, true},
			wantRestarts: []bool{false, false, true, false},
		},
		{
			name:         "each distinct outage recovers once",
			observations: []bool{true, false, true, false, true},
			wantRestarts: []bool{false, false, true, false, true},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var availability recoveryAvailability
			for index, observation := range test.observations {
				if got := availability.observe(observation); got != test.wantRestarts[index] {
					t.Fatalf(
						"observation %d restart = %t, want %t",
						index,
						got,
						test.wantRestarts[index],
					)
				}
			}
		})
	}
}

func TestRecoveryURLReachableRequiresSuccessfulHTTPResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") != "no-cache, no-store" {
			t.Errorf("Cache-Control = %q", r.Header.Get("Cache-Control"))
		}
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	if !recoveryURLReachable(server.URL + "/ready") {
		t.Fatal("successful recovery response reported unreachable")
	}
	if recoveryURLReachable(server.URL + "/unavailable") {
		t.Fatal("failing recovery response reported reachable")
	}
	unreachableURL := server.URL + "/ready"
	server.Close()
	if recoveryURLReachable(unreachableURL) {
		t.Fatal("connection failure reported reachable")
	}
}

func writeExecutable(t *testing.T, directory, name, contents string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	return path
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	value := waitForFileContent(t, path)
	pid, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("parse child pid %q: %v", value, err)
	}
	return pid
}

func waitForFileContent(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if value, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(value)) != "" {
			return strings.TrimSpace(string(value))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func TestNavigateWithRetry(t *testing.T) {
	origNavigate, origRetryIn := navigate, navigateRetryIn
	t.Cleanup(func() { navigate, navigateRetryIn = origNavigate, origRetryIn })
	navigateRetryIn = time.Millisecond

	tests := []struct {
		name      string
		failures  int
		wantOK    bool
		wantCalls int
	}{
		{"first attempt succeeds", 0, true, 1},
		{"retry succeeds, no restart", 1, true, 2},
		{"both fail, restart needed", 2, false, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			navigate = func(string) error {
				calls++
				if calls <= tt.failures {
					return errors.New("timeout")
				}
				return nil
			}
			if got := navigateWithRetry("http://x", "navigate"); got != tt.wantOK {
				t.Fatalf("ok = %v, want %v", got, tt.wantOK)
			}
			if calls != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}
