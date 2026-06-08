package engine

import (
	"database/sql"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// PostgresStoreFactory tests
// ---------------------------------------------------------------------------

func TestNewPostgresStoreFactory_Defaults(t *testing.T) {
	db := &sql.DB{}
	f := NewPostgresStoreFactory(db, "")
	if f.schemaName != "public" {
		t.Errorf("empty schema: got %q, want %q", f.schemaName, "public")
	}
	if f.idempotencyKeyTTL != 720*time.Hour {
		t.Errorf("default TTL: got %v, want %v", f.idempotencyKeyTTL, 720*time.Hour)
	}
	if f.db != db {
		t.Error("db not set")
	}
}

func TestNewPostgresStoreFactory_CustomSchema(t *testing.T) {
	db := &sql.DB{}
	f := NewPostgresStoreFactory(db, "custom_schema")
	if f.schemaName != "custom_schema" {
		t.Errorf("custom schema: got %q, want %q", f.schemaName, "custom_schema")
	}
}

func TestNewPostgresStoreFactory_CustomTTL(t *testing.T) {
	db := &sql.DB{}
	customTTL := 24 * time.Hour
	f := NewPostgresStoreFactory(db, "", customTTL)
	if f.idempotencyKeyTTL != customTTL {
		t.Errorf("custom TTL: got %v, want %v", f.idempotencyKeyTTL, customTTL)
	}
}

func TestNewPostgresStoreFactory_CustomSchemaAndTTL(t *testing.T) {
	db := &sql.DB{}
	customTTL := 1 * time.Hour
	f := NewPostgresStoreFactory(db, "s", customTTL)
	if f.schemaName != "s" {
		t.Errorf("schema: got %q", f.schemaName)
	}
	if f.idempotencyKeyTTL != customTTL {
		t.Errorf("TTL: got %v", f.idempotencyKeyTTL)
	}
}

func TestNewPostgresStoreFactory_NilDB(t *testing.T) {
	f := NewPostgresStoreFactory(nil, "s")
	if f.db != nil {
		t.Error("nil db should be preserved")
	}
}

func TestPostgresStoreFactory_WithEncryption(t *testing.T) {
	db := &sql.DB{}
	f := NewPostgresStoreFactory(db, "")
	enc := &PayloadEncryption{}
	result := f.WithEncryption(enc, true)
	if result != f {
		t.Error("WithEncryption should return self for chaining")
	}
	if f.encryption != enc {
		t.Error("encryption not set")
	}
	if !f.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be true")
	}
}

func TestPostgresStoreFactory_WithEncryption_Disabled(t *testing.T) {
	db := &sql.DB{}
	f := NewPostgresStoreFactory(db, "")
	enc := &PayloadEncryption{}
	f.WithEncryption(enc, false)
	if f.encryption != enc {
		t.Error("encryption not set")
	}
	if f.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be false")
	}
}

func TestPostgresStoreFactory_DriverName(t *testing.T) {
	db := &sql.DB{}
	f := NewPostgresStoreFactory(db, "")
	if got := f.DriverName(); got != "postgres" {
		t.Errorf("DriverName: got %q, want %q", got, "postgres")
	}
}

func TestPostgresStoreFactory_Dialect(t *testing.T) {
	db := &sql.DB{}
	f := NewPostgresStoreFactory(db, "")
	if got := f.Dialect(); got != DialectPostgres {
		t.Errorf("Dialect: got %v, want %v", got, DialectPostgres)
	}
}

// ---------------------------------------------------------------------------
// PostgresStore option methods
// ---------------------------------------------------------------------------

func TestPostgresStore_WithIdempotencyKeyTTL(t *testing.T) {
	db := &sql.DB{}
	s := NewPostgresStore(db)
	want := 10 * time.Minute
	result := s.WithIdempotencyKeyTTL(want)
	if result == s {
		t.Error("WithIdempotencyKeyTTL should return a copy, not the same pointer")
	}
	if result.idempotencyKeyTTL != want {
		t.Errorf("TTL: got %v, want %v", result.idempotencyKeyTTL, want)
	}
	if s.idempotencyKeyTTL == want {
		t.Error("original store should not be modified")
	}
}

func TestPostgresStore_WithEncryption(t *testing.T) {
	db := &sql.DB{}
	s := NewPostgresStore(db)
	enc := &PayloadEncryption{}
	result := s.WithEncryption(enc, true)
	if result == s {
		t.Error("WithEncryption should return a copy")
	}
	if result.encryption != enc {
		t.Error("encryption not set on copy")
	}
	if !result.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be true on copy")
	}
	if s.encryption != nil {
		t.Error("original store encryption should be nil")
	}
}

func TestPostgresStore_WithEncryption_Disabled(t *testing.T) {
	db := &sql.DB{}
	s := NewPostgresStore(db)
	enc := &PayloadEncryption{}
	result := s.WithEncryption(enc, false)
	if result.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be false")
	}
}

func TestPostgresStore_WithReadRedactionDisabled(t *testing.T) {
	db := &sql.DB{}
	s := NewPostgresStore(db)
	result := s.WithReadRedactionDisabled(true)
	if result == s {
		t.Error("WithReadRedactionDisabled should return a copy")
	}
	if !result.disableReadRedaction {
		t.Error("disableReadRedaction should be true on copy")
	}
	if s.disableReadRedaction {
		t.Error("original should not be modified")
	}
}

func TestPostgresStore_optionChaining(t *testing.T) {
	db := &sql.DB{}
	s := NewPostgresStore(db)
	enc := &PayloadEncryption{}
	ttl := 5 * time.Minute

	result := s.WithEncryption(enc, true).WithIdempotencyKeyTTL(ttl).WithReadRedactionDisabled(true)

	if result.encryption != enc {
		t.Error("encryption not set through chain")
	}
	if !result.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads not set through chain")
	}
	if result.idempotencyKeyTTL != ttl {
		t.Errorf("TTL not set through chain: got %v", result.idempotencyKeyTTL)
	}
	if !result.disableReadRedaction {
		t.Error("disableReadRedaction not set through chain")
	}
	if s.encryption != nil || s.idempotencyKeyTTL == ttl || s.disableReadRedaction {
		t.Error("chaining should not modify original store")
	}
}
