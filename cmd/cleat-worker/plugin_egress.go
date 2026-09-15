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
// pluginEgressTransport is the RoundTripper every plugin's outbound HTTP goes
// through.
//
// operator is the deployment's policy and applies to everything. tenantStore is
// consulted only when a tenant is actually in context.
func pluginEgressTransport(tenantStore *engine.TenantEgressStore, operator *engine.HostAllowlist) http.RoundTripper {
	guard := &engine.EgressGuard{
		OperatorAllows: operatorAllowFunc(operator),

		// A plugin's client is built once and serves BOTH shapes, so the
		// tenant-less case is decided per call rather than per transport: a
		// host-function call has a tenant in context, a background sweep does
		// not. That is not a hole -- a sweep answers to the operator list
		// alone, which is why that list had to become general rather than
		// remain a plugin-shaped flag.
		TenantOptional: func(ctx context.Context) bool {
			_, ok := auth.TenantIDFromContext(ctx)
			return !ok
		},

		AllowHost: func(ctx context.Context, host string) (bool, error) {
			tid, ok := auth.TenantIDFromContext(ctx)
			if !ok || tenantStore == nil {
				// Unreachable when TenantOptional is doing its job; kept as a
				// refusal rather than a grant so that a future change to that
				// predicate fails closed.
				return false, nil
			}
			list, err := tenantStore.For(ctx, tid.String())
			if err != nil {
				// Not a grant. A database briefly unreachable must not be
				// indistinguishable from a tenant that permitted this host.
				return false, err
			}
			return list.Permits(host), nil
		},
	}
	return &http.Transport{DialContext: guard.DialContext}
}

// operatorAllowFunc turns the deployment list into the guard's operator hook.
//
// A nil or empty list returns nil, which the guard reads as UNSET and permits
// every public host -- owner decision 2026-09-15. That is safe to default open
// only because the floor sits underneath it.
func operatorAllowFunc(l *engine.HostAllowlist) func(context.Context, string) (bool, error) {
	if l == nil || len(l.Entries()) == 0 {
		return nil
	}
	return l.AllowHostFunc()
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
