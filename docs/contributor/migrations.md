# Changing the core schema

How `migrations/<dialect>/` is organised, and the one rule that changed when the baselines became
**generated**. Plugin migrations are a separate path — see
[plugins/plugin-migration-guide.md](plugins/plugin-migration-guide.md).

## The shape

Each dialect has a small generated baseline plus every migration added since:

```
001_schema.sql        tables, indexes, constraints, RLS policies, grants
002_defaults.sql      seed data and behaviour (HAND-ASSEMBLED — see below)
003_procedures.sql    the routines, final bodies only
004_*.sql, 005_*.sql  everything added after the baseline was cut
```

`migration.NewRunner` applies these in filename order and dedupes by name, so **the last definition
wins**. PostgreSQL was compacted from 87 files to these three in cleat#2059 (#2416, #2421); MySQL
and MSSQL still carry their original chains and are being compacted under cleat#2422.

## The rule: the baseline is frozen

**Change a routine, a table or a grant by ADDING A NEW NUMBERED MIGRATION. Never by editing
`001`/`002`/`003`.**

This is the one thing that is different from an ordinary migration chain, and the reason is that
those three files are *generated*:

- **A generated file has no conflict surface.** A hand-edit is silently reverted the next time
  anyone runs the generator, and the revert appears in no diff that a reviewer would see. Measured
  in cleat#2059: the `ON CONFLICT` arbiter rewrite and the `ONLY` removal both had to be made in
  the generator, because editing `003_procedures.sql` directly would have been undone.
- **The generator is a one-shot tool, not a build step.** `scripts/gen-postgres-baseline.py` reads a
  `pg_dump --schema-only` of a database built from the *pre-rebaseline* chain — a chain that no
  longer exists in the tree and must be reconstructed from git history
  (`git show 8f91b43a:migrations/postgres/`). Re-running it against the *current* chain instead
  would fold every post-baseline migration into `001` while leaving those files in place to be
  applied a second time.
- **Precedent, already in tree:** `migrations/mssql/004_fix_finalize_workflow_status_fence.sql`
  (`CREATE OR ALTER PROCEDURE`) sits on top of a compacted baseline. That is the shape.

Re-baselining is not something to do per change. It is a deliberate, once-per-era act with its own
verification, and it is only possible because a release required a fresh database anyway.

## The trap: a signature change is not a replacement

`CREATE OR REPLACE` **cannot change a function's or procedure's signature.** Given a different
parameter list it creates an *overload*, and the old definition stays installed and callable.

So when a migration adds, removes or retypes a parameter, drop the old signature explicitly:

```sql
DROP FUNCTION IF EXISTS admin.grant_plugin_to_tenant(text, text);
CREATE OR REPLACE FUNCTION admin.grant_plugin_to_tenant(text, text, text) ...
```

Skipping the `DROP` is not a cosmetic mistake and it is not symmetric: the old routine keeps
whatever privileges and `SECURITY DEFINER` semantics it had, so a caller that resolves to it gets
the *old, unscoped* behaviour. This is the failure mode cleat#2402 is about. Check
`\df <name>` (or `sys.parameters`) after applying, not just that the new one exists.

## What a baseline generator must prove

Two things, and neither is optional:

1. **Equivalence against the chain it replaces.** Build database A from the old chain and database
   B from the compacted files, and require an empty diff across columns, indexes, constraints, RLS
   policies, grants and routine bodies. `migration/catalogdiff` does this for all three dialects.
   Building both sides from a *shared* test database is not acceptable — see cleat#1281.
2. **A known-positive.** A clean diff is worth nothing until the diff has been shown *catching* a
   deliberate difference: drop an index in one side and watch it report. This project has paid for
   that rule repeatedly, and the baseline is the worst possible place to skip it.

### The dialect-specific things a per-database diff cannot see

Each dialect hides something outside the database, or inside it but outside a schema-only dump:

| Dialect | What the diff misses | Where it lives |
|---|---|---|
| PostgreSQL | roles, role **memberships**, role comments | `pg_authid`, `pg_auth_members`, `pg_shdescription` — cluster-wide |
| MySQL | **users and their privileges** | the `mysql` system schema, not the database |
| SQL Server | **logins** (server-level), and database **roles** | `sys.server_principals`; `sys.database_principals` |

Three of those need a caveat, because a table like this invites a check that measures something
else:

- **PostgreSQL's role memberships are real and were really lost** (cleat#2416), so that check has
  teeth.
- **SQL Server's role *existence* is a migration fact; its *membership* is not.** `cleat_admin` is
  created by `migrations/mssql/012_admin_role.sql`, and after a full migration it has **zero
  members** — measured on a chain-built database. Nothing in the tree grants membership; the
  *deployment* does it (`cmd/cleat-worker/setup.go`, `cmd/cleatctl/setsecret.go`). A membership
  check on a freshly-migrated database would therefore test nothing. Assert the role exists; do
  not assert who is in it.
- **SQL Server's object grants are not a fact about cleat at all — they are the server's.** A
  `sys.database_permissions` read returns **229 rows even in a brand-new empty database** (dbo 1,
  `public` 2 database-level, `public` 226 object-level), all on server-supplied objects like
  `sys.dm_pdw_nodes_os_tasks`. Nothing in `migrations/mssql/` issues a `GRANT`: the word appears
  only in prose. So a comparison of that catalog compares server defaults to server defaults, and
  a generator that *dumps* it would write 229 server defaults into the baseline as if they were
  schema. Filter to what the migrations created — or, here, emit no grants at all.

PostgreSQL lost two `cleat_sweep` memberships to exactly this in cleat#2416, with the per-database
diff reporting clean. Check the dialect's column *behaviourally* — query the catalogue — rather than
by grepping the compacted SQL, because the statements are frequently built by dynamic SQL inside a
`DO` block and an anchored text search will not find them.

MySQL has a hazard of the opposite kind, and it is worth stating precisely because it is easy to
misread. The MySQL tree contains **no `DEFINER=` clause and no `SQL SECURITY DEFINER`** anywhere —
but `mysqldump` *emits* `DEFINER=` clauses for routines even when the source DDL has none, so the
portability hazard is introduced entirely by the generator: a baseline dumped on one machine can
carry a definer naming a user the target does not have, and fail to apply there. A baseline
generator must therefore **strip `DEFINER`**, and the generated file must be **asserted to contain
none** — with a known-positive, since a check for an absent string passes on any tree.

> The trap to know about, because it has caught two sessions: `migrations/mysql/077_…:20-22`
> contains the word `DEFINER` in a comment reading *"There is no function to rebuild:
> `admin.get_due_schedules()` is a PostgreSQL SECURITY DEFINER function serving the cross-tenant
> read, and MySQL's cross-tenant path is a plain unscoped SELECT"*. It is an explicit denial. A
> `grep -c DEFINER` counts it and a reader may take the count as a finding — which is the
> "an explicit denial, read as a confirmation" trap, in the one place the rule is easiest to apply
> and easiest to forget.

### Three SQL Server traps, measured on a chain-built database (2026-09-26)

Found while spiking whether a committed Go emitter over `sys.*` can produce an applyable MSSQL
baseline. It can — all four modules re-applied **verbatim** from `sys.sql_modules.definition`,
hash-identical, and all 56 security predicates carry a non-NULL `predicate_definition`. These are
the places the emitter goes wrong:

- **`sys.objects.type` is `char(2)`, so a procedure's type is `'P '` — padded.** A Go map keyed on
  `"P"` never matches it, and the statement built from it reads `DROP  [admin]`, which fails with a
  *syntax* error that points at the schema name rather than at the real cause. `RTRIM(o.type)`
  first. (SQL itself is untroubled, because SQL Server ignores trailing spaces in comparison — so
  a `WHERE o.type IN ('P', …)` works while the Go comparison on the same value does not.)
- **`definition` is the verbatim source, leading comments included.** Half the modules here begin
  with `--`, so "the definition starts with `CREATE`" is false, and an emitter that parses the head
  or re-prefixes the verb corrupts them. Emit it unchanged.
- **Security policies reference the predicate function, so the order is forced.** `DROP FUNCTION
  dbo.fn_tenant_filter` fails with error 3729 while any `TenantFilter_*` policy exists. Create
  function **before** policy; drop policy **before** function.
