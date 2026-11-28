package backendkit

// cleat#3627: reading what a tenant holds and what is published, from a backend.
//
// Two tenant-key GETs next to POST /api/libraries/{library}/install: GET /api/library-installs
// (the bundles the key's tenant has installed) and GET /api/libraries (the published catalogue).
// Before them a service holding only a tenant key had no way to ask either without a database
// handle. The types are this package's own, as InstallLibrary's are: engine needs cgo.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ErrLibraryReadUnsupported: 501. This deployment cannot list workflow libraries, by design: a
// sharded deployment (owner decision D55, cleat#3606) and MySQL (D41) cannot. Retrying never helps.
var ErrLibraryReadUnsupported = errors.New("this deployment cannot list workflow libraries")

// InstalledLibrary is one library bundle the key's tenant holds.
type InstalledLibrary struct {
	Library       string    `json:"library"`
	BundleVersion string    `json:"bundle_version"`
	InstalledAt   time.Time `json:"installed_at"`
	// InstalledBy is the subject the install ran as, or "" when none was recorded.
	InstalledBy string `json:"installed_by,omitempty"`
}

// PublishedLibraryMember is one member of a published bundle: its digest and size, never its bytes.
type PublishedLibraryMember struct {
	Name       string `json:"name"`
	Version    int    `json:"version"`
	WasmSHA256 string `json:"wasm_sha256"`
	WasmLength int64  `json:"wasm_length"`
	Exposure   string `json:"exposure"`
}

// PublishedLibraryBundle is one published bundle. The catalogue does not say who published it.
type PublishedLibraryBundle struct {
	Library       string                   `json:"library"`
	BundleVersion string                   `json:"bundle_version"`
	PublishedAt   time.Time                `json:"published_at"`
	Members       []PublishedLibraryMember `json:"members"`
}

// InstalledLibraries lists the library bundles the tenant this client's key belongs to holds,
// ordered by library. A tenant with none gets an empty slice. The tenant is the key's:
// Client.TenantID plays no part, and another tenant's installs are never returned.
//
// A sharded or MySQL deployment answers 501, which is ErrLibraryReadUnsupported. A worker older
// than this client does not serve the route and answers 404, which is ErrLibraryRouteMissing:
// a list has no "not found" of its own, so any 404 here means the route.
func (c *Client) InstalledLibraries(ctx context.Context) ([]InstalledLibrary, error) {
	var body struct {
		Installs []InstalledLibrary `json:"installs"`
	}
	if err := c.getLibraryRead(ctx, "/api/library-installs", &body); err != nil {
		return nil, fmt.Errorf("list installed libraries: %w", err)
	}
	if body.Installs == nil {
		body.Installs = []InstalledLibrary{}
	}
	return body.Installs, nil
}

// PublishedLibraries lists the published library bundles a tenant may install, newest first
// within a library. library "" lists every library; otherwise only that one. Any tenant may
// install any published library (owner decision D45) unless the worker's entitlement check
// refuses it, in which case the library is left out of this list too.
//
// Errors as InstalledLibraries.
func (c *Client) PublishedLibraries(ctx context.Context, library string) ([]PublishedLibraryBundle, error) {
	path := "/api/libraries"
	if library != "" {
		path += "?library=" + url.QueryEscape(library)
	}
	var body struct {
		Bundles []PublishedLibraryBundle `json:"bundles"`
	}
	if err := c.getLibraryRead(ctx, path, &body); err != nil {
		return nil, fmt.Errorf("list published libraries: %w", err)
	}
	if body.Bundles == nil {
		body.Bundles = []PublishedLibraryBundle{}
	}
	return body.Bundles, nil
}

func (c *Client) getLibraryRead(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := c.doRequest(req)
	if err != nil {
		var up *UpstreamStatusError
		if errors.As(err, &up) {
			switch up.Status {
			case http.StatusNotImplemented:
				return fmt.Errorf("%w: %w", ErrLibraryReadUnsupported, up)
			case http.StatusNotFound:
				return fmt.Errorf("%w: %w", ErrLibraryRouteMissing, up)
			}
		}
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
