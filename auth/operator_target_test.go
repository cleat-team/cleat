package auth

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestOperatorTargetTenant(t *testing.T) {
	const b = "22222222-2222-2222-2222-222222222222"
	bid := uuid.MustParse(b)
	upperWant := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if got, ok := OperatorTargetTenant("/api/admin/tenants/" + strings.ToUpper(upperWant.String()) + "/x"); !ok || got != upperWant {
		t.Errorf("an upper-case UUID gave %v, %v; want the canonical %v", got, ok, upperWant)
	}

	named := []struct{ name, path string }{
		{"force-complete", "/api/admin/tenants/" + b + "/instances/wf-1/force-complete"},
		{"the instances listing", "/api/admin/tenants/" + b + "/instances/"},
		// The handler parses with uuid.Parse, so these name the same tenant and must land on the
		// same chain; the value returned is the canonical one.
		{"braces", "/api/admin/tenants/{" + b + "}/instances/wf-1/force-fail"},
		{"urn prefix", "/api/admin/tenants/urn:uuid:" + b + "/instances/wf-1/force-fail"},
		{"bare hex", "/api/admin/tenants/22222222222222222222222222222222/instances/wf-1/force-fail"},
	}
	for _, tc := range named {
		got, ok := OperatorTargetTenant(tc.path)
		if !ok || got != bid {
			t.Errorf("%s: OperatorTargetTenant(%q) = %v, %v; want %v, true", tc.name, tc.path, got, ok, bid)
		}
	}

	none := []struct{ name, path string }{
		{"health", "/api/admin/health"},
		{"drain", "/api/admin/drain"},
		{"retention sweep", "/api/admin/retention/sweep"},
		{"the id-only instance route", "/api/admin/instances/wf-1/force-complete"},
		{"the collection (tenant creation names none yet)", "/api/admin/tenants"},
		{"the collection with a slash", "/api/admin/tenants/"},
		{"a tenant with no tail", "/api/admin/tenants/" + b},
		{"empty segment", "/api/admin/tenants//instances/wf-1/force-complete"},
		{"not a UUID", "/api/admin/tenants/acme/instances/wf-1/force-complete"},
		{"the nil UUID", "/api/admin/tenants/00000000-0000-0000-0000-000000000000/instances/x"},
		// The mux is case-sensitive, so a differently-cased path reaches no handler and has no target.
		{"upper-cased prefix", "/API/ADMIN/TENANTS/" + b + "/instances/x"},
		{"a tenant route outside the admin prefix", "/api/tenants/" + b + "/instances/x"},
		{"empty", ""},
	}
	for _, tc := range none {
		if got, ok := OperatorTargetTenant(tc.path); ok || got != uuid.Nil {
			t.Errorf("%s: OperatorTargetTenant(%q) = %v, %v; want the nil UUID and false", tc.name, tc.path, got, ok)
		}
	}
}
