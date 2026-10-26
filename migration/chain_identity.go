package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Chain identity (cleat#3405, owner decision D18).
//
// schema_migrations was keyed by version number alone, so a database built by
// a different migration chain whose numbers overlap ours looked already
// migrated. v0.4.0 shipped 001-014; 0.5.0's re-baselined chain ships fewer
// files under the same low numbers, so a 0.5.0 worker pointed at a 0.4.0
// database found 001-007 "applied", applied nothing, and started against a
// schema it did not build.
//
// 0.5.0 does not convert such a database: it requires a fresh one (D18). What
// it must do is notice and refuse. So every applied version now records the
// file it came from -- its name and the SHA-256 of its bytes -- and a run
// refuses, before it changes anything, when what the database recorded is not
// what this binary ships.
//
// A recorded version counts as foreign when:
//
//   - the identity columns do not exist and any version is recorded (every
//     database migrated before this change, including every 0.4.0 one);
//   - its name or checksum is NULL;
//   - this binary ships that version under a different name or checksum.
//
// A recorded version this binary does not ship, carrying an identity, is NOT
// foreign. That is the ahead case of cleat#2117: during a rolling upgrade an N
// worker runs against a database the N+1 deploy step already migrated, and it
// cannot verify files it has never seen.
//
// The checksum is over the file's bytes exactly as the runner reads them,
// with no line-ending normalisation. The embedded tree is what ships; a
// --migrations-dir copy whose bytes differ is a different chain, and refusing
// it is the safe answer.

// ForeignChainMarker is in every foreign-chain refusal, at --migrate-only and
// at boot. scripts/upgrade_from_v040.py's REFUSAL_MARKER looks for it, so
// change both together.
const ForeignChainMarker = "foreign migration chain"

// chainIdentity is what a migration file records about itself.
type chainIdentity struct {
	name     string
	checksum string
}

func identityOf(m migration) chainIdentity {
	sum := sha256.Sum256([]byte(m.sql))
	return chainIdentity{name: m.name, checksum: hex.EncodeToString(sum[:])}
}

// recordedVersion is one schema_migrations row as the chain check sees it.
type recordedVersion struct {
	version  string
	name     sql.NullString
	checksum sql.NullString
}

// ChainMismatch is one recorded version that does not belong to this binary's
// chain, with both sides named.
type ChainMismatch struct {
	Version string
	// Recorded is what the database holds for this version: "name (checksum)",
	// or a description of why there is nothing to compare.
	Recorded string
	// Shipped is what this binary carries for this version, or "" when it
	// ships no file with that number.
	Shipped string
}

// ForeignChainError is the refusal. Its message carries ForeignChainMarker,
// both sides of every mismatch, and what to do instead.
type ForeignChainError struct {
	// Where names the database or schema checked, for a refusal that has to
	// say which of several it was.
	Where      string
	Mismatches []ChainMismatch
}

func (e *ForeignChainError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: the migrations recorded in %s were not applied by this binary's chain, "+
		"so this database was built by a different cleat version and cannot be used as is.", ForeignChainMarker, e.Where)
	const shown = 5
	for i, m := range e.Mismatches {
		if i == shown {
			fmt.Fprintf(&b, "\n  ... and %d more", len(e.Mismatches)-shown)
			break
		}
		shipped := m.Shipped
		if shipped == "" {
			shipped = "no file with this number"
		}
		fmt.Fprintf(&b, "\n  version %s: database has %s; this binary ships %s", m.Version, m.Recorded, shipped)
	}
	b.WriteString("\nNothing was changed. cleat 0.5.0 does not convert a database built by an earlier version: " +
		"create a fresh database, run `cleat-worker --migrate-only` against it, and move your data across " +
		"with an export and import. See docs/operations/upgrading.md.")
	return b.String()
}

// IsForeignChain reports whether err is, or wraps, a ForeignChainError.
func IsForeignChain(err error) bool {
	var fc *ForeignChainError
	return errors.As(err, &fc)
}

// CheckChain refuses a database whose recorded migrations are not this
// binary's chain. It changes nothing: no table is created, no lock taken. A
// database with no schema_migrations table is fresh and passes.
func (r *Runner) CheckChain(ctx context.Context) error {
	migrations, err := r.readMigrations()
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	return r.checkChain(ctx, r.db, migrations)
}

func (r *Runner) checkChain(ctx context.Context, q sqlSession, migrations []migration) error {
	exists, err := r.trackingTableExistsOn(ctx, q)
	if err != nil {
		return fmt.Errorf("check for schema_migrations: %w", err)
	}
	if !exists {
		return nil
	}
	hasIdentity, err := r.identityColumnsExist(ctx, q)
	if err != nil {
		return fmt.Errorf("check schema_migrations for identity columns: %w", err)
	}
	rows, err := r.recordedVersions(ctx, q, hasIdentity)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}

	shipped := make(map[string]chainIdentity, len(migrations))
	for _, m := range migrations {
		shipped[strconv.Itoa(m.version)] = identityOf(m)
	}

	var mismatches []ChainMismatch
	for _, row := range rows {
		want, ships := shipped[row.version]
		wantText := ""
		if ships {
			wantText = fmt.Sprintf("%s (%s)", want.name, want.checksum)
		}
		switch {
		case !hasIdentity:
			mismatches = append(mismatches, ChainMismatch{row.version,
				"no identity (schema_migrations has no name or checksum column)", wantText})
		case !row.name.Valid || !row.checksum.Valid:
			mismatches = append(mismatches, ChainMismatch{row.version, "no identity (name or checksum is NULL)", wantText})
		case !ships:
			// Ahead, with an identity: a later chain's file. Allowed.
		case row.name.String != want.name || row.checksum.String != want.checksum:
			mismatches = append(mismatches, ChainMismatch{row.version,
				fmt.Sprintf("%s (%s)", row.name.String, row.checksum.String), wantText})
		}
	}
	if len(mismatches) == 0 {
		return nil
	}
	sort.SliceStable(mismatches, func(i, j int) bool {
		a, aErr := strconv.Atoi(mismatches[i].Version)
		b, bErr := strconv.Atoi(mismatches[j].Version)
		if aErr != nil || bErr != nil {
			return mismatches[i].Version < mismatches[j].Version
		}
		return a < b
	})
	return &ForeignChainError{Where: r.trackingTable(), Mismatches: mismatches}
}

// identityColumnsExist reports whether schema_migrations has both identity
// columns. Asked of the catalog rather than inferred from a failed SELECT, for
// the same reason as trackingTableExists.
func (r *Runner) identityColumnsExist(ctx context.Context, q sqlSession) (bool, error) {
	for _, col := range []string{"name", "checksum"} {
		ok, err := r.trackingColumnExists(ctx, q, col)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// trackingColumnExists reports whether schema_migrations has the column.
func (r *Runner) trackingColumnExists(ctx context.Context, q sqlSession, col string) (bool, error) {
	var query string
	var args []any
	switch r.dialect {
	case DialectPostgres:
		query = `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'schema_migrations' AND column_name = $2`
		args = []any{r.schemaIdent(), col}
	case DialectMySQL:
		query = `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = 'schema_migrations' AND column_name = ?`
		args = []any{col}
	case DialectMSSQL:
		query = `SELECT CASE WHEN COL_LENGTH('schema_migrations', @p1) IS NULL THEN 0 ELSE 1 END`
		args = []any{col}
	default:
		return false, fmt.Errorf("unsupported dialect: %s", r.dialect)
	}
	n, err := queryInt(ctx, q, query, args...)
	return n > 0, err
}

func (r *Runner) recordedVersions(ctx context.Context, q sqlSession, hasIdentity bool) ([]recordedVersion, error) {
	cols := "version, NULL, NULL"
	if hasIdentity {
		cols = "version, name, checksum"
	}
	rows, err := q.QueryContext(ctx, "SELECT "+cols+" FROM "+r.trackingTable())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recordedVersion
	for rows.Next() {
		var v recordedVersion
		if err := rows.Scan(&v.version, &v.name, &v.checksum); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// queryInt runs a query returning one integer, through the QueryContext the
// session interface offers.
func queryInt(ctx context.Context, q sqlSession, query string, args ...any) (int, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, sql.ErrNoRows
	}
	if err := rows.Scan(&n); err != nil {
		return 0, err
	}
	return n, rows.Err()
}
