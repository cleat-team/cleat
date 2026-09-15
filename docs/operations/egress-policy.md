# Controlling what workflows and plugins can reach

A cleat workflow can ask the worker to fetch a URL. So can several plugins.
Both are outbound HTTP made *by your worker, from your network position* — so
the question "what may it reach" is a security one, and cleat answers it with
two policies that must both agree.

> **A destination is reachable only when the operator permits it *and* the
> requesting tenant permits it.** Either one saying no is a refusal, and the
> refusal says which.

## The three layers

| layer | set by | default | can it be widened? |
|---|---|---|---|
| **floor** — loopback, link-local, RFC1918 and friends | nobody | always refused | **no** |
| **operator** — `--egress-allowlist` | you | empty permits every *public* host | by you |
| **tenant** — `cleatctl egress-allow` | per tenant | empty permits **nothing** | only narrower |

The operator layer defaults **open** and the tenant layer defaults **closed**.
That is not an inconsistency. The floor sits underneath both, so "every public
host" really is public; and an unconfigured deployment keeps working while an
unconfigured tenant reaches nothing.

### The floor

Refused for every guest- and plugin-initiated request, whatever any list says:

- `127.0.0.0/8`, `::1` — your worker's own API and admin surface
- `169.254.0.0/16`, `fe80::/10` — link-local, and `169.254.169.254` is cloud
  instance metadata, which on most providers hands out credentials
- `10/8`, `172.16/12`, `192.168/16` — the private network your worker sits in
- `100.64/10` (CGNAT), `fc00::/7` (IPv6 ULA), multicast, and several reserved
  ranges

IPv4-mapped IPv6 forms are covered: `::ffff:169.254.169.254` is tested as
`169.254.169.254`.

**Allowlisting a hostname does not re-admit a private address.** A host on
either list that resolves into private space is refused — that is the DNS
rebinding case, not a grant.

## Operator policy

```
cleat-worker --egress-allowlist api.stripe.com,.internal-vendor.example
```

Entry forms, and only these two:

| entry | matches | does **not** match |
|---|---|---|
| `example.com` | `example.com` exactly, case-insensitively | `api.example.com` |
| `.example.com` | `api.example.com`, `a.b.example.com` | `example.com` — the apex is **excluded** |

The apex is excluded from the dotted form deliberately: an operator writing the
narrower-looking entry should not silently get the wider grant.

Empty — the default — permits every public host. It does **not** mean "deny".

### It is the only policy for requests that have no tenant

Some outbound requests are not made on any tenant's behalf:

- plugin **background loops** (`datadogexport`, `notifications`,
  `kafkaconnect`) — these are sweeps across all tenants
- requests on **auth-exempt routes**: `POST /ingest/{source_id}` and
  `GET /oauth/{provider}/callback`

There is no tenant list to consult for those, so the operator list governs them
alone. If a plugin background loop needs to reach a Kafka REST proxy or a
Datadog endpoint, that host goes in `--egress-allowlist`.

## Tenant policy

Point `cleatctl` at the database with `--db` or `CLEAT_DB_URL`, then:

```
cleatctl egress-allow list   <tenant-uuid>
cleatctl egress-allow add    <tenant-uuid> api.stripe.com .internal.example
cleatctl egress-allow remove <tenant-uuid> api.stripe.com
```

Entry forms are the operator list's. Hosts that cleat could never match are
refused at the command rather than stored — a scheme, a port, a path or a
wildcard — so a listing never shows an entry that silently does nothing.

**An empty list permits nothing.** A tenant whose list is empty cannot fetch
anything, which is the intended state for a tenant that has not asked for
egress.

Workers pick up a change within their allowlist cache TTL, 30 seconds by
default. That window is also how long a **revoked** host stays reachable.

## Reading a refusal

A refusal names the layer that produced it:

```
egress to metadata.example (169.254.169.254) is refused by cleat's network
  policy: link-local, and 169.254.169.254 is cloud instance metadata -- often
  credentials
```

```
egress to api.example.com is refused by cleat's network policy: host is not on
  this deployment's egress allowlist (operator policy)
```

```
egress to api.example.com is refused by cleat's network policy: host is not on
  this tenant's egress allowlist
```

The distinction matters operationally: the second is fixed with
`--egress-allowlist`, the third with `cleatctl egress-allow`, and editing the
wrong one changes nothing.

### A refusal you cannot configure away says so

The three gates are checked operator → tenant → floor, and the message names
the **first** one that said no. That ordering used to mislead in one case: a
destination that is already an IP address in a denied range is refused by every
gate, so it was reported as an allowlist failure — which names a list you can
edit. `cleatctl egress-allow add 127.0.0.1` is accepted and `list` shows it
afterwards, and the next call fails anyway, now citing the floor.

A denied **address literal** is now reported by the floor directly:

```
egress to 127.0.0.1 is refused by cleat's network policy: loopback: the
  worker's own API and admin surface
```

So if a refusal names an allowlist, editing that allowlist is worth doing; if
it names a floor rule, no configuration will change it and the destination has
to move.

**One case is deliberately not covered.** A *hostname* that resolves into a
denied range — `localhost`, or a name pointing at an RFC1918 address — still
reports the allowlist first, because finding out otherwise means resolving it,
and nothing is resolved until the allowlists have permitted the host. That
ordering is what stops a workflow making the worker look up a name of its
choosing. If a name-based destination is refused by an allowlist and adding it
does not help, the address behind it is on the floor.

**A refusal is permanent, not transient.** A workflow will not retry against
it, because the answer cannot change without a configuration change.

## What this is not

It is **not** a boundary against a hostile plugin. Plugins are Go compiled into
the worker; one that wanted to reach the metadata endpoint could open its own
socket. What it bounds is a plugin endpoint that is *misconfigured*, or one a
tenant supplied. The same reasoning is recorded in `migrations/postgres/063`
for why plugin row-level security is not a boundary against a hostile plugin
either.

One client is deliberately exempt and it is worth knowing why: the AWS IAM
credential chain in the `blobstore` plugin reaches `169.254.169.254` *by
design*, because that is how EC2 instance-profile and ECS task-role credentials
are obtained. Its destination comes from the AWS SDK rather than from a tenant,
a config or a workflow, so nothing a guest supplies reaches it.

## Related

- `docs/operations/deploying-to-production.md` — where `--egress-allowlist`
  belongs in a deployment
- cleat#1565 — the issue this implements, including the measurements behind the
  floor's contents
