package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/datadogexport"
	"github.com/cleat-team/cleat/plugins/pagerdutyalert"
)

// ---------------------------------------------------------------------------
// migrate-plugin-secrets command
// ---------------------------------------------------------------------------
//
// cleat#1992: dd_config.api_key and pd_config.routing_key move into tenant
// secrets, sealed under engine.SecretStore's envelope encryption. The
// migrations that drop those columns (plugins/datadogexport/migrations.go
// v4, plugins/pagerdutyalert/migrations.go v3) can only drop the plaintext
// column -- plugin.Migration.Up is plain SQL with no function hook, and
// PutSecret's envelope encryption needs a Go-held master key nothing there
// can reach. This command is the missing half: read every existing
// plaintext row and write it into tenant secrets, under the exact name the
// plugin will look it up under afterwards.
//
// RUN THIS BEFORE UPGRADING. An operator must run this against the OLD
// schema -- the one that still has api_key/routing_key -- and only then
// deploy the cleat-worker build carrying the column-dropping migration. Once
// that migration has run, the plaintext value is gone with no recovery path;
// this command reads nothing that is not there. See CHANGELOG.md's upgrade
// notes for the full procedure.
//
// IDEMPOTENT. PutSecret overwrites an existing row rather than erroring
// (engine/tenant_secrets.go), so running this command twice, or re-running
// it after a partial failure, does no harm -- every successfully-read row is
// simply written again with the same value.
//
// ONE PLUGIN AT A TIME, ONE INVOCATION EACH. The two plugins' tables share no
// schema and datadog-export's migration is independent of pagerduty-alert's,
// so there is nothing an operator gains from a combined "migrate everything"
// mode that a "run it twice, once per --plugin" does not already give them,
// and a combined mode would have to decide what a failure in one plugin's
// half means for the other's -- a question this design does not need to
// answer.
func runMigratePluginSecrets(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("migrate-plugin-secrets", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", migratePluginSecretsUsage) }

	pluginName := fs.String("plugin", "", "which plugin's secrets to migrate: datadog-export or pagerduty-alert")
	dryRun := fs.Bool("dry-run", false, "report what would be migrated, write nothing")
	if err := fs.Parse(args); err != nil {
		osExit(1)
		return
	}

	if *pluginName != "datadog-export" && *pluginName != "pagerduty-alert" {
		if *pluginName == "" {
			fmt.Fprintf(os.Stderr, "error: --plugin is required\n\n%s", migratePluginSecretsUsage)
		} else {
			fmt.Fprintf(os.Stderr, "error: unknown --plugin %q: expected datadog-export or pagerduty-alert\n", *pluginName)
		}
		osExit(1)
		return
	}

	// The ring, exactly as the workers read it -- see setsecret.go's identical
	// comment on why this must be the CURRENT key and why a stale environment
	// writes at the wrong version.
	ring, err := engine.SecretKeyRingFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	if ring == nil {
		fmt.Fprintf(os.Stderr,
			"error: CLEAT_SECRET_MASTER_KEY is not set.\n\n"+
				"It must be the SAME key the workers use, or they will not be able to read\n"+
				"what this writes. Generate one with:\n\n"+
				"  head -c 32 /dev/urandom | base64\n")
		osExit(1)
		return
	}
	store := engine.NewSecretStoreWithRing(db, d.name, ring)

	var migrated, failed int
	switch *pluginName {
	case "datadog-export":
		migrated, failed = migrateDatadogExportSecrets(ctx, db, d, store, *dryRun)
	case "pagerduty-alert":
		migrated, failed = migratePagerdutyAlertSecrets(ctx, db, d, store, *dryRun)
	}

	verb := "migrated"
	if *dryRun {
		verb = "would migrate"
	}
	fmt.Printf("%s %d row(s), %d failure(s)\n", verb, migrated, failed)
	if failed > 0 {
		fmt.Println("Re-run this command to retry the failures; it is idempotent.")
		osExit(1)
	}
}

// migrateDatadogExportSecrets reads every dd_config row with a non-empty
// api_key and writes it into tenant secrets under
// datadogexport.DatadogAPIKeySecretName(id) -- the exact name
// exportForConfig (plugins/datadogexport/background.go) and the admin routes
// look it up under, imported rather than re-typed so the two can never drift
// apart.
//
// THE DISCOVERY READ GOES THROUGH SQLDBAdapter.Query UNDER
// plugin.AcrossAllTenants, NOT A RAW db.QueryContext -- dd_config is
// TenantScoped (migrations.go v3), and on SQL Server that row-level policy is
// a BLOCK predicate enforced against every principal, sysadmin and dbo alike
// (see setsecret.go's comment on the same asymmetry). A raw, unscoped query
// on that dialect silently returns zero rows -- not an error, a discovery
// read that found nothing to migrate on a database that has plenty. Postgres
// and MySQL do not need the bypass (cleatctl's --db role has BYPASSRLS on
// Postgres, MySQL has no RLS at all), but routing every dialect through the
// same path is what makes that difference invisible to this function, the
// same reasoning exportMetrics' own discovery query gives for the identical
// choice (plugins/datadogexport/background.go).
func migrateDatadogExportSecrets(ctx context.Context, db *sql.DB, d dialect, store *engine.SecretStore, dryRun bool) (migrated, failed int) {
	adapter := &engine.SQLDBAdapter{DB: db, Dialect: d.query}
	discoverCtx := plugin.AcrossAllTenants(ctx, "cleatctl migrate-plugin-secrets: discovering dd_config rows with a plaintext api_key")
	rows, err := adapter.Query(discoverCtx, `SELECT id, tenant_id, api_key FROM dd_config WHERE api_key IS NOT NULL AND api_key <> ''`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: query dd_config: %v\n", err)
		osExit(1)
		return 0, 0
	}
	defer rows.Close()

	for rows.Next() {
		// plugin.GUID, not uuid.UUID directly: SQL Server returns
		// UNIQUEIDENTIFIER in mixed-endian byte order, which uuid.UUID's Scan
		// accepts without error and turns into a DIFFERENT id -- see its doc
		// comment, and plugins/scheduler for the same pattern this command
		// mirrors.
		var id, tenantID plugin.GUID
		var apiKey string
		if err := rows.Scan(&id, &tenantID, &apiKey); err != nil {
			fmt.Fprintf(os.Stderr, "error: scan dd_config row: %v\n", err)
			failed++
			continue
		}
		name := datadogexport.DatadogAPIKeySecretName(id.UUID)
		if dryRun {
			fmt.Printf("  dd_config %s (tenant %s): would write secret %q\n", id.UUID, tenantID.UUID, name)
			migrated++
			continue
		}
		tctx := tenantctx.With(ctx, tenantID.UUID)
		if err := store.PutSecret(tctx, tenantID.UUID.String(), name, apiKey); err != nil {
			fmt.Fprintf(os.Stderr, "error: write secret for dd_config %s (tenant %s): %v\n", id.UUID, tenantID.UUID, err)
			failed++
			continue
		}
		fmt.Printf("  dd_config %s (tenant %s): wrote secret %q\n", id.UUID, tenantID.UUID, name)
		migrated++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "error: iterating dd_config: %v\n", err)
		osExit(1)
		return migrated, failed
	}
	return migrated, failed
}

// migratePagerdutyAlertSecrets is migrateDatadogExportSecrets' mirror for
// pd_config.routing_key -- see its comments for the reasoning, which applies
// identically here.
func migratePagerdutyAlertSecrets(ctx context.Context, db *sql.DB, d dialect, store *engine.SecretStore, dryRun bool) (migrated, failed int) {
	adapter := &engine.SQLDBAdapter{DB: db, Dialect: d.query}
	discoverCtx := plugin.AcrossAllTenants(ctx, "cleatctl migrate-plugin-secrets: discovering pd_config rows with a plaintext routing_key")
	rows, err := adapter.Query(discoverCtx, `SELECT id, tenant_id, routing_key FROM pd_config WHERE routing_key IS NOT NULL AND routing_key <> ''`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: query pd_config: %v\n", err)
		osExit(1)
		return 0, 0
	}
	defer rows.Close()

	for rows.Next() {
		var id, tenantID plugin.GUID
		var routingKey string
		if err := rows.Scan(&id, &tenantID, &routingKey); err != nil {
			fmt.Fprintf(os.Stderr, "error: scan pd_config row: %v\n", err)
			failed++
			continue
		}
		name := pagerdutyalert.PagerdutyRoutingKeySecretName(id.UUID)
		if dryRun {
			fmt.Printf("  pd_config %s (tenant %s): would write secret %q\n", id.UUID, tenantID.UUID, name)
			migrated++
			continue
		}
		tctx := tenantctx.With(ctx, tenantID.UUID)
		if err := store.PutSecret(tctx, tenantID.UUID.String(), name, routingKey); err != nil {
			fmt.Fprintf(os.Stderr, "error: write secret for pd_config %s (tenant %s): %v\n", id.UUID, tenantID.UUID, err)
			failed++
			continue
		}
		fmt.Printf("  pd_config %s (tenant %s): wrote secret %q\n", id.UUID, tenantID.UUID, name)
		migrated++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "error: iterating pd_config: %v\n", err)
		osExit(1)
		return migrated, failed
	}
	return migrated, failed
}

// gosec G101 reports this constant as "potential hardcoded credentials" --
// the same false positive setsecret.go's and revokeapikey.go's usage text
// already carry, for the same reason: the word "secret" next to a string
// literal, and no credential in it.
//
//nolint:gosec // G101: usage text, not a credential -- see setsecret.go's identical finding.
const migratePluginSecretsUsage = `Usage: cleatctl --db <dsn> migrate-plugin-secrets --plugin <name> [--dry-run]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

Moves an existing plaintext credential column into tenant secrets:

  datadog-export    dd_config.api_key      -> datadogexport.api_key.<config-id>
  pagerduty-alert   pd_config.routing_key  -> pagerdutyalert.routing_key.<config-id>

RUN THIS BEFORE upgrading to a cleat-worker build whose plugin migrations drop
the plaintext column (datadog-export migration v4, pagerduty-alert migration
v3). Once that migration has run, the plaintext value is gone -- this command
has nothing left to read. It is idempotent: running it again, or against a
database where some rows are already migrated, does no harm.

Requires CLEAT_SECRET_MASTER_KEY, which must match what the workers use, and
CLEAT_SECRET_MASTER_KEY_VERSION if the current key is not version 1.

Flags:
  --plugin <name>   Required. datadog-export or pagerduty-alert.
  --dry-run         Report what would be written, write nothing.

Examples:
  cleatctl --db "$DSN" migrate-plugin-secrets --plugin datadog-export --dry-run
  cleatctl --db "$DSN" migrate-plugin-secrets --plugin datadog-export
  cleatctl --db "$DSN" migrate-plugin-secrets --plugin pagerduty-alert
`
