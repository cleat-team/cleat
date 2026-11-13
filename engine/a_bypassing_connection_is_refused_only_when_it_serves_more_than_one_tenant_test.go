package engine

import (
	"strings"
	"testing"
)

// cleat#3772, owner decision D82. DecideRLSMultiTenant draws the line the worker's startup, its
// --create-tenant and POST /api/admin/tenants all use: a connection that is not subject to row-level
// security and serves, or would serve, MORE THAN ONE tenant is refused. It is a pure function so the
// line can be tested without a database; the worker tests run the real binary against a real one.
func TestABypassingConnectionIsRefusedOnlyWhenItServesMoreThanOneTenant(t *testing.T) {
	reasons := []RLSBypassReason{
		{Kind: "superuser", Detail: `the connecting role "owner" is a superuser, and PostgreSQL never applies row-level security to a superuser`},
		{Kind: "no_policies", Detail: "no table in schema public has any row-level security policy"},
	}
	for _, c := range []struct {
		name     string
		reasons  []RLSBypassReason
		tenants  int
		creating bool
		refused  bool
	}{
		{"a connection subject to the policies, many tenants", nil, 50, false, false},
		{"a bypassing connection, no tenants", reasons, 0, false, false},
		{"a bypassing connection, one tenant (the tutorial's laptop)", reasons, 1, false, false},
		{"a bypassing connection, two tenants", reasons, 2, false, true},
		{"a bypassing connection, many tenants", reasons, 50, false, true},
		{"creating the second tenant on a bypassing connection", reasons, 2, true, true},
		{"creating a tenant on a connection subject to the policies", nil, 2, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := DecideRLSMultiTenant("owner", c.reasons, c.tenants, c.creating)
			if (got != nil) != c.refused {
				t.Fatalf("DecideRLSMultiTenant(%d tenants, %d reasons, creating=%v) refused=%v, want %v",
					c.tenants, len(c.reasons), c.creating, got != nil, c.refused)
			}
		})
	}

	t.Run("the message names the role, the tenant count, every reason, the two flags that do not lift it, and the fix", func(t *testing.T) {
		msg := DecideRLSMultiTenant("owner", reasons, 3, false).Message()
		for _, want := range []string{
			"refusing to start", `"owner"`, "the database holds 3 tenants",
			reasons[0].Detail, reasons[1].Detail,
			"--require-auth=false", "--rls-check=off", "do not lift",
			"cleat_app", "ALTER ROLE cleat_app LOGIN PASSWORD", "--migrate-db",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("the start refusal is missing %q:\n%s", want, msg)
			}
		}
		create := DecideRLSMultiTenant("owner", reasons, 2, true).Message()
		for _, want := range []string{"refusing to create a tenant", "would make 2 tenants", "cleat_app"} {
			if !strings.Contains(create, want) {
				t.Errorf("the creation refusal is missing %q:\n%s", want, create)
			}
		}
		if strings.Contains(create, "refusing to start") || strings.Contains(msg, "refusing to create") {
			t.Error("the two refusals must say which thing was refused")
		}
	})

	// The warning --create-tenant prints after creating one. It is rendered from the same decision as the
	// refusals, so the line it draws is the same line: with one tenant (or a connection that is subject to
	// the policies) there is nothing to render.
	t.Run("the creation warning names the role, the count, the restart consequence and the fix, and is absent where the decision is nil", func(t *testing.T) {
		warn := DecideRLSMultiTenant("owner", reasons, 2, false).CreatedWarning()
		for _, want := range []string{
			"WARNING", `"owner"`, "now holds 2 tenants", "until it restarts", "its next start will refuse",
			reasons[0].Detail, reasons[1].Detail, rlsBypassFix, "docs/explanation/postgresql-schema.md",
		} {
			if !strings.Contains(warn, want) {
				t.Errorf("the creation warning is missing %q:\n%s", want, warn)
			}
		}
		if strings.Contains(warn, "refusing") {
			t.Error("a warning must not read as a refusal: the command it comes from succeeded")
		}
		if d := DecideRLSMultiTenant("owner", reasons, 1, false); d != nil {
			t.Errorf("one tenant produced a decision to warn about: %+v", d)
		}
		if d := DecideRLSMultiTenant("cleat_app", nil, 5, false); d != nil {
			t.Errorf("a connection subject to the policies produced a decision to warn about: %+v", d)
		}
	})

	t.Run("both refusals end with the same fix as the single-tenant one", func(t *testing.T) {
		if !strings.Contains(FormatRLSBypass(reasons), rlsBypassFix) ||
			!strings.Contains(DecideRLSMultiTenant("owner", reasons, 2, false).Message(), rlsBypassFix) {
			t.Error("the fix paragraph has drifted between the two messages")
		}
	})
}
