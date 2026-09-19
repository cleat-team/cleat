# Entity lifecycle

Everything in cleat that is *created, changed and retired by an operator* — as opposed to a
workflow run, which has its own state machine — follows one contract. This is that contract, who
belongs to it, and what is deliberately outside it.

For runs, see [Workflow lifecycle](workflow-lifecycle.md). The two are separate on purpose: a run
has a status that the engine advances, and an entity has a retirement instant that a person sets.

Derived from the code on **2026-09-19**. Unlike most reference documents, this one describes
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

### Members — ten tables, subject to all four clauses

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

### Exempt — in the class, deliberately outside the contract

| table | why |
|---|---|
| `admin.tenants` | the tenant itself; its lifecycle is the thing the others hang off — see below |
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
on **other entities' work** — stopping runs from being claimed and schedules from firing — rather
than on a row anyone reads. `admin.orgs` is exempt for the same reason one level up.

That is a different mechanism from `disabled_at`, which is why it is exempt rather than
non-conforming. `admin.tenants.suspended` is the column reserved for it, and the
[B2B control-plane playbook](../playbooks/b2b-saas-control-plane.md) is where the tenant lifecycle
— suspension and deletion — is documented.

Deletion, for every entity here, is `cleatctl drop-tenant`: it removes the tenant's rows across
the full set of tenant-scoped tables. Retiring a single entity and deleting a whole tenant are the
two ends of the same lifecycle, and nothing in between removes one entity permanently.

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

As of 2026-09-19 the list is **empty**: 40 of 40 clauses (10 members × 4) are enforced.

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
