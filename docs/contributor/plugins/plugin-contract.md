# The plugin contract

A plugin is the one place where cleat's two guarantees meet: the determinism
sandbox that workflow code runs inside, and the host surface that makes cleat a
backend rather than a scheduler. Every plugin is four surfaces at once —
**determinism**, **tenant isolation**, **egress**, and **liveness** — so each
plugin added multiplies risk unless the boundary is governed.

It is governed, quite well, by **fourteen total-coverage guards**. Until this
page they were discoverable only by reading test filenames across two packages,
or by writing a plugin and watching CI reject it one rule at a time.

**This page is checked.** `plugin/contract_test.go` fails if a guard named below
does not exist, and fails if a total-coverage guard exists in `plugin/` or
`plugins/` that this page neither lists as a clause nor names as out of scope.
The second direction is the one that matters: it is what stops this page going
stale the next time a rule is added.

> **On the count.** cleat#1828 said five guards; `priorities-2026-09-17.md` said
> eleven. Measured at the top level of `plugin/` and `plugins/`: **15
> total-coverage test functions**, of which 13 are obligations on a plugin author
> and 2 are framework invariants. Both prior numbers were guesses, which is the
> reason the check below derives the set rather than trusting a total — including
> this one.

---

## Tenant isolation

Plugins hold the raw `*sql.DB`. Nothing in the type system stops a plugin
reading another tenant's rows, so five clauses cover it.

### C1 — a table with a `tenant_id` column is declared `TenantScoped`

**Guard:** `TestEveryPluginTableWithATenantColumnIsDeclaredTenantScoped`
(`plugin/every_plugin_tenant_table_is_declared_tenant_scoped_test.go`)

Declaring the table is what applies row-level security on PostgreSQL and the
filter predicate on SQL Server.

**What the guard cannot see:** it reads your `CREATE TABLE` SQL. A table created
some other way — a migration that builds it dynamically, or a table created
outside the plugin's own migrations — is invisible to it.

### C2 — every `AcrossAllTenants` call site carries a written reason

**Guard:** `TestEveryCrossTenantBypassIsDeclared`
(`plugin/a_cross_tenant_bypass_is_declared_test.go`)

### C3 — that reason is a string literal, not a variable

**Guard:** `TestEveryCrossTenantReasonIsAStringLiteral` (same file)

A reason assembled at runtime cannot be reviewed, which is the whole point of
requiring one.

### C4 — a tenant-scoped statement names its context

**Guard:** `TestEveryTenantScopedStatementNamesItsContext`
(`plugin/a_tenant_scoped_statement_names_its_context_test.go`)

### C5 — no production statement uses a bare `context.Background()`

**Guard:** `TestNoProductionStatementUsesABareContext` (same file)

A bare context carries no tenant, so the statement runs unscoped.

**Note what this layer rests on.** RLS is a *backstop*, not the mechanism —
PostgreSQL forces it on 16 of 20 tenant-bearing tables, SQL Server filters 13
with **no BLOCK predicates**, and MySQL has none. See
`docs/reference/multi-tenancy.md`. C1–C5 are the layer that actually enforces
the boundary on every dialect.

### C15 — every `AllTenantIDs` per-tenant loop site is declared

**Guard:** `TestEveryPerTenantLoopIsDeclared`
(`plugin/a_per_tenant_loop_is_declared_test.go`)

A per-tenant loop — `plugin.AllTenantIDs` plus `plugin.ForTenant` per id, the
shape cleat#2125/#2141 introduced for SQL Server — must run on a context that
carries no `AcrossAllTenants` marker, or `ForTenant` silently no-ops and every
"per-tenant" statement runs under the bypass instead (see `plugin.IsCrossTenant`'s
doc, `plugin/crosstenant.go`). Numbered after C14 because it was added later,
not because it belongs outside tenant isolation — it is C2's counterpart for the
loop shape rather than the single bypass call: every `plugin.AllTenantIDs` call
site must appear in `perTenantLoopLedger`, checked bidirectionally the same way
C2's ledger is — an undeclared site is a new, unreviewed loop; a stale ledger
entry is a grant covering nothing.

---

## Storage and dialects

### C6 — no two plugins declare the same table

**Guard:** `TestNoTwoPluginsDeclareTheSameTable`
(`plugin/no_two_plugins_declare_the_same_table_test.go`)

### C7 — every query arm is valid for its own dialect

**Guard:** `TestEveryPluginQueryArmIsValidForItsOwnDialect`
(`plugin/dialect_sql_test.go`)

### C8 — no plugin SQL reaches a dialect that rejects it

**Guard:** `TestNoPluginSQLReachesADialectThatRejectsIt`
(`plugin/plugin_sql_reachability_test.go`)

### C9 — no plugin write casts a value to JSON

**Guard:** `TestNoPluginWriteCastsAValueToJSON`
(`plugin/no_plugin_write_casts_a_value_to_json_test.go`)

### C10 — no plugin scans into `uuid.UUID` directly

**Guard:** `TestNoPluginScansIntoUUIDDirectly`
(`plugin/guid_scan_types_test.go`)

SQL Server returns GUIDs in a byte order that a direct scan gets wrong.

### C11 — every ledger kind is in the closed set

**Guard:** `TestEveryLedgerKindIsInTheClosedSet`
(`plugin/a_cross_tenant_bypass_is_declared_test.go`)

---

## Egress

### C12 — outbound HTTP goes through `env.HTTPTransport`

**Guard:** `TestEveryPluginRoutesItsEgressThroughTheGuard`
(`plugins/every_plugin_routes_its_egress_through_the_guard_test.go`)

The guard checks at `DialContext` on resolved IPs, closing redirect and
DNS-rebinding bypasses, with a non-overridable floor over
loopback/RFC1918/link-local/multicast/metadata.

**What the guard could not see, until recently:** it asserted the substring
`Transport:` was present, so a bare `&http.Transport{}` — fully unguarded —
passed. Fixed in cleat#1771; it now resolves the identifier. Worth knowing
because it is the shape a reader should assume of any clause here until they
check.

### C13 — every outbound call joins the caller's trace

**Guard:** `TestEveryOutboundCallJoinsTheTrace`
(`plugin/every_outbound_call_joins_the_trace_test.go`)

---

## Inter-plugin ordering

### C14 — a plugin reading another plugin's context value declares the ordering dependency

**Guard:** `TestEveryContextOrderDependencyHasProviderOuterOfConsumer`
(`cmd/cleat-worker/plugin_context_order_test.go`)

<!-- external-guard TestEveryContextOrderDependencyHasProviderOuterOfConsumer cmd/cleat-worker/plugin_context_order_test.go -->

Middleware wraps in `plugin.Discover()`'s order, and a plugin later in that
order wraps everything before it — so it runs first, and its context writes
are visible to every plugin nested inside it. Two plugins with no declared
relationship fall through to `topologicalSort`'s alphabetical tie-break
(`plugin/registry.go:186`), which orders them by name rather than by whether
one reads a value the other sets.

cleat#1881: `audit-log` read `oauth-provider`'s session identity out of
context this way, and it worked — because `"audit-log"` sorts before
`"oauth-provider"`. Renaming either plugin, or giving either an unrelated
`Requires` that moved it in the sort, would have silently reverted `user_id`
to always-empty, with no test failing.

**`Requires` is the wrong tool for this**, which is worth stating because it
is the obvious first reach. Declaring one plugin `Requires` another makes
`Discover()` refuse to run at all if the required plugin is not registered
(`plugin/registry.go:193`) — a real functional coupling, not a documentation
nicety, and wrong for two plugins that are each independently optional.
`contextOrderDependencies` in the guard above is a plain list of
(consumer, provider) name pairs, checked against the real registered order:
add a row there when writing a plugin that reads a value another plugin's
middleware sets.

### C15 — every `Secrets.ForTenant` call site is declared

**Guard:** `TestEverySecretsForTenantCallIsDeclared`
(`plugin/a_secrets_for_tenant_is_declared_test.go`)

`plugin.Secrets`/`plugin.Payloads` (cleat#1992) take no `tenantID` parameter
on the request-path methods — the tenant comes from `ctx` instead, so there
is no argument for a plugin bug to pass wrong. `Secrets.ForTenant` is the one
place a plugin still names a tenant directly: a background loop with no
request to derive one from (mirroring `plugin.ForTenant` on the SQL side,
cleat#2125), or an unauthenticated request naming its own tenant as its
subject. Neither is `crossTenantLedger`'s shape — every `ForTenant` call
still names exactly one tenant, never a bypass spanning all of them — so a
reviewer checking only that ledger for "does this plugin act on a tenant
outside its own request" gets an incomplete answer. This ledger is that
question's counterpart for the `Secrets`/`Payloads` surface, checked
bidirectionally the same way `crossTenantLedger` is: an undeclared site is a
new, unreviewed tenant-naming call; a stale entry is a grant covering
nothing.

(Numbered independently of any sibling ledger cleat#2141 adds for
`plugin.AllTenantIDs` call sites — reconcile the two files if both land, per
this guard's own file-level comment.)

---

## Two obligations with no clause of their own

Stated because writing this page to match the guards that happen to exist would
make the contract look complete when it is not.

### G1 — liveness: a plugin goroutine must not take the worker down

An unrecovered panic in a plugin goroutine killed the worker process until
cleat#1769. There **is** a guard —
`TestEveryStartedGoroutineIsRecovered` — and it does scan `plugins/`. But it
lives in `cmd/cleat-worker/`, so a plugin author reading `plugin/`'s tests will
not find it, and it is not framed as part of what a plugin must satisfy.

Use `plugin.RecoverGoroutine(name, tracker, fn)` for any goroutine a plugin
starts.

<!-- external-guard TestEveryStartedGoroutineIsRecovered cmd/cleat-worker/every_started_goroutine_is_recovered_test.go -->

### G2 — replay policy: `Idempotent` and `SameValueOnReplay`

A host call is re-invoked on replay only if it declares **both**
(`engine/plugins.go:26`, `engine/events.go:135`; split from a single bool in
cleat#1318). The allowlist test pins the current set — it does not tell an
author writing a *new* host call how to choose.

**The failure is silent.** `featureflags.evaluate_flag` was registered
`Idempotent: true` while reading mutable flag state, so replay produced a
different value than the original run. Nothing failed at registration; it
surfaced as a replay divergence.

The rule: `Idempotent` means calling it twice is safe. `SameValueOnReplay` means
calling it again **returns what it returned before**. Reading mutable state
satisfies the first and violates the second.

---

## Deliberately not clauses

These are total-coverage guards in the same packages that are **not** obligations
on a plugin author. They are listed so the check below can account for every
guard rather than silently ignoring some.

| Guard | Why not a clause |
|---|---|
| `TestEveryMultiBackendTestIsSelectedByTheMultiDialectJob` | CI wiring: asserts the multi-dialect job runs the tests that need it. A property of the pipeline, not of a plugin. |
| `TestNoBudgetMeansNoEviction` | Runtime behaviour of the tenant pool budget, which the framework owns; no plugin can violate or satisfy it. |

## Guards enforcing plugin obligations from outside these packages

The check below scans `plugin/` and `plugins/`. A guard that enforces a plugin
obligation from somewhere else is recorded here with its path, and the check
verifies it exists there — otherwise a citation to it could rot unnoticed, which
is the failure this page exists to prevent.

| Guard | Lives in | Clause |
|---|---|---|
| `TestEveryStartedGoroutineIsRecovered` | `cmd/cleat-worker/every_started_goroutine_is_recovered_test.go` | G1 |

**That it lives there is the finding, not an accident of layout.** A plugin
author reading `plugin/`'s tests will not find it.
