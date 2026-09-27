package plugin

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"regexp"
	"strconv"
	"strings"
)

var dollarRE = regexp.MustCompile(`\$(\d+)`)
var nowRE = regexp.MustCompile(`(?i)\bnow\s*\(\s*\)`)
var boolRE = regexp.MustCompile(`(?i)\b(true|false)\b`)

// Rebind translates a statement written in the primary dialect (PostgreSQL)
// into the spelling the target dialect accepts:
//
//	$N        ->                   @pN   (SQL Server)
//	now()     ->                   SYSUTCDATETIME()
//	TRUE/FALSE ->                  1 / 0
//
// PostgreSQL is the source dialect, so Rebind is the identity there.
//
// For MySQL, Rebind is ALSO the identity -- it leaves $N
// untouched rather than rewriting it to ?. That is a deliberate change
// (cleat#2259), not an oversight: MySQL's driver binds ? positionally, by
// TEXT occurrence order, so rewriting $N to ? here -- before a caller's args
// slice has been reordered to match -- is exactly the bug this issue closed.
// The rewrite-and-reorder pair now lives together in RebindArgs, which is
// the only thing that can do both without mis-binding a query whose $N
// tokens are not already in ascending text order.
//
// Existing callers that pair Rebind with their own args (the ~150 sites
// that already called Rebind before invoking a PluginDB method) are
// unaffected: their own Rebind call is now a MySQL no-op, and the adapter's
// internal RebindArgs call sees the intact $N text and does the real work.
// Only a caller that bypasses PluginDB and hands Rebind's output straight to
// a raw *sql.DB/*sql.Tx needs to switch to RebindArgs directly -- see
// plugins/plugintest/arms.go and cmd/cleatctl/dialect.go for the pattern.
// A future cleanup (tracked separately, sequenced after WS-2's #2232/#2247)
// will route MSSQL through RebindArgs too and delete the redundant call
// sites along with Rebind itself.
//
// Deliberately NOT a "// Deprecated:" doc comment: that marker tells
// go vet/staticcheck to flag every remaining call, and Rebind is still the
// correct, non-redundant function for MSSQL -- it does real rewriting
// there -- and a legitimate no-op at each of the ~150 pre-PluginDB call
// sites described above, none of which this change asks anyone to touch.
// A blanket deprecation warning across all of them would be exactly the
// kind of "fix" that isn't one: see scripts/check-no-raw-rebind.py for the
// actual, narrowly-scoped rule (bans the RAW-handle and test-file shapes
// only) that replaces what a real Go deprecation marker would have done
// too bluntly here.
//
// MySQL needs no boolean rewrite: TRUE and FALSE are documented aliases for 1
// and 0. T-SQL has no boolean type at all, which is why a bare `enabled = true`
// there is not a syntax error but a COLUMN BINDING error -- `Invalid column
// name 'true'` -- and why `SET PARSEONLY ON` accepts it. That is 22 of the
// sites in cleat#1133 and the reason a parse-only sweep reported them clean.
//
// WHY THIS SCANS RATHER THAN SUBSTITUTING, WHICH IS THE WHOLE POINT.
// The previous implementation ran each regex over the entire statement, and a
// regex cannot tell SQL from a string that appears inside SQL. Measured before
// this change:
//
//	Rebind(`SELECT * FROM t WHERE label = 'costs $100' AND id = $1`, MSSQL)
//	  -> SELECT * FROM t WHERE label = 'costs @p100' AND id = @p1
//	                                            ^^^^ corrupted user-visible data
//
// No shipped plugin has a $N inside a string literal today, so the defect was
// latent rather than live. It does not stay latent: this rewrite is about to be
// applied at the adapter to every plugin statement rather than at the 166 call
// sites that opt in, and adding TRUE/FALSE makes a far commoner token
// rewritable -- `WHERE note = 'set this to true'` is an ordinary thing to
// write.
//
// So the substitutions are applied only to the parts of the statement that are
// SQL. Quoted regions are copied through untouched, in all four spellings the
// three dialects use, plus both comment forms:
//
//	'...'   string literal, '' escapes     -- all dialects
//	"..."   identifier (PG, T-SQL) or string (MySQL); skipped either way
//	`...`   identifier                     -- MySQL
//	[...]   identifier, ]] escapes         -- T-SQL
//	-- ...  line comment
//	/* */   block comment
//
// Rebind is idempotent: @p1 contains no $N, SYSUTCDATETIME() does not match
// now(), and 1 does not match TRUE. That matters because the adapter applies it
// to statements that may already have been rebound by their call site, and it
// is asserted by TestRebindIsIdempotent rather than left as a claim.
func Rebind(query string, d Dialect) string {
	if d != DialectMSSQL {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	walkSQLSpans(query, d, func(span string, quoted bool) {
		if quoted {
			b.WriteString(span)
		} else {
			b.WriteString(rebindSQL(span, d))
		}
	})
	return b.String()
}

// walkSQLSpans calls fn once for each contiguous span of query, in
// left-to-right order, tagging each as quoted (a string/identifier literal
// or a comment, copied through untouched by every caller) or not (plain
// SQL, safe to rewrite). Rebind and rebindArgsMySQL both walk the query
// this same way so their two views of "what is SQL, not string data" cannot
// diverge -- see Rebind's own doc comment for why that distinction exists.
func walkSQLSpans(query string, d Dialect, fn func(span string, quoted bool)) {
	i, plain := 0, 0
	flush := func(upto int) {
		if upto > plain {
			fn(query[plain:upto], false)
		}
	}
	for i < len(query) {
		var end int
		switch {
		case query[i] == '\'':
			end = skipDelimited(query, i, '\'', '\'')
		case query[i] == '"':
			end = skipDelimited(query, i, '"', '"')
		case query[i] == '`':
			end = skipDelimited(query, i, '`', '`')
		case query[i] == '[' && d == DialectMSSQL:
			end = skipDelimited(query, i, '[', ']')
		case strings.HasPrefix(query[i:], "--"):
			end = strings.IndexByte(query[i:], '\n')
			if end < 0 {
				end = len(query)
			} else {
				end += i + 1
			}
		case strings.HasPrefix(query[i:], "/*"):
			end = strings.Index(query[i+2:], "*/")
			if end < 0 {
				end = len(query)
			} else {
				end += i + 4
			}
		default:
			i++
			continue
		}
		flush(i)
		fn(query[i:end], true)
		i, plain = end, end
	}
	flush(len(query))
}

// skipDelimited returns the index just past the region opened at start, where
// the closing delimiter may be escaped by doubling it (” or ]]). An unclosed
// region runs to the end of the statement -- the database will reject it, and
// rewriting its tail would only change which error it reports.
func skipDelimited(s string, start int, open, close byte) int {
	for i := start + 1; i < len(s); i++ {
		if s[i] != close {
			continue
		}
		if i+1 < len(s) && s[i+1] == close && open == close {
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == close && open == '[' {
			i++
			continue
		}
		return i + 1
	}
	return len(s)
}

// rebindSQL applies the substitutions to one span known to be outside every
// quoted region and comment. Only reached for MSSQL -- Rebind is the
// identity for every other dialect, including MySQL; see RebindArgs for
// MySQL's $N -> ? translation, which cannot be done correctly without the
// arg slice alongside it.
func rebindSQL(s string, d Dialect) string {
	if d != DialectMSSQL {
		return s
	}
	s = dollarRE.ReplaceAllString(s, "@p$1")
	s = nowRE.ReplaceAllString(s, "SYSUTCDATETIME()")
	return boolRE.ReplaceAllStringFunc(s, func(m string) string {
		if strings.EqualFold(m, "true") {
			return "1"
		}
		return "0"
	})
}

// RebindArgs translates both a statement and its bound arguments for the
// target dialect. Every caller that carries arguments alongside a query --
// which is the whole of engine/plugindb_adapter.go and engine/readonlydb.go
// -- should use this instead of Rebind.
//
// PostgreSQL and SQL Server bind $N/@pN by NUMBER, wherever it sits in the
// text and however many times it repeats, so the args slice a caller builds
// (args[0] is $1's value, args[1] is $2's value, and so on -- the only
// convention that makes sense to write against the primary dialect) is
// already correct for them, and Rebind alone suffices.
//
// MySQL's driver binds its ? placeholders POSITIONALLY: the Nth ? in the
// rewritten text takes the Nth value in the args slice, regardless of which
// $N used to be there. A naive left-to-right $N -> ? substitution therefore
// silently mis-binds every argument once a query's $N tokens are not
// already in ascending text order -- cleat#2259, found live in
// plugins/jobqueue/background.go's pollPending (cleat#2257). RebindArgs
// closes that for every MySQL call by rewriting $N to ? AND reordering args
// to the placeholders' TEXT occurrence order in the same pass, duplicating
// a value wherever its $N repeats, so the Nth ? and the Nth entry of the
// returned slice always agree.
//
// A caller that already renumbers its $N tokens to ascending text order (as
// the pollPending fix does) or that templates part of the statement so the
// $N that remain are already ascending (as notifications' markRetrying
// does, via fmt.Sprintf) needs no change: for those, placeholder text order
// already equals $N number order, so the reorder RebindArgs computes is the
// identity permutation.
//
// RebindArgs fails closed rather than mis-binding silently a second way:
//
//   - a query that mixes literal ? and $N is rejected. The two binding
//     conventions cannot be reconciled by reordering -- a ? already sits at
//     a fixed position no $N describes, so there is no permutation of args
//     that is correct for both.
//   - an arg with no $N referencing it is rejected. A caller with more args
//     than placeholders has miscounted one or the other, and passing the
//     extra value to the driver anyway would either be silently dropped or,
//     on a query that gains a placeholder later, silently bound to the
//     wrong one.
//
// Both are caller bugs that would already misbehave on PostgreSQL, the
// primary dialect that every plugin is written and tested against first --
// so a MySQL-only error here should not be reachable by any shipped query;
// it exists to fail loudly the first time one is written, not because one
// is expected.
func RebindArgs(query string, d Dialect, args []any) (string, []any, error) {
	if d != DialectMySQL {
		return Rebind(query, d), args, nil
	}
	return rebindArgsMySQL(query, args)
}

// rebindArgsMySQL rewrites $N to ? and reorders args to match, in one walk
// over query's plain (non-quoted, non-comment) spans -- see walkSQLSpans.
// Doing both in one pass, rather than computing placeholder order and then
// separately substituting, is what lets it also detect a bare ? sitting
// alongside a $N: by the time a second pass ran dollarRE over the text, the
// ? would already be indistinguishable from one RebindArgs itself produced.
func rebindArgsMySQL(query string, args []any) (string, []any, error) {
	var b strings.Builder
	b.Grow(len(query) + 16)
	var order []int
	sawBareQMark := false
	walkSQLSpans(query, DialectMySQL, func(span string, quoted bool) {
		if quoted {
			b.WriteString(span)
			return
		}
		if strings.ContainsRune(span, '?') {
			sawBareQMark = true
		}
		last := 0
		for _, loc := range dollarRE.FindAllStringIndex(span, -1) {
			b.WriteString(span[last:loc[0]])
			if n, err := strconv.Atoi(span[loc[0]+1 : loc[1]]); err == nil {
				order = append(order, n)
			}
			b.WriteByte('?')
			last = loc[1]
		}
		b.WriteString(span[last:])
	})
	rebound := b.String()

	if sawBareQMark {
		if order != nil {
			return "", nil, fmt.Errorf("plugin: RebindArgs: query mixes literal ? with $N placeholders, cannot determine MySQL binding order: %s", query)
		}
		// Already hand-written for MySQL (or has no placeholders at all):
		// nothing for RebindArgs to reorder, and no way to validate an arg
		// count without parsing the ?s, which is outside this function's
		// job. Pass through unchanged, same as Rebind always has.
		return rebound, args, nil
	}
	if order == nil {
		if len(args) != 0 {
			return "", nil, fmt.Errorf("plugin: RebindArgs: %d arg(s) given but query has no $N placeholders: %s", len(args), query)
		}
		return rebound, args, nil
	}

	reordered := make([]any, len(order))
	referenced := make([]bool, len(args))
	for i, n := range order {
		if n < 1 || n > len(args) {
			return "", nil, fmt.Errorf("plugin: RebindArgs: query references $%d but only %d arg(s) given: %s", n, len(args), query)
		}
		reordered[i] = args[n-1]
		referenced[n-1] = true
	}
	for i, ok := range referenced {
		if !ok {
			return "", nil, fmt.Errorf("plugin: RebindArgs: arg %d ($%d) is never referenced by the query: %s", i, i+1, query)
		}
	}
	return rebound, reordered, nil
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

// GUID scans a UUID-valued column into a uuid.UUID on any dialect.
//
// SQL Server's UNIQUEIDENTIFIER is not stored or transmitted in RFC 4122 byte
// order. Microsoft's GUID encoding is mixed-endian: the first three groups are
// little-endian and the last two big-endian. go-mssqldb hands those 16 bytes
// over unchanged, and uuid.UUID's own Scan accepts any 16-byte slice as-is --
// so scanning succeeds, reports no error, and yields a DIFFERENT id.
//
// Measured against SQL Server 2022: a row whose id the server prints as
// CAFBE5D6-8D74-4215-9908-9E01D7AE2654 scans into uuid.UUID as
// d6e5fbca-748d-1542-9908-9e01d7ae2654 -- each of the first three groups
// reversed, the last two intact.
//
// The consequence is silent rather than loud, which is why this survived: the
// corrupted id is well-formed, so a following `WHERE id = ?` is valid SQL that
// simply matches no row. The UPDATE reports success, having changed nothing.
// See cleat#1133, where it left every claimed schedule unadvanced on SQL Server
// with nothing in the log.
//
// Writes need no equivalent: uuid.UUID's Value returns the text form, which
// SQL Server converts correctly.
//
// The discriminator is a 16-byte slice, which only SQL Server produces here:
// this repo's UUID columns are UUID on PostgreSQL (lib/pq delivers the 36-byte
// text form) and CHAR(36) on MySQL. A BINARY(16) column on either would need
// its own handling rather than this one.
type GUID struct {
	uuid.UUID
}

func (g *GUID) Scan(src any) error {
	b, ok := src.([]byte)
	if !ok || len(b) != 16 {
		// Text forms, and PostgreSQL/MySQL's own representations, are already
		// correct -- defer to the standard behaviour.
		return g.UUID.Scan(src)
	}
	swapped := make([]byte, 16)
	copy(swapped, b)
	swapped[0], swapped[1], swapped[2], swapped[3] = b[3], b[2], b[1], b[0]
	swapped[4], swapped[5] = b[5], b[4]
	swapped[6], swapped[7] = b[7], b[6]
	return g.UUID.Scan(swapped)
}

// ScanRow scans a database row, correcting SQL Server's UNIQUEIDENTIFIER byte
// order for any destination that is a *uuid.UUID.
//
// cleat#1137: SQL Server returns UNIQUEIDENTIFIER in mixed-endian byte order.
// uuid.UUID's own Scan accepts those 16 bytes WITHOUT ERROR and yields a
// different uuid — so every plugin reading an id from SQL Server got the wrong
// one, silently, and nothing failed. GUID (above) exists to swap them.
//
// WHY A WRAPPER RATHER THAN 85 EDITS. The type checker found 85 Scan arguments
// across 15 plugins resolving to *uuid.UUID. Fixing each by hand means 85
// chances to get it wrong on paths no test exercises, and leaves the next
// author free to write the 86th. A backlog of near-identical findings is
// usually one missing abstraction; this is it. The call-site change is
// mechanical and uniform:
//
//	rows.Scan(&s.id, &s.tenantID, &s.name)
//	plugin.ScanRow(rows, &s.id, &s.tenantID, &s.name)
//
// and the byte-order knowledge lives in one tested place instead of being
// restated per site.
//
// Destinations that are not *uuid.UUID are passed through untouched, so this is
// safe to apply to a whole Scan call rather than to selected arguments — which
// matters, because deciding per-argument is what a reader gets wrong.
func ScanRow(s interface{ Scan(...any) error }, dest ...any) error {
	// Substitute in place, remembering which slots to copy back. The common
	// case has no uuid at all and allocates nothing.
	var swapped map[int]*GUID
	args := dest
	for i, d := range dest {
		u, ok := d.(*uuid.UUID)
		if !ok {
			continue
		}
		if swapped == nil {
			swapped = make(map[int]*GUID, len(dest))
			args = make([]any, len(dest))
			copy(args, dest)
		}
		g := &GUID{}
		swapped[i] = g
		args[i] = g
		_ = u
	}
	if err := s.Scan(args...); err != nil {
		return err
	}
	// Copy back only on success: a failed Scan leaves destinations untouched
	// under database/sql, and this must not differ from that.
	for i, g := range swapped {
		*(dest[i].(*uuid.UUID)) = g.UUID
	}
	return nil
}
