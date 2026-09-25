package main

// liveAPIKeyCountQuery returns the query main() uses to decide whether to
// auto-generate a startup API key.
//
// cleat#2352. The table is admin.tenant_api_keys on PostgreSQL and SQL
// Server, unqualified on MySQL -- see main.go's surrounding comment on
// cleat#1963 for why getting that wrong left the check reading an
// always-empty table. Excludes both disabled and expired rows: once OAuth
// login (#2340) starts minting short-lived keys, a tenant whose every key has
// long since expired would otherwise read keyCount > 0 forever from that
// expired history alone, permanently suppressing the one-time startup-key
// generation for a tenant that is, in every way that matters here, genuinely
// keyless.
//
// Split out from main() as its own named function so the mapping from driver
// to query text can be asserted without spawning a worker process --
// TestLiveAPIKeyCountQuery_ExcludesDisabledAndExpired (apikeycount_test.go)
// proves the count itself is right against a real database; this function is
// what both that test and main() call, so they cannot drift apart the way a
// query string copy-pasted into a test could.
func liveAPIKeyCountQuery(driver string) string {
	switch driver {
	case "postgres":
		return `SELECT COUNT(*) FROM admin.tenant_api_keys WHERE disabled_at IS NULL AND (expires_at IS NULL OR expires_at > now())`
	case "mssql":
		return `SELECT COUNT(*) FROM admin.tenant_api_keys WHERE disabled_at IS NULL AND (expires_at IS NULL OR expires_at > SYSUTCDATETIME())`
	default: // mysql
		return `SELECT COUNT(*) FROM tenant_api_keys WHERE disabled_at IS NULL AND (expires_at IS NULL OR expires_at > NOW(6))`
	}
}
