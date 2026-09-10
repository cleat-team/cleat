package plugin

import "testing"

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
			name:    "mysql replaces one param",
			query:   "SELECT * FROM users WHERE id = $1",
			dialect: DialectMySQL,
			want:    "SELECT * FROM users WHERE id = ?",
		},
		{
			name:    "mysql replaces multiple params",
			query:   "SELECT * FROM users WHERE id = $1 AND name = $2",
			dialect: DialectMySQL,
			want:    "SELECT * FROM users WHERE id = ? AND name = ?",
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
