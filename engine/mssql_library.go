package engine

// cleat#3540, PR 3a: InstallLibraryBundle on SQL Server. The same contract as
// PostgresStore.InstallLibraryBundle (store_library.go), in this dialect's terms: a
// MERGE WITH (HOLDLOCK) as DeployWorkflowDef uses, UPDLOCK where PostgreSQL has FOR
// UPDATE, and no IS DISTINCT FROM, which SQL Server only has from 2022.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// mssqlInstalledByMax is installed_by's width in UTF-16 code units, which is what
// nvarchar(255) counts: a character outside the Basic Multilingual Plane is two. A long
// subject is cut to it rather than failing the whole install on a column limit.
const mssqlInstalledByMax = 255

// truncateUTF16 cuts s to at most maxUnits UTF-16 code units without splitting a
// character. Cutting by rune is not enough for nvarchar: 128 characters outside the BMP
// are 256 units and the insert fails (Msg 2628).
func truncateUTF16(s string, maxUnits int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > maxUnits {
			return s[:i]
		}
		units += w
	}
	return s
}

// InstallLibraryBundle installs one bundle for this store's tenant, in one transaction.
func (s *MSSQLStore) InstallLibraryBundle(ctx context.Context, req InstallLibraryRequest) (InstallLibraryResult, error) {
	if err := ValidateLibraryRef(req.Library, req.BundleVersion); err != nil {
		return InstallLibraryResult{}, err
	}
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return InstallLibraryResult{}, fmt.Errorf("install library: begin: %w", err)
	}
	defer tx.Rollback()

	var previous sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT bundle_version FROM tenant_library_installs WITH (UPDLOCK, HOLDLOCK) WHERE tenant_id = @p1 AND library = @p2`,
		s.tenantID, req.Library).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return InstallLibraryResult{}, fmt.Errorf("install library: read the current install: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT m.name, m.version, m.exposure
		FROM library.bundle_members bm
		JOIN library.members m ON m.library = bm.library AND m.name = bm.name AND m.version = bm.version
		WHERE bm.library = @p1 AND bm.bundle_version = @p2
		ORDER BY m.name, m.version`, req.Library, req.BundleVersion)
	if err != nil {
		return InstallLibraryResult{}, fmt.Errorf("install library: read the bundle: %w", err)
	}
	var members []InstalledLibraryMember
	for rows.Next() {
		var m InstalledLibraryMember
		var declared string
		if err := rows.Scan(&m.Name, &m.Version, &declared); err != nil {
			rows.Close()
			return InstallLibraryResult{}, fmt.Errorf("install library: scan a member: %w", err)
		}
		m.Exposure, err = resolveInstalledExposure(declared, req.TenantAllowsPublic, s.tenantID)
		if err != nil {
			rows.Close()
			return InstallLibraryResult{}, fmt.Errorf("install library %q bundle %q: member %q: %w", req.Library, req.BundleVersion, m.Name, err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return InstallLibraryResult{}, fmt.Errorf("install library: read the bundle: %w", err)
	}
	rows.Close()
	if len(members) == 0 {
		return InstallLibraryResult{}, &LibraryBundleNotFoundError{Library: req.Library, BundleVersion: req.BundleVersion}
	}

	var colName string
	var colVersion int
	var colLibrary string
	err = tx.QueryRowContext(ctx, `
		SELECT TOP 1 d.name, d.version, COALESCE(d.library, N'')
		FROM workflow_defs d
		JOIN library.bundle_members bm ON bm.name = d.name AND bm.version = d.version
		WHERE bm.library = @p1 AND bm.bundle_version = @p2
		  AND d.tenant_id = @p3 AND (d.library IS NULL OR d.library <> @p1)
		ORDER BY d.name, d.version`, req.Library, req.BundleVersion, s.tenantID).
		Scan(&colName, &colVersion, &colLibrary)
	switch {
	case err == nil:
		return InstallLibraryResult{}, &LibraryDefinitionCollisionError{Name: colName, Version: colVersion, HeldFromLibrary: colLibrary}
	case !errors.Is(err, sql.ErrNoRows):
		return InstallLibraryResult{}, fmt.Errorf("install library: check for a colliding definition: %w", err)
	}

	// See PostgresStore.InstallLibraryBundle for what each part of this does. The
	// WHEN MATCHED AND target.library = src.library is the guard that keeps the
	// refresh off a row that is not this library's.
	res, err := tx.ExecContext(ctx, `
		MERGE workflow_defs WITH (HOLDLOCK) AS target
		USING (
			SELECT m.library, m.name, m.version, m.abi_version, m.min_version, m.plugin_deps, m.entry_points,
			       m.task_queue, m.max_history_length, m.dag_spec, m.entry_point_schemas, m.exposure, m.input_validation_disabled
			FROM library.members m
			JOIN library.bundle_members bm ON bm.library = m.library AND bm.name = m.name AND bm.version = m.version
			WHERE bm.library = @p1 AND bm.bundle_version = @p2
		) AS src
		ON target.tenant_id = @p3 AND target.name = src.name AND target.version = src.version
		WHEN MATCHED AND target.library = src.library THEN UPDATE SET
			abi_version = src.abi_version,
			min_version = src.min_version,
			plugin_deps = src.plugin_deps,
			entry_points = src.entry_points,
			task_queue = src.task_queue,
			max_history_length = src.max_history_length,
			dag_spec = src.dag_spec,
			entry_point_schemas = src.entry_point_schemas,
			exposure = src.exposure,
			input_validation_disabled = src.input_validation_disabled,
			updated_at = SYSUTCDATETIME()
		WHEN NOT MATCHED THEN INSERT (tenant_id, name, version, library, wasm_bytes, `+libraryInstallColumns+`)
		     VALUES (@p3, src.name, src.version, src.library, NULL, src.abi_version, src.min_version, src.plugin_deps, src.entry_points,
		             src.task_queue, src.max_history_length, src.dag_spec, src.entry_point_schemas, src.exposure, src.input_validation_disabled);`,
		req.Library, req.BundleVersion, s.tenantID)
	if err != nil {
		return InstallLibraryResult{}, fmt.Errorf("install library: write the definitions: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return InstallLibraryResult{}, fmt.Errorf("install library: count the definitions written: %w", err)
	} else if int(n) != len(members) {
		return InstallLibraryResult{}, fmt.Errorf("install library: wrote %d of %d definitions; one was created under a key of this bundle after the check: %w",
			n, len(members), ErrLibraryDefinitionCollision)
	}

	var installedBy any
	if req.InstalledBy != "" {
		installedBy = truncateUTF16(req.InstalledBy, mssqlInstalledByMax)
	}
	if _, err := tx.ExecContext(ctx, `
		MERGE tenant_library_installs WITH (HOLDLOCK) AS target
		USING (VALUES (@p2)) AS src(library)
		ON target.tenant_id = @p1 AND target.library = src.library
		WHEN MATCHED THEN UPDATE SET bundle_version = @p3, installed_at = SYSUTCDATETIME(), installed_by = @p4
		WHEN NOT MATCHED THEN INSERT (tenant_id, library, bundle_version, installed_at, installed_by)
		     VALUES (@p1, src.library, @p3, SYSUTCDATETIME(), @p4);`,
		s.tenantID, req.Library, req.BundleVersion, installedBy); err != nil {
		return InstallLibraryResult{}, fmt.Errorf("install library: record the install: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return InstallLibraryResult{}, fmt.Errorf("install library: commit: %w", err)
	}
	return InstallLibraryResult{
		Library:               req.Library,
		BundleVersion:         req.BundleVersion,
		PreviousBundleVersion: previous.String,
		Members:               members,
	}, nil
}
