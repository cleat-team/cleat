package backendkit

// cleat#3540, PR 4: installing a workflow library from a backend.
//
// The worker has one tenant-facing library route, POST /api/libraries/{library}/install,
// and this wraps it. Publishing and listing are not routes: `cleatctl library publish|list`
// talk to the database with an operator's publisher login, so a service that holds only a
// tenant key has no way to ask either (cleat#3627 tracks tenant-facing GET routes).
//
// The result and error types are this package's own and do not import engine: engine
// needs cgo and every backend that links backendkit would inherit it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Each install refusal has its own sentinel, because the statuses mean different things on
// this route and a caller acts differently on each. They are matched with errors.Is, and
// every one of them also satisfies errors.As for *UpstreamStatusError.
//
// They are scoped to InstallLibrary and NOT added to classifyError: 400, 404 and 422 mean
// other things on other routes, and a sentinel that named a library for a workflow-start
// refusal would be wrong.
var (
	// ErrLibraryBundleNotFound: 404 with the worker's JSON error. The library or the bundle
	// version has not been published. Retrying does not help; publishing does.
	ErrLibraryBundleNotFound = errors.New("the library or bundle version is not published")

	// ErrLibraryDefinitionCollision: 409. The tenant already holds a definition under a name
	// and version the bundle installs, and it did not come from this library. The install
	// wrote nothing. Retrying does not help.
	ErrLibraryDefinitionCollision = errors.New("the tenant already has a definition under a name and version the bundle installs")

	// ErrLibraryPluginsUnmet: 422. A member declares a plugin no worker has ever registered
	// (owner decision D52), and an install has no override. The error is an
	// *UnmetLibraryPluginsError, which carries the list. It clears once the plugins are
	// deployed to a worker.
	ErrLibraryPluginsUnmet = errors.New("the bundle declares plugins that no worker has registered")

	// ErrLibraryInstallBadRequest: 400. Either the library name or bundle version is not
	// valid, or the bundle declares exposure `public` and the operator has not opted this
	// tenant in. The worker uses one status for both, so tell them apart by the message in
	// the UpstreamStatusError body. Retrying unchanged does not help.
	ErrLibraryInstallBadRequest = errors.New("the worker refused the install request as invalid")

	// ErrLibraryInstallForbidden: 403. The worker's entitlement check refused this tenant the
	// library, or the key may not install. Nothing was written.
	ErrLibraryInstallForbidden = errors.New("this tenant may not install the library")

	// ErrLibraryInstallUnsupported: 501. This deployment cannot install libraries, by design:
	// a sharded deployment does not (owner decision D55, cleat#3606) and MySQL does not (D41).
	// Retrying never helps.
	ErrLibraryInstallUnsupported = errors.New("this deployment cannot install workflow libraries")

	// ErrLibraryRouteMissing: 404 WITHOUT the worker's JSON error, which is what the HTTP mux
	// itself answers for a route it does not have. The worker predates workflow libraries.
	// It is not ErrLibraryBundleNotFound, and publishing the bundle would not help.
	ErrLibraryRouteMissing = errors.New("the worker has no library install route; it predates workflow libraries")
)

// InstalledLibraryMember is one definition an install wrote or refreshed.
type InstalledLibraryMember struct {
	Name     string `json:"name"`
	Version  int    `json:"version"`
	Exposure string `json:"exposure"`
}

// InstallLibraryResult says what an install did. PreviousBundleVersion is "" for a tenant's
// first install of the library, and otherwise the bundle the tenant held before this call.
type InstallLibraryResult struct {
	Library               string                   `json:"library"`
	BundleVersion         string                   `json:"bundle_version"`
	PreviousBundleVersion string                   `json:"previous_bundle_version,omitempty"`
	Members               []InstalledLibraryMember `json:"members"`
}

// MissingPlugin is one unmet plugin dependency of a bundle.
//
// The tags are the keys the worker sends TODAY, which are the Go field names: the worker's
// own type has no JSON tags. "DeclaredBy" is not "declared_by" under encoding/json's
// case-insensitive match, so a lower-case snake_case tag here would decode to an empty
// string and nothing would say so.
type MissingPlugin struct {
	Name string `json:"Name"`
	// Constraint is the version constraint as the member declared it. When two members
	// declare the plugin differently, both appear ("a; b").
	Constraint string `json:"Constraint"`
	// DeclaredBy names the member that declared the dependency.
	DeclaredBy string `json:"DeclaredBy"`
}

// UnmetLibraryPluginsError is the 422 an install answers when the bundle needs plugins no
// worker has registered. It matches ErrLibraryPluginsUnmet with errors.Is, and a caller
// gets the list with errors.As.
type UnmetLibraryPluginsError struct {
	// Missing lists every unmet dependency, not the first.
	Missing []MissingPlugin
	// Truncated is true when the worker's reply was longer than this client reads (4096
	// bytes) and the list could not be decoded. Missing is then empty or short, and "no
	// missing plugins" is NOT what that means: deploy what the message names and try again.
	Truncated bool
	// Message is the worker's own sentence.
	Message string

	upstream *UpstreamStatusError
}

func (e *UnmetLibraryPluginsError) Error() string {
	if e.Truncated {
		return fmt.Sprintf("%s (the list of missing plugins was cut off by the %d-byte read limit): %s",
			ErrLibraryPluginsUnmet.Error(), doRequestErrorBodyLimit, e.upstream.Error())
	}
	return ErrLibraryPluginsUnmet.Error() + ": " + e.Message
}

// Unwrap lets errors.Is match ErrLibraryPluginsUnmet and errors.As reach the
// *UpstreamStatusError.
func (e *UnmetLibraryPluginsError) Unwrap() []error {
	return []error{ErrLibraryPluginsUnmet, e.upstream}
}

// InstallLibrary installs the published bundle `bundleVersion` of `library` into the
// tenant this client's key belongs to. The install is one transaction: every member is
// written and the install recorded, or nothing is. Installing the bundle the tenant
// already holds is idempotent; installing a different bundle of the same library is an
// upgrade, and in-flight runs stay on the definitions they started on.
//
// Install is callable with the tenant's own key (publish is operator-only, owner decision
// D40, and is not here). The tenant is the key's: Client.TenantID plays no part.
//
// A sharded deployment and a MySQL deployment answer 501 by design (owner decisions D55
// and D41), which is ErrLibraryInstallUnsupported. The refusals are listed on the
// ErrLibrary* variables; any other non-2xx is a plain *UpstreamStatusError.
func (c *Client) InstallLibrary(ctx context.Context, library, bundleVersion string) (*InstallLibraryResult, error) {
	if library == "" || bundleVersion == "" {
		return nil, errors.New("install library: a library name and a bundle version are both required")
	}
	body, err := json.Marshal(map[string]string{"bundle_version": bundleVersion})
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/libraries/"+url.PathEscape(library)+"/install", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doRequest(req)
	if err != nil {
		var up *UpstreamStatusError
		if errors.As(err, &up) {
			return nil, fmt.Errorf("install library %q: %w", library, classifyLibraryInstall(up))
		}
		return nil, fmt.Errorf("install library %q: %w", library, err)
	}
	defer resp.Body.Close()

	var result InstallLibraryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// classifyLibraryInstall turns the upstream refusal of an install into the typed error for
// its status, wrapping the *UpstreamStatusError so the status and body stay reachable.
func classifyLibraryInstall(up *UpstreamStatusError) error {
	switch up.Status {
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %w", ErrLibraryInstallBadRequest, up)
	case http.StatusForbidden:
		return fmt.Errorf("%w: %w", ErrLibraryInstallForbidden, up)
	case http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrLibraryDefinitionCollision, up)
	case http.StatusNotImplemented:
		return fmt.Errorf("%w: %w", ErrLibraryInstallUnsupported, up)
	case http.StatusNotFound:
		// The worker writes {"error": "..."} for a bundle it could not find. The mux writes
		// plain text for a route it does not have, and only the second means the worker is
		// too old to know libraries.
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(up.Body, &e) == nil && e.Error != "" {
			return fmt.Errorf("%w: %w", ErrLibraryBundleNotFound, up)
		}
		return fmt.Errorf("%w: %w", ErrLibraryRouteMissing, up)
	case http.StatusUnprocessableEntity:
		return unmetLibraryPlugins(up)
	}
	return up
}

func unmetLibraryPlugins(up *UpstreamStatusError) error {
	var parsed struct {
		Error          string          `json:"error"`
		MissingPlugins []MissingPlugin `json:"missing_plugins"`
	}
	if err := json.Unmarshal(up.Body, &parsed); err != nil {
		// The reply was cut at doRequest's read limit, or is not the worker's. Say that the
		// list is unknown rather than report an empty one.
		return &UnmetLibraryPluginsError{
			Truncated: len(up.Body) >= doRequestErrorBodyLimit,
			Message:   strings.TrimSpace(string(up.Body)),
			upstream:  up,
		}
	}
	return &UnmetLibraryPluginsError{Missing: parsed.MissingPlugins, Message: parsed.Error, upstream: up}
}
