package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

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
