package main

// cleat#2487. These are small, previously-untested functions found while
// closing cmd/cleatctl's coverage-floor gap: pure helpers with no direct
// caller test, a dialect-gate function, two usage printers, and the two
// api-key lookups that only ran indirectly through a full-command test.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

func TestBracketQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"tenants", "[tenants]"},
		{"weird]name", "[weird]]name]"},
		{"", "[]"},
	}
	for _, c := range cases {
		if got := bracketQuote(c.in); got != c.want {
			t.Errorf("bracketQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMysqlBaseDSN(t *testing.T) {
	cases := []struct{ in, want string }{
		{"root:pass@tcp(host:3306)/mydb?parseTime=true", "root:pass@tcp(host:3306)/?parseTime=true"},
		{"root:pass@tcp(host:3306)/mydb", "root:pass@tcp(host:3306)/"},
		{"no-slash-here", "no-slash-here"},
	}
	for _, c := range cases {
		if got := mysqlBaseDSN(c.in); got != c.want {
			t.Errorf("mysqlBaseDSN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDescribeSetting(t *testing.T) {
	if got := describeSetting(0); got != "unset (uses the operator's flag)" {
		t.Errorf("describeSetting(0) = %q, want the unset message", got)
	}
	if got := describeSetting(-5 * time.Second); got != "unset (uses the operator's flag)" {
		t.Errorf("describeSetting(negative) = %q, want the unset message", got)
	}
	if got, want := describeSetting(30*time.Second), "30s"; got != want {
		t.Errorf("describeSetting(30s) = %q, want %q", got, want)
	}
}

func TestIsSettingFlag(t *testing.T) {
	vals := map[string]int64{"--max-retries": 5}
	if !isSettingFlag("--max-retries", vals) {
		t.Error("isSettingFlag(--max-retries) = false, want true")
	}
	if !isSettingFlag("max-retries", vals) {
		t.Error("isSettingFlag(max-retries) with no leading dashes = false, want true (TrimLeft strips them)")
	}
	if isSettingFlag("--unknown-flag", vals) {
		t.Error("isSettingFlag(--unknown-flag) = true, want false")
	}
}

func TestReadPayloadKeyFile(t *testing.T) {
	validKey := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // base64 of 32 zero bytes
	dir := t.TempDir()

	goodPath := dir + "/good.key"
	if err := os.WriteFile(goodPath, []byte(validKey+"\n"), 0o600); err != nil {
		t.Fatalf("write good key: %v", err)
	}
	key, err := readPayloadKeyFile(goodPath, "--encryption-key-file")
	if err != nil {
		t.Fatalf("readPayloadKeyFile(good) = %v, want nil", err)
	}
	if len(key) != 32 {
		t.Errorf("readPayloadKeyFile(good) decoded to %d bytes, want 32", len(key))
	}

	if _, err := readPayloadKeyFile(dir+"/missing.key", "--from-key-file"); err == nil {
		t.Error("readPayloadKeyFile(missing file) = nil error, want one naming --from-key-file")
	} else if !strings.Contains(err.Error(), "--from-key-file") {
		t.Errorf("readPayloadKeyFile(missing) error %q does not name the flag", err.Error())
	}

	badB64Path := dir + "/bad-b64.key"
	if err := os.WriteFile(badB64Path, []byte("not-base64!!!"), 0o600); err != nil {
		t.Fatalf("write bad-b64 key: %v", err)
	}
	if _, err := readPayloadKeyFile(badB64Path, "--encryption-key-file"); err == nil {
		t.Error("readPayloadKeyFile(invalid base64) = nil error, want one")
	}

	shortPath := dir + "/short.key"
	if err := os.WriteFile(shortPath, []byte("QUJD"), 0o600); err != nil { // base64("ABC"), 3 bytes
		t.Fatalf("write short key: %v", err)
	}
	if _, err := readPayloadKeyFile(shortPath, "--encryption-key-file"); err == nil {
		t.Error("readPayloadKeyFile(3-byte key) = nil error, want a 32-byte-length error")
	}
}

func TestRequirePortedFor(t *testing.T) {
	// Not in portedOn at all: must return, not exit.
	requirePortedFor("debug", dialectMySQL)

	// oauth-allow is postgres-only (ported.go) -- supported dialect returns.
	requirePortedFor("oauth-allow", dialectPostgres)

	// An unsupported dialect must exit(1), not fall through.
	var code int
	_, stderr := withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		requirePortedFor("oauth-allow", dialectMySQL)
	})
	if code != 1 {
		t.Errorf("requirePortedFor(unsupported dialect) exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "oauth-allow") || !strings.Contains(stderr, "mysql") {
		t.Errorf("requirePortedFor stderr = %q, want it to name the command and dialect", stderr)
	}
}

func TestPrintUsageFunctionsDoNotPanic(t *testing.T) {
	// These are pure side-effecting printers with nothing to assert beyond
	// "runs to completion" -- the operator-facing text itself is covered by
	// reading it in review, not by a test re-asserting prose.
	withExitPanicOutput(t, func() {
		printBackupUsage()
		printOAuthAllowUsage()
		printSetDeploymentSecretUsage()
		printSetTenantSettingUsage()
	})
	var buf strings.Builder
	printOAuthAuthorityWarning(&buf)
	if buf.Len() == 0 {
		t.Error("printOAuthAuthorityWarning wrote nothing to its io.Writer")
	}
}

func apiKeyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	return db
}

func seedAPIKeyForTest(t *testing.T, db *sql.DB, tenant uuid.UUID) (uuid.UUID, []byte) {
	t.Helper()
	id := uuid.New()
	hash := sha256.Sum256([]byte(id.String()))
	_, err := db.Exec(`
		INSERT INTO admin.tenant_api_keys (tenant_id, key_id, key_hash, description)
		VALUES ($1, $2, $3, $4)`,
		tenant, id, hash[:], "coverage-floor-remediation-test")
	if err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE key_id = $1`, id)
	})
	return id, hash[:]
}

func TestFindAPIKey(t *testing.T) {
	db := apiKeyTestDB(t)
	tenant := uuid.MustParse(engine.DefaultTenantUUID)
	id, hash := seedAPIKeyForTest(t, db, tenant)

	row, err := findAPIKey(context.Background(), db, revokeKeySelector{keyID: id})
	if err != nil {
		t.Fatalf("findAPIKey(by id) = %v", err)
	}
	if row == nil || row.keyID != id {
		t.Fatalf("findAPIKey(by id) = %+v, want keyID %s", row, id)
	}

	row, err = findAPIKey(context.Background(), db, revokeKeySelector{keyHash: hash})
	if err != nil {
		t.Fatalf("findAPIKey(by hash) = %v", err)
	}
	if row == nil || row.keyID != id {
		t.Fatalf("findAPIKey(by hash) = %+v, want keyID %s", row, id)
	}

	row, err = findAPIKey(context.Background(), db, revokeKeySelector{keyID: uuid.New()})
	if err != nil {
		t.Fatalf("findAPIKey(unknown id) returned an error rather than nil,nil: %v", err)
	}
	if row != nil {
		t.Fatalf("findAPIKey(unknown id) = %+v, want nil", row)
	}
}

func TestListAPIKeys(t *testing.T) {
	db := apiKeyTestDB(t)
	tenant := uuid.MustParse(engine.DefaultTenantUUID)
	seedAPIKeyForTest(t, db, tenant)

	if err := listAPIKeys(context.Background(), db, tenant.String()); err != nil {
		t.Errorf("listAPIKeys(seeded tenant) = %v, want nil", err)
	}

	emptyTenant := uuid.New()
	if err := listAPIKeys(context.Background(), db, emptyTenant.String()); err != nil {
		t.Errorf("listAPIKeys(tenant with no keys) = %v, want nil (prints 'No API keys')", err)
	}

	if err := listAPIKeys(context.Background(), db, "not-a-uuid"); err == nil {
		t.Error("listAPIKeys(invalid uuid) = nil error, want one")
	}
}

func TestLookupTenantName(t *testing.T) {
	db := apiKeyTestDB(t)
	tenantID := uuid.New()
	// admin.tenants.name is unique, so it carries the tenant id the same way
	// seedTenant's does -- a literal name collides with a prior run against a
	// database this test does not get to recreate.
	name := "lookup-tenant-name-test-" + tenantID.String()
	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, tenantID, name); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	got, err := lookupTenantName(context.Background(), db, dialectPostgres, tenantID)
	if err != nil {
		t.Fatalf("lookupTenantName(seeded tenant) = %v", err)
	}
	if got != name {
		t.Errorf("lookupTenantName() = %q, want %q", got, name)
	}

	if _, err := lookupTenantName(context.Background(), db, dialectPostgres, uuid.New()); err == nil {
		t.Error("lookupTenantName(unknown tenant) = nil error, want one")
	}
}

func TestOperatorKeyState(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		k    auth.Operator
		want string
	}{
		{"revoked", auth.Operator{Disabled: true}, "revoked"},
		{"expired", auth.Operator{ExpiresAt: timePtr(now.Add(-time.Hour))}, "expired"},
		{"revoked takes precedence over expired", auth.Operator{Disabled: true, ExpiresAt: timePtr(now.Add(-time.Hour))}, "revoked"},
		{"live, no expiry", auth.Operator{}, "live"},
		{"live, not yet expired", auth.Operator{ExpiresAt: timePtr(now.Add(time.Hour))}, "live"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := operatorKeyState(c.k, now); got != c.want {
				t.Errorf("operatorKeyState() = %q, want %q", got, c.want)
			}
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func operatorKeyTestDB(t *testing.T) *auth.OperatorStore {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	// TestDB does not clean admin.operator_api_keys between tests -- that is
	// an opt-in helper, not automatic -- and the table has no tenant to scope
	// a DELETE by, so a prior test's rows are otherwise still here.
	if _, err := db.Exec(`DELETE FROM admin.operator_api_keys`); err != nil {
		t.Fatalf("clear admin.operator_api_keys: %v", err)
	}
	store, err := auth.NewOperatorStoreForDialect(db, "postgres")
	if err != nil {
		t.Fatalf("NewOperatorStoreForDialect: %v", err)
	}
	return store
}

func TestOperatorKeyCreateListRevoke(t *testing.T) {
	store := operatorKeyTestDB(t)
	ctx := context.Background()

	// create
	stdout, _ := withExitPanicOutput(t, func() {
		operatorKeyCreate(ctx, store, []string{"--description", "coverage-floor-remediation"})
	})
	if !strings.Contains(stdout, "Operator key created") {
		t.Fatalf("operatorKeyCreate stdout = %q, want it to report creation", stdout)
	}

	keys, err := store.ListOperatorKeys(ctx)
	if err != nil {
		t.Fatalf("ListOperatorKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("ListOperatorKeys() = %d keys, want 1", len(keys))
	}
	keyID := keys[0].KeyID

	// create with no --description is refused
	var code int
	withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		operatorKeyCreate(ctx, store, []string{})
	})
	if code != 2 {
		t.Errorf("operatorKeyCreate(no --description) exit code = %d, want 2", code)
	}

	// list
	stdout, _ = withExitPanicOutput(t, func() {
		operatorKeyList(ctx, store, []string{})
	})
	if !strings.Contains(stdout, keyID) {
		t.Errorf("operatorKeyList stdout = %q, want it to mention %s", stdout, keyID)
	}

	// revoke
	stdout, _ = withExitPanicOutput(t, func() {
		operatorKeyRevoke(ctx, store, []string{"--key-id", keyID})
	})
	if !strings.Contains(stdout, keyID) {
		t.Errorf("operatorKeyRevoke stdout = %q, want it to name %s", stdout, keyID)
	}

	keys, err = store.ListOperatorKeys(ctx)
	if err != nil {
		t.Fatalf("ListOperatorKeys (after revoke): %v", err)
	}
	if len(keys) != 1 || !keys[0].Disabled {
		t.Errorf("after revoke, key state = %+v, want Disabled=true", keys)
	}

	// revoke with no --key-id is refused
	withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		operatorKeyRevoke(ctx, store, []string{})
	})
	if code != 2 {
		t.Errorf("operatorKeyRevoke(no --key-id) exit code = %d, want 2", code)
	}
}

func TestOperatorKeyListWhenEmpty(t *testing.T) {
	store := operatorKeyTestDB(t)
	stdout, _ := withExitPanicOutput(t, func() {
		operatorKeyList(context.Background(), store, []string{})
	})
	if !strings.Contains(stdout, "No operator keys") {
		t.Errorf("operatorKeyList(empty) stdout = %q, want it to say so", stdout)
	}
}

func TestRunPublicExposureGrant(t *testing.T) {
	db := apiKeyTestDB(t)
	tenantID := uuid.New()
	name := "public-exposure-test-" + tenantID.String()
	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, tenantID, name); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ctx := context.Background()

	// allow, with --yes so it skips the stdin confirmation prompt.
	stdout, _ := withExitPanicOutput(t, func() {
		runPublicExposureGrant(ctx, db, dialectPostgres, []string{tenantID.String(), "--yes"}, true)
	})
	var allowed bool
	if err := db.QueryRow(`SELECT allow_public_exposure FROM admin.tenants WHERE tenant_id = $1`, tenantID).
		Scan(&allowed); err != nil {
		t.Fatalf("read back allow_public_exposure: %v", err)
	}
	if !allowed {
		t.Fatalf("runPublicExposureGrant(allow) did not set the column; stdout=%q", stdout)
	}

	// granting again is a no-op, reported rather than rewritten.
	stdout, _ = withExitPanicOutput(t, func() {
		runPublicExposureGrant(ctx, db, dialectPostgres, []string{tenantID.String(), "--yes"}, true)
	})
	if !strings.Contains(stdout, "nothing to do") {
		t.Errorf("runPublicExposureGrant(allow, already allowed) stdout = %q, want it to say nothing to do", stdout)
	}

	// revoke needs no confirmation.
	withExitPanicOutput(t, func() {
		runPublicExposureGrant(ctx, db, dialectPostgres, []string{tenantID.String()}, false)
	})
	if err := db.QueryRow(`SELECT allow_public_exposure FROM admin.tenants WHERE tenant_id = $1`, tenantID).
		Scan(&allowed); err != nil {
		t.Fatalf("read back allow_public_exposure: %v", err)
	}
	if allowed {
		t.Error("runPublicExposureGrant(revoke) did not clear the column")
	}

	// an unknown tenant id exits 1.
	var code int
	withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		runPublicExposureGrant(ctx, db, dialectPostgres, []string{uuid.New().String()}, false)
	})
	if code != 1 {
		t.Errorf("runPublicExposureGrant(unknown tenant) exit code = %d, want 1", code)
	}

	// no operand exits 2.
	withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		runPublicExposureGrant(ctx, db, dialectPostgres, []string{}, true)
	})
	if code != 2 {
		t.Errorf("runPublicExposureGrant(no args) exit code = %d, want 2", code)
	}
}

func TestRunOAuthAllow(t *testing.T) {
	db := oauthAllowTestDB(t)
	ctx := context.Background()
	tenant := oauthTestTenant
	const provider = "coverage-floor-remediation-provider"
	const identity = "operator@example.com"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM oauth_allowed_identities WHERE provider = $1`, provider)
	})

	// list, with nothing admitted yet.
	stdout, _ := withExitPanicOutput(t, func() {
		runOAuthAllow(ctx, db, dialectPostgres, []string{"list", tenant, "--provider", provider})
	})
	if !strings.Contains(stdout, "admits NO identities") {
		t.Fatalf("runOAuthAllow(list, empty) stdout = %q", stdout)
	}

	// add.
	stdout, _ = withExitPanicOutput(t, func() {
		runOAuthAllow(ctx, db, dialectPostgres, []string{"add", tenant, identity, "--provider", provider})
	})
	if !strings.Contains(stdout, "now admits") {
		t.Fatalf("runOAuthAllow(add) stdout = %q", stdout)
	}

	// list, now admitting one.
	stdout, _ = withExitPanicOutput(t, func() {
		runOAuthAllow(ctx, db, dialectPostgres, []string{"list", tenant, "--provider", provider})
	})
	if !strings.Contains(stdout, identity) {
		t.Fatalf("runOAuthAllow(list, after add) stdout = %q, want it to mention %s", stdout, identity)
	}

	// remove.
	stdout, _ = withExitPanicOutput(t, func() {
		runOAuthAllow(ctx, db, dialectPostgres, []string{"remove", tenant, identity, "--provider", provider})
	})
	if !strings.Contains(stdout, "no longer admits") {
		t.Fatalf("runOAuthAllow(remove) stdout = %q", stdout)
	}

	// remove again: nothing left to remove, so this is the error path -- it
	// exits 1 and writes to stderr, not stdout.
	var code int
	_, stderr := withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		runOAuthAllow(ctx, db, dialectPostgres, []string{"remove", tenant, identity, "--provider", provider})
	})
	if code != 1 {
		t.Errorf("runOAuthAllow(remove, nothing left) exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "nothing was removed") {
		t.Errorf("runOAuthAllow(remove, nothing left) stderr = %q", stderr)
	}

	// an unknown tenant uuid exits 1 before reaching any subcommand.
	withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		runOAuthAllow(ctx, db, dialectPostgres, []string{"list", "not-a-uuid", "--provider", provider})
	})
	if code != 1 {
		t.Errorf("runOAuthAllow(invalid tenant uuid) exit code = %d, want 1", code)
	}
}

func TestRunOperatorKey(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	if _, err := db.Exec(`DELETE FROM admin.operator_api_keys`); err != nil {
		t.Fatalf("clear admin.operator_api_keys: %v", err)
	}
	ctx := context.Background()

	// A full round trip through the dispatcher itself, not through the
	// subcommand functions directly -- this is the only test that calls
	// runOperatorKey, and it is the thing that resolves args[0] into one of
	// them.
	stdout, _ := withExitPanicOutput(t, func() {
		runOperatorKey(ctx, db, dialectPostgres, []string{"create", "--description", "via-run-operator-key"})
	})
	if !strings.Contains(stdout, "Operator key created") {
		t.Fatalf("runOperatorKey(create) stdout = %q", stdout)
	}

	stdout, _ = withExitPanicOutput(t, func() {
		runOperatorKey(ctx, db, dialectPostgres, []string{"list"})
	})
	if !strings.Contains(stdout, "via-run-operator-key") {
		t.Errorf("runOperatorKey(list) stdout = %q, want it to mention the created key", stdout)
	}

	var code int
	withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		runOperatorKey(ctx, db, dialectPostgres, []string{"not-a-real-subcommand"})
	})
	if code != 2 {
		t.Errorf("runOperatorKey(unknown subcommand) exit code = %d, want 2", code)
	}

	withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		runOperatorKey(ctx, db, dialectPostgres, []string{})
	})
	if code != 2 {
		t.Errorf("runOperatorKey(no args) exit code = %d, want 2", code)
	}
}
