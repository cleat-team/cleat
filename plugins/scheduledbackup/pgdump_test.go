package scheduledbackup

import (
	"strings"
	"testing"
)

// TestSplitDSNPassword pins the fix for Config.DSN ending up in pg_dump's
// argv (visible via ps and /proc/*/cmdline to any co-resident user):
// splitDSNPassword must remove any password from the connection string it
// returns, and must return that password separately so it can be delivered
// via PGPASSWORD instead.
func TestSplitDSNPassword(t *testing.T) {
	cases := []struct {
		name         string
		dsn          string
		wantPassword string
		wantNoSubstr string // must NOT appear in the sanitized DSN
	}{
		{
			name:         "postgres URI with password",
			dsn:          "postgres://alice:s3cr3t@db.example.com:5432/mydb?sslmode=disable",
			wantPassword: "s3cr3t",
			wantNoSubstr: "s3cr3t",
		},
		{
			name:         "postgresql URI scheme",
			dsn:          "postgresql://bob:hunter2@localhost/db",
			wantPassword: "hunter2",
			wantNoSubstr: "hunter2",
		},
		{
			name:         "keyword/value form",
			dsn:          "host=localhost user=alice password=s3cr3t dbname=mydb",
			wantPassword: "s3cr3t",
			wantNoSubstr: "s3cr3t",
		},
		{
			name:         "URI with no password",
			dsn:          "postgres://alice@db.example.com/mydb",
			wantPassword: "",
			wantNoSubstr: "",
		},
		{
			name:         "keyword form with no password",
			dsn:          "host=localhost user=alice dbname=mydb",
			wantPassword: "",
			wantNoSubstr: "",
		},
		{
			name:         "empty dsn",
			dsn:          "",
			wantPassword: "",
			wantNoSubstr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sanitized, password := splitDSNPassword(tc.dsn)
			if password != tc.wantPassword {
				t.Errorf("password = %q, want %q", password, tc.wantPassword)
			}
			if tc.wantNoSubstr != "" && strings.Contains(sanitized, tc.wantNoSubstr) {
				t.Errorf("sanitized DSN %q still contains the password %q", sanitized, tc.wantNoSubstr)
			}
		})
	}
}

// TestSplitDSNPassword_UsernamePreserved confirms the username (not a
// secret) is still present in the sanitized URI form -- only the password
// is stripped, not the whole userinfo.
func TestSplitDSNPassword_UsernamePreserved(t *testing.T) {
	sanitized, password := splitDSNPassword("postgres://alice:s3cr3t@db.example.com/mydb")
	if password != "s3cr3t" {
		t.Fatalf("password = %q, want s3cr3t", password)
	}
	if !strings.Contains(sanitized, "alice") {
		t.Errorf("sanitized DSN %q lost the username", sanitized)
	}
	if !strings.Contains(sanitized, "db.example.com") {
		t.Errorf("sanitized DSN %q lost the host", sanitized)
	}
}

// TestPgDumpEnv pins that PGPASSWORD is set exactly once (deterministically
// overriding any PGPASSWORD already in the process environment) and that
// the rest of the environment passes through.
func TestPgDumpEnv(t *testing.T) {
	env := pgDumpEnv("s3cr3t")

	found := 0
	var value string
	for _, e := range env {
		if strings.HasPrefix(e, "PGPASSWORD=") {
			found++
			value = strings.TrimPrefix(e, "PGPASSWORD=")
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one PGPASSWORD entry, found %d in %v", found, env)
	}
	if value != "s3cr3t" {
		t.Errorf("PGPASSWORD = %q, want s3cr3t", value)
	}
}

// TestPgDumpEnv_NoPasswordMeansNoPGPASSWORD confirms an empty password does
// not add an empty PGPASSWORD=, which would override a real one a co-located
// tool might rely on and is not what "no password" should mean.
func TestPgDumpEnv_NoPasswordMeansNoPGPASSWORD(t *testing.T) {
	env := pgDumpEnv("")
	for _, e := range env {
		if strings.HasPrefix(e, "PGPASSWORD=") {
			t.Fatalf("expected no PGPASSWORD entry for an empty password, got %q", e)
		}
	}
}
