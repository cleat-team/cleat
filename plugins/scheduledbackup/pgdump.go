package scheduledbackup

import (
	"context"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// runPgDump executes pg_dump against dsn, writing the dump to outPath and
// stderr to stderr.
//
// dsn is never passed to pg_dump as-is if it carries a password: pg_dump's
// connection argument ends up in the child process's argv, which is
// readable by any co-resident user via `ps` and /proc/*/cmdline. Instead,
// runPgDump strips the password out of dsn and passes it via the
// PGPASSWORD environment variable, which libpq (and so pg_dump) reads when
// the connection string omits a password, and which is only visible to the
// same UID or root (via /proc/*/environ) -- not to every user on the host.
func runPgDump(ctx context.Context, dsn, outPath string, stderr io.Writer) error {
	sanitized, password := splitDSNPassword(dsn)
	cmd := exec.CommandContext(ctx, "pg_dump", "-f", outPath, sanitized)
	cmd.Stderr = stderr
	cmd.Env = pgDumpEnv(password)
	return cmd.Run()
}

// splitDSNPassword separates a password from a PostgreSQL connection
// string, if one is present, returning the connection string with the
// password removed and the password itself. If dsn carries no password (or
// isn't recognized as either supported format), it is returned unchanged
// with an empty password.
//
// Both connection-string forms pg_dump accepts are handled:
//   - URI form: postgres://user:password@host:port/dbname?sslmode=...
//   - Keyword/value form: "host=... user=... password=... dbname=..."
func splitDSNPassword(dsn string) (sanitized, password string) {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			u.User = url.User(u.User.Username())
			return u.String(), pw
		}
		return dsn, ""
	}

	fields := strings.Fields(dsn)
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if pw, ok := strings.CutPrefix(f, "password="); ok {
			password = pw
			continue
		}
		kept = append(kept, f)
	}
	if password == "" {
		return dsn, ""
	}
	return strings.Join(kept, " "), password
}

// pgDumpEnv builds the environment for a pg_dump child process: the
// current process's environment, with any existing PGPASSWORD stripped
// (so there is exactly one, deterministically) and password set as
// PGPASSWORD if non-empty.
func pgDumpEnv(password string) []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+1)
	for _, e := range base {
		if strings.HasPrefix(e, "PGPASSWORD=") {
			continue
		}
		env = append(env, e)
	}
	if password != "" {
		env = append(env, "PGPASSWORD="+password)
	}
	return env
}
