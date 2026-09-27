package plugin

import (
	"reflect"
	"strings"
	"testing"
)

func TestRebind(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		dialect Dialect
		want    string
	}{
		{
			name:    "postgres passthrough",
			query:   "SELECT * FROM users WHERE id = $1",
			dialect: DialectPostgres,
			want:    "SELECT * FROM users WHERE id = $1",
		},
		{
			name:    "postgres multiple params",
			query:   "SELECT * FROM users WHERE id = $1 AND name = $2",
			dialect: DialectPostgres,
			want:    "SELECT * FROM users WHERE id = $1 AND name = $2",
		},
		{
			name:    "postgres no params",
			query:   "SELECT * FROM users",
			dialect: DialectPostgres,
			want:    "SELECT * FROM users",
		},
		{
			// Rebind is the identity for MySQL (cleat#2259): rewriting $N to
			// ? here, before an args slice has been reordered to match text
			// occurrence order, is exactly the bug this issue closed. See
			// RebindArgs, which does both together.
			name:    "mysql: $N is left untouched -- see RebindArgs",
			query:   "SELECT * FROM users WHERE id = $1",
			dialect: DialectMySQL,
			want:    "SELECT * FROM users WHERE id = $1",
		},
		{
			name:    "mysql: multiple $N are left untouched -- see RebindArgs",
			query:   "SELECT * FROM users WHERE id = $1 AND name = $2",
			dialect: DialectMySQL,
			want:    "SELECT * FROM users WHERE id = $1 AND name = $2",
		},
		{
			name:    "mysql no params",
			query:   "SELECT * FROM users",
			dialect: DialectMySQL,
			want:    "SELECT * FROM users",
		},
		{
			name:    "mssql replaces one param",
			query:   "SELECT * FROM users WHERE id = $1",
			dialect: DialectMSSQL,
			want:    "SELECT * FROM users WHERE id = @p1",
		},
		{
			name:    "mssql replaces multiple params",
			query:   "SELECT * FROM users WHERE id = $1 AND name = $2",
			dialect: DialectMSSQL,
			want:    "SELECT * FROM users WHERE id = @p1 AND name = @p2",
		},
		{
			name:    "mssql replaces now()",
			query:   "SELECT * FROM orders WHERE created_at < now()",
			dialect: DialectMSSQL,
			want:    "SELECT * FROM orders WHERE created_at < SYSUTCDATETIME()",
		},
		{
			name:    "mssql mixed params and now()",
			query:   "SELECT * FROM orders WHERE created_at > now() AND user_id = $1",
			dialect: DialectMSSQL,
			want:    "SELECT * FROM orders WHERE created_at > SYSUTCDATETIME() AND user_id = @p1",
		},
		{
			name:    "mssql no params",
			query:   "SELECT * FROM users",
			dialect: DialectMSSQL,
			want:    "SELECT * FROM users",
		},
		{
			name:    "unknown dialect fallback",
			query:   "SELECT * FROM users WHERE id = $1",
			dialect: Dialect("unknown"),
			want:    "SELECT * FROM users WHERE id = $1",
		},
		{
			name:    "empty dialect fallback",
			query:   "SELECT * FROM users WHERE id = $1",
			dialect: Dialect(""),
			want:    "SELECT * FROM users WHERE id = $1",
		},
		{
			name:    "mssql uppercase NOW()",
			query:   "SELECT * FROM orders WHERE created_at < NOW()",
			dialect: DialectMSSQL,
			want:    "SELECT * FROM orders WHERE created_at < SYSUTCDATETIME()",
		},
		{
			// This case's NAME has always been right and its expectation was
			// always wrong. "$1 not a param" is exactly the point -- it is
			// inside a string literal, so it is a price, not a placeholder --
			// and the want then asserted that Rebind rewrites it anyway. The
			// expectation was captured from what the implementation did rather
			// than derived from what the name says should happen, so it locked
			// the defect in and the name went on describing the fix.
			//
			// Corrected with the literal-aware scanner (cleat#1133).
			name:    "mysql with dollar sign not a param",
			query:   "SELECT '$1' as price FROM users",
			dialect: DialectMySQL,
			want:    "SELECT '$1' as price FROM users",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Rebind(tt.query, tt.dialect)
			if got != tt.want {
				t.Errorf("Rebind(%q, %q) = %q, want %q", tt.query, tt.dialect, got, tt.want)
			}
		})
	}
}

// TestRebindArgs pins cleat#2259: MySQL binds ? placeholders by TEXT
// occurrence order, not by the $N number that was there before rewriting,
// so a caller with non-ascending $N tokens needs its args reordered (and,
// for a repeated $N, duplicated) to match -- see RebindArgs's doc comment.
func TestRebindArgs(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		dialect   Dialect
		args      []any
		wantQuery string
		wantArgs  []any
	}{
		{
			name:      "postgres: args pass through unchanged regardless of $N order",
			query:     "UPDATE t SET b = $2, a = $1 WHERE id = $3",
			dialect:   DialectPostgres,
			args:      []any{"a-val", "b-val", "id-val"},
			wantQuery: "UPDATE t SET b = $2, a = $1 WHERE id = $3",
			wantArgs:  []any{"a-val", "b-val", "id-val"},
		},
		{
			name:      "mssql: args pass through unchanged regardless of $N order",
			query:     "UPDATE t SET b = $2, a = $1 WHERE id = $3",
			dialect:   DialectMSSQL,
			args:      []any{"a-val", "b-val", "id-val"},
			wantQuery: "UPDATE t SET b = @p2, a = @p1 WHERE id = @p3",
			wantArgs:  []any{"a-val", "b-val", "id-val"},
		},
		{
			name:      "mysql: ascending $N needs no reorder",
			query:     "UPDATE t SET a = $1, b = $2 WHERE id = $3",
			dialect:   DialectMySQL,
			args:      []any{"a-val", "b-val", "id-val"},
			wantQuery: "UPDATE t SET a = ?, b = ? WHERE id = ?",
			wantArgs:  []any{"a-val", "b-val", "id-val"},
		},
		{
			name: "mysql: non-ascending $N -- the pollPending shape (cleat#2257)",
			// SET run_id = $1 comes before the WHERE clause's $2-$4 in the
			// text, but $1's ARG is the LAST one a caller naturally writes
			// (run_id, then the WHERE columns in order). RebindArgs must
			// deliver run_id's value to the FIRST ? and the WHERE values to
			// the following three, in text order -- not args[0..3] as written.
			query:     "UPDATE task_queue SET status = 'dispatched', run_id = $4 WHERE job_id = $1 AND tenant_id = $2 AND queue_name = $3",
			dialect:   DialectMySQL,
			args:      []any{"job-1", "tenant-1", "queue-1", "run-1"},
			wantQuery: "UPDATE task_queue SET status = 'dispatched', run_id = ? WHERE job_id = ? AND tenant_id = ? AND queue_name = ?",
			wantArgs:  []any{"run-1", "job-1", "tenant-1", "queue-1"},
		},
		{
			name: "mysql: a reused $N is duplicated once per occurrence",
			// $1 (eventID) appears three times: once in each of the two
			// subquery WHERE clauses and once wouldn't otherwise recur --
			// each occurrence needs its own copy of the arg, positionally.
			query:     "SELECT max_retries FROM t WHERE tenant_id = (SELECT tenant_id FROM u WHERE id = $1) AND event_type = (SELECT event_type FROM u WHERE id = $1)",
			dialect:   DialectMySQL,
			args:      []any{"event-1"},
			wantQuery: "SELECT max_retries FROM t WHERE tenant_id = (SELECT tenant_id FROM u WHERE id = ?) AND event_type = (SELECT event_type FROM u WHERE id = ?)",
			wantArgs:  []any{"event-1", "event-1"},
		},
		{
			// Only one arg: $2 inside the string literal is text, not a
			// placeholder, so it must not be counted when checking every
			// arg is referenced -- a second arg here would (correctly) be
			// rejected by the unreferenced-arg check below.
			name:      "mysql: a $N inside a string literal is not a placeholder and is left untouched",
			query:     "SELECT * FROM t WHERE label = 'costs $2 more than $1' AND id = $1",
			dialect:   DialectMySQL,
			args:      []any{"id-val"},
			wantQuery: "SELECT * FROM t WHERE label = 'costs $2 more than $1' AND id = ?",
			wantArgs:  []any{"id-val"},
		},
		{
			name:      "mysql: no placeholders at all",
			query:     "DELETE FROM t WHERE created_at < NOW()",
			dialect:   DialectMySQL,
			args:      nil,
			wantQuery: "DELETE FROM t WHERE created_at < NOW()",
			wantArgs:  nil,
		},
		{
			// $10 must parse as ten, not as $1 followed by a literal "0" --
			// dollarRE's \d+ is greedy, but this is the case that would
			// expose a regression to a non-greedy or single-digit pattern.
			name:      "mysql: a two-digit $N is parsed whole, not as $1 followed by 0",
			query:     "SELECT * FROM t WHERE c1=$1 AND c2=$2 AND c3=$3 AND c4=$4 AND c5=$5 AND c6=$6 AND c7=$7 AND c8=$8 AND c9=$9 AND c10=$10",
			dialect:   DialectMySQL,
			args:      []any{"v1", "v2", "v3", "v4", "v5", "v6", "v7", "v8", "v9", "v10"},
			wantQuery: "SELECT * FROM t WHERE c1=? AND c2=? AND c3=? AND c4=? AND c5=? AND c6=? AND c7=? AND c8=? AND c9=? AND c10=?",
			wantArgs:  []any{"v1", "v2", "v3", "v4", "v5", "v6", "v7", "v8", "v9", "v10"},
		},
		{
			// A caller that already writes MySQL's own ? form (no $N at
			// all) is passed through untouched, args included -- the same
			// no-op Rebind alone used to be for it.
			name:      "mysql: a hand-written ? query passes through with args untouched",
			query:     "SELECT * FROM t WHERE a = ? AND b = ?",
			dialect:   DialectMySQL,
			args:      []any{"a-val", "b-val"},
			wantQuery: "SELECT * FROM t WHERE a = ? AND b = ?",
			wantArgs:  []any{"a-val", "b-val"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotQuery, gotArgs, err := RebindArgs(tt.query, tt.dialect, tt.args)
			if err != nil {
				t.Fatalf("RebindArgs(%q, %q) unexpected error: %v", tt.query, tt.dialect, err)
			}
			if gotQuery != tt.wantQuery {
				t.Errorf("RebindArgs(%q, %q) query = %q, want %q", tt.query, tt.dialect, gotQuery, tt.wantQuery)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Errorf("RebindArgs(%q, %q) args = %#v, want %#v", tt.query, tt.dialect, gotArgs, tt.wantArgs)
			}
		})
	}
}

// TestRebindArgsRejectsAmbiguousBinding covers the two shapes RebindArgs
// fails closed on rather than mis-binding silently, per RebindArgs's doc
// comment: a query that mixes literal ? with $N (no permutation of args is
// correct for both conventions at once), and an arg no $N references (a
// caller has miscounted one or the other).
func TestRebindArgsRejectsAmbiguousBinding(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		args    []any
		wantErr string
	}{
		{
			name:    "a literal ? alongside a $N",
			query:   "SELECT * FROM t WHERE a = ? AND b = $1",
			args:    []any{"b-val"},
			wantErr: "mixes literal ? with $N",
		},
		{
			name:    "an arg no $N references",
			query:   "SELECT * FROM t WHERE id = $1",
			args:    []any{"id-val", "extra-val"},
			wantErr: "never referenced",
		},
		{
			name:    "an arg no $N references, with no $N at all",
			query:   "SELECT * FROM t",
			args:    []any{"extra-val"},
			wantErr: "no $N placeholders",
		},
		{
			name:    "a $N with no matching arg",
			query:   "SELECT * FROM t WHERE id = $1 AND name = $2",
			args:    []any{"id-val"},
			wantErr: "only 1 arg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := RebindArgs(tt.query, DialectMySQL, tt.args)
			if err == nil {
				t.Fatalf("RebindArgs(%q, mysql, %#v) = nil error, want one containing %q", tt.query, tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("RebindArgs(%q, mysql, %#v) error = %q, want it to contain %q", tt.query, tt.args, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestQueryFor(t *testing.T) {
	tests := []struct {
		name    string
		query   Query
		dialect Dialect
		want    string
	}{
		{
			name: "mysql override returns mysql variant",
			query: Query{
				Default: "SELECT * FROM users WHERE id = $1",
				MySQL:   "SELECT * FROM users WHERE id = ?",
			},
			dialect: DialectMySQL,
			want:    "SELECT * FROM users WHERE id = ?",
		},
		{
			name: "mysql override returns mysql variant with now",
			query: Query{
				Default: "SELECT * FROM orders WHERE created_at < now()",
				MySQL:   "SELECT * FROM orders WHERE created_at < ?",
			},
			dialect: DialectMySQL,
			want:    "SELECT * FROM orders WHERE created_at < ?",
		},
		{
			name: "mssql override returns mssql variant",
			query: Query{
				Default: "SELECT * FROM users WHERE id = $1",
				MSSQL:   "SELECT * FROM users WHERE id = @p1",
			},
			dialect: DialectMSSQL,
			want:    "SELECT * FROM users WHERE id = @p1",
		},
		{
			name: "mssql override with now()",
			query: Query{
				Default: "SELECT * FROM orders WHERE created_at < now()",
				MSSQL:   "SELECT * FROM orders WHERE created_at < SYSUTCDATETIME()",
			},
			dialect: DialectMSSQL,
			want:    "SELECT * FROM orders WHERE created_at < SYSUTCDATETIME()",
		},
		{
			name: "no mysql override falls back to default",
			query: Query{
				Default: "SELECT * FROM users WHERE id = $1",
			},
			dialect: DialectMySQL,
			want:    "SELECT * FROM users WHERE id = $1",
		},
		{
			name: "no mssql override falls back to default",
			query: Query{
				Default: "SELECT * FROM users WHERE id = $1",
			},
			dialect: DialectMSSQL,
			want:    "SELECT * FROM users WHERE id = $1",
		},
		{
			name: "postgres returns default",
			query: Query{
				Default: "SELECT * FROM users WHERE id = $1",
			},
			dialect: DialectPostgres,
			want:    "SELECT * FROM users WHERE id = $1",
		},
		{
			name: "unknown dialect returns default",
			query: Query{
				Default: "SELECT * FROM users WHERE id = $1",
			},
			dialect: Dialect("unknown"),
			want:    "SELECT * FROM users WHERE id = $1",
		},
		{
			name: "empty dialect returns default",
			query: Query{
				Default: "SELECT * FROM users WHERE id = $1",
			},
			dialect: Dialect(""),
			want:    "SELECT * FROM users WHERE id = $1",
		},
		{
			name: "mssql empty string override falls back",
			query: Query{
				Default: "SELECT * FROM users",
				MSSQL:   "",
			},
			dialect: DialectMSSQL,
			want:    "SELECT * FROM users",
		},
		{
			name: "mysql empty string override falls back",
			query: Query{
				Default: "SELECT * FROM users",
				MySQL:   "",
			},
			dialect: DialectMySQL,
			want:    "SELECT * FROM users",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.query.For(tt.dialect)
			if got != tt.want {
				t.Errorf("Query.For(%q) = %q, want %q", tt.dialect, got, tt.want)
			}
		})
	}
}
