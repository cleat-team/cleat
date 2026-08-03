package plugin

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

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

// QuoteIdent quotes a SQL identifier for the given dialect.
//
// It exists because `key` and `value` -- both natural column names, and both
// used by shipped plugins -- are reserved words in MySQL and SQL Server. An
// unquoted `key` produced
//
//	Error 1064 (42000): You have an error in your SQL syntax ...
//	mssql: Incorrect syntax near the keyword 'key'.
//
// on every kvstore and feature-flags route, on both backends. Quoting is
// per-dialect: PostgreSQL and standard SQL use double quotes, MySQL uses
// backticks, SQL Server uses square brackets.
//
// Embedded quote characters are doubled/escaped so that a caller cannot
// inject through an identifier, but identifiers should still come from
// constants rather than user input.
func QuoteIdent(name string, d Dialect) string {
	switch d {
	case DialectMySQL:
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	case DialectMSSQL:
		return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
	default:
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
}

// LimitClause returns a row-limiting clause using the given placeholder.
//
// SQL Server has no LIMIT. It uses OFFSET/FETCH, which is only valid after an
// ORDER BY -- so callers must already be ordering their results, which any
// query with a row limit should be doing anyway to be deterministic.
func LimitClause(placeholder string, d Dialect) string {
	if d == DialectMSSQL {
		return "OFFSET 0 ROWS FETCH NEXT " + placeholder + " ROWS ONLY"
	}
	return "LIMIT " + placeholder
}

// JSONColumn scans a JSON-valued column into json.RawMessage regardless of
// whether the driver delivers it as []byte or as string.
//
// database/sql will convert a driver string into *[]byte, but not into a
// *json.RawMessage: json.RawMessage is a named []byte type and the conversion
// is not in convertAssign's fast path. lib/pq returns jsonb as []byte, so
// scanning straight into json.RawMessage works on PostgreSQL and hides the
// problem; go-mssqldb returns NVARCHAR as string, and every read of a JSON
// column failed with
//
//	sql: Scan error on column index 0, name "value": unsupported Scan,
//	storing driver.Value type string into type *json.RawMessage
//
// That surfaced as HTTP 500 on single-row reads and as silently empty lists,
// because the row-scan error was logged and the row skipped.
type JSONColumn struct {
	Raw json.RawMessage
}

// Scan implements sql.Scanner.
func (j *JSONColumn) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		j.Raw = nil
	case []byte:
		// Copied: the driver may reuse the buffer for the next row.
		j.Raw = append(json.RawMessage(nil), v...)
	case string:
		j.Raw = json.RawMessage(v)
	default:
		return fmt.Errorf("plugin: cannot scan %T into a JSON column", src)
	}
	return nil
}

// Value implements driver.Valuer so the same type can be used for writes.
//
// It yields a string, not []byte, and that is deliberate. go-mssqldb maps a
// []byte argument to VARBINARY; inserting that into the NVARCHAR column that
// backs a JSON value stores the binary representation, which reads back as
// text that is not valid JSON. The symptom is a 200 with an empty body,
// because encoding/json fails part-way through writing the response. A string
// argument is sent as NVARCHAR by go-mssqldb, and lib/pq and go-sql-driver
// both accept a string for jsonb/JSON columns, so one form is correct
// everywhere.
func (j JSONColumn) Value() (driver.Value, error) {
	if len(j.Raw) == 0 {
		return nil, nil
	}
	return string(j.Raw), nil
}
