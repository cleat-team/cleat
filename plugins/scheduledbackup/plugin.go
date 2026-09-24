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
// This is unmarshaled from the same bytes purely to detect that and WARN, and
// -- as of the owner's #2172 1A precedent extended here on the coordinator's
// instruction -- to require scheduledbackup.dsn at boot when it is present.
// Config above no longer has anywhere to put the value even if this found
// one.
//
// Why boot-refusal, not just a WARN: dsn's presence is proof this deployment
// ran scheduled backups against a real database before upgrading, and a
// missing dsn secret would otherwise fail every backup attempt silently
// (into backup_history, not the operator's face) until someone happens to
// need a restore and finds nothing there. "dsn" is also a generic key a
// future plugin's own --plugin-config could collide with, which is why the
// WARN below and RequiredDeploymentSecrets' error both say to remove the key
// itself, not only to add the replacement secret.
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
				"(cleat#1992 part 1b) and now makes scheduledbackup.dsn required at " +
				"boot. Run `cleatctl set-deployment-secret --name scheduledbackup.dsn`, " +
				"then remove dsn from --plugin-config -- removing the key is what stops " +
				"this WARN and the boot requirement, not just setting the new secret.")
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

// backupDSNUnavailableMessage is what a tenant sees in backup_history.error_message
// when backupDSN fails -- never the underlying error text, which names internal
// state ("not found", "no secret master key") that means nothing to a tenant and
// is an operator's business, not theirs. The detailed error still reaches the
// operator, via p.logger.Error at each call site.
const backupDSNUnavailableMessage = "backup target credentials unavailable; contact the operator"

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

// RequiredDeploymentSecrets implements plugin.HasRequiredDeploymentSecrets.
// Conditional, the same shape as slacknotify's (cleat#2172 GAP 2, owner
// decision 1A), extended here to scheduledbackup on the coordinator's
// instruction: a leftover dsn in --plugin-config is what makes
// scheduledbackup.dsn required, not an unconditional requirement on every
// deployment that merely has this plugin loaded (plugin.Discover loads every
// registered plugin unconditionally -- see CLAUDE.md -- so an unconditional
// requirement here would refuse to boot any worker that has never used
// scheduled backups at all).
func (p *Plugin) RequiredDeploymentSecrets(config []byte) ([]string, error) {
	var legacy legacyScheduledBackupConfig
	if err := json.Unmarshal(config, &legacy); err == nil && legacy.DSN != "" {
		return []string{"scheduledbackup.dsn"}, nil
	}
	return nil, nil
}
