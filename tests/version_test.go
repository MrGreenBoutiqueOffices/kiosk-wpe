package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatusRecoversCogVersionAfterStartupFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("KIOSK_CACHE_ROOT", dir)
	marker := filepath.Join(dir, "ready")
	t.Setenv("VERSION_READY", marker)
	writeExecutable(t, dir, "cog", "#!/bin/sh\n[ -f \"$VERSION_READY\" ] || exit 1\nprintf '0.18.4\\n'\n")

	k := newKiosk()
	defer k.Stop()
	h := &handler{kiosk: k}
	version := func() string {
		w := httptest.NewRecorder()
		h.handleStatus(w, httptest.NewRequest("GET", "/status", nil))
		var status struct {
			CogVersion string `json:"cog_version"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		return status.CogVersion
	}
	if got := version(); got != "unknown" {
		t.Fatalf("initial Cog version = %q", got)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	go k.retryCogVersion(10 * time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if version() == "0.18.4" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Cog version after recovery = %q", version())
}
