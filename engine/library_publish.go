package engine

// cleat#3597 (PR 3b, first half): the operator publishes a library bundle.
//
// Publishing is NOT a store method and NOT a worker route. D40 makes it the cleat
// operator's act, and the database makes that true: only a role holding
// cleat_library_publisher can write library.members, library.bundles and
// library.bundle_members, and that role can INSERT, DELETE and SELECT but never UPDATE
// (migrations/*/005_workflow_libraries.sql). So these functions take the *sql.DB the
// publisher connected with and nothing else, and the application role could not call
// them to any effect. The tenant-facing half is InstallLibraryBundle (library_install.go).
//
// What a publish guarantees:
//
//   - A member is IMMUTABLE and content-addressed. Publishing a (library, name, version)
//     that already exists with the same bytes AND the same entry-point schemas is a no-op;
//     with different bytes, or the same bytes and different schemas (which come from a
//     sidecar file, so the digest cannot vouch for them), it is refused. Nothing is
//     overwritten, because the schema grants no UPDATE and a tenant may be running the
//     old bytes.
//   - A bundle is IMMUTABLE too. Publishing a bundle version that already exists with
//     exactly the same members is a no-op (so a publish that stopped halfway is repaired
//     by running it again); with a different member set it is refused.
//   - A bundle is a lock-consistent set: every ChildVersions pin in a member's stamp must
//     name a member of the same bundle at that version, so a tenant that installs the
//     bundle can never resolve a child the bundle does not contain.
//   - It is ONE transaction. Either the members, the bundle and its listing are all
//     written, or none are.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrLibraryMemberConflict is returned, wrapped in a LibraryMemberConflictError, when a
// member with that key is already published with different bytes, or with the same bytes and
// different entry-point schemas.
var ErrLibraryMemberConflict = errors.New("a different build is already published under this library member")

// ErrLibraryBundleConflict is returned, wrapped in a LibraryBundleConflictError, when the
// bundle version is already published with a different set of members.
var ErrLibraryBundleConflict = errors.New("this bundle version is already published with different members")

// ErrLibraryPublishRefused is returned, wrapped, for a request that is wrong before any
// statement runs: a bad name, a missing stamp, a child pin outside the bundle.
var ErrLibraryPublishRefused = errors.New("the library bundle cannot be published as given")

// LibraryMemberConflictError names the member and both digests.
type LibraryMemberConflictError struct {
	Library, Name    string
	Version          int
	Stored, Supplied string
	// SchemasDiffer is set when the bytes are the same (Stored == Supplied) and the
	// entry-point schemas, which come from a sidecar file and not from the bytes, are not.
	SchemasDiffer bool
}

func (e *LibraryMemberConflictError) Error() string {
	if e.SchemasDiffer {
		return fmt.Sprintf("library %q already has %q version %d published with these exact bytes and different entry_point_schemas. "+
			"The schemas come from the .schema.json beside the binary, not from the bytes, and a published member is immutable "+
			"(install copies the stored schemas to every tenant): publish the changed build under a new version", e.Library, e.Name, e.Version)
	}
	return fmt.Sprintf("library %q already has %q version %d published with sha256 %s; the bytes supplied are %s. "+
		"A published member is immutable: publish the changed build under a new version", e.Library, e.Name, e.Version, e.Stored, e.Supplied)
}

func (e *LibraryMemberConflictError) Is(target error) bool { return target == ErrLibraryMemberConflict }

// checkPublishedMember decides what to do when a member's key is already taken: the same bytes
// AND the same entry-point schemas make the publish a no-op; anything else is refused. The
// schemas are compared as well as the digest because they do not come from the bytes: they are
// read from a sidecar file, so the digest cannot vouch for them, and install copies the stored
// column into every tenant's definition.
func checkPublishedMember(library string, m PublishLibraryMember, c libraryMemberColumns, storedDigest string, storedSchemas sql.NullString) error {
	if storedDigest != c.digest {
		return &LibraryMemberConflictError{Library: library, Name: m.Name, Version: m.Version, Stored: storedDigest, Supplied: c.digest}
	}
	same, err := sameEntryPointSchemas(storedSchemas, c.entryPointSchemas)
	if err != nil {
		return fmt.Errorf("publish library: compare the schemas of member %s@%d: %w", m.Name, m.Version, err)
	}
	if !same {
		return &LibraryMemberConflictError{Library: library, Name: m.Name, Version: m.Version, Stored: storedDigest, Supplied: c.digest, SchemasDiffer: true}
	}
	return nil
}

// sameEntryPointSchemas compares what is stored with what was supplied by MEANING, not by text:
// PostgreSQL's jsonb returns its own spelling (key order, spacing) and SQL Server keeps the text
// it was given, so a text comparison would refuse an honest re-run on one of them. Numbers are
// decoded exactly. No schemas at all is NULL, an empty object, or nothing supplied: all one thing.
func sameEntryPointSchemas(stored sql.NullString, supplied []byte) (bool, error) {
	decode := func(raw []byte) (any, error) {
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil, nil
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if m, ok := v.(map[string]any); ok && len(m) == 0 {
			return nil, nil
		}
		return v, nil
	}
	var storedRaw []byte
	if stored.Valid {
		storedRaw = []byte(stored.String)
	}
	a, err := decode(storedRaw)
	if err != nil {
		return false, fmt.Errorf("stored schemas: %w", err)
	}
	b, err := decode(supplied)
	if err != nil {
		return false, fmt.Errorf("supplied schemas: %w", err)
	}
	return reflect.DeepEqual(a, b), nil
}

// LibraryBundleConflictError says how the stored bundle differs from the one supplied.
type LibraryBundleConflictError struct {
	Library, BundleVersion string
	// Differences is one line per member that is in only one of the two sets, or pinned at a
	// different version, sorted.
	Differences []string
}

func (e *LibraryBundleConflictError) Error() string {
	return fmt.Sprintf("library %q already has a bundle %q with different members (%d difference(s), first: %s). "+
		"A published bundle is immutable: publish the changed set under a new bundle version", e.Library, e.BundleVersion, len(e.Differences), e.Differences[0])
}

func (e *LibraryBundleConflictError) Is(target error) bool { return target == ErrLibraryBundleConflict }

// PublishLibraryMember is one build to publish. Name and Version are the build's own
// stamp (its cleat.metadata workflow_name and workflow_version), not a label the
// publisher chose, so a member cannot be filed under a name its bytes do not claim.
type PublishLibraryMember struct {
	Name       string
	Version    int
	WasmBytes  []byte
	ABIVersion int
	// MinVersion is the stamp's min_compatible_version.
	MinVersion int
	// PluginDeps is the stamp's plugin_deps; stored as given and checked against the
	// registry when a tenant installs, not here.
	PluginDeps map[string]string
	// ChildVersions is the stamp's child_versions. It is validated against the bundle and
	// is not stored: the bytes carry it.
	ChildVersions map[string]int
	EntryPoints   []string
	// Exposure is the declared class; empty means the default, auth.
	Exposure                ExposureClass
	EntryPointSchemas       map[string]EntryPointSchema
	InputValidationDisabled bool
}

// PublishLibraryRequest is one bundle of one library.
type PublishLibraryRequest struct {
	Library       string
	BundleVersion string
	// PublishedBy is recorded beside the bundle. The operator names themselves; it is not
	// authenticated beyond the database role that connected.
	PublishedBy string
	Members     []PublishLibraryMember
}

// PublishLibraryResult says what a publish did.
type PublishLibraryResult struct {
	Library       string `json:"library"`
	BundleVersion string `json:"bundle_version"`
	// MembersWritten is the members this publish added; MembersAlreadyPublished is the
	// members that were already there with the same bytes and schemas.
	MembersWritten          int `json:"members_written"`
	MembersAlreadyPublished int `json:"members_already_published"`
	// BundleAlreadyPublished is true when the bundle and its full listing already existed,
	// which makes the whole publish a no-op.
	BundleAlreadyPublished bool `json:"bundle_already_published"`
}

// LibraryBundleSummary is one row of a listing.
type LibraryBundleSummary struct {
	Library       string                 `json:"library"`
	BundleVersion string                 `json:"bundle_version"`
	PublishedAt   time.Time              `json:"published_at"`
	PublishedBy   string                 `json:"published_by,omitempty"`
	Members       []LibraryMemberSummary `json:"members"`
}

// LibraryMemberSummary is one member of a listed bundle. It carries the digest and size,
// never the bytes.
type LibraryMemberSummary struct {
	Name       string        `json:"name"`
	Version    int           `json:"version"`
	WasmSHA256 string        `json:"wasm_sha256"`
	WasmLength int64         `json:"wasm_length"`
	Exposure   ExposureClass `json:"exposure"`
}

var libraryMemberNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const maxLibraryMemberNameLen = 255

// libraryMemberPrefix is what every member name of a library starts with: the library name and a
// dot. A library name cannot contain a dot, so no two libraries' prefixes overlap.
func libraryMemberPrefix(library string) string { return library + "." }

func publishRefused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLibraryPublishRefused, fmt.Sprintf(format, args...))
}

// ValidatePublishLibraryRequest refuses a request that is wrong on its face, before any
// statement runs and so before any connection is needed. It does not look at the database.
func ValidatePublishLibraryRequest(req PublishLibraryRequest) error {
	if err := ValidateLibraryRef(req.Library, req.BundleVersion); err != nil {
		return fmt.Errorf("%w: %s", ErrLibraryPublishRefused, err.Error())
	}
	if len(req.Members) == 0 {
		return publishRefused("a bundle needs at least one member")
	}
	byName := make(map[string]PublishLibraryMember, len(req.Members))
	for _, m := range req.Members {
		if m.Name == "" || len(m.Name) > maxLibraryMemberNameLen || !libraryMemberNamePattern.MatchString(m.Name) {
			return publishRefused("member name %q is not valid: 1 to %d characters of letters, digits, '.', '_' and '-', starting with a letter or digit", m.Name, maxLibraryMemberNameLen)
		}
		// D59 (cleat#3629): a library's members are named "<library>.<name>". The prefix is what
		// keeps a library's names and a tenant's own names apart, and the deploy and install
		// refusals compare against it. Case-sensitive, and something must follow the dot.
		if prefix := libraryMemberPrefix(req.Library); !strings.HasPrefix(m.Name, prefix) || len(m.Name) == len(prefix) {
			return publishRefused("member %q does not carry the library's prefix %q: every member of library %q must be named %q followed by a name (owner decision D59). "+
				"Set the workflow's name to e.g. %q in its stamp and rebuild it", m.Name, prefix, req.Library, prefix, prefix+"review")
		}
		if m.Version < 1 {
			return publishRefused("member %q has stamp version %d: the stamp must carry a workflow_version of at least 1", m.Name, m.Version)
		}
		if len(m.WasmBytes) == 0 {
			return publishRefused("member %q has no bytes", m.Name)
		}
		if prior, dup := byName[m.Name]; dup {
			return publishRefused("member %q appears twice in the bundle (versions %d and %d): a bundle holds one version of each name", m.Name, prior.Version, m.Version)
		}
		byName[m.Name] = m
		if m.Exposure != "" {
			if _, ok := ParseExposure(string(m.Exposure)); !ok {
				return publishRefused("member %q declares exposure %q, which is not one of auth, internal or public", m.Name, m.Exposure)
			}
		}
	}
	// The lock-consistency rule: every pin names a member of THIS bundle at that version.
	for _, m := range req.Members {
		children := make([]string, 0, len(m.ChildVersions))
		for child := range m.ChildVersions {
			children = append(children, child)
		}
		sort.Strings(children)
		for _, child := range children {
			want := m.ChildVersions[child]
			got, ok := byName[child]
			switch {
			case !ok:
				return publishRefused("member %q pins child %q at version %d, but the bundle has no member named %q", m.Name, child, want, child)
			case got.Version != want:
				return publishRefused("member %q pins child %q at version %d, but the bundle's %q is version %d", m.Name, child, want, child, got.Version)
			}
		}
	}
	return nil
}

// PublishLibraryBundle publishes one bundle through db, which must be connected as a role
// holding cleat_library_publisher. PostgreSQL and SQL Server only (D41).
func PublishLibraryBundle(ctx context.Context, db *sql.DB, d Dialect, req PublishLibraryRequest) (PublishLibraryResult, error) {
	if err := ValidatePublishLibraryRequest(req); err != nil {
		return PublishLibraryResult{}, err
	}
	switch d {
	case DialectPostgres:
		return publishLibraryPostgres(ctx, db, req)
	case DialectMSSQL:
		return publishLibraryMSSQL(ctx, db, req)
	default:
		return PublishLibraryResult{}, ErrLibrariesUnsupported
	}
}

// ListLibraryBundles lists the published bundles, newest first within a library. library
// "" lists every library. It reads only, so any role that can SELECT the library schema
// may call it.
func ListLibraryBundles(ctx context.Context, db *sql.DB, d Dialect, library string) ([]LibraryBundleSummary, error) {
	switch d {
	case DialectPostgres, DialectMSSQL:
		return listLibraryBundles(ctx, db, d, library)
	default:
		return nil, ErrLibrariesUnsupported
	}
}

// bundleMemberKey is how a stored listing and a supplied one are compared.
func bundleMemberKey(name string, version int) string { return fmt.Sprintf("%s@%d", name, version) }

// bundleDifferences compares the stored listing (name -> version) with the supplied one and
// returns one line per difference, sorted; empty means identical.
func bundleDifferences(stored map[string]int, req PublishLibraryRequest) []string {
	var out []string
	supplied := make(map[string]int, len(req.Members))
	for _, m := range req.Members {
		supplied[m.Name] = m.Version
	}
	for name, v := range supplied {
		sv, ok := stored[name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s is supplied but not in the stored bundle", bundleMemberKey(name, v)))
		case sv != v:
			out = append(out, fmt.Sprintf("%q is version %d in the stored bundle and %d in the one supplied", name, sv, v))
		}
	}
	for name, sv := range stored {
		if _, ok := supplied[name]; !ok {
			out = append(out, fmt.Sprintf("%s is in the stored bundle but not supplied", bundleMemberKey(name, sv)))
		}
	}
	sort.Strings(out)
	return out
}
