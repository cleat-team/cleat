package main

// cleatctl reseal-secrets (cleat#1991), driven the way an operator drives it, on
// all three dialects: env in, report and exit status out.
//
// The engine's a_secret_rotation_test.go covers ResealSecrets itself. This covers
// what the command does WITH it -- the dry run that reports and writes nothing,
// the exit status a loop can trust, and the refusals -- which the engine tests
// cannot see because they never go through flag parsing, the environment or exit.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/google/uuid"
)

func ringKeyB64(fill byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = fill
	}
	return base64.StdEncoding.EncodeToString(k)
}

func ringKeyRaw(fill byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = fill
	}
	return k
}

// setRingEnv sets all four ring variables, blanking the ones not given, so a
// value left by an earlier test cannot leak into this one.
func setRingEnv(t *testing.T, cur, curVer, prev, prevVer string) {
	t.Helper()
	t.Setenv("CLEAT_SECRET_MASTER_KEY", cur)
	t.Setenv("CLEAT_SECRET_MASTER_KEY_VERSION", curVer)
	t.Setenv("CLEAT_SECRET_MASTER_KEY_PREVIOUS", prev)
	t.Setenv("CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION", prevVer)
}

// runCapturingExit runs fn with stdout, stderr and osExit intercepted, and
// returns what was printed and the exit status (0 when fn returned normally).
// withExitPanicOutput does the same interception and discards the status, which
// is the one thing these tests are about.
func runCapturingExit(t *testing.T, fn func()) (stdout, stderr string, code int) {
	t.Helper()
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	oldOut, oldErr, origExit := os.Stdout, os.Stderr, osExit
	os.Stdout, os.Stderr = wOut, wErr
	type exitSignal struct{ code int }
	osExit = func(c int) { panic(exitSignal{c}) }

	outCh, errCh := make(chan string), make(chan string)
	go func() { var b bytes.Buffer; io.Copy(&b, rOut); outCh <- b.String() }()
	go func() { var b bytes.Buffer; io.Copy(&b, rErr); errCh <- b.String() }()

	func() {
		defer func() {
			if r := recover(); r != nil {
				if es, ok := r.(exitSignal); ok {
					code = es.code
					return
				}
				panic(r)
			}
		}()
		fn()
	}()

	osExit, os.Stdout, os.Stderr = origExit, oldOut, oldErr
	wOut.Close()
	wErr.Close()
	return <-outCh, <-errCh, code
}

type resealFixture struct {
	dialect testutil.Dialect
	db      *sql.DB
	tenants []uuid.UUID
}

func newResealFixture(t *testing.T, dialect testutil.Dialect) *resealFixture {
	t.Helper()
	db := testutil.TestDB(t, dialect)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, dialect)
	f := &resealFixture{dialect: dialect, db: db, tenants: []uuid.UUID{uuid.MustParse(engine.DefaultTenantUUID)}}

	if dialect != testutil.DialectMySQL { // MySQL is single-tenant by constraint
		suspended := uuid.New()
		ins := map[testutil.Dialect]string{
			testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, true)`,
			testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES (@p1, @p2, 1)`,
		}[dialect]
		if _, err := db.Exec(ins, suspended.String(), "cleat-1991-ctl-"+suspended.String()[:8]); err != nil {
			t.Fatalf("seed a suspended tenant: %v", err)
		}
		f.tenants = append(f.tenants, suspended)
		del := map[testutil.Dialect]string{
			testutil.DialectPostgres: `DELETE FROM admin.tenants WHERE tenant_id = $1`,
			testutil.DialectMSSQL:    `DELETE FROM admin.tenants WHERE tenant_id = @p1`,
		}[dialect]
		t.Cleanup(func() { db.Exec(del, suspended.String()) }) //nolint:errcheck // best-effort cleanup
	}
	// cleat#2126: the retired-secret test leaves one row behind on SQL Server. Reseal is
	// global, so it would sit in every report and force a non-zero exit. Remove
	// THAT ONE, by name, so a NEW leak still shows up as a failure here instead of being
	// hidden. Delete this line when #2126 is fixed.
	f.deleteRow(t, uuid.MustParse(engine.DefaultTenantUUID), "cleat-1989-retirement-check")
	return f
}

// deleteRow removes one secret row. On SQL Server the row is invisible unless
// SESSION_CONTEXT('tenant_id') carries its tenant, and the setting and the
// DELETE must share one connection or the DELETE affects zero rows and reports
// success.
func (f *resealFixture) deleteRow(t *testing.T, tenant uuid.UUID, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := f.db.Conn(ctx)
	if err != nil {
		t.Errorf("cleanup: acquire a connection: %v", err)
		return
	}
	defer conn.Close()
	if f.dialect == testutil.DialectMSSQL {
		if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant.String()); err != nil {
			t.Errorf("cleanup: scope the connection: %v", err)
			return
		}
	}
	q := map[testutil.Dialect]string{
		testutil.DialectPostgres: `DELETE FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`,
		testutil.DialectMySQL:    `DELETE FROM tenant_secrets WHERE tenant_id = ? AND name = ?`,
		testutil.DialectMSSQL:    `DELETE FROM tenant_secrets WHERE tenant_id = @p1 AND name = @p2`,
	}[f.dialect]
	if _, err := conn.ExecContext(ctx, q, tenant.String(), name); err != nil {
		t.Errorf("cleanup: delete %q: %v", name, err)
	}
}

func (f *resealFixture) claim(t *testing.T, tenant uuid.UUID, name string) {
	t.Helper()
	f.deleteRow(t, tenant, name)
	t.Cleanup(func() { f.deleteRow(t, tenant, name) })
}

func (f *resealFixture) storeFor(cur engine.VersionedKey, prev ...engine.VersionedKey) *engine.SecretStore {
	ring, err := engine.NewKeyRing(cur, prev...)
	if err != nil {
		panic(err)
	}
	return engine.NewSecretStoreWithRing(f.db, string(f.dialect), ring)
}

func (f *resealFixture) put(t *testing.T, s *engine.SecretStore, tenant uuid.UUID, name, value string) {
	t.Helper()
	if err := s.PutSecret(tenantctx.With(context.Background(), tenant), tenant.String(), name, value); err != nil {
		t.Fatalf("PutSecret %q: %v", name, err)
	}
}

func (f *resealFixture) get(s *engine.SecretStore, tenant uuid.UUID, name string) (string, error) {
	return s.GetSecret(tenantctx.With(context.Background(), tenant), tenant.String(), name)
}

func (f *resealFixture) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	return runCapturingExit(t, func() {
		runResealSecrets(context.Background(), f.db, dialect{name: string(f.dialect)}, args)
	})
}

func forEachResealDialect(t *testing.T, fn func(t *testing.T, f *resealFixture)) {
	for _, d := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(d), func(t *testing.T) { fn(t, newResealFixture(t, d)) })
	}
}

var (
	ctlV1 = engine.VersionedKey{Version: 1, Key: ringKeyRaw(0x41)}
	ctlV2 = engine.VersionedKey{Version: 2, Key: ringKeyRaw(0x42)}
	ctlV3 = engine.VersionedKey{Version: 3, Key: ringKeyRaw(0x43)}
)

// The operator's happy path: a dry run that finds work reports it, writes nothing
// and exits non-zero; the real run converts everything and exits zero; a second
// run has nothing to do and exits zero.
func TestResealSecretsCommandConvergesAndIsIdempotent(t *testing.T) {
	forEachResealDialect(t, func(t *testing.T, f *resealFixture) {
		before := f.storeFor(ctlV1)
		var names []string
		for i, tn := range f.tenants {
			name := fmt.Sprintf("cleat-1991-ctl-%c", 'a'+i)
			f.claim(t, tn, name)
			f.put(t, before, tn, name, "value-"+name)
			names = append(names, name)
		}
		setRingEnv(t, ringKeyB64(0x42), "2", ringKeyB64(0x41), "1")
		newOnly := f.storeFor(ctlV2)

		// Dry run: reports the work, exits non-zero, writes nothing.
		out, errOut, code := f.run(t, "--dry-run")
		if code != 1 {
			t.Fatalf("a dry run that found work exited %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		if !strings.Contains(out, "would reseal:") || strings.Contains(out, "would reseal:         0") {
			t.Errorf("a dry run must say what it WOULD do; got:\n%s", out)
		}
		for i, tn := range f.tenants {
			if _, err := f.get(newOnly, tn, names[i]); err == nil {
				t.Fatalf("a DRY RUN made %q readable under the new key alone: it wrote", names[i])
			}
		}

		// The real run.
		out, errOut, code = f.run(t)
		if code != 0 {
			t.Fatalf("reseal-secrets exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		for _, want := range []string{"key ring:", "current key_version 2", "resealed:", "unreadable:            0"} {
			if !strings.Contains(out, want) {
				t.Errorf("the report should contain %q; got:\n%s", want, out)
			}
		}
		for i, tn := range f.tenants {
			if got, err := f.get(newOnly, tn, names[i]); err != nil || got != "value-"+names[i] {
				t.Errorf("%q under the NEW key alone: %q, %v", names[i], got, err)
			}
		}

		// Idempotent: nothing left to do, and it says so.
		out, errOut, code = f.run(t)
		if code != 0 || !strings.Contains(out, "resealed:              0") {
			t.Fatalf("a second run: exit %d, want 0 with nothing resealed\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
	})
}

// A row the ring cannot open is REPORTED by tenant, name and version, left
// exactly as it was, and forces a non-zero exit -- so a loop does not declare the
// rotation finished while a secret is still under a key that is about to go.
func TestResealSecretsCommandReportsAnUnreadableRowAndExitsNonZero(t *testing.T) {
	forEachResealDialect(t, func(t *testing.T, f *resealFixture) {
		tn := f.tenants[0]
		const name = "cleat-1991-ctl-unreadable"
		f.claim(t, tn, name)
		f.put(t, f.storeFor(ctlV3), tn, name, "sealed-under-a-key-this-ring-lacks")

		setRingEnv(t, ringKeyB64(0x42), "2", ringKeyB64(0x41), "1")
		out, errOut, code := f.run(t)
		if code != 1 {
			t.Fatalf("exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "UNREADABLE") && strings.Contains(l, name) {
				line = l
			}
		}
		if line == "" || !strings.Contains(line, "key_version=3") || !strings.Contains(line, "tenant="+tn.String()) {
			t.Fatalf("the row must be reported with its tenant, name and key_version; got:\n%s", out)
		}
		if got, err := f.get(f.storeFor(ctlV3), tn, name); err != nil || got != "sealed-under-a-key-this-ring-lacks" {
			t.Errorf("the unreadable row was TOUCHED: %q, %v", got, err)
		}
	})
}

// The refusals need no database, so they run once.
func TestResealSecretsCommandRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      [4]string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"no key configured", [4]string{"", "", "", ""}, nil, 1, "CLEAT_SECRET_MASTER_KEY is not set"},
		{"a previous key with no version", [4]string{ringKeyB64(0x42), "2", ringKeyB64(0x41), ""}, nil, 1, "CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION is not"},
		{"the same key under two versions", [4]string{ringKeyB64(0x42), "2", ringKeyB64(0x42), "1"}, nil, 1, "carry the same key"},
		{"a stray argument", [4]string{ringKeyB64(0x42), "2", ringKeyB64(0x41), "1"}, []string{"extra"}, 2, "takes no arguments"},
		{"an unknown flag", [4]string{ringKeyB64(0x42), "2", ringKeyB64(0x41), "1"}, []string{"--nonsense"}, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRingEnv(t, tc.env[0], tc.env[1], tc.env[2], tc.env[3])
			_, errOut, code := runCapturingExit(t, func() {
				// A nil *sql.DB is enough: every case here stops before the first query,
				// and a case that did not would panic, which is a louder failure than a skip.
				runResealSecrets(context.Background(), nil, dialect{name: "postgres"}, tc.args)
			})
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, tc.wantCode, errOut)
			}
			if tc.wantErr != "" && !strings.Contains(errOut, tc.wantErr) {
				t.Errorf("stderr should contain %q; got:\n%s", tc.wantErr, errOut)
			}
		})
	}
}
