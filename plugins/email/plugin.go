// Package email provides SendGrid transactional email integration for workflows.
// It lets workflows send transactional emails via SendGrid, and exposes
// HostCall functions callable from WASM workflow code.
package email

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "email-notify",
		Version:     "0.1.0",
		Description: "Send transactional email via SendGrid",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements SendGrid email sending for workflows.
type Plugin struct {
	logger            *slog.Logger
	httpClient        *http.Client
	deploymentSecrets plugin.DeploymentSecrets
	defaultFrom       string
}

// Config holds optional configuration for the email plugin.
//
// SendGridAPIKey lived here until cleat#1992 part 1 moved it to a deployment
// secret ("email.sendgrid_api_key") so it can be rotated with
// `cleatctl set-deployment-secret` and take effect without a worker restart.
// See sendGridAPIKey (host_functions.go) for the per-call lookup that
// replaced the cached apiKey field this struct used to carry.
//
// Enabled is new, and load-bearing for enablement now in a way DefaultFrom
// never was. env.Config is the SAME raw --plugin-config bytes every plugin's
// Init receives (cmd/cleat-worker/main.go builds one Environment and copies
// it per plugin, Config included) -- there is no per-plugin section, only a
// flat object every plugin's own Config struct picks its own field names out
// of. Before cleat#1992, "config non-empty" WAS an email-specific signal,
// because a non-empty config with no sendgrid_api_key was a hard error
// ("sendgrid_api_key is required"), so an llm-only deployment simply never
// set one. Once the key moved out of Config entirely, that fell away: any
// worker with ANY --plugin-config -- for llm, for any other plugin, for
// nothing email cares about -- made len(env.Config) == 0 false, so email
// read itself as configured and checkRequiredDeploymentSecrets then refused
// to start the whole worker over a missing email.sendgrid_api_key nobody
// asked for. Found in cleat-review's #2202 pass. A prefixed name, not a bare
// "enabled": this struct is unmarshaled from the SAME shared blob every
// other plugin's Config is, so a bare field name is one any other plugin
// could also define and collide with by accident.
type Config struct {
	Enabled     bool   `json:"email_enabled"`
	DefaultFrom string `json:"default_from,omitempty"`
}

// legacyEmailConfig catches sendgrid_api_key left over in --plugin-config
// from before cleat#1992 part 1. json.Unmarshal ignores fields a target
// struct does not declare, so once Config dropped the field a leftover value
// there silently stopped doing anything -- no error, no log, just quietly
// wrong. This is unmarshaled from the same bytes purely to detect that and
// react (WARN or refuse to boot, see Init); Config above no longer has
// anywhere to put the value even if this found one.
//
// plugin.Secret, not string: TestPluginCredentialFieldsUseTheSecretType
// flags any credential-shaped field held as a plain string, since that is
// how five plugins leaked one by marshaling it back to a caller. This field
// never round-trips through a handler -- it is read once, at Init, purely to
// decide whether to WARN or refuse to boot -- but the guard is deliberately
// name-driven rather than reachability-driven, and Secret costs nothing
// here. Found in cleat-review's #2202 re-check.
type legacyEmailConfig struct {
	SendGridAPIKey plugin.Secret `json:"sendgrid_api_key"`
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "email-notify",
		Version:     "0.1.0",
		Description: "Send transactional email via SendGrid",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment.
//
// It no longer reads a SendGrid API key: that moved to a deployment secret
// (cleat#1992 part 1), fetched fresh on every call by sendGridAPIKey in
// host_functions.go rather than cached here. Enablement is decided by
// Config.Enabled ("email_enabled" in --plugin-config), not by config-section
// presence -- see Config's doc comment for why len(env.Config) == 0 alone
// stopped being a safe signal once sendgrid_api_key left this struct.
// plugin.ErrNotConfigured is how a disabled email says so quietly rather
// than logging ERROR on every stock start. Whether the deployment secret
// itself is set is checked separately, at worker boot, by the fail-closed
// check setup.go runs over every enabled plugin's RequiredDeploymentSecrets.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.httpClient = &http.Client{
		// cleat#1565: every outbound request goes through the egress guard.
		// Nil in tests that build an Environment directly, which falls back to
		// the default transport -- TestEveryPluginRoutesItsEgressThroughTheGuard
		// is what keeps that from being how production works.
		Transport: env.HTTPTransport,
		Timeout:   30 * time.Second,
	}

	p.deploymentSecrets = env.DeploymentSecrets

	if len(env.Config) == 0 {
		// No config section at all -- most deployments never touch this
		// plugin. Disable it quietly rather than logging ERROR on every
		// stock worker start.
		return fmt.Errorf("email: %w", plugin.ErrNotConfigured)
	}
	var cfg Config
	if err := json.Unmarshal(env.Config, &cfg); err != nil {
		return fmt.Errorf("email: invalid config: %w", err)
	}

	// Checked BEFORE the !cfg.Enabled return below, on purpose: cleat-review's
	// #2202 re-check found that the original ordering let a pre-upgrade
	// config -- sendgrid_api_key present, email_enabled not yet added --
	// disable email through the ordinary ErrNotConfigured path with only an
	// INFO log line, never reaching the WARN a few lines down. A deployment
	// that was clearly sending email would silently stop.
	var legacy legacyEmailConfig
	hasLegacyKey := false
	if err := json.Unmarshal(env.Config, &legacy); err == nil && legacy.SendGridAPIKey != "" {
		hasLegacyKey = true
	}

	if !cfg.Enabled {
		if hasLegacyKey {
			// Fail closed, the same call the owner made for deployment
			// secrets generally (checkRequiredDeploymentSecrets): a
			// deployment that clearly meant to send email must not quietly
			// stop. ErrFatalMisconfiguration makes cmd/cleat-worker refuse
			// to start rather than merely mark this plugin unhealthy.
			return fmt.Errorf("email: sendgrid_api_key is set in --plugin-config but "+
				"email_enabled is not -- this deployment was sending email before "+
				"cleat#1992 part 1 moved the key to a deployment secret, and would "+
				"silently stop if allowed to boot. Fix: set \"email_enabled\": true, move "+
				"the key with `cleatctl set-deployment-secret --name email.sendgrid_api_key`, "+
				"then remove sendgrid_api_key from --plugin-config: %w", plugin.ErrFatalMisconfiguration)
		}
		// A config file is present -- for email, for some other plugin, or
		// both -- but it does not name email_enabled. Same quiet disable as
		// no config at all; see Config's doc comment for why config
		// presence alone can no longer mean "this is for email".
		return fmt.Errorf("email: %w", plugin.ErrNotConfigured)
	}
	p.defaultFrom = cfg.DefaultFrom

	if hasLegacyKey {
		p.logger.Warn("email: sendgrid_api_key in --plugin-config is no longer read " +
			"(cleat#1992 part 1); it has no effect. Use " +
			"`cleatctl set-deployment-secret --name email.sendgrid_api_key` instead.")
	}

	p.logger.Info("email: initialized", "has_default_from", p.defaultFrom != "")
	return nil
}

// sendGridAPIKey fetches the current SendGrid API key. Called at the moment
// of use -- send, sendTemplate, checkStatus -- rather than cached, so a key
// rotated with `cleatctl set-deployment-secret` takes effect on the next
// call without a worker restart (plugin.DeploymentSecrets' own doc comment,
// "PER-USE, NOT PER-Init").
func (p *Plugin) sendGridAPIKey(ctx context.Context) (string, error) {
	if p.deploymentSecrets == nil {
		return "", fmt.Errorf("email: no deployment secret store configured")
	}
	key, err := p.deploymentSecrets.Get(ctx, "email.sendgrid_api_key")
	if err != nil {
		return "", fmt.Errorf("email: sendgrid_api_key: %w", err)
	}
	return key, nil
}

// RequiredDeploymentSecrets implements plugin.HasRequiredDeploymentSecrets.
// Unconditional: this is only consulted for a plugin Init already accepted
// (a config section is present), and every call this plugin serves needs the
// SendGrid key, so there is no enabled-but-key-optional case to encode here.
func (p *Plugin) RequiredDeploymentSecrets(config []byte) ([]string, error) {
	return []string{"email.sendgrid_api_key"}, nil
}

// DeploymentSecretPrefix implements plugin.HasDeploymentSecretPrefix: email
// only ever reads "email.sendgrid_api_key".
func (p *Plugin) DeploymentSecretPrefix() string {
	return "email."
}
