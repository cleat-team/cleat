package engine

import (
	"strings"
	"testing"
)

// TestEnablingPayloadEncryptionIsRefusedOnADialectThatCannotSeal pins cleat#3906.
//
// MySQL and SQL Server cannot seal payloads, and a store that says it can is
// only HALF sealed: StartChildWorkflowAtomic routes its event through
// encodeEventForStorage with encryptSensitivePayloads
// (engine/mysql_store.go, engine/mssql_signals_promises.go), so it would seal
// a child's start event while AppendEventHistoryBatch keeps every ordinary
// event beside it plaintext -- and their populateFromPayload reads do not
// decrypt, so the sealed row is silent field loss rather than an error.
//
// The PostgreSQL arm is the positive control. Without it, a setter that
// panics on every dialect, or a helper that recovers everything, would pass.
func TestEnablingPayloadEncryptionIsRefusedOnADialectThatCannotSeal(t *testing.T) {
	enc := &PayloadEncryption{}

	tests := []struct {
		dialect string
		enable  func()
	}{
		{"MySQL", func() { NewMySQLStore(nil).WithEncryption(enc, true) }},
		{"SQL Server", func() { NewMSSQLStore(nil).WithEncryption(enc, true) }},
	}
	for _, tt := range tests {
		t.Run(tt.dialect, func(t *testing.T) {
			assertEnablingPayloadEncryptionIsRefused(t, tt.dialect, tt.enable)
		})
	}

	// Positive control: the identical call on the dialect that CAN seal must
	// not panic, and must actually record what it was given.
	t.Run("postgres accepts it", func(t *testing.T) {
		store := NewPostgresStore(nil).WithEncryption(enc, true)
		if !store.encryptSensitivePayloads {
			t.Fatal("PostgreSQL accepted WithEncryption(enc, true) but did not enable encryption")
		}
		if store.encryption != enc {
			t.Fatal("PostgreSQL accepted WithEncryption(enc, true) but did not store the encryption")
		}
	})

	// And the disabled form still works on both dialects, so an embedder that
	// passes the option through unchanged (enabled=false) is not broken.
	t.Run("disabled form is unchanged", func(t *testing.T) {
		mysql := NewMySQLStore(nil).WithEncryption(enc, false)
		if mysql.encryptSensitivePayloads {
			t.Fatal("MySQL: disabled form enabled encryption")
		}
		if mysql.encryption != enc {
			t.Fatal("MySQL: disabled form did not store the encryption")
		}
		mssql := NewMSSQLStore(nil).WithEncryption(enc, false)
		if mssql.encryptSensitivePayloads {
			t.Fatal("SQL Server: disabled form enabled encryption")
		}
		if mssql.encryption != enc {
			t.Fatal("SQL Server: disabled form did not store the encryption")
		}
	})
}

// assertEnablingPayloadEncryptionIsRefused fails unless fn panics with a
// message naming the dialect and PostgreSQL -- the dialect that can seal.
// Shared with the sealed arms of the cross-dialect store tests, which used to
// enable fake encryption on MySQL and SQL Server (cleat#3906).
func assertEnablingPayloadEncryptionIsRefused(t *testing.T, dialect string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("%s accepted WithEncryption(enc, true); it must refuse, because a store that believes it can seal there is only half sealed", dialect)
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("%s panicked with %T, want a string naming the dialect", dialect, r)
		}
		if !strings.Contains(msg, dialect) {
			t.Errorf("%s panic message does not name the dialect: %q", dialect, msg)
		}
		if !strings.Contains(msg, "PostgreSQL") {
			t.Errorf("%s panic message does not name the dialect that can seal: %q", dialect, msg)
		}
	}()
	fn()
}
