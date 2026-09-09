package main

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestResolveDeployTenant is cleat#1038.
//
// `cleat deploy` wrote workflow_defs with an INSERT that did not list
// tenant_id, so the column took its schema DEFAULT and every deploy landed on
// the default tenant whatever the configuration said. The conflict target is
// (tenant_id, name, version), so that was not misattribution but OVERWRITE: a
// second tenant deploying the same name and version replaced the first one's
// binary.
//
// The fix is one column and one argument; what is worth a test is the ORDER,
// because it decides which tenant owns the row and it is the half a reader
// cannot check by looking at the SQL.
func TestResolveDeployTenant(t *testing.T) {
	const flagTenant = "11111111-1111-1111-1111-111111111111"
	const envTenant = "22222222-2222-2222-2222-222222222222"

	for _, tc := range []struct {
		name, flag, env, want string
	}{
		{"flag wins over env", flagTenant, envTenant, flagTenant},
		{"env when no flag", "", envTenant, envTenant},
		{"default when neither", "", "", engine.DefaultTenantUUID},
		{"flag alone", flagTenant, "", flagTenant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDeployTenant(tc.flag, tc.env); got != tc.want {
				t.Errorf("resolveDeployTenant(%q, %q) = %q, want %q",
					tc.flag, tc.env, got, tc.want)
			}
		})
	}

	// The control, and the case the defect actually was: with nothing
	// configured the answer must be the default tenant AND NOT THE EMPTY
	// STRING. An empty tenant would be passed to the INSERT as '', which
	// Postgres rejects for a UUID column -- so a regression here would surface
	// as a deploy failure rather than as a silent wrong-tenant write, and that
	// is worth pinning separately from the ordering above.
	if got := resolveDeployTenant("", ""); got == "" {
		t.Error("resolved the empty string with nothing configured; the INSERT " +
			"would send '' for a UUID column")
	}
}
