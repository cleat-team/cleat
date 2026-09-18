package main

import (
	"fmt"
	"net/url"
	"strings"
)

// parseServiceEndpoints turns --service-endpoints into a service -> base URL map.
//
// The format is `name=url`, comma separated, matching --egress-allowlist's
// shape rather than inventing a second one:
//
//	--service-endpoints "billing=https://billing.internal,crm=https://crm.internal"
//
// KEYED BY SERVICE, NOT BY SERVICE.OPERATION. forwardToService already builds
// `{base}/call/{service}/{operation}`, so the operation is a route on the
// service rather than a separate registration. A service that needs two base
// URLs for two operations is two services.
//
// EVERY ERROR HERE IS FATAL AT BOOT, and that is the point of validating in one
// place rather than at the call site. An endpoint that is missing, malformed or
// http-on-a-typo is a configuration mistake, and the moment to report a
// configuration mistake is startup -- not the first time a workflow reaches
// that service, which may be days later, inside a retry, with the error
// surfacing as a failed call rather than as a bad flag.
func parseServiceEndpoints(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, entry := range splitCommaList(v) {
		name, raw, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("service endpoint %q is not name=url", entry)
		}
		name = strings.TrimSpace(name)
		raw = strings.TrimSpace(raw)
		if name == "" || raw == "" {
			return nil, fmt.Errorf("service endpoint %q has an empty name or url", entry)
		}
		if strings.Contains(name, ".") {
			// The guest calls h.DurableCall("billing", "charge", ...), so the
			// service is the first argument alone. Accepting "billing.charge"
			// here would register a key nothing ever looks up, and the failure
			// would be a "not configured" error naming a service that is in
			// the config file.
			return nil, fmt.Errorf("service endpoint name %q contains a dot: register the "+
				"service, not service.operation -- the operation is a route on it", name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("service %q is registered twice", name)
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("service %q: url must be http or https, got %q", name, u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("service %q: url has no host: %q", name, raw)
		}
		out[name] = strings.TrimRight(raw, "/")
	}
	return out, nil
}
