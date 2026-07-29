package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
