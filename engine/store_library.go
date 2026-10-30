package engine

// cleat#3540, PR 3a: InstallLibraryBundle on PostgreSQL. See library_install.go for
// what an install is and why a tenant's own key may do it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// libraryInstallColumns are the workflow_defs columns an install copies from the
// member. EVERY non-bytes column the two tables share is here: the readers that follow
// a library reference take only the bytes, the length and the digest from the member,
// and everything else (the schemas input validation reads, the exposure class, the
// plugin dependencies, the task queue) comes from THIS tenant's row, so a column left
// out is not a missing detail, it is a NULL where a value was published. The test
// derives the list from information_schema, so a column added to both tables fails it.
const libraryInstallColumns = `abi_version, min_version, plugin_deps, entry_points, task_queue, max_history_length, dag_spec, entry_point_schemas, exposure, input_validation_disabled`

// InstallLibraryBundle installs one bundle for this store's tenant, in one transaction.
func (s *PostgresStore) InstallLibraryBundle(ctx context.Context, req InstallLibraryRequest) (InstallLibraryResult, error) {
	if err := ValidateLibraryRef(req.Library, req.BundleVersion); err != nil {
		return InstallLibraryResult{}, err
	}
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return InstallLibraryResult{}, fmt.Errorf("install library: begin: %w", err)
	}
	defer tx.Rollback()

	// Serialise two installs of the same library by this tenant. When the tenant has
	// no install row yet there is nothing to lock, and the upsert below is what makes
	// a race between two first installs harmless.
	var previous sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT bundle_version FROM tenant_library_installs WHERE tenant_id = $1 AND library = $2 FOR UPDATE`,
		s.tenantID, req.Library).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return InstallLibraryResult{}, fmt.Errorf("install library: read the current install: %w", err)
	}

	// The bundle's members and what each declares.
	rows, err := tx.QueryContext(ctx, `
		SELECT m.name, m.version, m.exposure, m.plugin_deps::text
		FROM library.bundle_members bm
		JOIN library.members m ON m.library = bm.library AND m.name = bm.name AND m.version = bm.version
		WHERE bm.library = $1 AND bm.bundle_version = $2
		ORDER BY m.name, m.version`, req.Library, req.BundleVersion)
	if err != nil {
		return InstallLibraryResult{}, fmt.Errorf("install library: read the bundle: %w", err)
	}
	var members []InstalledLibraryMember
	var memberDeps []MemberPluginDeps
	for rows.Next() {
		var m InstalledLibraryMember
		var declared string
		var deps sql.NullString
		if err := rows.Scan(&m.Name, &m.Version, &declared, &deps); err != nil {
			rows.Close()
			return InstallLibraryResult{}, fmt.Errorf("install library: scan a member: %w", err)
		}
		// Refused for the whole bundle, before any write, when one member's class is
		// not one this tenant may hold.
		m.Exposure, err = resolveInstalledExposure(declared, req.TenantAllowsPublic, s.tenantID)
		if err != nil {
			rows.Close()
			return InstallLibraryResult{}, fmt.Errorf("install library %q bundle %q: member %q: %w", req.Library, req.BundleVersion, m.Name, err)
		}
		members = append(members, m)
		memberDeps = append(memberDeps, MemberPluginDeps{Name: m.Name, Version: m.Version, Raw: []byte(deps.String)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return InstallLibraryResult{}, fmt.Errorf("install library: read the bundle: %w", err)
	}
	rows.Close()
	if len(members) == 0 {
		return InstallLibraryResult{}, &LibraryBundleNotFoundError{Library: req.Library, BundleVersion: req.BundleVersion}
	}

	// Never shadow, and never take over: a definition this tenant deployed, or holds
	// from another library, under any of these keys stops the install.
	var colName string
	var colVersion int
	var colLibrary string
	err = tx.QueryRowContext(ctx, `
		SELECT d.name, d.version, COALESCE(d.library, '')
		FROM workflow_defs d
		JOIN library.bundle_members bm ON bm.name = d.name AND bm.version = d.version
		WHERE bm.library = $1 AND bm.bundle_version = $2
		  AND d.tenant_id = $3 AND d.library IS DISTINCT FROM $1
		ORDER BY d.name, d.version LIMIT 1`, req.Library, req.BundleVersion, s.tenantID).
		Scan(&colName, &colVersion, &colLibrary)
	switch {
	case err == nil:
		return InstallLibraryResult{}, &LibraryDefinitionCollisionError{Name: colName, Version: colVersion, HeldFromLibrary: colLibrary}
	case !errors.Is(err, sql.ErrNoRows):
		return InstallLibraryResult{}, fmt.Errorf("install library: check for a colliding definition: %w", err)
	}

	// cleat#3600 (owner D52): refused for the whole bundle, before any write, when a member
	// declares a plugin no worker has ever registered. No override: an install goes into a
	// running system. Read on this transaction.
	if err := CheckMemberPluginDeps(ctx, querierPluginSource{q: tx, d: DialectPostgres}, req.Library, memberDeps); err != nil {
		return InstallLibraryResult{}, fmt.Errorf("install library %q bundle %q: %w", req.Library, req.BundleVersion, err)
	}

	// One statement copies every non-bytes column. wasm_bytes stays NULL and library
	// is set, which is what the table's CHECK requires. On a row this library already
	// installed, the metadata is refreshed from the member and disabled_at and
	// gc_eligible are left alone: reinstalling must not re-enable a version the tenant
	// disabled, and an install never arms gc_eligible. The WHERE keeps the conflict
	// arm from touching a row of anyone else's library that appeared since the check.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_defs (tenant_id, name, version, library, wasm_bytes, `+libraryInstallColumns+`)
		SELECT $3, m.name, m.version, m.library, NULL, m.abi_version, m.min_version, m.plugin_deps, m.entry_points,
		       m.task_queue, m.max_history_length, m.dag_spec, m.entry_point_schemas, m.exposure, m.input_validation_disabled
		FROM library.members m
		JOIN library.bundle_members bm ON bm.library = m.library AND bm.name = m.name AND bm.version = m.version
		WHERE bm.library = $1 AND bm.bundle_version = $2
		ON CONFLICT (tenant_id, name, version) DO UPDATE SET
			abi_version = EXCLUDED.abi_version,
			min_version = EXCLUDED.min_version,
			plugin_deps = EXCLUDED.plugin_deps,
			entry_points = EXCLUDED.entry_points,
			task_queue = EXCLUDED.task_queue,
			max_history_length = EXCLUDED.max_history_length,
			dag_spec = EXCLUDED.dag_spec,
			entry_point_schemas = EXCLUDED.entry_point_schemas,
			exposure = EXCLUDED.exposure,
			input_validation_disabled = EXCLUDED.input_validation_disabled,
			updated_at = now()
		WHERE workflow_defs.library = EXCLUDED.library`, req.Library, req.BundleVersion, s.tenantID)
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
		installedBy = req.InstalledBy
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_library_installs (tenant_id, library, bundle_version, installed_at, installed_by)
		VALUES ($1, $2, $3, now(), $4)
		ON CONFLICT (tenant_id, library) DO UPDATE SET
			bundle_version = EXCLUDED.bundle_version,
			installed_at = now(),
			installed_by = EXCLUDED.installed_by`,
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
