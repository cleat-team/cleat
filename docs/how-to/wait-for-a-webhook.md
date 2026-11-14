# How to wait for an inbound webhook

A workflow can pause until an outside system -- a payment provider, a CRM --
delivers an HTTP event. Two pieces do it:

- the **`webhook-ingest` plugin** receives the event at `POST /ingest/{source_id}`
  and stores it;
- your workflow **claims** it with the plugin's `await_webhook` function.

This page is the whole path: set up a source, wait in a workflow, deliver an
event, and what goes wrong. The commands and both loops below were run against a
worker started the way the [quick start](../tutorials/quick-start.md) starts it,
plus the one setting it leaves out. The last two rows of the table and the closing
paragraph describe the plugin from its source and were not run.

## 1. Give the worker a secret master key

A source has a signing secret, and the worker seals it before storing it. With no
master key it cannot, and creating a source fails:

```
HTTP/1.1 500 Internal Server Error
{"error":"failed to store secret: this worker has no secret master key configured; set CLEAT_SECRET_MASTER_KEY (a base64-encoded 32-byte key) on the worker and restart it"}
```

Generate a key and start the worker with it:

```bash
export CLEAT_SECRET_MASTER_KEY=$(head -c 32 /dev/urandom | base64)
cleat-worker --db "$CLEAT_APP_DSN" --api-addr :8080
```

**Keep that value.** A secret sealed under one key cannot be read under another;
see [Use secrets](use-secrets.md) for rotation and for giving the same key to every
worker.

## 2. Create a source

A source is one sender of events. Create it once, with the key from the quick start
(`$CLEAT_API_KEY`):

```bash
curl -X POST http://localhost:8080/ingest/sources \
    -H "Authorization: Bearer $CLEAT_API_KEY" \
    -H "Content-Type: application/json" \
    -d '{"name": "payments", "source_type": "payments", "secret": "whsec_local_dev"}'
```

The response carries the source's `id`. That is the `source_id` the workflow waits
on, and `/ingest/<id>` is the address the sender posts to. `secret` is required:
an unsigned source cannot be created.

## 3. Wait in the workflow

`await_webhook` takes a `source_id` and an `event_type`. **It is a single poll.**
When no matching event is waiting it returns `{"found": false}` at once -- a workflow
that calls it one time and returns has not waited, and finishes `done` with that
result, which looks like success. To wait, loop, and sleep durably between attempts:

```go
func WaitForPayment(h cleat.HostCalls, input string) (string, error) {
	var in struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return "", fmt.Errorf(`input must be {"source_id": "..."}: %w`, err)
	}
	req, err := json.Marshal(map[string]string{
		"source_id":  in.SourceID,
		"event_type": "payment.confirmed",
	})
	if err != nil {
		return "", err
	}

	for attempt := 0; attempt < 60; attempt++ {
		raw, err := h.PluginCall("webhook-ingest", "await_webhook", string(req))
		if err != nil {
			return "", fmt.Errorf("await_webhook: %w", err)
		}
		var got struct {
			Found   bool            `json:"found"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			return "", fmt.Errorf("decode await_webhook response: %w", err)
		}
		if got.Found {
			return string(got.Payload), nil
		}
		h.DurableSleepMs(1000) // recorded: a restart resumes past the sleeps
	}
	return "", fmt.Errorf("no payment.confirmed event within 60 s")
}
```

The attempt count and the sleep are your wait window. Start the run with the source
id as an **object** -- `{"input": {"source_id": "<id>"}}` -- because the entry
point's single `string` parameter receives the input's JSON as written (`cleat
build` warns about this as W003).

The same loop in Python, with the SDK's real method names:

```python
import json
from cleat_sdk import HostCalls, cleat_entry

@cleat_entry("wait_for_payment")
def wait_for_payment(h: HostCalls, source_id: str) -> str:
    for _ in range(60):
        got = json.loads(h.plugin_call(
            "webhook-ingest", "await_webhook",
            {"source_id": source_id, "event_type": "payment.confirmed"}))
        if got.get("found"):
            return json.dumps(got.get("payload"))
        h.sleep(1)
    raise RuntimeError("no payment.confirmed event within 60 s")
```

### One call, for a wait of minutes

In Go, the SDK in this repository has a typed call that does the waiting for you.
It suspends on the plugin's own wake-up signal instead of polling on a timer, takes
a window as a `time.Duration`, and can correlate one event to one order with
`Keys`: `webhookingest.AwaitWebhookWaiter.WaitFor(h, webhookingest.AwaitWebhookInput{SourceID: id, EventType: "payment.confirmed"}, 5*time.Minute)`,
from `github.com/cleat-team/cleat/cleat/pluginclients/webhookingest`. It returns the
claimed event's `AwaitWebhookOutput` (its `Payload` is the event body) and an error.

**It is not in a published release yet, so this page does not show it as a code
block**: the snippet guard compiles every block against the released SDK, which
does not have it. `awaitPaymentConfirmation` in `examples/order-lifecycle/order.go`
is the working call, built and tested in CI.

A window that closes with no event is an **error**, not `found: false`
(on a worker, `wait_for_event: no event for webhook-ingest/await_webhook within <n>ms`;
the in-process `cleattest` runtime words it differently), so the `err` branch is also
your timeout branch. `examples/order-lifecycle` uses it and
turns the timeout into a compensation.

Two limits. It is **for waits of minutes**: while no event arrives the engine
re-checks about every 6 s and records history events each time: about 950 an
hour, measured on a real worker (cleat#3806 tracks an engine-level wait that
records none). For a wait of hours or days use the loop above, with a
longer sleep between attempts. And it landed in cleat#3804; **as of 2026-11-14 it is not in 0.6.0**,
so on 0.6.0 use the loop.

## 4. Deliver an event

The sender posts to `/ingest/<source_id>`. The source has a secret, so the request
must be signed: an HMAC-SHA256 of the body, hex-encoded, in `X-Hub-Signature-256`.
The event **type** is a header, `X-Event-Type` (or `X-Github-Event`), not a field in
the body:

```bash
BODY='{"order":"o-1","amount":4200}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac 'whsec_local_dev' -r | cut -d' ' -f1)"

curl -X POST "http://localhost:8080/ingest/$SOURCE_ID" \
    -H "Content-Type: application/json" \
    -H "X-Event-Type: payment.confirmed" \
    -H "X-Hub-Signature-256: $SIG" \
    -d "$BODY"
# {"event_type":"payment.confirmed","id":"...","received":true}
```

No auth header is needed on this route: the signature is the authentication.

## What goes wrong

| Symptom | Cause |
|---|---|
| `unknown field "event_types"` (or `"source_name"`, `"timeout_secs"`) from `await_webhook` | The input has fields the plugin does not take. It takes `source_id` (required), `event_type`, and `keys`. |
| The run finishes `done` with `{"found":false}` | The workflow polled once and returned. Loop, as in step 3. |
| The run fails after the whole window with "no event" although the event is in the table | The event type did not match. With neither header present the type is stored as the literal `webhook`, and `await_webhook` filters on the exact type it was given. Set `X-Event-Type`. |
| `500 failed to store secret: this worker has no secret master key configured…` | Step 1. |
| An event arrives that no run is waiting for | It stays stored. The next run that waits on that source and type claims the **oldest** unclaimed one, so leftovers from a test appear in the next run. |
| A run keeps getting `{"found":false}` after its source was deleted | Deleting a source cancels its pending events. The call cannot tell a deleted source from a quiet one; check `GET /ingest/sources/<id>`. |

Many orders can share one source. To hand each event to the order it belongs to, give
the source a correlation key field when you create it and pass the order's value in
`keys` (at most two) when you wait; the source and the call must agree, or the call
is refused. `examples/order-lifecycle` is the worked case.
