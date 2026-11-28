package engine

// cleat#3540, PR 3a: a tenant installs a published library bundle.
//
// An install writes ordinary workflow_defs rows into THAT tenant's own table, one
// per member of the bundle, each naming the member it came from (library, name,
// version) and holding no bytes. It grants no power over any other tenant, which is
// why it is callable with the tenant's own key (decision on cleat#3540, D45). What
// it reads, library.members and the bundle tables, the application role can SELECT
// and cannot write; publishing is the operator's act (D40) and is not here.
//
// The interface is optional, like TenantExposurePolicyReader: the Store interface
// has dozens of implementations and mocks, and only two of them can do this. A store
// that cannot is asked nothing and the route says so.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// ErrLibrariesUnsupported is returned by a store that has no library schema:
// MySQL (owner decision D41) and, until cleat#3597, the sharded store.
var ErrLibrariesUnsupported = errors.New("workflow libraries are not supported by this store")

// ErrLibraryBundleNotFound is what InstallLibraryBundle returns, wrapped in a
// LibraryBundleNotFoundError, when no bundle with that library and version exists.
var ErrLibraryBundleNotFound = errors.New("no such library bundle")

// ErrLibraryDefinitionCollision is what InstallLibraryBundle returns, wrapped in a
// LibraryDefinitionCollisionError, when the tenant already holds a definition under
// one of the bundle's member keys that did not come from this library.
var ErrLibraryDefinitionCollision = errors.New("the tenant already has a definition under a name and version the bundle installs")

// LibraryBundleNotFoundError names the bundle that was asked for.
type LibraryBundleNotFoundError struct {
	Library       string
	BundleVersion string
}

func (e *LibraryBundleNotFoundError) Error() string {
	return fmt.Sprintf("library %q has no bundle %q (or the bundle has no members)", e.Library, e.BundleVersion)
}

func (e *LibraryBundleNotFoundError) Is(target error) bool { return target == ErrLibraryBundleNotFound }

// LibraryDefinitionCollisionError names the definition in the way, and where it came
// from. An install never shadows or replaces a definition the tenant deployed, and
// never takes one over from a different library: either would change what the
// tenant's existing runs replay.
type LibraryDefinitionCollisionError struct {
	Name    string
	Version int
	// HeldFromLibrary is the library the existing definition was installed from, or
	// "" when the tenant deployed it itself.
	HeldFromLibrary string
}

func (e *LibraryDefinitionCollisionError) Error() string {
	if e.HeldFromLibrary == "" {
		return fmt.Sprintf("this tenant already deployed a definition %q version %d of its own; an install does not replace it", e.Name, e.Version)
	}
	return fmt.Sprintf("this tenant already holds %q version %d from library %q, not from this one", e.Name, e.Version, e.HeldFromLibrary)
}

func (e *LibraryDefinitionCollisionError) Is(target error) bool {
	return target == ErrLibraryDefinitionCollision
}

// InstallLibraryRequest is one install of one bundle for the store's tenant.
type InstallLibraryRequest struct {
	Library       string
	BundleVersion string
	// InstalledBy is the caller's identity, recorded beside the install.
	InstalledBy string
	// TenantAllowsPublic is the operator's per-tenant opt-in for the `public`
	// exposure class, read by the caller exactly as a deploy reads it. A member that
	// declares `public` is refused for a tenant that has not opted in, the same way
	// a deploy of such an artifact is.
	TenantAllowsPublic bool
}

// InstalledLibraryMember is one definition the install wrote or refreshed.
type InstalledLibraryMember struct {
	Name     string        `json:"name"`
	Version  int           `json:"version"`
	Exposure ExposureClass `json:"exposure"`
}

// InstallLibraryResult says what an install did. PreviousBundleVersion is "" for a
// tenant's first install of the library.
type InstallLibraryResult struct {
	Library               string                   `json:"library"`
	BundleVersion         string                   `json:"bundle_version"`
	PreviousBundleVersion string                   `json:"previous_bundle_version,omitempty"`
	Members               []InstalledLibraryMember `json:"members"`
}

// LibraryInstaller is implemented by a store that can install a library bundle.
//
// InstallLibraryBundle is ONE transaction for the whole bundle: either every member
// is installed and the install is recorded, or nothing is written. A half-installed
// upgrade cannot exist. Installing the bundle a tenant already has is idempotent.
// Installing a different bundle of the same library is an upgrade: the tenant's
// definition rows are refreshed from their members, the record of which bundle the
// tenant holds changes, and installed_at is rewritten.
type LibraryInstaller interface {
	InstallLibraryBundle(ctx context.Context, req InstallLibraryRequest) (InstallLibraryResult, error)
}

var (
	libraryNamePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	libraryBundleVersionExpr = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
)

const (
	maxLibraryNameLen   = 128
	maxBundleVersionLen = 128
)

// ValidateLibraryRef checks the two strings an install names before any statement
// runs. They become a path segment and a column value, so what is accepted is a
// plain token: no slash, no whitespace, no control characters.
func ValidateLibraryRef(library, bundleVersion string) error {
	if library == "" || len(library) > maxLibraryNameLen || !libraryNamePattern.MatchString(library) {
		return fmt.Errorf("library %q is not a valid name: 1 to %d characters of letters, digits, '_' and '-', starting with a letter or digit; a '.' is not allowed because it separates the library name from its members' names (cleat#3629)", library, maxLibraryNameLen)
	}
	if bundleVersion == "" || len(bundleVersion) > maxBundleVersionLen || !libraryBundleVersionExpr.MatchString(bundleVersion) {
		return fmt.Errorf("bundle version %q is not valid: 1 to %d characters of letters, digits, '.', '_', '+' and '-', starting with a letter or digit", bundleVersion, maxBundleVersionLen)
	}
	return nil
}

// resolveInstalledExposure is the exposure an installed member gets: its declared
// class, resolved against the installing tenant's opt-in exactly as a deploy
// resolves an artifact's. The member's stored class is NOT NULL and checked, so the
// declaration is never empty, and the resolved class is therefore the declared one;
// a mismatch would mean the two disagree about the rules and is an error here
// rather than a row written with a class nobody chose.
func resolveInstalledExposure(declared string, tenantAllowsPublic bool, tenantID string) (ExposureClass, error) {
	resolved, err := ResolveDeployableExposure(ExposureClass(declared), "", tenantAllowsPublic, tenantID)
	if err != nil {
		return "", err
	}
	if string(resolved) != declared {
		return "", fmt.Errorf("a library member declares exposure %q but it resolves to %q", declared, resolved)
	}
	return resolved, nil
}

// Each store that can install a bundle says so at compile time, so a signature that
// drifts is a build error and not a route that quietly answers 501.
var (
	_ LibraryInstaller = (*PostgresStore)(nil)
	_ LibraryInstaller = (*MSSQLStore)(nil)
	_ LibraryInstaller = (*MySQLStore)(nil)
)
