package engine

// cleat#3627: reading what a tenant holds. The read side of POST /api/libraries/{library}/install,
// for a service that has only a tenant key.
//
// The boundary is held by two layers and both are kept on purpose, exactly as InstallLibraryBundle
// holds it: the store's tenant-scoped transaction (row-level security on PostgreSQL, the session
// context policy on SQL Server) AND an explicit tenant_id in the WHERE. Either alone is enough for
// the answer to be right today; neither is left to carry it alone, because a test passes on
// whichever layer it is not about.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// InstalledLibrary is one row of what a tenant has installed: the library, the bundle of it the
// tenant holds, when, and by whom (the subject the install ran as).
type InstalledLibrary struct {
	Library       string    `json:"library"`
	BundleVersion string    `json:"bundle_version"`
	InstalledAt   time.Time `json:"installed_at"`
	InstalledBy   string    `json:"installed_by,omitempty"`
}

// LibraryInstallLister is implemented by a store that can say which library bundles its tenant
// holds. The listing is never nil: a tenant with no installs gets an empty list.
type LibraryInstallLister interface {
	ListInstalledLibraries(ctx context.Context) ([]InstalledLibrary, error)
}

// LibraryCatalogue is implemented by a store that can say which library bundles are published.
// The catalogue is the same for every tenant (owner decision D45: any tenant may install any
// published library), so it is not tenant-scoped; the store supplies the connection and the dialect,
// which the worker's handlers do not have. library "" lists every library; the listing is never nil.
type LibraryCatalogue interface {
	ListPublishedLibraries(ctx context.Context, library string) ([]LibraryBundleSummary, error)
}

const (
	listInstalledLibrariesPostgres = `SELECT library, bundle_version, installed_at, COALESCE(installed_by, '') FROM tenant_library_installs WHERE tenant_id = $1 ORDER BY library`
	listInstalledLibrariesMSSQL    = `SELECT library, bundle_version, installed_at, COALESCE(installed_by, '') FROM tenant_library_installs WHERE tenant_id = @p1 ORDER BY library`
)

// ListInstalledLibraries lists the bundles this store's tenant holds, ordered by library.
func (s *PostgresStore) ListInstalledLibraries(ctx context.Context) ([]InstalledLibrary, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list installed libraries: begin: %w", err)
	}
	defer tx.Rollback()
	return scanInstalledLibraries(ctx, tx, listInstalledLibrariesPostgres, s.tenantID)
}

// ListInstalledLibraries lists the bundles this store's tenant holds, ordered by library.
func (s *MSSQLStore) ListInstalledLibraries(ctx context.Context) ([]InstalledLibrary, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list installed libraries: begin: %w", err)
	}
	defer tx.Rollback()
	return scanInstalledLibraries(ctx, tx, listInstalledLibrariesMSSQL, s.tenantID)
}

// ListInstalledLibraries is refused: workflow libraries are PostgreSQL and SQL Server only
// (owner decision D41, cleat#3540).
func (s *MySQLStore) ListInstalledLibraries(context.Context) ([]InstalledLibrary, error) {
	return nil, ErrLibrariesUnsupported
}

// ListPublishedLibraries lists the published bundles, newest first within a library.
func (s *PostgresStore) ListPublishedLibraries(ctx context.Context, library string) ([]LibraryBundleSummary, error) {
	return nonNilBundles(listLibraryBundles(ctx, s.db, DialectPostgres, library))
}

// ListPublishedLibraries lists the published bundles, newest first within a library.
func (s *MSSQLStore) ListPublishedLibraries(ctx context.Context, library string) ([]LibraryBundleSummary, error) {
	return nonNilBundles(listLibraryBundles(ctx, s.db, DialectMSSQL, library))
}

// ListPublishedLibraries is refused: workflow libraries are PostgreSQL and SQL Server only (D41).
func (s *MySQLStore) ListPublishedLibraries(context.Context, string) ([]LibraryBundleSummary, error) {
	return nil, ErrLibrariesUnsupported
}

// nonNilBundles makes an empty listing an empty list, so it encodes as [] and not null.
// listLibraryBundles returns nil when nothing matches, and cleatctl's --json output depends on
// that, so the fix is here and not there.
func nonNilBundles(in []LibraryBundleSummary, err error) ([]LibraryBundleSummary, error) {
	if err == nil && in == nil {
		in = []LibraryBundleSummary{}
	}
	return in, err
}

func scanInstalledLibraries(ctx context.Context, tx *sql.Tx, query, tenantID string) ([]InstalledLibrary, error) {
	rows, err := tx.QueryContext(ctx, query, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list installed libraries: %w", err)
	}
	defer rows.Close()
	out := []InstalledLibrary{}
	for rows.Next() {
		var i InstalledLibrary
		if err := rows.Scan(&i.Library, &i.BundleVersion, &i.InstalledAt, &i.InstalledBy); err != nil {
			return nil, fmt.Errorf("list installed libraries: scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list installed libraries: %w", err)
	}
	return out, nil
}

var (
	_ LibraryInstallLister = (*PostgresStore)(nil)
	_ LibraryInstallLister = (*MSSQLStore)(nil)
	_ LibraryInstallLister = (*MySQLStore)(nil)
	_ LibraryCatalogue     = (*PostgresStore)(nil)
	_ LibraryCatalogue     = (*MSSQLStore)(nil)
	_ LibraryCatalogue     = (*MySQLStore)(nil)
)
