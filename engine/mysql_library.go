package engine

import "context"

// InstallLibraryBundle is refused: workflow libraries are PostgreSQL and SQL Server
// only (owner decision D41, cleat#3540). MySQL has one database per tenant and no
// library schema to read.
func (s *MySQLStore) InstallLibraryBundle(context.Context, InstallLibraryRequest) (InstallLibraryResult, error) {
	return InstallLibraryResult{}, ErrLibrariesUnsupported
}
