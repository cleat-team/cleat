package main

import (
	"log/slog"
	"strings"
)

// pluginPrivateHosts is the operator's set of plugin endpoints permitted to
// resolve into private address space. cleat#1627.
//
// A TYPE rather than a bare map because two things have to travel together: the
// set itself, and the fact that it was configured at all. An empty set and an
// unset flag are the same answer to every question -- permits nothing -- but
// they are different things to say at startup, and an operator who configured
// something and sees nothing logged has learned their flag did not arrive.
type pluginPrivateHosts struct {
	hosts map[string]bool
	raw   []string
}

func newPluginPrivateHosts(entries []string) *pluginPrivateHosts {
	p := &pluginPrivateHosts{hosts: map[string]bool{}, raw: entries}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		e = strings.TrimSuffix(e, ".")
		if e == "" {
			continue
		}
		p.hosts[e] = true
	}
	return p
}

// permits reports whether this host was named by the operator.
//
// EXACT match only, with no suffix form, deliberately. The operator allowlist
// has a leading-dot form for "any host under this domain" because a deployment
// reaches many hosts in a vendor's domain; this names individual endpoints the
// operator runs, and a wildcard over private space would be a much larger grant
// than anything the motivating case needs.
//
// Nil-safe: a guard that never had one of these calls through a nil receiver
// rather than a nil func, and the answer is still no.
func (p *pluginPrivateHosts) permits(host string) bool {
	if p == nil || len(p.hosts) == 0 {
		return false
	}
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	return p.hosts[h]
}

// logStartup records what was configured, because an exemption from a security
// floor should appear in the log of the process that holds it rather than only
// in the command line that started it.
//
// Silent when nothing is configured: a line saying "no exemptions" on every
// worker start is noise that trains people to skip the line that matters.
func (p *pluginPrivateHosts) logStartup(l *slog.Logger) {
	if p == nil || len(p.hosts) == 0 {
		return
	}
	names := make([]string, 0, len(p.hosts))
	for h := range p.hosts {
		names = append(names, h)
	}
	l.Warn("plugin egress: private-address exemptions are configured",
		"hosts", strings.Join(names, ","),
		"scope", "plugin endpoints only; guest fetches and the embedded runner are unaffected",
		"note", "link-local, unspecified, multicast and reserved ranges are refused regardless",
		"issue", "cleat#1627")
}
