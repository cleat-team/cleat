package engine

// cleat#3597: PublishLibraryBundle on PostgreSQL. See library_publish.go for the contract.
//
// The publisher role holds SELECT, INSERT and DELETE and no UPDATE, and PostgreSQL's
// row-locking reads (FOR SHARE, FOR UPDATE) require UPDATE. So nothing here locks a row
// and nothing upserts: each write is INSERT ... ON CONFLICT DO NOTHING, which is atomic
// against a concurrent publisher of the same key, followed by a plain read of what is
// stored when the insert found the key taken.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// libraryMemberColumns prepares the values both dialects bind for one member. The JSON
// columns are NOT NULL with a '{}' / '[]' spelling for "none" (json.Marshal of a nil map is
// the four bytes "null", which SQL Server's ISJSON rejects), and entry_point_schemas is
// nullable, where nil binds to SQL NULL.
type libraryMemberColumns struct {
	digest            string
	pluginDeps        string
	entryPoints       string
	entryPointSchemas []byte
	exposure          string
}

func prepareLibraryMember(m PublishLibraryMember) (libraryMemberColumns, error) {
	sum := sha256.Sum256(m.WasmBytes)
	c := libraryMemberColumns{digest: hex.EncodeToString(sum[:]), pluginDeps: "{}", entryPoints: "[]", exposure: string(m.Exposure)}
	if len(m.PluginDeps) > 0 {
		b, err := json.Marshal(m.PluginDeps)
		if err != nil {
			return c, fmt.Errorf("marshal plugin_deps of %q: %w", m.Name, err)
		}
		c.pluginDeps = string(b)
	}
	if len(m.EntryPoints) > 0 {
		b, err := json.Marshal(m.EntryPoints)
		if err != nil {
			return c, fmt.Errorf("marshal entry_points of %q: %w", m.Name, err)
		}
		c.entryPoints = string(b)
	}
	if len(m.EntryPointSchemas) > 0 {
		b, err := json.Marshal(m.EntryPointSchemas)
		if err != nil {
			return c, fmt.Errorf("marshal entry_point_schemas of %q: %w", m.Name, err)
		}
		c.entryPointSchemas = b
	}
	if c.exposure == "" {
		c.exposure = string(ExposureAuth)
	}
	return c, nil
}

func publishLibraryPostgres(ctx context.Context, db *sql.DB, req PublishLibraryRequest) (PublishLibraryResult, error) {
	res := PublishLibraryResult{Library: req.Library, BundleVersion: req.BundleVersion}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("publish library: begin: %w", err)
	}
	defer tx.Rollback()

	for _, m := range req.Members {
		c, err := prepareLibraryMember(m)
		if err != nil {
			return res, publishRefused("%v", err)
		}
		r, err := tx.ExecContext(ctx, `
			INSERT INTO library.members (library, name, version, wasm_bytes, wasm_sha256, wasm_length,
				abi_version, min_version, plugin_deps, entry_points, entry_point_schemas, exposure, input_validation_disabled)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, ARRAY(SELECT jsonb_array_elements_text($10::jsonb)), $11::jsonb, $12, $13)
			ON CONFLICT (library, name, version) DO NOTHING`,
			req.Library, m.Name, m.Version, m.WasmBytes, c.digest, int64(len(m.WasmBytes)),
			m.ABIVersion, m.MinVersion, c.pluginDeps, c.entryPoints, c.entryPointSchemas, c.exposure, m.InputValidationDisabled)
		if err != nil {
			return res, fmt.Errorf("publish library: write member %s@%d: %w", m.Name, m.Version, err)
		}
		if n, _ := r.RowsAffected(); n == 1 {
			res.MembersWritten++
			continue
		}
		// The key was taken. The same bytes make this a no-op; different bytes are refused.
		var stored string
		if err := tx.QueryRowContext(ctx,
			`SELECT wasm_sha256 FROM library.members WHERE library = $1 AND name = $2 AND version = $3`,
			req.Library, m.Name, m.Version).Scan(&stored); err != nil {
			return res, fmt.Errorf("publish library: read member %s@%d: %w", m.Name, m.Version, err)
		}
		if stored != c.digest {
			return res, &LibraryMemberConflictError{Library: req.Library, Name: m.Name, Version: m.Version, Stored: stored, Supplied: c.digest}
		}
		res.MembersAlreadyPublished++
	}

	var by any
	if req.PublishedBy != "" {
		by = req.PublishedBy
	}
	r, err := tx.ExecContext(ctx,
		`INSERT INTO library.bundles (library, bundle_version, published_by) VALUES ($1, $2, $3)
		 ON CONFLICT (library, bundle_version) DO NOTHING`, req.Library, req.BundleVersion, by)
	if err != nil {
		return res, fmt.Errorf("publish library: write bundle: %w", err)
	}
	if n, _ := r.RowsAffected(); n == 0 {
		// The bundle exists. It is a no-op only if its listing is exactly this one.
		stored, err := storedBundleListing(ctx, tx, DialectPostgres, req.Library, req.BundleVersion)
		if err != nil {
			return res, err
		}
		if diff := bundleDifferences(stored, req); len(diff) > 0 {
			return res, &LibraryBundleConflictError{Library: req.Library, BundleVersion: req.BundleVersion, Differences: diff}
		}
		res.BundleAlreadyPublished = true
		return res, tx.Commit()
	}
	for _, m := range req.Members {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO library.bundle_members (library, bundle_version, name, version) VALUES ($1, $2, $3, $4)`,
			req.Library, req.BundleVersion, m.Name, m.Version); err != nil {
			return res, fmt.Errorf("publish library: list member %s@%d: %w", m.Name, m.Version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("publish library: commit: %w", err)
	}
	return res, nil
}
