# Travel Booking

Demonstrates a travel booking workflow that books flight, hotel, and car rentals in parallel with automatic Saga compensation on failure.

## What it shows

- `Saga.AddParallel` for concurrent booking with automatic LIFO compensation
- `PollCancellation` for mid-booking cancellation handling
- `DurableCallTyped` for type-safe service calls (flights, hotels, cars)
- `SetQueryState` for external booking status queries
- Best-effort notification (send confirmation email)
- Conditional step inclusion (car rental only when requested)

## Build

```bash
cleat build -o /tmp/out ./examples/travel/
```

## Run

```bash
cleat deploy travel /tmp/out/travel.wasm
cleat run BookTravel '{"user_id":"usr_001","flight":{"origin":"PDX","destination":"SFO","date":"2026-06-01","passengers":1},"hotel":{"city":"San Francisco","check_in":"2026-06-01","check_out":"2026-06-03","guests":1},"car":{"city":"San Francisco","pickup_date":"2026-06-01","dropoff_date":"2026-06-03"}}'
```

## Key files

- `booking.go` — workflow entry point (`BookTravel`), domain types, and helpers
