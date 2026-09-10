package plugin

import (
	"testing"

	"github.com/google/uuid"
)

// The mapping, with no database involved. The values are the ones measured
// against SQL Server 2022 in cleat#1137: a row the server prints as
// CAFBE5D6-8D74-4215-9908-9E01D7AE2654 arrives from go-mssqldb as these 16
// bytes, and uuid.UUID's own Scan reads them as d6e5fbca-748d-1542-... --
// well-formed, no error, and a different id.
func TestAUUIDFromSQLServerSurvivesTheByteOrder(t *testing.T) {
	// UNIQUEIDENTIFIER wire bytes: first three groups little-endian.
	wire := []byte{
		0xd6, 0xe5, 0xfb, 0xca, // CAFBE5D6 reversed
		0x74, 0x8d, // 8D74 reversed
		0x15, 0x42, // 4215 reversed
		0x99, 0x08, 0x9e, 0x01, 0xd7, 0xae, 0x26, 0x54, // intact
	}
	const want = "cafbe5d6-8d74-4215-9908-9e01d7ae2654"

	var g GUID
	if err := g.Scan(wire); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := g.UUID.String(); got != want {
		t.Errorf("GUID.Scan gave %s, want %s", got, want)
	}

	// The control that gives the assertion its meaning: the plain type accepts
	// the same bytes without complaint and produces the wrong id. If this ever
	// stops holding, the swap above is no longer needed and this file should go.
	var plain uuid.UUID
	if err := plain.Scan(wire); err != nil {
		t.Fatalf("uuid.UUID.Scan unexpectedly failed: %v", err)
	}
	if plain.String() == want {
		t.Errorf("uuid.UUID.Scan now handles SQL Server's byte order itself (%s); GUID is obsolete", plain)
	}
}

// Text forms must pass through untouched: PostgreSQL and MySQL deliver those,
// and swapping them would corrupt the dialects that were already correct.
func TestAUUIDFromTheOtherDialectsIsUnchanged(t *testing.T) {
	const want = "cafbe5d6-8d74-4215-9908-9e01d7ae2654"
	for _, src := range []any{want, []byte(want)} {
		var g GUID
		if err := g.Scan(src); err != nil {
			t.Fatalf("scan %T: %v", src, err)
		}
		if got := g.UUID.String(); got != want {
			t.Errorf("scanning %T gave %s, want %s", src, got, want)
		}
	}
}
