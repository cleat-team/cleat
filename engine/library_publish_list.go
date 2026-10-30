package engine

// cleat#3597: reading what is published. Shared by publish (to compare an existing bundle
// with the one supplied) and by ListLibraryBundles.

import (
	"context"
	"database/sql"
	"fmt"
)

// storedBundleListing returns name -> version for one bundle, through the publish transaction.
func storedBundleListing(ctx context.Context, tx *sql.Tx, d Dialect, library, bundleVersion string) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT name, version FROM library.bundle_members WHERE library = `+d.placeholder(1)+` AND bundle_version = `+d.placeholder(2),
		library, bundleVersion)
	if err != nil {
		return nil, fmt.Errorf("publish library: read the stored bundle: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var v int
		if err := rows.Scan(&name, &v); err != nil {
			return nil, fmt.Errorf("publish library: scan the stored bundle: %w", err)
		}
		out[name] = v
	}
	return out, rows.Err()
}

func listLibraryBundles(ctx context.Context, db *sql.DB, d Dialect, library string) ([]LibraryBundleSummary, error) {
	p1 := d.placeholder(1)
	rows, err := db.QueryContext(ctx, `
		SELECT b.library, b.bundle_version, b.published_at, COALESCE(b.published_by, ''),
		       m.name, m.version, m.wasm_sha256, m.wasm_length, m.exposure
		FROM library.bundles b
		LEFT JOIN library.bundle_members bm ON bm.library = b.library AND bm.bundle_version = b.bundle_version
		LEFT JOIN library.members m ON m.library = bm.library AND m.name = bm.name AND m.version = bm.version
		WHERE (`+p1+` = '' OR b.library = `+p1+`)
		ORDER BY b.library, b.published_at DESC, b.bundle_version DESC, m.name`, library)
	if err != nil {
		return nil, fmt.Errorf("list library bundles: %w", err)
	}
	defer rows.Close()
	var out []LibraryBundleSummary
	index := map[string]int{}
	for rows.Next() {
		var s LibraryBundleSummary
		var name, sha, exposure sql.NullString
		var version sql.NullInt64
		var length sql.NullInt64
		if err := rows.Scan(&s.Library, &s.BundleVersion, &s.PublishedAt, &s.PublishedBy, &name, &version, &sha, &length, &exposure); err != nil {
			return nil, fmt.Errorf("list library bundles: scan: %w", err)
		}
		key := s.Library + "\x00" + s.BundleVersion
		i, seen := index[key]
		if !seen {
			s.Members = []LibraryMemberSummary{}
			out = append(out, s)
			i = len(out) - 1
			index[key] = i
		}
		if name.Valid {
			out[i].Members = append(out[i].Members, LibraryMemberSummary{
				Name: name.String, Version: int(version.Int64), WasmSHA256: sha.String,
				WasmLength: length.Int64, Exposure: ExposureClass(exposure.String),
			})
		}
	}
	return out, rows.Err()
}
