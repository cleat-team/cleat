# Stream a workflow's tokens to a client

A workflow that streams from a model — `llm.chat_stream`, or any plugin function registered
through `StreamFuncRegistry` — can be followed by a browser as it produces tokens.

    GET /api/workflows/{id}/stream

Server-sent events. `EventSource` in a browser needs no client code beyond the URL; anything
else can read the same stream with `curl -N`.

    curl -N -H "Authorization: Bearer $TOKEN" \
      "$CLEAT/api/workflows/$RUN/stream"

---

## The one thing to understand first

**The durable record is the truth. The live stream is a preview of it.**

That is a decision, recorded on cleat#1572, and everything below follows from it. It is not the
weak version of that sentence — it does not mean "the stream is unreliable, good luck". Every
chunk a workflow streams is written to `event_history` as it arrives, so the preview and the
truth differ only in *latency*, never in content:

| | |
|---|---|
| what you see live | a chunk, milliseconds after the guest produced it |
| what you see on reconnect | the same chunk, from `event_history`, at the same `step` |

A client is therefore never asked to trust the preview. Anything it saw live it can ask for
again. That is why reconnection below is exact rather than best-effort, and it is the property
that makes this endpoint different from tailing a log.

**And a chunk the database REFUSED is never sent at all.** If the write fails — most often because
the run was reclaimed by another worker and this one's fence is lost — the token is dropped rather
than shown. That case is not a preview: this worker's events are not going into the run's history
at all, so there would be nothing to reconnect to. See *Durability*, below.

---

## The events

Every event is JSON in `data:`. Chunks carry an `id:`, which is the cursor.

**`attached`** — sent once, first.

```json
{"workflow_id":"…","generation":3,"status":"running","live":true,"resumed":false}
```

`live` is false when this worker is not the one executing the run; see *Worker-local*, below.
`resumed` is true when the request carried a cursor.

**`chunk`** — one streamed chunk.

```json
{"step":11,"index":1,"content":"lo ","finish":false,"stream_start":false,
 "durable":true,"replay":true,"plugin":"llm","func":"chat_stream"}
```

**`step` and `index` are different things and the difference matters.**

| | |
|---|---|
| `step` | the event's position in the run's whole history. Unique, strictly increasing, **the cursor**. |
| `index` | the chunk's position within *one answer*. **Restarts at 0** for every streaming call. |

A run that makes three model calls produces steps `0,1,2` then `7,8,9` then `15,16` (other events
take the steps in between), with indices `0,1,2`, `0,1,2`, `0,1`. So `step` is what you resume
from and `index` is what tells you where one answer ends and the next begins.

`stream_start` is `index == 0` stated outright, so a client does not have to know that rule.
`finish` marks the last chunk of an answer. `durable` says the chunk's row was in `event_history`
before it was sent — see *Durability*.

`replay` distinguishes a chunk served from `event_history` from one that arrived live. It exists
so a client can tell a re-sent token from a new one — a resumed stream that cannot say which is
which forces the client to guess, and a wrong guess duplicates a paragraph.

**`gap`** — chunks in a range are no longer in `event_history` and cannot be replayed.

```json
{"from":2,"to":4,"message":"…"}
```

History compaction folds old events into a compacted blob and deletes the rows it folded. The
endpoint detects the discontinuity from the **indices** — consecutive within one answer — and says
so. It is deliberately not detected from `step`, which is not contiguous even in a healthy run:
any other event takes a step.

**A transcript with a silent hole in it reads as continuous prose**, which is why this event
exists rather than the endpoint quietly serving what it has.

**`lagged`** — this reader fell behind and was dropped from the live tail.

Reconnect with `Last-Event-ID` and the missing chunks are served from `event_history`. Nothing is
lost; see *Backpressure*, below.

**`end`** — the stream is over, with a `reason`: the stream finished, the run reached a terminal
status, or there is no live tail on this worker.

**`error`** — the history read failed.

Keepalives are SSE comment lines (`: keepalive`), every 15 seconds. A client's event handler
never sees them; they exist so a proxy does not close an idle connection while a model thinks.

---

## Reconnection

Event ids are `generation:step`. A browser resends the last one it saw as `Last-Event-ID`
automatically, so reconnection needs no client code. Other clients can pass it as a query
parameter instead:

    curl -N "$CLEAT/api/workflows/$RUN/stream?last_event_id=3:17"

The endpoint serves every chunk past that step from `event_history`, then joins the live tail.

**One number, because one is enough.** `step` increases for every event in a run, so it is already
a complete cursor. The index is not — it restarts per answer, so a cursor carrying it would resume
in the wrong one.

`generation` rides along so a client can see the run changed hands. It does **not** invalidate
anything: history is append-only and a reclaimed run replays rather than rewriting its chunks.
It is reported, not enforced.

A cursor the endpoint cannot parse is **ignored, not refused**, and the whole stream is served from
the beginning. Refusing would strand a client whose only recovery is the request being refused.

### What reconnection cannot recover

A crash **genuinely mid-stream** — the worker dies with a streaming call in flight. Chunks
written before the crash are in history and are served. What happens to the run afterwards is
ordinary durable-execution behaviour and is not this endpoint's to decide.

---

## Durability, and `?durable_only=true`

Every chunk carries `durable`, saying whether its row was in `event_history` at the moment it was
sent. There are exactly three cases, and only two of them reach a client:

| | `durable` | sent? |
|---|---|---|
| written, then published — **the ordinary worker** | `true` | yes |
| not written *yet*: `--no-per-step-flush`, or an engine with no database | `false` | yes |
| write **refused** — the fence was lost to another worker | — | **no** |

The third is the one worth understanding. A refusal is not a delay: this worker no longer owns the
run, so its events are not going into that run's history at all. Showing those tokens would put
output on a user's screen that the system does not believe happened, and unlike a preview there is
no row to reconnect to. So they are dropped.

Anything served **from** `event_history` on connect or reconnect is `durable: true` by
construction — it was just read from there.

**`?durable_only=true`** refuses to send anything not already persisted. On an ordinary worker this
changes nothing, and that is the point: chunks are written as they arrive, so the default stream is
already durable-only. The option exists for two readers:

- a deployment running `--no-per-step-flush`, where events reach the database at segment end and
  the live tail genuinely runs ahead of the record;
- a caller who would rather **state** the requirement than depend on a default staying true — an
  audit view, a transcript that gets stored, anything where showing a token that later is not in
  the history is worse than showing it late.

Under `--no-per-step-flush` a `durable_only` reader sees little or nothing live; the chunks arrive
in `event_history` when the segment finalizes and a reconnect serves them. That is the honest
trade, not a bug — but if it is your situation, prefer leaving per-step flush on.

## One connection per run, not per answer

The stream carries **every** answer the run produces, and stays open until the run reaches a
terminal status. A client segments the transcript itself: `stream_start` opens an answer,
`finish` closes it.

There is no per-answer filter, and that is a measurement rather than an omission. A step
identifies one **chunk** — `recordEvent` increments the step counter for every event — so a filter
on it would select a single token, not a call. What separates answers is the index restarting,
and that boundary already travels with each chunk.

For an agent UI this is the shape you want anyway: one connection for the run, several answers
down it.

---

## Worker-local

The live tail is in memory on the worker executing the run. A request that lands on a different
worker gets:

- the durable history, complete as of the moment it read, and
- `attached` with `live: false`, and
- `end` if the run has already finished.

It is not wrong — the history it served is real — but it is degraded, and the response says so
rather than letting a client conclude the model stopped producing tokens. **Routing a reader to
the worker running the job is not solved in this version** — cleat#1639. Behind a load balancer,
expect `live: false` on a fraction of connections proportional to your worker count. A client that
sees it can poll `GET /api/workflows/{id}` for the recorded result instead, or reconnect and hope
for a better worker.

---

## Backpressure

The publisher is a workflow executing a plugin call. A reader that could block it would stall
guest execution on that worker's slot — so a reader that falls behind is **dropped, not waited
on**, and told with a `lagged` event.

This is affordable only because chunks are durable: a dropped reader reconnects with its cursor
and is served the missing chunks from the database. On the premise this feature was originally
filed with — that partial output had no durable existence — dropping a reader would have been
data loss.

`--max-stream-readers` (default 1024, 0 for unlimited) bounds concurrent readers on a worker.
Over the ceiling the route answers `503` with `Retry-After`.

**This is not `--connection-budget`.** That budget governs database pools, and a reader **holds**
no database connection: it costs a goroutine, an HTTP connection and a bounded chunk buffer.

It is not free of the database either, and the difference matters at the ceiling. Each reader
issues one status query per heartbeat — a single indexed row read every 15 seconds, so 1024
readers is on the order of 70 queries a second against `workflow_instances`. Brief, pooled, and
not a held connection, but budget for it rather than reading "no database connection" as "no
database load".

---

## Authorization

The stream is scoped exactly like every other run-scoped read: the request's tenant-scoped store
is asked for the run, and a run belonging to another tenant is a `404`, indistinguishable from an
id that does not exist.

The check happens **before** any subscription is opened. The hub that carries live chunks is keyed
by workflow id and enforces nothing itself.

---

## See also

- `docs/playbooks/ai-agent-platform.md` — where this fits in a full agent application
- cleat#1572 — the issue, including the measurement that settled whether chunks persist as they
  arrive, and the decision note on preview-versus-truth
