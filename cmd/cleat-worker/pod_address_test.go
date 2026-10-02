package main

import "testing"

// podAddress takes hostname as a parameter rather than reading os.Hostname()
// itself, specifically so the string-building logic can be tested directly
// rather than only indirectly through a manifest's flag text or a fixture
// that sets WorkerRegistration.Address by hand (cleat#2196 review).
func TestPodAddress(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hostname    string
		serviceName string
		want        string
	}{
		{"both set", "worker-abc123", "cleat-worker-headless", "worker-abc123.cleat-worker-headless"},
		{"no service name -- not under the flag", "worker-abc123", "", ""},
		{"no hostname -- os.Hostname() failed", "", "cleat-worker-headless", ""},
		{"neither set", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podAddress(tc.hostname, tc.serviceName); got != tc.want {
				t.Errorf("podAddress(%q, %q) = %q, want %q", tc.hostname, tc.serviceName, got, tc.want)
			}
		})
	}
}
