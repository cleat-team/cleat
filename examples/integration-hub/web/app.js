// The integration hub's page.
//
// It holds no worker credentials and never calls the worker: every request goes
// to this origin, and the backend attaches the API key server-side. See the
// comment in index.html -- a browser holding that key would be handing the whole
// tenant to anyone who opened dev tools.
//
// The three statuses it paints are the three the workflow actually publishes
// through SetQueryState, read back with one GET /api/syncs/{id}.

const $ = (sel) => document.querySelector(sel);

// pollMs is the runs/deliveries refresh. Two seconds, because the thing being
// watched is a durable sleep the scenario measures in tens of seconds, and a
// page that polled faster would spend its time proving the network works.
const pollMs = 2000;

let config = {};

// ---- rendering ----

function el(tag, text, className) {
  const n = document.createElement(tag);
  if (text !== undefined && text !== null) n.textContent = String(text);
  if (className) n.className = className;
  return n;
}

function shortID(id) {
  return id ? id.slice(0, 8) + "…" : "";
}

function renderConfig() {
  const dl = $("#config");
  dl.replaceChildren();
  for (const [k, v] of [
    ["workflow", config.workflow],
    ["entry point", config.entry_point],
    ["tenant", config.tenant || "(worker default)"],
    ["ingest source", shortID(config.source_id) || "(CLEAT_SOURCE_ID unset)"],
  ]) {
    dl.append(el("dt", k), el("dd", v));
  }
  $("#event-type").value = config.default_event || "contact.updated";
}

function renderRuns(runs) {
  const tbody = $("#runs tbody");
  tbody.replaceChildren();
  $("#runs-empty").hidden = runs.length > 0;

  for (const run of runs) {
    const tr = el("tr");

    tr.append(el("td", shortID(run.id), "mono"));

    const status = el("td");
    status.append(el("span", run.status, "pill " + pillFor(run.status)));
    tr.append(status);

    // The stage is the QUERY STATE, not the run status. It is the half the
    // workflow publishes for an operator: which step a live sync is on, and for
    // a failed one which step failed. Two vocabularies, two columns -- and note
    // that BOTH end at the word `done`, for unrelated reasons: one is
    // workflow_instances.status, the other is what hub.go chose to publish.
    tr.append(el("td", run.state.status || ""));
    tr.append(el("td", run.customer_id || ""));
    tr.append(el("td", shortID(run.delivery_id), "mono"));

    const actions = el("td");
    // Offered while the run can still be waiting on its inbound event, which is
    // every status before it settles. `ready` is the run that has been created
    // and not yet claimed, which is precisely the one that needs the event.
    if (["ready", "running", "terminating"].includes(run.status)) {
      const btn = el("button", "Deliver it");
      btn.type = "button";
      btn.className = "secondary";
      btn.addEventListener("click", () => deliverInbound(run));
      actions.append(btn);
    }
    tr.append(actions);

    tbody.append(tr);

    // The failure is a full-width row under its run rather than a cell, because
    // it is long and it is the thing an operator needs to read.
    if (run.error) {
      const td = el("td", run.error, "run-error");
      td.colSpan = 6;
      const row = el("tr");
      row.append(td);
      tbody.append(row);
    }
  }
}

// pillFor colours two vocabularies, because the page shows two -- and they are
// different state machines, which is the distinction cleat#2050 found a document
// conflating.
//
//   RUN       ready | running | terminating | cancelled | done | failed |
//             dead_lettered | terminated          (engine/status_vocabulary.go)
//   DELIVERY  pending | retrying | delivered | failed | cancelled
//
// THE RUN VOCABULARY HAS NO `completed`. A run that finished successfully is
// `done`. `completed` is a plausible-looking word that appears nowhere in
// `workflow_instances.status`, and this function had it until a scenario run
// printed "FAIL: the app's run did not complete; it reads 'done'" -- the wait
// was looking for a status the engine does not write, against a run that had
// already finished. The same spelling was in scripts/run-integration-hub-
// scenario.sh's wait and is fixed there too.
//
// `retrying` is painted separately from `failed` on the delivery side: it is a
// FAILED ATTEMPT the loop intends to repeat, so it is neither finished nor a
// state an operator has to act on. `failed` is the attempt ceiling and does.
// There is no `dead_lettered` DELIVERY -- that word belongs to runs, and to
// inbound events, both of which write it and neither of which is this table.
function pillFor(status) {
  switch (status) {
    case "done":
    case "delivered":
      return "ok";
    case "cancelled":
    case "terminated":
      // Over, but not by failing: a webhook deleted underneath a delivery, or a
      // run stopped deliberately.
      return "muted";
    case "failed":
    case "dead_lettered":
      return "bad";
    default:
      // ready | running | terminating | pending | retrying
      return "wait";
  }
}

function renderDeliveries(payload) {
  const tbody = $("#deliveries tbody");
  tbody.replaceChildren();
  const rows = payload.deliveries || [];
  $("#deliveries-empty").hidden = rows.length > 0;

  for (const d of rows) {
    const tr = el("tr");
    tr.append(el("td", new Date(d.created_at).toLocaleTimeString(), "mono"));
    tr.append(el("td", d.event_type || ""));

    const status = el("td");
    status.append(el("span", d.status, "pill " + pillFor(d.status)));
    tr.append(status);

    tr.append(el("td", d.attempt_count));
    tr.append(el("td", d.response_code ? String(d.response_code) : "", "mono"));
    tbody.append(tr);
  }
}

// ---- requests ----

async function getJSON(url) {
  const res = await fetch(url, { headers: { Accept: "application/json" } });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.status + " " + res.statusText);
  return body;
}

async function startSync(event) {
  event.preventDefault();
  const form = new FormData(event.target);
  const status = $("#start-status");
  status.textContent = "";
  try {
    let payload;
    try {
      payload = JSON.parse(form.get("payload") || "{}");
    } catch (e) {
      throw new Error("payload is not valid JSON: " + e.message);
    }
    const res = await fetch("/api/syncs", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        // A fresh key per press, so a double-click returns the ORIGINAL run
        // rather than dispatching the customer's event twice. The backend
        // forwards it and reports back whether this was a replay.
        "Idempotency-Key": crypto.randomUUID(),
      },
      body: JSON.stringify({
        customer_id: form.get("customer_id"),
        event_type: form.get("event_type"),
        payload,
      }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || res.status + " " + res.statusText);
    status.textContent =
      "started " + shortID(body.id) + (body.idempotent_replay ? " (idempotent replay)" : "");
    status.className = "status ok";
    refresh();
  } catch (e) {
    status.textContent = e.message;
    status.className = "status bad";
  }
}

async function deliverInbound(run) {
  const status = $("#start-status");
  try {
    await fetch("/api/inbound", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        event_type: run.event_type || config.default_event,
        payload: { id: run.customer_id },
      }),
    }).then(async (res) => {
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error || res.status + " " + res.statusText);
      }
    });
    status.textContent = "delivered the inbound event";
    status.className = "status ok";
    refresh();
  } catch (e) {
    status.textContent = e.message;
    status.className = "status bad";
  }
}

async function filterDeliveries(event) {
  if (event) event.preventDefault();
  const form = new FormData($("#delivery-filter"));
  const q = new URLSearchParams();
  if (form.get("status")) q.set("status", form.get("status"));
  // Sent as an ASSERTION, not a filter: the worker scopes this route by the API
  // key's tenant, so a request naming another one is refused with the reason.
  // A control that quietly accepted it would be a dropdown that filters nothing.
  if (form.get("tenant")) q.set("tenant", form.get("tenant"));

  const status = $("#delivery-status");
  try {
    renderDeliveries(await getJSON("/api/deliveries?" + q.toString()));
    status.textContent = "";
    status.className = "status";
  } catch (e) {
    status.textContent = e.message;
    status.className = "status bad";
    renderDeliveries({ deliveries: [] });
  }
}

// ---- polling ----

async function refresh() {
  try {
    const { runs } = await getJSON("/api/syncs");
    // The list endpoint carries the run summary; the stage and the delivery id
    // come from the query state, which is a second call per run.
    const detailed = await Promise.all(
      (runs || []).map(async (r) => {
        const one = await getJSON("/api/syncs/" + encodeURIComponent(r.id));
        return Object.assign({}, r, {
          state: one.state || {},
          delivery_id: (one.state || {}).delivery_id,
          customer_id: (one.state || {}).customer_id,
          event_type: (one.state || {}).event_type,
          error: one.error,
        });
      })
    );
    renderRuns(detailed);
  } catch (e) {
    $("#runs-empty").hidden = false;
    $("#runs-empty").textContent = "Could not read the runs: " + e.message;
  }
}

async function main() {
  $("#start-form").addEventListener("submit", startSync);
  $("#delivery-filter").addEventListener("submit", filterDeliveries);
  try {
    config = await getJSON("/api/config");
    renderConfig();
  } catch (e) {
    $("#start-status").textContent = "Could not read /api/config: " + e.message;
    $("#start-status").className = "status bad";
  }
  await refresh();
  await filterDeliveries();
  setInterval(() => {
    refresh();
    filterDeliveries();
  }, pollMs);
}

main();
