package main

// cleat#3552, B2d. backendkit.Provisioner is a hand-written client for POST /api/admin/tenants and its
// confirm, and it decodes JSON whose field names are spelled once in this package (api_admin_provision.go) and
// once there. Nothing but a test connects the two spellings. This is that test: each scenario is driven
// through the real handlers (the fake store makes the ids and the secret deterministic), and the status and
// body are compared with the golden file backendkit's own tests decode.
//
// The fixtures live under cleat/backendkit/testdata/provisioning because backendkit's tests must be able to read
// them without importing this module. They are read from outside this package's directory, so `go test` does
// not notice an edit to one unless it is run with -count=1 (CI is). To capture the files from the handlers
// instead of typing them, run with CLEAT_WRITE_WIRE_FIXTURES=1 and read the diff.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
)

const wireFixtureDir = "../../cleat/backendkit/testdata/provisioning"

type wireFixture struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

const (
	wireTenantID    = "aaaaaaaa-0000-4000-8000-000000000001"
	wireCreateBody  = `{"tenant_id":"` + wireTenantID + `","org_name":"Acme Org","key":{"description":"first"}}`
	wireConfirmPath = "/api/admin/tenants/" + wireTenantID + "/confirm"
)

func TestTheProvisioningResponsesMatchWhatBackendkitDecodes(t *testing.T) {
	type scenario struct {
		name, path, body string
		setup            func(f *provisionHTTP, fp *fakeProvisioner)
	}
	create := func(name string, setup func(f *provisionHTTP, fp *fakeProvisioner)) scenario {
		return scenario{name, "/api/admin/tenants", wireCreateBody, setup}
	}
	confirm := func(name string, setup func(f *provisionHTTP, fp *fakeProvisioner)) scenario {
		return scenario{name, wireConfirmPath, confirmBody, setup}
	}
	conflict := func(c *auth.ProvisionConflictError) func(*provisionHTTP, *fakeProvisioner) {
		return func(_ *provisionHTTP, fp *fakeProvisioner) { fp.err = c }
	}
	scenarios := []scenario{
		create("created", func(_ *provisionHTTP, fp *fakeProvisioner) { fp.result = okResult(auth.ProvisionCreated, true) }),
		create("replayed_with_key", func(_ *provisionHTTP, fp *fakeProvisioner) { fp.result = okResult(auth.ProvisionReplayed, true) }),
		create("replayed_no_key", func(_ *provisionHTTP, fp *fakeProvisioner) { fp.result = okResult(auth.ProvisionReplayed, false) }),
		create("name_taken", conflict(&auth.ProvisionConflictError{Reason: auth.ConflictNameTaken})),
		create("tenant_exists", conflict(&auth.ProvisionConflictError{Reason: auth.ConflictTenantExists})),
		create("tenant_differs", conflict(&auth.ProvisionConflictError{Reason: auth.ConflictTenantDiffers, Fields: []string{"name", "org_id"}})),
		create("org_not_found", func(_ *provisionHTTP, fp *fakeProvisioner) { fp.err = auth.ErrOrgNotFound }),
		create("unavailable", func(f *provisionHTTP, _ *fakeProvisioner) { f.api.provisioner = nil }),
		confirm("confirm_ok", func(_ *provisionHTTP, fp *fakeProvisioner) { fp.confirmResult = auth.ConfirmResult{Confirmed: 1} }),
		confirm("confirm_already", func(_ *provisionHTTP, fp *fakeProvisioner) {
			fp.confirmResult = auth.ConfirmResult{AlreadyConfirmed: true}
		}),
		confirm("confirm_no_provisional_key", func(_ *provisionHTTP, fp *fakeProvisioner) { fp.confirmErr = auth.ErrNoProvisionalKey }),
		confirm("confirm_not_found", func(_ *provisionHTTP, fp *fakeProvisioner) { fp.confirmErr = auth.ErrTenantNotFound }),
		// Byte for byte the answer above, from a worker with the admin API off: the reason backendkit gives a
		// confirm's 404 no sentinel of its own.
		confirm("confirm_admin_api_off", func(_ *provisionHTTP, _ *fakeProvisioner) {
			off := false
			old := enableAdminAPI
			enableAdminAPI = &off
			t.Cleanup(func() { enableAdminAPI = old })
		}),
	}

	write := os.Getenv("CLEAT_WRITE_WIRE_FIXTURES") != ""
	seen := map[string]bool{}
	for _, sc := range scenarios {
		f, fp := buildProvisionHTTP(t, nil)
		sc.setup(f, fp)
		w := doBody(f.chainFixture, http.MethodPost, sc.path, f.provisionKey, sc.body)
		got := wireFixture{Status: w.Code, Body: json.RawMessage(strings.TrimSpace(w.Body.String()))}
		file := filepath.Join(wireFixtureDir, sc.name+".json")
		seen[sc.name+".json"] = true

		if write {
			b, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("%s: no fixture (%v): backendkit has nothing to decode for this response", sc.name, err)
			continue
		}
		var want wireFixture
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Errorf("%s: fixture is not valid: %v", sc.name, err)
			continue
		}
		var gotBody, wantBody any
		if err := json.Unmarshal(got.Body, &gotBody); err != nil {
			t.Errorf("%s: the handler's body is not JSON: %q", sc.name, w.Body.String())
			continue
		}
		_ = json.Unmarshal(want.Body, &wantBody)
		if got.Status != want.Status || !reflect.DeepEqual(gotBody, wantBody) {
			t.Errorf("%s: the handler answered %d %s, but backendkit's fixture says %d %s -- if the worker's wire format changed on purpose, update the fixture AND backendkit's decoding of it",
				sc.name, got.Status, got.Body, want.Status, want.Body)
		}
	}

	// A fixture no scenario produces is a file backendkit still decodes after the worker stopped saying it.
	entries, err := os.ReadDir(wireFixtureDir)
	if err != nil {
		t.Fatalf("reading the fixture directory: %v", err)
	}
	var stale []string
	for _, e := range entries {
		if !seen[e.Name()] {
			stale = append(stale, e.Name())
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("fixtures no scenario produces: %v", stale)
	}
}
