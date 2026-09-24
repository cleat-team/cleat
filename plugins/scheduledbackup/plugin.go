// Package scheduledbackup provides scheduled PostgreSQL backups with pg_dump.
// It supports cron-based scheduling and manual backup via HTTP API and CLI
// commands, and records backup history in PostgreSQL. Dumps are written to
// local disk (Config.DumpDir) only -- there is no restore path and no
// off-host upload; see the s3_bucket/s3_prefix doc comments on backupConfig
// in routes.go before relying on either.
package scheduledbackup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/cleat-team/cleat/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "scheduled-backup",
		Version:     "0.1.0",
		Description: "Scheduled PostgreSQL backups to local disk",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements scheduled PostgreSQL backups with tenant isolation.
type Plugin struct {
	db      plugin.PluginDB
	mux     *http.ServeMux
	logger  *slog.Logger
	dialect plugin.Dialect
	config  Config

	deploymentSecrets plugin.DeploymentSecrets

	// bgBackups tracks in-flight scheduled backups started off Run's own
	// goroutine (background.go). Run does not wait on it -- a backup already
	// running when ctx is cancelled is deliberately left to finish rather
	// than killed, see runDueBackups. It exists so tests can wait for a
	// dispatched backup to actually finish without a fixed sleep.
	bgBackups sync.WaitGroup
}

// Config controls backup storage and pg_dump output location.
//
// DSN lived here until cleat#1992 part 1b moved it to a deployment secret
// ("scheduledbackup.dsn"), fetched fresh on every backup attempt rather than
// cached here -- see backupDSN below, and background.go/routes.go for the
// two call sites (scheduled and manually-triggered) that replaced the
// cached p.config.DSN this struct used to carry.
type Config struct {
	DumpDir string `json:"dump_dir"` // Directory for dump output files
}

// legacyScheduledBackupConfig catches dsn left over in --plugin-config from
// before cleat#1992 part 1b. json.Unmarshal ignores fields a target struct
// does not declare, so once Config dropped the field a leftover value there
// silently stopped doing anything -- no error, no log, just quietly wrong.
// This is unmarshaled from the same bytes purely to detect that and WARN;
// Config above no longer has anywhere to put the value even if this found
// one.
//
// Deliberately NOT wired into a RequiredDeploymentSecrets, unlike
// slacknotify's identically-shaped legacySlackConfig (cleat#2172 GAP 2,
// owner decision 1A): that is a genuine, structurally identical case for
// the same boot-refusal treatment -- this field's presence would equally
// prove backups were configured before -- but making that call for a
// second plugin without it being asked for is scope creep on a security
// boot-behavior decision. Flagged to the coordinator/owner separately
// rather than decided here.
type legacyScheduledBackupConfig struct {
	DSN plugin.Secret `json:"dsn"`
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "scheduled-backup",
		Version:     "0.1.0",
		Description: "Scheduled PostgreSQL backups to local disk",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.dialect = env.Dialect
	p.mux = env.Mux

	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("scheduledbackup: invalid config: %w", err)
		}
		var legacy legacyScheduledBackupConfig
		if err := json.Unmarshal(env.Config, &legacy); err == nil && legacy.DSN != "" {
			p.logger.Warn("scheduledbackup: dsn in --plugin-config is no longer read " +
				"(cleat#1992 part 1b); it has no effect. Use " +
				"`cleatctl set-deployment-secret --name scheduledbackup.dsn` instead.")
		}
	}

	p.deploymentSecrets = env.DeploymentSecrets

	if p.config.DumpDir == "" {
		p.config.DumpDir = "/tmp/cleat-backups"
	}

	if err := os.MkdirAll(p.config.DumpDir, 0755); err != nil {
		return fmt.Errorf("scheduledbackup: create dump dir: %w", err)
	}

	if err := p.checkDBVersion(ctx); err != nil {
		return err
	}

	p.logger.Info("scheduledbackup: initialized",
		"dump_dir", p.config.DumpDir,
	)
	return nil
}

// checkDBVersion verifies the database meets minimum requirements.
// MySQL 8.0+ is required for FOR UPDATE SKIP LOCKED.
func (p *Plugin) checkDBVersion(ctx context.Context) error {
	if p.db == nil || p.dialect != plugin.DialectMySQL {
		return nil
	}
	var version string
	if err := p.db.QueryRow(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return fmt.Errorf("scheduledbackup: failed to check MySQL version: %w", err)
	}
	major := 0
	for _, part := range strings.Split(version, ".") {
		if n, err := fmt.Sscanf(part, "%d", &major); err == nil && n == 1 {
			break
		}
	}
	if major < 8 {
		return fmt.Errorf("scheduledbackup: MySQL 8.0+ is required (found %s). FOR UPDATE SKIP LOCKED is not available in older MySQL versions", version)
	}
	return nil
}

// backupDSN fetches the current PostgreSQL connection string pg_dump backs
// up. Called at the moment of use -- once per scheduled or manually
// triggered backup attempt (background.go, routes.go) -- rather than cached,
// so a DSN set or rotated with `cleatctl set-deployment-secret` takes effect
// on the very next backup attempt without a worker restart (the same
// "PER-USE, NOT PER-Init" convention as email's sendGridAPIKey).
//
// Unlike slacknotify's signingSecret, this is deliberately NOT the only gate
// on whether backups can run at all: Run's background loop (background.go)
// no longer refuses to start when this errors -- see its doc comment for
// why -- so an unresolvable DSN surfaces per-attempt, recorded in
// backup_history like any other pg_dump failure, rather than silencing the
// whole plugin.
func (p *Plugin) backupDSN(ctx context.Context) (string, error) {
	if p.deploymentSecrets == nil {
		return "", fmt.Errorf("scheduledbackup: no deployment secret store configured")
	}
	dsn, err := p.deploymentSecrets.Get(ctx, "scheduledbackup.dsn")
	if err != nil {
		return "", fmt.Errorf("scheduledbackup: dsn: %w", err)
	}
	return dsn, nil
}

// DeploymentSecretPrefix implements plugin.HasDeploymentSecretPrefix:
// scheduledbackup only ever reads "scheduledbackup.dsn".
func (p *Plugin) DeploymentSecretPrefix() string {
	return "scheduledbackup."
}
