package engine

// cleat#3597: PublishLibraryBundle on SQL Server. The same contract as
// publishLibraryPostgres, in this dialect's terms: no ON CONFLICT, so each insert is
// INSERT ... SELECT ... WHERE NOT EXISTS under UPDLOCK, HOLDLOCK on the key, which makes
// two publishers of the same key serialise instead of deadlocking on shared range locks.

import (
	"context"
	"database/sql"
	"fmt"
)

// publishLibraryMSSQL replays the whole publish when the server rolled it back (a deadlock
// victim, 1205): the transaction is all-or-nothing and every write in it is idempotent, so a
// replay is sound. A conflict or a refusal is not such an error and returns at once.
func publishLibraryMSSQL(ctx context.Context, db *sql.DB, req PublishLibraryRequest) (PublishLibraryResult, error) {
	pub := mssqlLibraryPublisher{db: db}
	var res PublishLibraryResult
	err := withRollbackGuaranteedRetry(ctx, "publish library bundle", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		res, err = pub.publishOnce(ctx, req)
		return err
	})
	return res, err
}

// mssqlLibraryPublisher is the connection a publish runs on. It is a type, not a bare function,
// so the transaction below is a method the retry closure calls the way every other SQL Server
// transaction boundary is reached (mssql_tx_boundaries_retried_test.go reads exactly that shape).
type mssqlLibraryPublisher struct{ db *sql.DB }

func (m mssqlLibraryPublisher) publishOnce(ctx context.Context, req PublishLibraryRequest) (PublishLibraryResult, error) {
	db := m.db
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
			SELECT @p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10, @p11, @p12, @p13
			WHERE NOT EXISTS (SELECT 1 FROM library.members WITH (UPDLOCK, HOLDLOCK)
			                  WHERE library = @p1 AND name = @p2 AND version = @p3)`,
			req.Library, m.Name, m.Version, m.WasmBytes, c.digest, int64(len(m.WasmBytes)),
			m.ABIVersion, m.MinVersion, c.pluginDeps, c.entryPoints, nullableBytesAsString(c.entryPointSchemas), c.exposure, m.InputValidationDisabled)
		if err != nil {
			return res, fmt.Errorf("publish library: write member %s@%d: %w", m.Name, m.Version, err)
		}
		if n, _ := r.RowsAffected(); n == 1 {
			res.MembersWritten++
			continue
		}
		var stored string
		if err := tx.QueryRowContext(ctx,
			`SELECT wasm_sha256 FROM library.members WHERE library = @p1 AND name = @p2 AND version = @p3`,
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
		by = truncateUTF16(req.PublishedBy, mssqlInstalledByMax)
	}
	r, err := tx.ExecContext(ctx, `
		INSERT INTO library.bundles (library, bundle_version, published_by)
		SELECT @p1, @p2, @p3
		WHERE NOT EXISTS (SELECT 1 FROM library.bundles WITH (UPDLOCK, HOLDLOCK) WHERE library = @p1 AND bundle_version = @p2)`,
		req.Library, req.BundleVersion, by)
	if err != nil {
		return res, fmt.Errorf("publish library: write bundle: %w", err)
	}
	if n, _ := r.RowsAffected(); n == 0 {
		stored, err := storedBundleListing(ctx, tx, DialectMSSQL, req.Library, req.BundleVersion)
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
			`INSERT INTO library.bundle_members (library, bundle_version, name, version) VALUES (@p1, @p2, @p3, @p4)`,
			req.Library, req.BundleVersion, m.Name, m.Version); err != nil {
			return res, fmt.Errorf("publish library: list member %s@%d: %w", m.Name, m.Version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("publish library: commit: %w", err)
	}
	return res, nil
}

// nullableBytesAsString binds nil to SQL NULL and anything else to its text: the SQL Server
// column is nvarchar(max), which the driver would otherwise send a []byte to as binary.
func nullableBytesAsString(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}
