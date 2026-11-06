package main

import (
	"errors"
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
