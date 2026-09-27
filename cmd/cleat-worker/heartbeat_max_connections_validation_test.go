package main

import "testing"

// A negative --heartbeat-max-connections is refused at startup, mirroring
// validateHeartbeat and validateReclaimTimeout: silently treating it the same
// as 0 would start a worker with the reserved pool disabled after the
// operator asked for something else. Zero itself stays valid -- it is the
// documented way to disable the pool.
func TestValidateHeartbeatMaxConnectionsRefusesNegative(t *testing.T) {
	cases := []struct {
		name     string
		maxConns int
		wantErr  bool
	}{
		{"disabled explicitly", 0, false},
		{"default of 3", 3, false},
		{"a large reservation", 100, false},
		{"negative one is refused", -1, true},
		{"deeply negative is refused", -100, true},
	}
	for _, c := range cases {
		err := validateHeartbeatMaxConnections(c.maxConns)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: validateHeartbeatMaxConnections(%d) error = %v, wantErr = %v",
				c.name, c.maxConns, err, c.wantErr)
		}
	}
}
