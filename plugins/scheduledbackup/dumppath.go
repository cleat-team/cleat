package scheduledbackup

import (
	"fmt"
	"path/filepath"
	"strings"
)

// maxConfigNameLen bounds a backup config's name.
//
// A bound is here because the name is interpolated into a filename and most
// filesystems cap a path component at 255 bytes. Without one, a long name makes
// pg_dump fail with a filesystem error at backup time rather than the API
// refusing it at creation time -- a failure the operator sees only when a
// scheduled backup silently stops producing files.
const maxConfigNameLen = 128

// ValidConfigName reports whether a backup config's name is safe to interpolate
// into a dump filename.
//
// cleat#1305: the only check was `name != ""`, and the name reaches
// `pg_dump -f` through `fmt.Sprintf("manual_%s_%s.dump", name, ts)` and
// `filepath.Join(DumpDir, filename)`. An authenticated tenant could direct a
// full cross-tenant database dump outside DumpDir, as the worker's OS user.
//
// The charset matches engine/memory.go's validServiceName, which this codebase
// already enforces on service and operation names for the same reason.
//
// `.` and `..` are rejected EXPLICITLY. Both pass the charset -- they are made
// entirely of allowed characters -- and `..` is precisely the payload. A rule
// that reads "only safe characters" and admits the traversal token is the kind
// of allowlist that looks complete and is not.
func ValidConfigName(name string) bool {
	if name == "" || len(name) > maxConfigNameLen {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// configNameRule is the message every rejection uses, so an operator who trips
// it is told the rule rather than that something was "invalid".
const configNameRule = "name must be 1-128 characters of letters, digits, '.', '_' or '-', and cannot be '.' or '..'"

// SafeDumpPath joins filename to dumpDir and refuses anything that escapes it.
//
// THIS IS NOT BELT-AND-BRACES OVER ValidConfigName, and the distinction is the
// reason it exists. Input validation guards the two HTTP doors; it does nothing
// about rows ALREADY IN backup_config with a traversing name, which the cron
// sweep (background.go) and the CLI command (commands.go) will use without ever
// passing through a handler. This is the only guard those two paths reach.
//
// It is also what keeps the allowlist sufficient after someone adds a second
// interpolated field to the filename -- at which point the charset check is
// still correct and no longer complete.
func SafeDumpPath(dumpDir, filename string) (string, error) {
	if dumpDir == "" {
		return "", fmt.Errorf("scheduledbackup: dump directory is not configured")
	}
	base, err := filepath.Abs(dumpDir)
	if err != nil {
		return "", fmt.Errorf("scheduledbackup: resolving dump directory %q: %w", dumpDir, err)
	}
	full, err := filepath.Abs(filepath.Join(base, filename))
	if err != nil {
		return "", fmt.Errorf("scheduledbackup: resolving dump path: %w", err)
	}
	// Compare against base+separator so that a sibling directory sharing a
	// prefix -- /var/lib/cleat/dumps-evil against /var/lib/cleat/dumps -- is
	// not accepted by a plain HasPrefix.
	if full != base && !strings.HasPrefix(full, base+string(filepath.Separator)) {
		return "", fmt.Errorf(
			"scheduledbackup: refusing to write a dump outside the dump directory: %q resolves to %q, which is not under %q",
			filename, full, base)
	}
	if full == base {
		return "", fmt.Errorf("scheduledbackup: dump filename %q resolves to the dump directory itself", filename)
	}
	return full, nil
}
