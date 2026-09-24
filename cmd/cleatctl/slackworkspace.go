package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Written in PostgreSQL $N form and rewritten per dialect through d.rebind
// at every call site -- the same convention quota.go's statements follow
// and TestQuotaStatementsRebindPerDialect asserts for them;
// TestSlackWorkspaceStatementsRebindPerDialect asserts it here.
const (
	// tenant_id is CAST to a fixed-width string rather than SELECTed raw:
	// on SQL Server it is UNIQUEIDENTIFIER, and go-mssqldb scans that into a
	// Go string as the 16 raw storage bytes, not the canonical hyphenated
	// form (engine/mssql_schedule_tenant_id_test.go documents the same trap
	// for a different table). CHAR(36), not VARCHAR(36): MySQL's CAST()
	// target-type list does not include VARCHAR at all ("You have an error
	// in your SQL syntax", measured) -- CHAR is the one width-bearing string
	// type all three accept as a CAST target. It is a no-op on postgres'
	// uuid and mysql's already-textual CHAR(36), so one statement still
	// covers all three through d.rebind.
	//
	// SQL Server's CAST renders UPPERCASE hex (mssql_block_predicates_test.go);
	// postgres and mysql return whatever case was written. runMapWorkspace
	// compares the result against the caller's --tenant with
	// strings.EqualFold for exactly this reason -- ==, tried first, made
	// "already maps to this tenant" never match on SQL Server.
	slackWorkspaceGetSQL    = `SELECT CAST(tenant_id AS CHAR(36)) FROM slack_workspace WHERE team_id = $1`
	slackWorkspaceInsertSQL = `INSERT INTO slack_workspace (team_id, tenant_id, created_at) VALUES ($1, $2, $3)`
	slackWorkspaceUpdateSQL = `UPDATE slack_workspace SET tenant_id = $1 WHERE team_id = $2`
	slackWorkspaceDeleteSQL = `DELETE FROM slack_workspace WHERE team_id = $1`
	// No $N: lists every row, unfiltered. Same CAST, same reason.
	slackWorkspaceListSQL = `SELECT team_id, CAST(tenant_id AS CHAR(36)), created_at FROM slack_workspace ORDER BY team_id`
)

// slackTeamIDShape matches Slack's own team_id shape: a leading 'T' (a
// regular workspace) or 'E' (an Enterprise Grid org unit), then
// alphanumerics, at most 32 characters -- the same bound
// migrations/postgres/104_a_slack_workspace_maps_to_one_tenant.sql's CHECK
// constraint enforces there. MySQL enforces the same shape with its own
// CHECK (REGEXP is portable there); SQL Server enforces neither (092's and
// 073's own precedent: no regexp in a CHECK, and a LIKE pattern cannot
// express a bounded repeat of a character class), so on that dialect THIS
// validator is the only gate -- every write here goes through it regardless
// of dialect, which is what makes that acceptable rather than a gap.
var slackTeamIDShape = regexp.MustCompile(`^[TE][A-Z0-9]+$`)

func validateSlackTeamID(teamID string) error {
	if len(teamID) > 32 {
		return fmt.Errorf("team_id %q is longer than 32 characters", teamID)
	}
	if !slackTeamIDShape.MatchString(teamID) {
		return fmt.Errorf("team_id %q does not look like a Slack team/org id "+
			"(expected a leading T or E followed by uppercase letters and digits)", teamID)
	}
	return nil
}

// runSlack dispatches cleatctl's operator-only Slack-workspace-mapping
// commands (cleat#2230, owner decision "3A"/"1B"). Tenants cannot write
// slack_workspace themselves -- there is no OAuth install flow in this
// plugin that would make a tenant's own claim to a Slack team_id
// trustworthy, so mapping a workspace to a tenant is an operator action,
// the same asymmetry deployment_secrets and tenant_domains already have.
func runSlack(ctx context.Context, db *sql.DB, d dialect, args []string) {
	if len(args) < 1 {
		printSlackUsage()
		osExit(1)
		return
	}
	switch args[0] {
	case "map-workspace":
		runMapWorkspace(ctx, db, d, args[1:])
	case "list-workspaces":
		runListWorkspaces(ctx, db, d, args[1:])
	case "unmap-workspace":
		runUnmapWorkspace(ctx, db, d, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown slack subcommand: %s\n\n", args[0])
		printSlackUsage()
		osExit(1)
	}
}

func printSlackUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl --db <dsn> slack <map-workspace|list-workspaces|unmap-workspace> [flags]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

  slack map-workspace --team <team_id> --tenant <uuid> [--reassign]
                                  map a Slack workspace (or Enterprise Grid
                                  org unit) to a tenant. Refuses to change an
                                  EXISTING mapping's tenant unless --reassign
                                  is also given, and prints old -> new when it
                                  does.
  slack list-workspaces          list every team_id -> tenant_id mapping
  slack unmap-workspace --team <team_id>
                                  remove a mapping. POST /slack/interactive
                                  refuses every click from that workspace
                                  immediately afterward -- there is no cache
                                  to wait out.
`)
}

// parseSlackFlags scans args for --team/--tenant/--reassign in any order and
// in either `--flag value` or `--flag=value` form, the same hand-rolled scan
// quota's parseQuotaFlags uses and for the same reason: flag.FlagSet stops
// at the first non-flag argument, and every flag here is a --flag.
func parseSlackFlags(args []string) (teamID, tenantID string, reassign bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		key, raw := a, ""
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			key, raw = a[:eq], a[eq+1:]
		}
		next := func() (string, error) {
			if raw != "" {
				return raw, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", key)
			}
			i++
			return args[i], nil
		}
		switch key {
		case "--team":
			if teamID, err = next(); err != nil {
				return "", "", false, err
			}
		case "--tenant":
			if tenantID, err = next(); err != nil {
				return "", "", false, err
			}
		case "--reassign":
			reassign = true
		default:
			return "", "", false, fmt.Errorf("unknown flag: %s", a)
		}
	}
	return teamID, tenantID, reassign, nil
}

// runMapWorkspace upserts one team_id -> tenant_id mapping.
//
// UPSERT, not INSERT-only: an operator remapping a workspace to a different
// tenant (correcting a fat-fingered tenant id, for example) is a legitimate,
// if rare, operation, and should not require an unmap-then-map round trip.
// But an upsert that silently changes an EXISTING mapping's tenant is
// exactly the kind of surprising, hard-to-notice change an operator-only
// surface still needs a rail against -- so a mapping that already points
// somewhere else is refused unless --reassign is explicit, and the old and
// new tenant are both printed when it is.
func runMapWorkspace(ctx context.Context, db *sql.DB, d dialect, args []string) {
	teamID, tenantID, reassign, err := parseSlackFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printSlackUsage()
		osExit(1)
		return
	}
	if teamID == "" || tenantID == "" {
		fmt.Fprintf(os.Stderr, "slack map-workspace requires --team and --tenant\n\n")
		printSlackUsage()
		osExit(1)
		return
	}
	if err := validateSlackTeamID(teamID); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		osExit(1)
		return
	}
	if _, err := uuid.Parse(tenantID); err != nil {
		fmt.Fprintf(os.Stderr, "not a tenant UUID: %q: %v\n", tenantID, err)
		osExit(1)
		return
	}

	var existingTenant string
	err = db.QueryRowContext(ctx, d.rebind(slackWorkspaceGetSQL), teamID).Scan(&existingTenant)
	switch {
	case err == sql.ErrNoRows:
		_, err := db.ExecContext(ctx,
			d.rebind(slackWorkspaceInsertSQL),
			teamID, tenantID, time.Now().UTC())
		if err != nil {
			fmt.Fprintf(os.Stderr, "mapping %s -> %s: %v\n", teamID, tenantID, err)
			osExit(1)
			return
		}
		fmt.Printf("mapped %s -> %s\n", teamID, tenantID)
	case err != nil:
		fmt.Fprintf(os.Stderr, "reading existing mapping for %s: %v\n", teamID, err)
		osExit(1)
		return
	case strings.EqualFold(existingTenant, tenantID):
		fmt.Printf("%s already maps to %s -- nothing to do\n", teamID, tenantID)
	case !reassign:
		fmt.Fprintf(os.Stderr, "%s already maps to %s -- pass --reassign to point it at %s instead\n",
			teamID, existingTenant, tenantID)
		osExit(1)
		return
	default:
		_, err := db.ExecContext(ctx, d.rebind(slackWorkspaceUpdateSQL), tenantID, teamID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reassigning %s: %v\n", teamID, err)
			osExit(1)
			return
		}
		fmt.Printf("reassigned %s: %s -> %s\n", teamID, existingTenant, tenantID)
	}
}

func runListWorkspaces(ctx context.Context, db *sql.DB, d dialect, args []string) {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "slack list-workspaces takes no arguments\n\n")
		printSlackUsage()
		osExit(1)
		return
	}
	rows, err := db.QueryContext(ctx, slackWorkspaceListSQL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listing workspaces: %v\n", err)
		osExit(1)
		return
	}
	defer rows.Close()

	fmt.Printf("%-16s  %-36s  %s\n", "TEAM_ID", "TENANT_ID", "CREATED_AT")
	n := 0
	for rows.Next() {
		var teamID, tenantID string
		var createdAt time.Time
		if err := rows.Scan(&teamID, &tenantID, &createdAt); err != nil {
			fmt.Fprintf(os.Stderr, "reading row: %v\n", err)
			osExit(1)
			return
		}
		fmt.Printf("%-16s  %-36s  %s\n", teamID, tenantID, createdAt.Format(time.RFC3339))
		n++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "reading rows: %v\n", err)
		osExit(1)
		return
	}
	if n == 0 {
		fmt.Println("(no Slack workspaces mapped)")
	}
}

// runUnmapWorkspace removes one mapping. There is no cache anywhere in this
// path -- handleInteractiveCallback (once cleat#2230(b) wires it up) reads
// slack_workspace fresh on every request -- so this takes effect on the very
// next click, not after some TTL. That is deliberate: an unmap is the
// documented way to kill a compromised or decommissioned workspace's access
// immediately.
func runUnmapWorkspace(ctx context.Context, db *sql.DB, d dialect, args []string) {
	teamID, _, _, err := parseSlackFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printSlackUsage()
		osExit(1)
		return
	}
	if teamID == "" {
		fmt.Fprintf(os.Stderr, "slack unmap-workspace requires --team\n\n")
		printSlackUsage()
		osExit(1)
		return
	}
	res, err := db.ExecContext(ctx, d.rebind(slackWorkspaceDeleteSQL), teamID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unmapping %s: %v\n", teamID, err)
		osExit(1)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unmapping %s: %v\n", teamID, err)
		osExit(1)
		return
	}
	if n == 0 {
		fmt.Printf("%s was not mapped -- nothing to do\n", teamID)
		return
	}
	fmt.Printf("unmapped %s\n", teamID)
}
