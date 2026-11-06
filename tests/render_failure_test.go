package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

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

func TestRenderFailureWriterDetectsFatalStartupFailures(t *testing.T) {
	for _, diagnostic := range []string{
		"Platform setup failed: Failed to initialize DRM",
		"Could not instantiate any WPE backend.",
		"creating thread 'gmain': Error creating thread: Resource temporarily unavailable",
		"Unable to spawn a new child process: Failed to spawn child process WPENetworkProcess",
	} {
		t.Run(diagnostic, func(t *testing.T) {
			failed := make(chan struct{})
			writer := &renderFailureWriter{failed: failed}
			// Real stderr reads can split a diagnostic at any byte boundary.
			for _, character := range []byte(diagnostic) {
				_, _ = writer.Write([]byte{character})
			}
			select {
			case <-failed:
			default:
				t.Fatal("fatal browser startup failure was reported as usable output")
			}
		})
	}
}
