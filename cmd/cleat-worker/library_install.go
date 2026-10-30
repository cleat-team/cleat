package main

// cleat#3540, PR 3a: POST /api/libraries/{library}/install.
//
// A tenant installs a published library bundle with its OWN key. The install writes only
// that tenant's definition rows and records the install; it reads a library schema the
// application role can SELECT and not write, so it grants no power over anyone else.
// Publishing is the operator's act and is not here (D40).
//
// Every published library is installable by every tenant (D45). That is a decision, and
// the one place it would change is checkLibraryEntitlement below: a single function
// called before anything is written. It allows by default.

import (
	"context"
	"errors"
	"net/http"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// libraryBundleNotFoundDetail is the machine-readable code on the 404 an install answers when the
// library or bundle version is not published. A 404 without it did not come from this handler.
const libraryBundleNotFoundDetail = "library_bundle_not_found"

// libraryEntitlementCheck decides whether a tenant may install a library. A non-nil
// error refuses, and the install writes nothing.
type libraryEntitlementCheck func(ctx context.Context, tenantID, library string) error

// checkLibraryEntitlement is the one hook point for a per-tenant entitlement. With no
// check configured every tenant may install every library (D45).
func (s *apiServer) checkLibraryEntitlement(ctx context.Context, tenantID, library string) error {
	if s.libraryEntitlement == nil {
		return nil
	}
	return s.libraryEntitlement(ctx, tenantID, library)
}

// handleInstallLibrary serves POST /api/libraries/{library}/install with a body of
// {"bundle_version": "..."}.
func (s *apiServer) handleInstallLibrary(w http.ResponseWriter, r *http.Request) {
	library := r.PathValue("library")
	var req struct {
		BundleVersion string `json:"bundle_version"`
	}
	if !s.decodeJSONBody(w, r, s.configuredBodyLimit(), &req) {
		return
	}
	if err := engine.ValidateLibraryRef(library, req.BundleVersion); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	installer, ok := st.(engine.LibraryInstaller)
	if !ok {
		s.writeError(w, http.StatusNotImplemented,
			"this deployment's store cannot install workflow libraries (a sharded deployment does not: serving many tenants on a sharded worker is not supported yet, cleat#3606, though a library can be published to every shard)")
		return
	}

	tid := ""
	if id, ok := auth.TenantIDFromContext(r.Context()); ok {
		tid = id.String()
	}
	subject, _ := auth.SubjectFromContext(r.Context())

	// Before anything is read or written. A refusal here leaves no trace.
	if err := s.checkLibraryEntitlement(r.Context(), tid, library); err != nil {
		s.writeError(w, http.StatusForbidden, err.Error())
		return
	}

	// The operator's per-tenant opt-in for `public`, read exactly as the deploy route
	// reads it: a store that cannot say is "not opted in".
	tenantAllowsPublic := false
	if reader, ok := st.(engine.TenantExposurePolicyReader); ok && tid != "" {
		allowed, err := reader.AllowsPublicExposure(r.Context(), tid)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "checking public exposure opt-in: "+err.Error())
			return
		}
		tenantAllowsPublic = allowed
	}

	res, err := installer.InstallLibraryBundle(r.Context(), engine.InstallLibraryRequest{
		Library:            library,
		BundleVersion:      req.BundleVersion,
		InstalledBy:        subject,
		TenantAllowsPublic: tenantAllowsPublic,
	})
	if err != nil {
		var notOptedIn *engine.ErrExposurePublicNotOptedIn
		var unmet *engine.UnmetPluginDepsError
		switch {
		case errors.As(err, &unmet):
			// cleat#3600 (owner D52): a member declares a plugin no worker has ever
			// registered. 422 like a refused deploy, with every missing plugin and the member
			// that declared it. There is no override on an install.
			s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":           err.Error(),
				"missing_plugins": unmet.Missing,
			})
		case errors.Is(err, engine.ErrPluginSourceUnavailable):
			s.writeError(w, http.StatusNotImplemented, err.Error())
		case errors.Is(err, engine.ErrLibrariesUnsupported):
			s.writeError(w, http.StatusNotImplemented, err.Error())
		case errors.Is(err, engine.ErrLibraryBundleNotFound):
			// 404 alone cannot say what was not found: apiAware404 answers every unmatched
			// /api/ path with a JSON 404 too, so a client talking to a worker that lacks this
			// route sees the same status and the same {"error": ...} shape. The detail is the
			// positive marker that THIS handler ran and the bundle is what is missing
			// (backendkit.Client.InstallLibrary reads it).
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error(), "detail": libraryBundleNotFoundDetail})
		case errors.Is(err, engine.ErrLibraryDefinitionCollision):
			s.writeError(w, http.StatusConflict, err.Error())
		case errors.As(err, &notOptedIn):
			// The caller's tenant cannot hold what the bundle declares: 400, as a deploy
			// of such an artifact is.
			s.writeError(w, http.StatusBadRequest, err.Error())
		default:
			s.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}
