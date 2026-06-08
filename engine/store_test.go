package engine

import "testing"

func TestDialectConstants(t *testing.T) {
	tests := []struct {
		d    Dialect
		want string
	}{
		{DialectPostgres, "postgres"},
		{DialectMySQL, "mysql"},
		{DialectMSSQL, "mssql"},
	}
	for _, tt := range tests {
		if string(tt.d) != tt.want {
			t.Errorf("Dialect = %q, want %q", tt.d, tt.want)
		}
	}
}

func TestPostgresStoreFactoryImplementsStoreFactory(t *testing.T) {
	var _ StoreFactory = (*PostgresStoreFactory)(nil)
}
