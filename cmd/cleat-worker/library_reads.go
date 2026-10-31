package main

// cleat#3627: the tenant-facing reads that go with POST /api/libraries/{library}/install.
//
//	GET /api/library-installs   what THIS tenant holds (tenant_library_installs, its own rows)
//	GET /api/libraries          what is published (the catalogue `cleatctl library list` reads)
//
// Both are callable with the tenant's own key, so a service that holds only that (backendkit,
// clew-service) can ask what it has installed and what it could install without a database
// handle. They are read-only and add no scope: the same authentication as the install, answering
// for the caller's tenant. The installs list is the tenant's own and no other's (the store holds
// that with both a tenant-scoped transaction and an explicit tenant_id; see
// engine/library_installed.go). The catalogue is global, because any tenant may install any
// published library (D45), and goes through the same checkLibraryEntitlement hook the install
// calls, once per library, so the list never offers a library the install would refuse.
//
// Two paths, and not /api/libraries/installed, so no library name can collide with a reserved word.
//
// A store that cannot do libraries answers 501, as the install does: a sharded deployment (D55,
// cleat#3606) has no store that implements these, and MySQL (D41) refuses.

import (
	"errors"
	"net/http"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// librariesUnavailableMessage is the 501 body. Kept beside the install's sentence, which says the same thing.
const librariesUnavailableMessage = "this deployment's store cannot list workflow libraries (a sharded deployment cannot: serving many tenants on a sharded worker is not supported yet, cleat#3606; MySQL has no libraries)"

// handleListLibraryInstalls serves GET /api/library-installs: {"installs": [...]}.
func (s *apiServer) handleListLibraryInstalls(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	lister, ok := st.(engine.LibraryInstallLister)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, librariesUnavailableMessage)
		return
	}
	installs, err := lister.ListInstalledLibraries(r.Context())
	if err != nil {
		s.writeLibraryReadError(w, err)
		return
	}
	if installs == nil {
		installs = []engine.InstalledLibrary{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"installs": installs})
}

// handleListLibraries serves GET /api/libraries[?library=<name>]: {"bundles": [...]}.
func (s *apiServer) handleListLibraries(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	catalogue, ok := st.(engine.LibraryCatalogue)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, librariesUnavailableMessage)
		return
	}
	bundles, err := catalogue.ListPublishedLibraries(r.Context(), r.URL.Query().Get("library"))
	if err != nil {
		s.writeLibraryReadError(w, err)
		return
	}

	tid := ""
	if id, ok := auth.TenantIDFromContext(r.Context()); ok {
		tid = id.String()
	}
	// One question to the hook per library, not per bundle. A refused library is left out whole.
	entitled := map[string]bool{}
	out := []engine.LibraryBundleSummary{}
	for _, b := range bundles {
		allowed, asked := entitled[b.Library]
		if !asked {
			allowed = s.checkLibraryEntitlement(r.Context(), tid, b.Library) == nil
			entitled[b.Library] = allowed
		}
		if !allowed {
			continue
		}
		// Who published is an operator's identity and a tenant has no use for it.
		b.PublishedBy = ""
		out = append(out, b)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"bundles": out})
}

func (s *apiServer) writeLibraryReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, engine.ErrLibrariesUnsupported) {
		s.writeError(w, http.StatusNotImplemented, librariesUnavailableMessage)
		return
	}
	s.writeError(w, http.StatusInternalServerError, err.Error())
}
