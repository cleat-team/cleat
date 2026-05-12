# Your first workflow: order processing

This tutorial builds a realistic order processing workflow. You will learn:

- How to define domain types and entry points
- How the HostCalls interface works
- How to make DurableCall steps
- How to handle errors with compensation
- How to build, deploy, and trigger the workflow
- How to inspect event history

The final workflow looks like the example in `testdata/basic/order.go`.

## Step 1: Set up the project

Create a new directory and initialize a Go module:

```bash
mkdir order-workflow
cd order-workflow
go mod init order-workflow
```

## Step 2: Define domain types

Create `order.go` with the input types for your workflow:

```go
package main

import (
    "encoding/json"
    "fmt"

    "github.com/rcownie/cleat/cleat"
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
    return trackingID, nil
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

This produces `out/order.wasm`. The build command:

1. **Analyzes** your Go package, finding `PlaceOrder` as an entry point
2. **Traces the call graph** from `PlaceOrder` through all helper functions
3. **Computes the cleat closure** -- verifying every path correctly threads
   `HostCalls`
4. **Transforms** the source -- generating WASM import/export declarations
5. **Compiles** to a `wasip1` binary

To validate without compiling:

```bash
cleat vet ./order.go
```

This reports entry points, leaf functions, and any threading errors.

## Step 6: Deploy

```bash
cleat deploy --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --name place_order ./out/order.wasm
```

## Step 7: Run the worker

```bash
cleat-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --api-addr :8080
```

## Step 8: Trigger execution

```bash
curl -X POST http://localhost:8080/api/workflows \
    -H "Content-Type: application/json" \
    -d '{
        "def_name": "place_order",
        "entry_point": "PlaceOrder",
        "input": {
            "user_id": "user_42",
            "cart": [
                {"sku": "SKU-001", "quantity": 2}
            ]
        }
    }'
```

Record the `workflow_id` from the response.

## Step 9: Inspect event history

```bash
curl http://localhost:8080/api/workflows/<workflow_id>
```

The response includes:

- **status**: `completed`, `failed`, or `running`
- **event_history**: ordered list of every DurableCall, with service, operation,
  request, response, and timing
- **result**: the return value of the entry point
- **error_msg**: the error message if the workflow failed

A successful run will show events like:

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
s := cleat.NewSaga()
s.AddStep("reserve_inventory",
    func(h cleat.HostCalls) (string, error) {
        reservation, err := reserveInventory(h, userID, cart)
        return "", err
    },
    func(h cleat.HostCalls) error {
        return releaseReservation(h, reservation.ReservationID)
    },
)
s.AddStep("charge_payment",
    func(h cleat.HostCalls) (string, error) {
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

The Saga runs forward steps in order. If any step fails, previously completed
steps are automatically compensated in reverse order. See
[Common patterns](common-patterns.md) for more details.

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
- [Deploying to production](../guide/deploying-to-production.md) -- configuration,
  monitoring, scaling
