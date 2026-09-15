package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// pluginEgressTransport is the RoundTripper every plugin's outbound HTTP goes
// through. cleat#1565, open question 4.
//
// PLUGINS ARE HOST CODE, so this is not the same boundary the guest policy is.
// A plugin is compiled into the worker and could open its own socket; nothing
// here stops a hostile plugin, and migration 063 records the same reasoning for
// why plugin RLS is not a boundary against one either. What this does stop is
// the failure that actually happens: a plugin endpoint that is misconfigured,
// or supplied by a tenant, pointing somewhere it should not reach.
//
// THREE LAYERS, because plugin egress has two shapes and they need different
// answers:
//
//   - The FLOOR always applies. Loopback, link-local, RFC1918 are refused
//     whatever anyone configures, so no plugin endpoint reaches cloud instance
//     metadata or the worker's own admin port. This layer needs no
//     configuration and breaks nothing.
//   - A TENANT'S allowlist applies when a tenant is in context. That is the
//     host-function path -- slacknotify, pagerdutyalert, kafkaconnect, email --
//     where the call is made on behalf of a tenant and the tenant's policy is
//     the right one.
//   - The DEPLOYMENT allowlist applies when no tenant is in context. That is
//     the background-loop path -- datadogexport, notifications, kafkaconnect --
//     which are sweeps and have no tenant, the same shape cleat#1278 records
//     for plugin RLS. A sweep's destination is operator configuration, so it is
//     configured by the operator.
//
// Resolved per REQUEST rather than per plugin, which is what makes one
// transport correct for a client built once at Init: DialContext receives the
// context of the request being dialled, and every one of these plugins already
// uses http.NewRequestWithContext.
func pluginEgressTransport(store *engine.TenantEgressStore, deployment *engine.HostAllowlist) http.RoundTripper {
	guard := &engine.EgressGuard{
		AllowHost: func(ctx context.Context, host string) (bool, error) {
			if tid, ok := auth.TenantIDFromContext(ctx); ok && store != nil {
				list, err := store.For(ctx, tid.String())
				if err != nil {
					// Not a grant. A database briefly unreachable must not be
					// indistinguishable from a tenant that permitted this host.
					return false, err
				}
				return list.Permits(host), nil
			}
			// No tenant: a background sweep. deployment is nil unless the
			// operator set --plugin-egress-allowlist, and a nil list permits
			// nothing -- the same rule the tenant path follows.
			return deployment.Permits(host), nil
		},
	}
	return &http.Transport{DialContext: guard.DialContext}
}

// splitCommaList turns a flag value into entries, dropping empties so that
// "" and "a,,b" both behave.
func splitCommaList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
