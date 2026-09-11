# Plugin-created tables: review and proposal

**Status:** exploration and proposal, open for review.
**Baseline:** `develop` at `fabe4b37`, 2026-09-11. **Re-baselined mid-review** — `1f410d68`
("plugin tables can carry a tenant policy, and kvstore does") landed while this was being written,
so the first half of the exploration was against a tree that no longer exists.
**Relationship to existing issues:** [#1277](https://github.com/cleat-team/cleat/issues/1277)
(plugin tables have no policy; grant path dead),
[#1278](https://github.com/cleat-team/cleat/issues/1278) (a tenant reaches a plugin on one path
only, so seventeen plugins cannot be RLS-scoped) and
[#1279](https://github.com/cleat-team/cleat/issues/1279) (docs claim per-tenant schemas) cover part
of this ground. Nothing below re-files them; the overlaps are marked. **§2.3(a) duplicated #1278
with a worse method and now defers to it** — my tracker search missed #1278 and #1280 entirely, and
the session working them had to ask before I noticed.

---

## 1. The mechanism as it stands

Seventeen plugins ship migrations, creating **27 distinct tables, all in one flat namespace**
(`public`, or whatever `--schema` names — see §3.2). A migration is a Go value with three
hand-written SQL arms:

```go
{
    Version: 1,
    Up:      `CREATE TABLE IF NOT EXISTS kv_store (...)`,   // PostgreSQL
    UpMySQL: `CREATE TABLE IF NOT EXISTS kv_store (...)`,   // optional
    UpMSSQL: `IF NOT EXISTS (...) CREATE TABLE kv_store (...)`,
    Down:    `DROP TABLE IF EXISTS kv_store;`,
    TenantScoped: []string{"kv_store"},                     // new, #1277
}
```

`plugin.RunMigrations` applies these at worker boot, immediately after the core migrations, under a
`pg_advisory_lock` and with `search_path` pinned to `public`. Applied versions are tracked in
`plugin_migrations`.

`TenantScoped` is new. The runtime reads it and emits the RLS that plugin authors would otherwise
each have to write; `SQLDBAdapter.tenantTx` supplies the `cleat.tenant_id` the policy filters on,
transaction-locally, whenever the context carries a tenant.

---

## 2. Skeptical review

### 2.1 What holds up

I went looking for defects in the new mechanism and did not find any. Recording what was checked, so
the absence means something:

| Checked | Finding |
|---|---|
| `applyTenantScoping` uses `FORCE` as well as `ENABLE` | **Yes.** Without it the table owner — whoever ran the migration — bypasses the policy silently. This is the exact trap that made a superuser connection read 100000 rows through a policy earlier in this review. |
| The predicate fails closed | **Yes.** `cleat.assert_tenant_set()` raises rather than filtering, so a missing tenant is an exception rather than an empty result that reads as "no data". |
| Re-running a migration is safe | **Yes.** `DROP POLICY IF EXISTS` precedes `CREATE POLICY`. |
| Table names are guarded before interpolation | **Yes.** `isPlainIdentifier`, and the doc is honest that this is a blast-radius limiter rather than an injection defence, since the names come from source. |
| Ordering: GUC-setting landed before any policy existed | **Correct, and deliberately the reverse of the obvious order.** Setting a GUC no policy reads is a no-op; shipping policies first would fail every plugin statement closed on contact. |
| kvstore genuinely qualifies for a fail-closed policy | **Yes.** Its only access paths are four HTTP handlers in `routes.go`, all on `r.Context()`. No `HostCall`, no background loop. A regression test exists (`kv_store_has_a_policy_behind_its_predicate_test.go`). |
| The three dialect arms agree | **Yes — 0 of 27 definitions differ.** See §2.2 for how much work that answer took. |

### 2.2 What I got wrong, and why it is the review's most useful finding

I attempted the dialect-arm comparison **four times and got four different wrong answers** before
getting a trustworthy one:

| Attempt | Method | Reported | Why it was wrong |
|---|---|---|---|
| 1 | regex, arms concatenated across versions | 2 tables drift, with columns named `alter` and `create` | `''.join` ran v1's `CREATE TABLE` into v2's `ALTER TABLE` |
| 2 | regex, per version, naive paren match | 12 arm-pairs drift | stopped at the first `)`, truncating the column list |
| 3 | regex, balanced parens | 12 arm-pairs drift, MySQL "missing" nearly everything | a Go raw string cannot contain a backtick, so MySQL DDL is written as `` `...` + "`key`" + `...` `` — the regex captured only the first fragment |
| 4 | Go's own parser, evaluating concatenation | 6 arm-pairs drift | a column legitimately named `key` matched my `KEY ` skip-rule for MySQL index syntax, dropping it from the PG arm only |
| 5 | as 4, skip `KEY ` only when the fragment contains `(` | **0 differ** | — |

**This is not just a story about my regexes.** The format — three hand-written SQL dialects
embedded in Go string concatenation, inside composite literals, across multiple versions per file —
is *hostile to automated analysis*. The practical consequence is that **nothing checks these arms
agree, and drift could appear without anyone noticing**, because the check is expensive enough to
write that nobody has.

That they agree today is luck plus care, not a property anything enforces.

### 2.3 What is actually broken

**(a) The adoption ceiling is structural, not a backlog — see
[#1278](https://github.com/cleat-team/cleat/issues/1278), which owns this finding.**

`tenantTx` scopes a statement only when the context carries a tenant, and the policy fails closed,
so for a plugin with a background sweep *"add a policy"* and *"break the sweep"* are the same change.

I reached this independently and worse, and am recording the correction rather than my number.
My test was binary per plugin — has `RegisterRoutes`, lacks `HostCall`/`HasBackground` — and gave
"5 of 17 can adopt". #1278 instead counts **access sites** and classifies each by whether its context
can hold a tenant, which is the question that actually matters. Its table scores `featureflags` at
7 request-scoped and 1 not; my binary test called it purely request-scoped, which is wrong. Use
#1278's census, not this paragraph.

What survives from my side is only the tally of the surface: **23 tenant-bearing plugin tables, 1
covered by a policy today** (`kv_store`).

**(b) `--schema` puts plugin tables where plugins cannot see them.** Core tables honour `--schema`
(`dsnWithSchema` appends `search_path=<schema>`). Plugin migrations pin `search_path = public`
unconditionally (`plugin/migration.go:181`). Measured on postgres:16:

```
-- migration connection, as pluginMigrationSession does:
SET search_path = public;  CREATE TABLE IF NOT EXISTS kv_store (...);
   -> created in: public

-- runtime connection, as dsnWithSchema produces:
SET search_path = cleat_prod;  SELECT count(*) FROM kv_store;
   -> ERROR: relation "kv_store" does not exist
```

Related to #1279, which reports the documentation claim. This is the runtime consequence: on any
deployment with a non-default `--schema`, every plugin is broken.

**(c) A flat namespace plus `IF NOT EXISTS` means a collision is silent.** The 27 names include
`kv_store`, `task_queue`, `schedules`, `backup_config`, `webhook_config`, `rate_limits`,
`event_stream` — names a third-party plugin would plausibly also choose. No collisions exist among
the built-ins today. What happens when one does, measured:

```
-- plugin one already owns kv_store (tenant_id, k, v).
-- plugin two declares its own shape:
CREATE TABLE IF NOT EXISTS kv_store (owner text, payload jsonb, expires_at timestamptz);
   -> CREATE TABLE        (reported as success; it is a no-op)

SELECT string_agg(column_name, ', ') FROM information_schema.columns WHERE table_name='kv_store';
   -> tenant_id, k, v     (plugin one's shape survives)

SELECT owner, payload FROM kv_store;
   -> ERROR: column "owner" does not exist
```

The migration **succeeds and is recorded as applied**, so the plugin is considered migrated. The
failure surfaces later, at query time, as a confusing column error with nothing pointing at the
cause.

**(d) Dropping a tenant leaves all of its plugin data behind.** `admin.drop_tenant`
(`migrations/postgres/059_...`) deletes from eight core tables by name, drops the `tenant_<uuid>`
schema and the role. It touches **no plugin table**. A dropped tenant's `kv_store` entries,
`audit_events`, `oauth_sessions`, `webhook_events` and `blob_index` rows all survive, in `public`,
indefinitely. For a deletion path that exists to honour a tenant leaving, that is the wrong default.

**(e) `Down` is declared and never executed.** `plugin.Migration.Down` is populated by plugins and
`grep -rn '\.Down\b' plugin/ migration/` finds no caller. It is a field promising rollback that does
not exist.

**(f) `RegisterPluginTables` is still dead.** No production caller; `admin.plugin_tables` never
populated. Already in #1277, noted here only because §3.4 proposes reusing the idea.

---

## 3. Proposal

Ordered by ratio of value to risk. (1) and (2) are the substantial ones.

### 3.1 Give each plugin its own schema

`plugin_<name>.<table>` instead of `public.<table>`. This is one change that closes three problems:

- **Collisions become impossible** rather than silent — two plugins named `kv_store` get
  `plugin_kvstore.kv_store` and `plugin_othercache.kv_store`.
- **Uninstall becomes possible.** `DROP SCHEMA plugin_<name> CASCADE` is the removal path that does
  not exist today.
- **Ownership becomes legible** in `\dn` and in any audit, instead of 27 anonymous tables sharing a
  schema with the engine's own.

**The migration is cheaper than it looks, because nothing needs to qualify its SQL.** Plugin queries
are unqualified and resolve through `search_path`. Set `search_path = plugin_<name>, public` for the
plugin's statements and every existing query keeps working untouched.

`SQLDBAdapter` already opens a transaction per statement on the tenant-scoped Postgres path
(`tenantTx`), so `SET LOCAL search_path` has an obvious home. The cost to weigh: extending that to
*all* plugin statements means a transaction per statement for plugins that do not have one today.
Measure before committing.

This also fixes (b) properly rather than papering over it: the plugin schema is created under the
configured `--schema`, so the two stop disagreeing.

### 3.2 Bridge the engine's tenant into the context at the plugin-call boundary

This is the concrete unblock for [#1278](https://github.com/cleat-team/cleat/issues/1278).

The tenant is **already known** during workflow execution — `cmd/cleat-worker/setup.go:1740` passes
`engine.WithTenantID(wf.TenantID)`. It is simply not in the `context.Context` that reaches
`tenantctx.From`, which is populated only by the HTTP middleware
(`cmd/cleat-worker/main.go:1199`). The engine's own comment at `setup.go:1510` names this directly:
*"there are two notions of tenant here and nothing else compares them."*

Putting the engine's tenant into the context before `PluginCall` invokes a plugin would make
host-call paths tenant-scoped, and let the twelve non-request-scoped plugins adopt `TenantScoped`
for everything except their background sweeps.

**Background sweeps stay genuinely cross-tenant and need the opposite treatment** — an explicit,
named bypass rather than an accidental one. The codebase already has the pattern: the engine's own
cross-tenant reads go through `admin.claim_workflows`, a `SECURITY DEFINER` function owned by a
`BYPASSRLS` role, so the widening is a deliberate grant rather than a missing predicate. Plugin
sweeps should go through something of the same shape.

### 3.3 Make the arms checkable

A test that parses `plugins/*/migrations.go` with `go/parser`, evaluates the concatenated string
literals, and asserts that `Up`, `UpMySQL` and `UpMSSQL` declare the same columns for the same
table. It reports 0 differences today, so it can land green and stay meaningful.

Per this repo's standing rule, it needs a **known-positive**: prove it catches a deliberately
introduced column difference, not merely that it passes a clean tree. §2.2 is the argument for why —
four plausible-looking checks all reported drift that was not there, and a fifth would have reported
none that was.

### 3.4 Make tenant deletion cover plugin data — using the registry that now exists

`TenantScoped` is already a declarative list of every plugin table holding tenant-owned rows. That
is exactly the registry `RegisterPluginTables` was meant to build, arrived at from the other
direction and actually populated.

Have `RunMigrations` write the `TenantScoped` names into `admin.plugin_tables`, and have
`admin.drop_tenant` loop over it issuing `DELETE FROM <t> WHERE tenant_id = p_tenant_id`. That gives
(d) a fix and (f) a purpose in the same change, and it means a plugin declaring `TenantScoped` gets
tenant deletion for free rather than as a second thing to remember.

If §3.1 lands, the `DELETE` becomes schema-qualified and the two compose cleanly.

### 3.5 Delete `Down`, or implement it

A field that plugins populate and nothing executes is worse than no field: it reads as a rollback
guarantee. Either wire it into an uninstall path — which §3.1 makes coherent, since `DROP SCHEMA`
is the real uninstall — or remove it and say in the type's doc that plugin migrations are
forward-only.

---

## 4. What I deliberately did not propose

- **Partitioning plugin tables.** Covered in `schema-partitioning-design.md` and argued against
  there: most plugin tables are config-shaped, and pushing partition DDL into a hand-written
  three-dialect API is an interface that will be got wrong quietly.
- **Re-filing #1277 or #1279.** §2.3(b) and (f) overlap them and are marked as such.
- **Anything about the dialect-skip mechanics.** That was #1157, fixed by #1273.
- **A fix for MySQL and SQL Server tenant scoping.** `TenantScoped` is PostgreSQL-only and the field
  documents why: MySQL has no RLS, and SQL Server binds a tenant to a connection pool, which a
  per-request tenant does not fit. Both are real gaps, neither is a rounding error, and closing them
  is a separate piece of work from anything above.

---

## 5. Provenance

Measured 2026-09-11 against `fabe4b37`, on `postgres:16` with stock configuration. Counts of a
growing population — plugins, tables, adoption — should be re-derived:

```bash
# plugins with migrations, and which are purely request-scoped
for d in plugins/*/; do [ -f "$d/migrations.go" ] || continue
  grep -ql "RegisterRoutes" $d*.go && ! grep -ql "HostCall\|HasBackground" $d*.go && echo "$d"
done

# tenant-bearing plugin tables vs those covered by a policy
grep -rn "TenantScoped:" plugins/*/migrations.go

# does drop_tenant touch any plugin table?
sed -n '/FUNCTION admin.drop_tenant/,/\$\$ LANGUAGE/p' \
  migrations/postgres/059_a_dropped_tenants_definitions_go_with_it.sql | grep "DELETE FROM"
```

The dialect-arm comparison needs Go's parser rather than a regex; see §2.2 for why, and §3.3 for
turning it into a test.
