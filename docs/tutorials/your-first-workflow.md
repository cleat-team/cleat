# Your first workflow: order processing

This tutorial builds a realistic order processing workflow. You will learn:

- How to define domain types and entry points
- How the HostCalls interface works
- How to make DurableCall steps
- How to handle errors with compensation
- How to build, deploy, and trigger the workflow
- How to inspect event history

The final workflow looks like the example in `testdata/basic/order.go`.

> Added 2026-10-04 (cleat#3027): the "Before you start" list below. Every DSN in
> this tutorial is written as a placeholder and assumes a running, migrated
> database and an installed CLI, and nothing here said so -- a reader landing on
> this page directly had no way to know what was missing until Step 5 failed.

## Before you start

This tutorial assumes three things, none of which its steps set up:

- `cleat` and `cleat-worker` on your `PATH`
- a Postgres you can reach, with a `cleat` database in it
- that database migrated to the current schema

[Quick start](quick-start.md) covers all three in its Steps 1--3; do those
first if you have not. The connection strings below are placeholders
(`postgres://user:pass@localhost/cleat`) -- substitute the superuser role and
password you created the database with, and a password of your choosing for the
app role in Step 6.

## Step 1: Set up the project

Create a new directory, initialize a Go module, and add the cleat Go SDK:

```bash
mkdir order-workflow
cd order-workflow
go mod init order-workflow
go get github.com/cleat-team/cleat/cleat@latest
```

> Corrected 2026-10-04 (cleat#3027): the `go get` line was missing, and without
> it nothing in this tutorial runs. Step 2's code imports the SDK, and an
> unrequired import means both commands in Step 5 fail before they reach your
> code:
>
> ```
> no required module provides package github.com/cleat-team/cleat/cleat; to add it:
>     go get github.com/cleat-team/cleat/cleat
> ```
>
> Note the path is the **`cleat/cleat` submodule**, not the repository root --
> `go get github.com/cleat-team/cleat@latest` resolves to a different, older
> line, which is the mistake [quick-start](quick-start.md)'s Step 1 warns
> about. Until `order.go` exists nothing imports the SDK yet, so the `require`
> line may carry a `// indirect` comment; `go mod tidy` clears it and the build
> works either way. Measured on the tree build: `go get
> github.com/cleat-team/cleat/cleat@latest` adds `.../cleat/cleat v0.3.2` and
> Step 5 then succeeds.

## Step 2: Define domain types

Create `order.go` with the input types for your workflow:

```go
package main

import (
    "encoding/json"
    "fmt"

    "github.com/cleat-team/cleat/cleat"
)

type CartItem struct {
    SKU      string `json:"sku"`
    Quantity int    `json:"quantity"`
}

type Reservation struct {
    ReservationID string `json:"reservation_id"`
    TotalCents    int    `json:"total_cents"`
}

type Charge struct {
    ChargeID string `json:"charge_id"`
    Amount   int    `json:"amount"`
}
```

## Step 3: Write the main workflow

Add the `PlaceOrder` entry point. This is the function that will be exported as
a WASM entry point:

```go
func PlaceOrder(h cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    if len(cart) == 0 {
        return "", fmt.Errorf("cart is empty")
    }

    // Step 1: Validate items and reserve inventory.
    reservation, err := validateAndReserve(h, userID, cart)
    if err != nil {
        return "", fmt.Errorf("inventory step failed: %w", err)
    }

    // Step 2: Process payment.
    charge, err := processPayment(h, userID, reservation.TotalCents)
    if err != nil {
        // Compensate: release the inventory reservation.
        releaseReservation(h, reservation.ReservationID)
        return "", fmt.Errorf("payment failed: %w", err)
    }

    // Step 3: Fulfill the order (create shipment).
    trackingID, err := fulfillOrder(h, reservation, charge)
    if err != nil {
        // Compensate: refund the payment and release inventory.
        refundPayment(h, charge.ChargeID)
        releaseReservation(h, reservation.ReservationID)
        return "", fmt.Errorf("fulfillment failed: %w", err)
    }

    // Step 4: Notify the customer (best-effort).
    _ = notifyCustomer(h, userID, trackingID)

    // Return a JSON OBJECT, not a bare value.
    //
    // A workflow's result is a string containing a JSON-encoded object -- see
    // ABI.md. `return trackingID, nil` would hand back TRACK-123456, which is
    // not JSON at all, and the host stores {} instead.
    return fmt.Sprintf(`{"tracking_id":%q}`, trackingID), nil
}
```

### About the HostCalls interface

`cleat.HostCalls` is the bridge between your deterministic workflow code and
the outside world. It is passed as the first parameter to all entry point
functions. Every external interaction (database queries, API calls, payment
processing) must go through this interface.

Key methods used in this workflow:

| Method | Purpose |
|--------|---------|
| `DurableCall(service, operation, requestJSON)` | Call an external service. Records request/response in event history. On replay, returns cached result. |
| `DurableLog(message)` | Add a log entry to the workflow event history. |
| `SetQueryState(key, value)` | Store queryable key-value state accessible via REST API. |
| `Now()` | Return the current deterministic time (same on replay). |

## Step 4: Add helper functions

The `PlaceOrder` function calls several helpers. Each helper uses `HostCalls`
to interact with external services. When you pass `h` as a parameter, the
transformer's auto-threading pass ensures it reaches all leaf functions:

```go
func validateAndReserve(h cleat.HostCalls, userID string, cart []CartItem) (Reservation, error) {
    for _, item := range cart {
        if err := checkItemAvailability(h, item.SKU); err != nil {
            return Reservation{}, fmt.Errorf("item %s unavailable: %w", item.SKU, err)
        }
    }
    return reserveInventory(h, userID, cart)
}

func checkItemAvailability(h cleat.HostCalls, sku string) error {
    req, _ := json.Marshal(map[string]string{"sku": sku})
    response, err := h.DurableCall("catalog", "LookupItem", string(req))
    if err != nil {
        return err
    }
    if response == "" {
        return fmt.Errorf("SKU %s not found", sku)
    }
    return nil
}

func reserveInventory(h cleat.HostCalls, userID string, items []CartItem) (Reservation, error) {
    req, _ := json.Marshal(map[string]interface{}{
        "user_id":    userID,
        "item_count": len(items),
    })
    response, err := h.DurableCall("inventory", "Reserve", string(req))
    if err != nil {
        return Reservation{}, err
    }
    _ = response
    return Reservation{ReservationID: "resv_abc123", TotalCents: 3299}, nil
}
```

### Payment and fulfillment

```go
func processPayment(h cleat.HostCalls, userID string, amountCents int) (Charge, error) {
    req, _ := json.Marshal(map[string]interface{}{
        "user_id":      userID,
        "amount_cents": amountCents,
    })
    response, err := h.DurableCall("payments", "Charge", string(req))
    if err != nil {
        return Charge{}, err
    }
    _ = response
    return Charge{ChargeID: "chg_xyz789", Amount: amountCents}, nil
}

func fulfillOrder(h cleat.HostCalls, r Reservation, c Charge) (string, error) {
    req, _ := json.Marshal(map[string]string{
        "reservation_id": r.ReservationID,
        "charge_id":      c.ChargeID,
    })
    response, err := h.DurableCall("shipping", "CreateShipment", string(req))
    if err != nil {
        return "", err
    }
    _ = response
    return "TRACK-123456", nil
}
```

### Compensation functions

These are called when a later step fails:

```go
func releaseReservation(h cleat.HostCalls, reservationID string) error {
    req, _ := json.Marshal(map[string]string{"reservation_id": reservationID})
    _, err := h.DurableCall("inventory", "Release", string(req))
    return err
}

func refundPayment(h cleat.HostCalls, chargeID string) error {
    req, _ := json.Marshal(map[string]string{"charge_id": chargeID})
    _, err := h.DurableCall("payments", "Refund", string(req))
    return err
}

func notifyCustomer(h cleat.HostCalls, userID, trackingID string) error {
    req, _ := json.Marshal(map[string]string{
        "user_id":     userID,
        "tracking_id": trackingID,
    })
    _, err := h.DurableCall("notifications", "SendEmail", string(req))
    return err
}
```

## Step 5: Build the workflow

```bash
cleat build -o ./out ./order.go
```

This produces `out/order.wasm` -- verified 2026-10-04 (cleat#3027). The name
comes from the entry point's source file (`order.go`), because this project has
no `cleat.yaml`; a manifest carrying a `name:` would name the artifact instead.
See [quick-start](quick-start.md)'s Step 5 note for that rule. The build command:

1. **Analyzes** your Go package, finding `PlaceOrder` as an entry point
2. **Traces the call graph** from `PlaceOrder` through all helper functions
3. **Computes the cleat closure** -- verifying every path correctly threads
   `HostCalls`
4. **Transforms** the source -- generating WASM import/export declarations
5. **Compiles** to a `wasip1` binary

To validate without compiling:

```bash
cleat vet .
```

This reports entry points, leaf functions, and any threading errors -- on this
workflow, `Entry points: PlaceOrder` and `Durable leaves: 7`.

> Corrected 2026-10-04 (cleat#3027): this said `cleat vet ./order.go`. `vet`
> takes a **package or directory**, not a file, and the file form exits 1 with
> *"cannot read directory ./order.go: open ./order.go: not a directory. Use
> --lang to specify the language."* (`cleat build` accepts the file form, so
> only this line changes.) The `.` form above exits 0 on the project as
> scaffolded here.

## Step 6: Migrate and deploy

`workflow_defs` carries row-level security, so it has to exist before
`cleat deploy` can write to it. Migrate first, using the Postgres superuser
(or an equivalently privileged role) you created the database with:

```bash
cleat-worker --migrate-only --db "postgres://user:pass@localhost/cleat?sslmode=disable"
```

This applies the schema baseline, which creates the `cleat_app` role
`NOLOGIN`, and exits `0`, without starting the worker.
Run it again on the same database and it changes nothing -- it is safe to
run from a script every deploy.

`cleat_app` is created `NOLOGIN`, so give it a password before anything can
connect as it -- verified 2026-10-04 (cleat#3027): after the migration above the
role reports `rolcanlogin = f`, so this line is what makes Step 7 possible:

```bash
psql "postgres://user:pass@localhost/cleat?sslmode=disable" \
    -c "ALTER ROLE cleat_app LOGIN PASSWORD 'a-password-you-choose';"
```

> This is the one step that needs a PostgreSQL command-line client, and nothing
> above installs or mentions one. Without `psql`, use whatever client your
> Postgres offers -- for instance, if you started it in a container the way
> [quick-start](quick-start.md) Step 2 does (`--name cleat-postgres`):
>
> ```bash
> docker exec cleat-postgres psql -U postgres -d cleat \
>     -c "ALTER ROLE cleat_app LOGIN PASSWORD 'a-password-you-choose';"
> ```
>
> What matters is the effect, not the client: the role must be able to log in.

Now deploy, using the `cleat_app` connection rather than the superuser's --
see [Database role and
tenant](../how-to/deploy-workflows.md#database-role-and-tenant). Verified
2026-10-04 (cleat#3027): `cleat deploy` as `cleat_app` exits 0 and registers the
workflow, so the app role carries the grants deploy needs even though it has no
DDL rights and cannot migrate. (This differs from
[quick-start](quick-start.md), which deploys on the owner DSN; both work.)

```bash
cleat deploy --db "postgres://cleat_app:a-password-you-choose@localhost/cleat?sslmode=disable" \
    --name place_order ./out/order.wasm
```

## Step 7: Run the worker

A worker does not migrate the database on start -- migration is the separate
step above -- and an ordinary start only *verifies* the schema is current,
refusing with the remediation if it is behind.

The worker also refuses to start on a connection row-level security does not
apply to -- a superuser or a role with `BYPASSRLS` -- because
`GetWorkflowByID` and `ListWorkflows` have no application-level tenant
filter and would return every tenant's data on such a connection. So `--db`
has to be the `cleat_app` role from Step 6, not the database's superuser:

```bash
cleat-worker --db "postgres://cleat_app:a-password-you-choose@localhost/cleat?sslmode=disable" \
    --api-addr :8080 \
    --require-auth=false
```

The next time you ship a new migration, apply it the same way you did in
Step 6 (`cleat-worker --migrate-only --db <superuser DSN>`) before
restarting the workers.

A single `--migrate-on-start` flag also exists, as a one-command shortcut
for local, single-node dev -- no separate migrate step:

```bash
cleat-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --api-addr :8080 \
    --require-auth=false \
    --migrate-on-start
```

This connects as the superuser rather than `cleat_app`, since applying the
schema needs DDL rights that role doesn't have. That means the RLS check
from above logs a warning instead of refusing -- `--require-auth=false` is
what turns the refusal into a warning; drop it and this exact command
refuses to start, for the same reason Step 7 does. Fine alone on a laptop;
not once a second tenant's data could land on that connection. See
[Upgrading](../operations/upgrading.md#migration-is-a-deploy-step) for how
this compares to the two-step flow above.

> Verified 2026-10-04 (cleat#3027), because this paragraph reads as though it
> contradicts [quick-start](quick-start.md)'s Step 3 note -- which says
> `--migrate-on-start` on the owner DSN migrates and then **refuses to serve**.
> Both are right, and `--require-auth=false` is the difference: with it, the same
> command logs the RLS text at **WARN** rather than **ERROR** and keeps running
> (measured -- the start endpoint on that port answered `201`); without it, it
> refuses. So quick-start's sentence describes the run *without* the flag, and
> this tutorial's one-command shortcut is real, with the caveat that follows it.

`--require-auth=false` also skips the API key `--generate-api-key` would
otherwise require on every request below -- that's local development only.
See [Deploy via REST API](../how-to/deploy-workflows.md#step-3-deploy-via-rest-api)
for the production path.

## Step 8: Trigger execution

```bash
curl -X POST http://localhost:8080/api/workflows/place_order/start \
    -H "Content-Type: application/json" \
    -d '{
        "entry_point": "PlaceOrder",
        "input": {
            "userID": "user_42",
            "cart": [
                {"sku": "SKU-001", "quantity": 2}
            ]
        }
    }'
```

Record the `id` from the response.

> **Measured 2026-10-04 (cleat#3027): the run fails at its first `DurableCall`,
> and that is expected here.** This workflow calls five external services --
> `catalog`, `inventory`, `payments`, `shipping`, `notifications` -- through
> `h.DurableCall`. A `DurableCall` reaches a **registered endpoint**, and the
> worker started in Step 7 has none, so the run ends `failed` with:
>
> ```
> service catalog.LookupItem not configured: no endpoint registered. Register
> one with --service-endpoints catalog=https://your-service, or implement it as
> a plugin
> ```
>
> The event history below is real and worth reading either way -- it is exactly
> the failed run's, and Step 9's own note says a `failed` run's history is the
> one that is retained. To see a **successful** run, give the worker endpoints:
> `--service-endpoints catalog=...,inventory=...,payments=...,shipping=...,notifications=...`,
> each pointing at something that answers. A loopback or cluster-internal
> destination additionally needs `--plugin-egress-allow-private`, because
> private addresses are refused by default.

## Step 9: Inspect event history

```bash
curl http://localhost:8080/api/workflows/<workflow_id>
```

The response includes:

- **id**: the run's identifier. The field is `id` -- not `workflow_id`, which no endpoint returns.
- **status**: one of the eight `workflow_instances.status` values -- `ready`, `running`, `done`,
  `failed`, `dead_lettered`, `terminated`, `cancelled` or `terminating`. **There is no `completed`,
  and no `suspended`**: a sleeping run is `ready` with `next_wake_at` set. Branch on
  terminal-versus-not rather than on `running`, which most outstanding work never reports.
- **result**: the return value of the entry point, once the run is `done`
- **error**: the error message if the workflow failed (the JSON field is `error`, not `error_msg`)

**The event history is not in this response.** `GET /api/workflows/<id>` returns the instance row
verbatim, and that row has no history on it -- read it from
`GET /api/workflows/<id>/history` instead. That history is still available for a `failed` run
(`--retention-days`, default 30 days) but a `done` run's is deleted at finalize, so do not build a
success trail out of it. See
[Workflow lifecycle](../reference/workflow-lifecycle.md#outcomes).

With the services registered (see Step 8's note), a successful run shows events
like:

1. `call catalog.LookupItem`
2. `call inventory.Reserve`
3. `call payments.Charge`
4. `call shipping.CreateShipment`
5. `call notifications.SendEmail`

If payment fails, the history includes:

1. `call catalog.LookupItem`
2. `call inventory.Reserve`
3. `call payments.Charge` (with error)
4. `call inventory.Release` (compensation)

## Step 10: Handle errors

The workflow above uses manual compensation -- when `processPayment` fails, it
calls `releaseReservation` directly. For workflows with many steps, consider
using `cleat.NewSaga()` for structured compensation:

```go
// The results are declared OUTSIDE the steps: a compensation closure runs
// after its forward closure has returned, so it cannot read a variable
// declared inside it.
var (
    reservation Reservation
    charge      Charge
)
s := cleat.NewSaga()
s.AddStep("reserve_inventory",
    func(h cleat.HostCalls) (string, error) {
        var err error
        reservation, err = reserveInventory(h, userID, cart)
        return "", err
    },
    func(h cleat.HostCalls) error {
        return releaseReservation(h, reservation.ReservationID)
    },
)
s.AddStep("charge_payment",
    func(h cleat.HostCalls) (string, error) {
        var err error
        charge, err = processPayment(h, userID, totalCents)
        return "", err
    },
    func(h cleat.HostCalls) error {
        return refundPayment(h, charge.ChargeID)
    },
)
if err := s.Run(h); err != nil {
    return "", err
}
```

> Corrected 2026-10-04 (cleat#3027): as previously written the forward closures
> declared `reservation` and `charge` with `:=` **inside themselves**, so the
> compensation closures could not see them and the snippet did not compile --
> `declared and not used: reservation`, `undefined: reservation`,
> `undefined: charge`. Both are now declared in the enclosing scope, and the
> corrected snippet compiles (`cleat.NewSaga` and `AddStep` exist with the
> signatures used here, `cleat/runtime_workflow.go`).

The Saga runs forward steps in order. If any step fails, previously completed
steps are automatically compensated in reverse order. See
[Common patterns](../how-to/common-patterns.md) for more details.

## Using DurableCallTyped

The examples above use raw `DurableCall` with manual `json.Marshal`/`json.Unmarshal`.
For production code, prefer `DurableCallTyped` which handles serialization
automatically:

```go
type chargeRequest struct {
    UserID      string `json:"user_id"`
    AmountCents int    `json:"amount_cents"`
}

type chargeResponse struct {
    ChargeID string `json:"charge_id"`
}

var resp chargeResponse
err := h.DurableCallTyped("payments", "Charge",
    chargeRequest{UserID: userID, AmountCents: totalCents},
    &resp,
)
```

This eliminates magic strings and reduces boilerplate. For a fully typed
experience, use `cleat-gen` to generate client wrappers:

```bash
cleat-gen client -o clients/payments/ -service payments -p payments ./specs/payments/
```

## Next steps

- [Common patterns](../how-to/common-patterns.md) -- Saga, fan-out, signals, child
  workflows, retry policies, and polling
- [Deploying to production](../operations/deploying-to-production.md) -- configuration,
  monitoring, scaling
