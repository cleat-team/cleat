package engine

import (
	"context"
	"sort"
	"strings"
)

// HostAllowlist is an explicit set of hosts egress is permitted to reach.
//
// cleat#1565, owner decision 2026-09-14: an EMPTY LIST MEANS DENY ALL. Not
// "unconfigured, so allow" -- a platform that runs code it did not write must
// not treat the absence of a policy as permission. There are no live
// deployments to migrate, which is what makes the strict reading affordable
// now and would not be later.
//
// Two entry forms, and no others:
//
//	example.com     exact host, case-insensitive
//	.example.com    any host ENDING in .example.com -- so api.example.com
//	                matches and example.com does NOT
//
// The apex is excluded from the suffix form on purpose. ".example.com" reads
// as "subdomains of", and a form that quietly also granted the apex would mean
// an operator writing the narrower-looking entry got the wider grant.
//
// Not a regex and not a glob: both make "what does this permit" a question
// nobody can answer by reading, and this list is a security boundary.
type HostAllowlist struct {
	exact  map[string]bool
	suffix []string
}

// NewHostAllowlist builds a list. Empty input denies everything.
func NewHostAllowlist(hosts ...string) *HostAllowlist {
	l := &HostAllowlist{exact: map[string]bool{}}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.HasPrefix(h, ".") {
			l.suffix = append(l.suffix, h)
			continue
		}
		l.exact[h] = true
	}
	sort.Strings(l.suffix)
	return l
}

// Permits reports whether host is on the list.
func (l *HostAllowlist) Permits(host string) bool {
	if l == nil {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if l.exact[h] {
		return true
	}
	for _, s := range l.suffix {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

// Entries returns the list in a stable order, for logging and for tests.
func (l *HostAllowlist) Entries() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.exact)+len(l.suffix))
	for h := range l.exact {
		out = append(out, h)
	}
	out = append(out, l.suffix...)
	sort.Strings(out)
	return out
}

// AllowHostFunc adapts a list to EgressGuard.AllowHost.
func (l *HostAllowlist) AllowHostFunc() func(context.Context, string) (bool, error) {
	return func(_ context.Context, host string) (bool, error) { return l.Permits(host), nil }
}
