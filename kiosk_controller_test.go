package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		name       string
		running    bool
		ready      bool
		crashCount int
		wantStatus int
	}{
		{name: "running and ready", running: true, ready: true, wantStatus: http.StatusOK},
		{name: "not running", ready: true, wantStatus: http.StatusServiceUnavailable},
		{name: "not ready", running: true, wantStatus: http.StatusServiceUnavailable},
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
				kiosk.process = &proc{exited: make(chan struct{})}
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
