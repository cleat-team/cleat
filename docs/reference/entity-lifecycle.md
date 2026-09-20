# Entity lifecycle

Everything in cleat that is *created, changed and retired by an operator* — as opposed to a
workflow run, which has its own state machine — follows one contract. This is that contract, who
belongs to it, and what is deliberately outside it.

For runs, see [Workflow lifecycle](workflow-lifecycle.md). The two are separate on purpose: a run
has a status that the engine advances, and an entity has a retirement instant that a person sets.

Derived from the code on **2026-09-20**. Unlike most reference documents, this one describes
something a guard already enforces — `scripts/check-entity-contract.py`, which runs in CI — so the
drift this page can suffer is bounded. Where a statement here and the guard disagree, the guard is
right and this page is a bug.

---

## The contract

Every **member** entity carries exactly three columns for its lifecycle, and none of the legacy
spellings:

| column | type | meaning |
|---|---|---|
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | when it came into existence |
| `updated_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | when it last changed |
| `disabled_at` | `TIMESTAMPTZ` (nullable) | when it was retired. **`NULL` means live.** |

So an entity has two states, and the column records *when* rather than only *whether*:

```
                 disabled_at = now()
    live  ─────────────────────────────────▶  retired
      ▲                                          │
      └──────────────────────────────────────────┘
                 disabled_at = NULL
```

Retirement is **reversible** by design. Deletion is a separate, irreversible operation —
`cleatctl drop-tenant` for a whole tenant — and the gap between the two is the point: a
non-paying customer, an entity under investigation, and an offboarding grace period all want the
thing switched off and the data kept.

### Why a nullable timestamp and not a boolean

The class had **four spellings of one concept, and one of them was inverted**: `revoked_at` (set =
retired), `deprecated BOOLEAN` (true = retired), `enabled BOOLEAN` (**true = live**), and seven
members with no mechanism at all.

A generic "is this live?" helper over that set is right for three tables and backwards for one, and
nothing in the schema says which. A timestamp has no polarity to get backwards, it records when
rather than only whether, and it matches `revoked_at` — the one member that already had the better
shape.

`NULL` means live because there is **no "unknown" state**: an entity either has been retired at
some instant or has not. That makes `NULL` the correct spelling of the common case rather than a
placeholder, which is why the column has no `DEFAULT` and no `NOT NULL`.

### The fourth clause: no legacy spelling survives

`revoked_at`, `deprecated` and `enabled` are refused on a member table. Carrying both the contract
column and a legacy one is worse than carrying only the legacy one, because then two columns
describe the same fact and nothing says which is the authority — and the code reads one of them.

---

## Who is in the class

Membership is a **semantic judgement and cannot be derived from column shape**, which is why it is
a checked-in list (`scripts/entity-contract.tsv`) rather than a predicate. `admin.tenant_egress_allow`
and `public.tenant_domains` are structurally identical to `public.workflow_routing` and
`public.workflow_tags`, and no test over columns separates them.

### Members — eleven tables, subject to all four clauses

| entity | what retiring it does |
|---|---|
| `admin.tenant_api_keys` | the key stops authenticating |
| `admin.tenant_roles` | the role is no longer handed out |
| `admin.tenant_egress_allow` | the host is no longer reachable from a workflow |
| `workflow_defs` | the version is no longer deployable to |
| `workflow_schedules` | the cron stops firing |
| `workflow_routing` | the routing rule stops steering new runs |
| `workflow_tags` | the tag stops matching |
| `tenant_settings` | the per-tenant override stops applying |
| `tenant_secrets` | the secret stops resolving |
| `tenant_domains` | the domain stops mapping to the tenant |
| `queues` | the declared concurrency limit stops applying — see below, it is **not** a stop |

### Exempt — in the class, deliberately outside the contract

| table | why |
|---|---|
| `admin.tenants` | the tenant itself; its lifecycle is the thing the others hang off — it is **suspended**, not disabled, and that acts on other entities' work. See below. |
| `admin.orgs` | the same shape one level up: it groups tenants the way tenants group everything else (`created_at` + `suspended`, no `updated_at`) |
| `admin.plugin_tables` | registry of plugin-owned tables, not operator-created |
| `public.plugin_defs` | derived from plugin binaries, not operator-created |

### Not an entity

Runs and everything hanging off one (`workflow_instances`, `event_history`, `workflow_promises`,
`workflow_signals`, `workflow_update_requests`), holder-scoped TTL-reclaimed rows
(`concurrency_keys`, `idempotency_keys`), statistics (`workflow_memory_samples`,
`workflow_memory_stats`), and infrastructure (`admin.workers`, `admin.rls_predicate_form`).

A run is not retired, it *finishes* — see [Workflow lifecycle](workflow-lifecycle.md). A
concurrency key is not retired, it *expires*.

---

## The tenant is the exception worth knowing

`admin.tenants` is exempt, and the reason is not that its lifecycle is simpler. Every other entity
here is a thing a tenant owns; the tenant is what they hang off, and its own off switch has to act
on **other entities' work** rather than on a row anyone reads. `admin.orgs` is exempt for the same
reason one level up.

So the tenant has its own two states, spelled `admin.tenants.suspended`:

| | suspended |
|---|---|
| new workflows claimed | **no** |
| cron schedules fired | **no** |
| `POST /start` | **403**, naming the reason |
| runs already executing | **finish normally**, heartbeating as usual |
| reads — runs, history, state | **unaffected** |

```
cleatctl suspend-tenant <tenant-id>     # reversible
cleatctl resume-tenant  <tenant-id>
cleatctl drop-tenant    <tenant-id>     # not
```

The last two rows of that table are the design rather than omissions.

**A run mid-flight is not frozen.** Suspension stops new claims; it does not interrupt what is
already executing. That is why suspension needs no special handling anywhere else in the engine —
nothing is left half-run for the reclaim loop to find, and `cleat_workflows_stuck` never sees a
population that is not actually stuck. To stop work that is already running, cancel it: suspension
and cancellation are different instruments, and conflating them would make the reversible one
destructive.

**Reads stay open on purpose.** A tenant suspended for non-payment can still see its own runs;
blocking that punishes the wrong thing and makes the state harder to reason about rather than
easier.

Enforcement is one predicate in the worker's tenant enumeration, which both the dispatch claim and
the due-schedule read go through — so a single line stops work and cron together. A tenant with no
`admin.tenants` row is **not** suspended: that table is a registry a deployment can run without
populating, so treating absent as suspended would refuse every start on a configuration that works
today.

---

## Retiring a queue changes admission; it does not stop it

`queues` is the one member where "retired" is easy to read as "off", and it is not. Every claim
statement joins the table with `AND q.disabled_at IS NULL`, so a disabled queue is
indistinguishable from a name that was never registered — and an unregistered `concurrency_key` is
cleat's original mechanism, a **mutex**. Disabling therefore drops admission from N to **one at a
time**, not to zero. Runs keep being claimed, serially; nothing in flight is cancelled.

```
cleatctl queue disable <tenant> <name>    # N  ->  1, not 0
cleatctl queue enable  <tenant> <name>    # back to N
cleatctl suspend-tenant <tenant>          # THIS is the one that stops work
```

The fallback is deliberate, and it is the safer of the two directions: it never admits more than
the declared limit did, and it cannot wedge a tenant's runs. Refusing instead would let one
operator command silently stall every workflow carrying that key, with no error on any path to say
why — a start still succeeds, the work simply never runs.

`TestADisabledQueueFallsBackToTheBareKeyMutex` holds this on all three dialects, and
`cleatctl queue disable` prints it in as many words, because it is the thing an operator is most
likely to assume the opposite of.

Deletion is the other end: `cleatctl drop-tenant` removes the tenant's rows across the full set of
tenant-scoped tables. Retiring one entity and deleting a whole tenant are the two ends of the same
lifecycle, and nothing in between removes a single entity permanently.

See the [B2B control-plane playbook](../playbooks/b2b-saas-control-plane.md) for the operational
cases these serve.

---

## How this is kept true

`scripts/check-entity-contract.py` parses the shipped migrations and checks every clause against
every member. Three properties make it more than a lint:

**Total coverage.** Every table in `migrations/postgres/`, `migrations/mysql/` or
`migrations/mssql/` must appear in the registry with a class. An unclassified table **fails the
guard**. A new table cannot quietly land outside the contract — the author has to say which class
it is in, in a diff someone reads.

**The grandfather list may only shrink.** Members converted one at a time, and each unconverted
`(table, clause)` pair was recorded as skipped. The guard carries a ceiling on how many pairs may
be skipped, and that ceiling lives in the script rather than in the list — because a plain
allowlist has an obvious cheat: the cheapest way to make the guard pass is to add a line, and a
pass bought that way looks exactly like conforming.

As of 2026-09-20 the list is **empty**: 44 of 44 clauses (11 members × 4) are enforced.

**Cross-dialect membership is matched on the bare name.** MySQL cannot express a schema, so it
writes `CREATE TABLE tenants` where the other two write `admin.tenants`. Comparing qualified names
reports twelve differences — the same six tables in both directions, every one spurious.

Run it directly:

```
python3 scripts/check-entity-contract.py
```

---

## Related

- [Workflow lifecycle](workflow-lifecycle.md) — the run state machine, which this is not
- [Multi-tenancy](multi-tenancy.md)
- [B2B SaaS control plane](../playbooks/b2b-saas-control-plane.md) — tenant suspension and deletion
