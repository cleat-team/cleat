package plugin

import "regexp"

var dollarRE = regexp.MustCompile(`\$(\d+)`)
var nowRE = regexp.MustCompile(`(?i)\bnow\s*\(\s*\)`)

// Rebind translates PostgreSQL $N parameter placeholders to the
// dialect-appropriate form. It also replaces now() with SYSUTCDATETIME()
// for MSSQL. PostgreSQL placeholders ($1, $2, ...) are left as-is.
func Rebind(query string, d Dialect) string {
	q := query
	switch d {
	case DialectMySQL:
		q = dollarRE.ReplaceAllString(q, "?")
	case DialectMSSQL:
		q = dollarRE.ReplaceAllString(q, "@p$1")
	}
	if d == DialectMSSQL {
		q = nowRE.ReplaceAllString(q, "SYSUTCDATETIME()")
	}
	return q
}

// Query holds dialect-specific variants of a runtime SQL query.
// Default (PostgreSQL) is required; MySQL and MSSQL are optional overrides.
type Query struct {
	Default string // required — PostgreSQL
	MySQL   string // optional
	MSSQL   string // optional
}

// For returns the SQL string appropriate for the given dialect.
// Falls back to Default when no dialect-specific override exists.
func (q Query) For(d Dialect) string {
	switch d {
	case DialectMySQL:
		if q.MySQL != "" {
			return q.MySQL
		}
	case DialectMSSQL:
		if q.MSSQL != "" {
			return q.MSSQL
		}
	}
	return q.Default
}
