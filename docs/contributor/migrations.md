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
| SQL Server | **logins** (server-level) and, in most dump tools, database **users/roles and object grants** | `sys.server_principals`; `sys.database_principals` / `sys.database_permissions` |

PostgreSQL lost two `cleat_sweep` memberships to exactly this in cleat#2416, with the per-database
diff reporting clean. Check the dialect's column *behaviourally* — query the catalogue — rather than
by grepping the compacted SQL, because the statements are frequently built by dynamic SQL inside a
`DO` block and an anchored text search will not find them.

MySQL has the opposite hazard as well: a dump can *carry* a `DEFINER=` clause naming a user the
target environment does not have, so a baseline that applies on the machine it was dumped from can
fail elsewhere.
