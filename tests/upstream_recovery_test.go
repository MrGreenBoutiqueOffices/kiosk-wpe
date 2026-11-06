package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
