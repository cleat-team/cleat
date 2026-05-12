# FoodDash

A complete food delivery orchestration workflow: validate menu, charge, dispatch driver, notify restaurant, and track pickup.

## What it shows

- Multi-step durable orchestration (validate, charge, dispatch, notify, track)
- Saga-based compensation on failure (refund, release driver, cancel restaurant)
- `cleat.NewSaga` / `AddStep` with forward and compensate callbacks
- `AwaitSignals` with timeout for driver acceptance and pickup confirmation
- `SetQueryState` for external order-status queries
- `DurableCallTyped` for type-safe service calls
- `ContinueAsNew` for long-running orders with many items
- Version evolution pattern with `h.Version()` / `h.MinVersion()`

## Build

```bash
cleat build -o /tmp/out ./examples/fooddash/
```

## Run

```bash
cleat deploy fooddash /tmp/out/fooddash.wasm
cleat run PlaceOrder '{"userID":"usr_001","restaurantID":"rest_1","items":[{"sku":"pizza","name":"Pepperoni","quantity":1}],"address":{"street":"123 Main St","city":"Portland","zipCode":"97201"}}'
```

## Key files

- `order.go` — workflow entry points (`PlaceOrder`, `CancelOrder`, `GetOrderStatus`, `PlaceLargeOrder`)
- `order_test.go` — workflow tests
- `order_typed.go` — typed domain types and helpers
- `clients/` — external service client stubs
- `spec/` — specification tests
