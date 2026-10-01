package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// pluginForTenantLedger lists every plugin.ForTenant call site, keyed the same
// way crossTenantLedger, perTenantLoopLedger and secretsForTenantLedger are:
// "<repo-relative file>:<enclosing function>".
//
// WHY THIS EXISTS, SEPARATELY FROM THE OTHER THREE. plugin.ForTenant(ctx, id)
// rewrites the tenant a context carries -- it is how a plugin marks a context
// for its OWN SQL (cleat#2125) when it has no request-scoped ctx to inherit a
// tenant from: a background loop with no request, or an unauthenticated
// request that names its own tenant out-of-band. Nothing tracked where it was
// called before this (cleat#2164): a plugin calling it on a REQUEST context
// silently makes every tenant-scoped operation on that request act as another
// tenant, including #1992's Environment.Secrets request-path methods, whose
// design depends on the tenant coming from the context. Measured: tenant B's
// request context, plugin.ForTenant(reqB, A), Secrets.Get returned tenant A's
// value.
//
// This is the SQL-side counterpart to secretsForTenantLedger (the
// Secrets/Payloads-side ledger for the same kind of question) and is kept
// separate from it for the same reason that file gives for staying separate
// from perTenantLoopLedger: the two check different shapes closely enough that
// folding them together would make one file's diff answer a question about the
// other's scanner. Documented as C20 in
// docs/contributor/plugins/plugin-contract.md, kept in Tenant isolation
// beside C15/C16/C17 though its number comes after C19 -- added later, per
// that section's own convention for C15's number.
//
// WHAT IT DOES NOT COVER, said out loud so a green is not read as more than it
// is (same caveat crossTenantLedger's own doc gives): this guard checks the
// idiom, not every spelling. Hand-written SQL against admin.tenants, or a
// tenant read out of a header/claim and threaded some other way than
// plugin.ForTenant, is invisible to it. It answers "did this plugin re-mark a
// context through the one function that does that", not "is this plugin's
// tenant handling correct".
var pluginForTenantLedger = map[string]bool{
	// Request path: source.TenantID comes from the URL-derived source row this
	// handler already looked up (discoverCtx, cleat#1538), not from the
	// caller's own credential -- the external webhook sender holds no cleat
	// credential at all, so r.Context() carries no tenant to inherit.
	"plugins/webhookingest/routes.go:(*Plugin).handleIngestWebhook": true,

	// Background export loop (a plugin_lease-elected leader sweeping every
	// tenant's configs), no request in scope. cfg.TenantID comes off the
	// config row being exported. Same function as secretsForTenantLedger's
	// exportForConfig entry for that function's Secrets.ForTenant call two
	// lines below this one.
	"plugins/datadogexport/background.go:(*Plugin).exportForConfig": true,

	// Background delivery retry loop, no request in scope -- same function as
	// secretsForTenantLedger's "deliver" entry: this is the SQL-side ForTenant
	// call scoping the config lookup that later yields cfg.TenantID for that
	// Secrets.ForTenant call.
	"plugins/notifications/background.go:(*Plugin).deliver": true,

	// Background sweep loop over every tenant (AllTenantIDs, perTenantLoopLedger's
	// shape for this same function): id is the loop's own iteration variable.
	"plugins/blobstore/background.go:(*Plugin).allInFlightWorkflowIDsMSSQL": true,

	// getConfig is called from both an authenticated admin route and
	// handleLogin, which is unauthenticated by design (it accepts
	// ?tenant_id= because a login has no session yet, cleat#1512) -- scoping
	// by the tenantID PARAMETER rather than by whatever the request happens to
	// carry is correct on both paths, not a workaround for the unauthenticated
	// one.
	"plugins/oauthprovider/routes.go:(*Plugin).getConfig": true,

	// handleLogin: unauthenticated by definition (the flow has not started
	// yet); tid may have come from ?tenant_id=, so nothing has put it in the
	// context carrier the policy reads.
	"plugins/oauthprovider/routes.go:(*Plugin).handleLogin": true,

	// finishLogin: the identity provider's callback carries no cleat
	// credential. tid is the tenant the earlier oauth_sessions state row
	// named, not anything from r.Context() -- the UPDATE that follows
	// addresses the row by id with no tenant predicate, so this ForTenant is
	// what stops a state collision from writing another tenant's session.
	"plugins/oauthprovider/routes.go:(*Plugin).finishLogin": true,

	// handleListSessions/handleDeleteSession: session.TenantID comes from
	// extractSession's own lookup, which is this handler's actual
	// authentication -- r.Context() carries nothing to inherit here.
	"plugins/oauthprovider/routes.go:(*Plugin).handleListSessions":  true,
	"plugins/oauthprovider/routes.go:(*Plugin).handleDeleteSession": true,

	// identityAllowed runs on the same unauthenticated callback as
	// finishLogin and for the same reason: tid is the state row's tenant, the
	// only reliable value on this path.
	"plugins/oauthprovider/identity.go:(*Plugin).identityAllowed": true,

	// Background retry loop, built from context.Background() to both
	// detach from the tick's cancellation and avoid inheriting the
	// sweep's AcrossAllTenants bypass, which a ForTenant on top of would
	// silently no-op (cleat#1515).
	"plugins/eventtriggers/background.go:(*Plugin).processBatch": true,

	// PublishEvent: tenantID is the webhook_sources row's own tenant, which
	// must be used even when the request context disagrees -- the value
	// comes from a lookup keyed on the event, not from the requester
	// (cleat#1538). Scoping once here, rather than at each of the three
	// downstream tenant-scoped calls this ctx feeds, is what makes that
	// correct by construction instead of by every caller's convention.
	"plugins/eventtriggers/publish.go:PublishEvent": true,

	// Background abandonment sweep over every tenant (AllTenantIDs): id is
	// the loop's own iteration variable, no request in scope.
	"plugins/jobqueue/background.go:(*Plugin).sweepAbandonedJobsPerTenant": true,

	// appendEvent builds from context.Background() with a timeout so a write
	// survives the originating request's cancellation -- which discards the
	// tenant, and audit_events' row-level policy refuses an insert with none
	// (cleat#1278). ForTenant puts it back; deliberately not AcrossAllTenants,
	// which would also pass every test here while disabling isolation for
	// every audit write.
	"plugins/auditlog/queue.go:(*Plugin).appendEvent": true,

	// ExportTenant/retainTenant/VerifyChain: each takes an explicit tenant
	// parameter from its own caller (an admin export request, the retention
	// sweep's own AllTenantIDs loop, a verify request) rather than an HTTP
	// request -- there is no request-scoped ctx to inherit on any of these
	// paths, only the parameter.
	"plugins/auditlog/export.go:ExportTenant":                    true,
	"plugins/auditlog/chain_retention.go:(*Plugin).retainTenant": true,
	"plugins/auditlog/verify.go:VerifyChain":                     true,

	// slacknotify's interactive-message callback: Slack posts here directly
	// with no cleat credential. tid is resolved from slack_workspace, keyed
	// on the callback payload -- this is the only point in the handler that
	// ever sets a tenant on the ctx DeliverSignal's chain scopes its
	// statement to.
	"plugins/slacknotify/interactive.go:(*Plugin).handleInteractiveCallback": true,
}

// pluginForTenantSite is one plugin.ForTenant-shaped call, located by go/ast --
// same reasoning as crossTenantSite, perTenantLoopSite and secretsForTenantSite:
// a regex cannot tell a call from a comment describing one (plugin/secrets.go
// has exactly such a comment, at the line this ledger's own doc quotes it
// from), and a parser does not need to try.
type pluginForTenantSite struct {
	File string // repo-relative
	Func string // enclosing function, "(*Recv).Name" for a method
	Line int
}

func (s pluginForTenantSite) key() string { return s.File + ":" + s.Func }

// isPluginForTenantCall matches both spellings, the same way isAcrossAllTenants
// does for plugin.AcrossAllTenants: plugin.ForTenant(ctx, tenantID) from
// outside the package, and the bare ForTenant(ctx, tenantID) form a call from
// within package plugin itself would use. This is the exact complement of
// isSecretsForTenant's exclusion in a_secrets_for_tenant_is_declared_test.go:
// that scanner matches every X.ForTenant(...) EXCEPT this one; this scanner
// matches ONLY this one.
func isPluginForTenantCall(fun ast.Expr) bool {
	switch e := fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		return ok && pkg.Name == "plugin" && e.Sel.Name == "ForTenant"
	case *ast.Ident:
		return e.Name == "ForTenant"
	}
	return false
}

// scanPluginForTenantSites parses each file and returns every plugin.ForTenant
// call. Shares trackedGoFiles/funcName/repoRoot with the other three ledger
// scanners (a_cross_tenant_bypass_is_declared_test.go, same package).
func scanPluginForTenantSites(t *testing.T, root string, files []string) []pluginForTenantSite {
	t.Helper()
	fset := token.NewFileSet()
	var sites []pluginForTenantSite

	for _, rel := range files {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		assertDefaultPluginImportName(t, f, rel)
		tracked := gateBareIdentOnPackage(isPluginForTenantCall, f.Name.Name == "plugin")
		assertNoPackageScopeReferenceEscapes(t, f, fset, rel, tracked)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := funcName(fn)
			assertNoTrackedReferenceEscapes(t, fn, fset, rel, name, tracked)
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !tracked(call.Fun) {
					return true
				}
				sites = append(sites, pluginForTenantSite{
					File: rel,
					Func: name,
					Line: fset.Position(call.Pos()).Line,
				})
				return true
			})
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites
}

// TestEveryPluginForTenantCallIsDeclared is pluginForTenantLedger's ratchet,
// checked in both directions for the same reason
// TestEveryCrossTenantBypassIsDeclared, TestEveryPerTenantLoopIsDeclared and
// TestEverySecretsForTenantCallIsDeclared are: an undeclared site is a new
// tenant-naming call nobody reviewed, and a stale entry is a grant covering
// something that is not there.
func TestEveryPluginForTenantCallIsDeclared(t *testing.T) {
	root := repoRoot(t)
	sites := scanPluginForTenantSites(t, root, trackedGoFiles(t, root))

	found := map[string]pluginForTenantSite{}
	for _, s := range sites {
		found[s.key()] = s
	}

	if len(found) == 0 {
		t.Fatal("scanPluginForTenantSites found 0 call sites in the live tree -- " +
			"plugin.ForTenant has real callers today, so this means the scanner " +
			"cannot see them, not that they are gone")
	}

	for key, site := range found {
		if !pluginForTenantLedger[key] {
			t.Errorf("undeclared plugin.ForTenant call at %s:%d\n"+
				"  plugin.ForTenant re-marks a context's tenant. Add %q to\n"+
				"  pluginForTenantLedger with a one-line reason this call site has no\n"+
				"  request-scoped tenant to inherit, or use the request's own context\n"+
				"  instead if one is actually available on this path -- a ForTenant call\n"+
				"  that did not need to exist is the failure this ledger is for.",
				site.File, site.Line, key)
		}
	}
	for key := range pluginForTenantLedger {
		if _, ok := found[key]; !ok {
			t.Errorf("stale pluginForTenantLedger entry %q: no plugin.ForTenant call there.\n"+
				"  The call was removed or its function was renamed. A ledger line that\n"+
				"  matches nothing is a grant covering something that is not there -- it\n"+
				"  will silently cover the NEXT thing to take that name.", key)
		}
	}
}

// TestThePluginForTenantScannerReportsAnUndeclaredSite is the known-positive.
// TestEveryPluginForTenantCallIsDeclared answers "does it pass when the tree is
// fine?", which every broken version of it also answers yes to. This is the
// test that answers the harder question: does it REPORT a case that is
// genuinely wrong? Reuses testdata/barecontext/unscoped.go's theCorrectForm,
// which already calls plugin.ForTenant and is excluded from the live scan by
// living under testdata/ -- same fixture TestTheSecretsForTenantScannerIgnores
// ThePluginForTenantFreeFunction below reuses for the opposite assertion.
func TestThePluginForTenantScannerReportsAnUndeclaredSite(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "barecontext", "unscoped.go")

	sites := scanPluginForTenantSites(t, root, []string{fixture})
	if len(sites) != 1 {
		t.Fatalf("scanner found %d site(s) in the fixture, want 1 -- it can no longer see "+
			"what it is looking for, and would report a real undeclared plugin.ForTenant "+
			"call as absent", len(sites))
	}
	if pluginForTenantLedger[sites[0].key()] {
		t.Fatalf("the fixture site %s is in the ledger; it must stay undeclared, "+
			"or this test proves nothing", sites[0].key())
	}
}

// TestThePluginForTenantScannerIgnoresSecretsForTenant is the negative control
// for isPluginForTenantCall's exclusion, the mirror of
// TestTheSecretsForTenantScannerIgnoresThePluginForTenantFreeFunction: a
// Secrets.ForTenant / TenantSecrets call -- X.ForTenant(...) for any X other
// than the package identifier "plugin" -- must NOT be reported here. Without
// this control, a matcher that accepted every ".ForTenant(" call would make
// TestEveryPluginForTenantCallIsDeclared fail on the live tree wherever
// secretsForTenantLedger's own call sites are, and the failure would look like
// this guard working rather than like it being wrong.
func TestThePluginForTenantScannerIgnoresSecretsForTenant(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "secretsfortenant", "undeclared.go")

	sites := scanPluginForTenantSites(t, root, []string{fixture})
	if len(sites) != 0 {
		t.Fatalf("scanner found %d plugin.ForTenant-shaped site(s) in a fixture that only "+
			"calls Secrets.ForTenant, want 0 -- it can no longer tell the two apart, which "+
			"would make every real Secrets.ForTenant call site an undeclared "+
			"plugin.ForTenant finding", len(sites))
	}
}
