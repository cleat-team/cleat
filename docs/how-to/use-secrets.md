# Per-tenant secrets

Third-party credentials — payment keys, CRM tokens, model API keys — stored
encrypted, scoped to a tenant, and referenced from a workflow by name.

**The value never reaches the guest, and never reaches event history.** That is
the point of the design, and it is what makes this different from putting a
credential in `kvstore`.

## Set up a master key, once

```sh
head -c 32 /dev/urandom | base64
```

Give it to every worker and to `cleatctl` as `CLEAT_SECRET_MASTER_KEY`.

**From the environment, never a flag.** A flag value appears in `ps`, in
`/proc/<pid>/cmdline` to any local user, and in whatever records the command
line — a container spec, a systemd unit, a shell history.

**Keep it.** Secrets are sealed under a key derived from it, so losing it means
losing every stored value. There is no recovery path, deliberately.

A worker started without it, on a deployment that holds secrets, **refuses to
start**. The alternative is a worker that runs fine until the first workflow
needing a credential, then fails from inside a plugin call with an error that
does not mention keys.

## Write a secret

```sh
printf %s "$API_KEY" | cleatctl --db "$DSN" set-secret <tenant-uuid> --name openai
```

The value is read from **stdin**, or `--from-file`, for the same reason the
master key is not a flag.

Writes are **operator-only** — no request path in cleat writes here. Tenant
self-service needs an ownership story this does not yet have, and would let one
tenant probe the namespace of another.

## Retire a secret

```sh
cleatctl --db "$DSN" retire-secret <tenant-uuid> --name openai
```

Stops it resolving: any `${secret:openai}` lookup after this fails with the
same "not found" error as a name that was never set. Needs no master key —
retiring changes only `disabled_at`, never the encrypted value, so an operator
cutting off a leaked credential during an incident is not blocked on
`CLEAT_SECRET_MASTER_KEY` being available.

**Reversible.** Run `set-secret` again for the same name — a real rotation, not
just a revival — and it starts resolving again. There is no separate "revive"
command; `set-secret` already writes the value an operator would need to bring
one back.

`--dry-run` shows what would change without changing anything.

## Reference it from a workflow

Inside a plugin call argument, write `${secret:NAME}`:

```go
h.DurableCall("llm", "chat", `{
  "provider": "anthropic",
  "api_key": "${secret:openai}",
  "messages": [...]
}`)
```

The host substitutes the value on the way in to the plugin.

**The same reference works in an `http.fetch` request**, which is where a
credential is most often needed:

```go
h.DurableCall("http", "fetch", `{
  "url": "https://api.example.com/v1/charges",
  "method": "POST",
  "headers": {"Authorization": "Bearer ${secret:stripe_key}"},
  "body": "{...}"
}`)
```

Write the reference, not the credential. Putting the real value in that request
puts it in `event_history.request`, where the only thing in front of it is
`engine.Redact` — a field-name heuristic, described below, which matches seven
substrings and returns non-JSON input untouched. It does not match `X-Auth`, a
`Cookie`, or a key in a URL query string, where the field name is `url`.

A reference naming a secret the tenant does not have is a **permanent** error,
not a retryable one: retrying cannot create it, and a retry budget spent on a
typo reports as a timeout.

A call made with no tenant in context passes the reference through as literal
text rather than resolving it. One worker serves many tenants, so resolving
against a default would risk sending another tenant's credential; literal text
fails at the far end, loudly, against the right blast radius.

## What is guaranteed, and how

**The guest never holds the value.** A workflow writes a reference. Nothing
returns a secret to WASM — there is no `secrets.get` host call, deliberately,
because it would put plaintext in guest memory and in whatever the guest passed
onward.

**Event history records the reference, not the value.** This is structural
rather than a redaction rule, and it holds on both paths for the same reason.

`PluginCall` records `PluginInput: inputJSON` and separately hands that same
string to the plugin function; substitution happens *inside* a wrapper around
that function, so the recorder cannot observe it.

`DurableCall` is the same shape one layer out: `engine/durablecalls.go` calls
`callService(..., requestJSON, ...)` and then records `Request: requestJSON`
from the SAME variable, and `callintent.go` writes its write-ahead intent row
from that variable BEFORE dispatch. Substitution happens downstream of both,
inside the service caller, so neither the event nor the intent can see a
resolved value.

`TestSecretResolutionStaysPastTheRecorder` asserts that nothing in package
`engine` calls `ResolveSecretRefs`, because moving resolution up into the engine
would write the plaintext into history with no behavioural test failing — the
call would still succeed.

That distinction matters because `engine.Redact` is a **field-name heuristic**:
it catches `password` and `token`, and would not catch a credential under a
field named `config`. Keeping the value out of the recorded string entirely does
not depend on guessing field names.

**A value cannot break out of its JSON string.** References sit inside quotes,
so a value containing `"` would otherwise terminate the string and let the rest
be read as structure — turning a secret into a way to reshape the document the
plugin receives. Values are escaped on substitution.

**One tenant's key does not open another's.** The encryption key is derived per
tenant via HKDF, and the tenant id is additional authenticated data, so a
ciphertext moved between rows fails to open rather than decrypting.

## What is not covered

- **Rotation with overlapping validity.** The schema carries `key_version` so it
  is not precluded, but nothing implements it. Rotating today means rewriting
  every secret under a new master key.
- **An external KMS.** The master key is supplied directly.
- **References outside plugin call arguments.** Workflow input, signals and
  schedule payloads are not scanned.
- **Reading a secret back.** There is no `get-secret`. Values go in and are used;
  they do not come out.

## Failure modes

**A reference naming a secret that is not set is an error, not an empty
string.** Substituting `""` would send an empty credential and the failure would
arrive from the third-party service as an authentication error naming nothing.

**A malformed reference is not a reference.** `${secret:has space}` does not
match, so it reaches the plugin as literal text rather than being
half-interpreted.

**No tenant in context means no substitution.** The reference passes through
unresolved rather than being resolved against a default tenant, which would hand
one tenant's credential to a call made on behalf of nobody.

**A retired secret fails resolution exactly like a name that was never set** —
the same "not found" error, not a distinct "retired" one, so a workflow's error
handling does not need to know the difference. `retire-secret` sets
`disabled_at`; it does not delete the row or touch the ciphertext, which is
what makes the reversal above possible.

**Deleting a tenant deletes its secrets**, by `ON DELETE CASCADE`.
`cleatctl drop-tenant` reports the count.
