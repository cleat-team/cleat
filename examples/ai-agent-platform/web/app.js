// The AI agent platform's page.
//
// It holds no worker credentials and never calls the worker: every request goes
// to this origin, and the backend attaches the API key server-side. See the
// comment in index.html -- a browser holding that key would be handing the whole
// tenant to anyone who opened dev tools.
//
// The statuses it paints are what the workflow itself publishes through
// SetQueryState (examples/ai-agent-platform/agent.go), read back with one
// GET /api/agents/{id}. Nothing here is derived from the run's engine status,
// which cannot tell a run calling a model from a run parked on a person.

const $ = (sel) => document.querySelector(sel);

// pollMs is the run-list refresh. Two seconds, because what is being watched is
// a durable run: it sleeps between steps, and it parks for about thirty seconds
// on a human. A page that polled faster would spend its time proving the network
// works, and the approval wait is explicitly polled at this same interval by the
// workflow itself (ApprovalPollMs), so nothing here can resolve finer than that
// anyway.
const pollMs = 2000;

let config = {};

// selected is the run whose detail panel is open, by id, or null. It is an id
// rather than the object so that the panel re-reads on every poll instead of
// holding whatever the run looked like when it was clicked.
let selected = null;

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

// money renders a dollar amount, and its one job beyond the dollar sign is NOT
// to round a small nonzero amount down to "$0.00".
//
// The stub model's prices make sub-cent spends ordinary, and a spend column that
// reads "$0.00 / $0.05" is wrong in the one direction that matters: it says the
// run has spent nothing while it is spending. Four decimals below half a cent is
// ugly and honest; two would be tidy and false.
function money(s) {
  const n = Number(s);
  if (!isFinite(n)) return "$0.00";
  if (n !== 0 && n < 0.005) return "$" + n.toFixed(4);
  return "$" + n.toFixed(2);
}

// pillFor colours the workflow's own status vocabulary. These nine are what
// agent.go publishes, and they are the whole of it:
//
//   running | thinking | tool_call | awaiting_approval | approval_received |
//   approval_timeout | done | failed | budget_exceeded
//
// THERE IS NO `completed` STATE, and the trap is not hypothetical. A run that
// finished successfully is `done` -- the last SetQueryState before RunAgent
// returns, and AgentOutput.Status. `completed` is a plausible-looking word that
// appears nowhere in this workflow. examples/integration-hub/web/app.js carried
// it until a scenario run printed "FAIL: the app's run did not complete; it
// reads 'done'" -- a wait looking for a status the engine never writes, against
// a run that had already finished. A status map that looks for it shows every
// finished run as still working.
//
// TWO OF THESE ARE UNHAPPY WITHOUT BEING FAILURES, and painting them red would
// say the system broke when it did what it was told:
//
//   budget_exceeded  -- the ceiling the CALLER set, reached. That is the feature
//     working, and agent.go stops the loop for it rather than running on.
//   approval_timeout -- nobody answered inside the window. agent.go is explicit
//     that this must not fail the run: "nobody approved in time" and "the
//     approval machinery is down" have to stay distinguishable in the run list,
//     and a failed run makes them look alike.
//
// So those two get the warning class and the rest of a live run gets the neutral
// one, which is the point of having both: an operator's eye should land on the
// runs that want a decision, not on the eight that are simply working.
function pillFor(status) {
  switch (status) {
    case "done":
    case "approval_received":
      return "ok";
    case "budget_exceeded":
    case "approval_timeout":
      return "wait";
    case "failed":
      return "bad";
    default:
      // running | thinking | tool_call | awaiting_approval -- and "" for a run
      // created a moment ago that has published nothing yet.
      return "info";
  }
}

// stateOrder is the order a reader wants: what the run is doing, then what it
// has spent, then the fields that explain a stop. The optional third element
// formats the value.
//
// Anything the workflow publishes that is NOT in this list is still rendered,
// after these -- see stateRows. A panel that iterates only its known keys hides
// a field the day the workflow adds one, and the omission is invisible from the
// page: the state simply looks complete.
const stateOrder = [
  ["status", "agent status"],
  ["current_tool", "current tool"],
  ["steps", "steps"],
  ["spent_usd", "spent", money],
  ["budget_usd", "budget", money],
  ["total_tokens", "tokens"],
  ["approval_reason", "approval reason"],
  ["approval_polls", "approval polls"],
  ["stopped_reason", "stopped reason"],
  ["failed_step", "failed step"],
  ["tenant_id", "customer"],
];

function stateRows(state) {
  const known = new Set(stateOrder.map((e) => e[0]));
  const rows = [];
  for (const [key, label, fmt] of stateOrder) {
    if (!(key in state)) continue;
    rows.push([label, (fmt || String)(state[key])]);
  }
  for (const key of Object.keys(state).sort()) {
    if (!known.has(key)) rows.push([key, state[key]]);
  }
  return rows;
}

function renderConfig() {
  const dl = $("#config");
  dl.replaceChildren();
  for (const [k, v] of [
    ["workflow", config.workflow],
    ["entry point", config.entry_point],
    ["tenant", config.tenant || "(worker default)"],
    ["approval event", config.approval_event_type],
  ]) {
    dl.append(el("dt", k), el("dd", v));
  }

  // The form's two numeric defaults come from the backend rather than being
  // written here, so the value a reader sees pre-filled and the value a run is
  // actually given have one source. A page that hard-coded them would be right
  // until someone changed the workflow's.
  $("#max-steps").value = config.default_max_steps;
  $("#budget-usd").value = config.default_budget_usd;
}

// spendCell renders "spent / budget", and the two halves are published at
// different moments: budget_usd once at the start, spent_usd after every model
// call. A run that has not made a call yet has published no spent_usd at all,
// and "$0.00" is the honest reading of that rather than a missing value.
function spendCell(state) {
  if (!state.budget_usd) return "—";
  return money(state.spent_usd) + " / " + money(state.budget_usd);
}

function renderRuns(runs) {
  const tbody = $("#runs tbody");
  tbody.replaceChildren();
  $("#runs-empty").hidden = runs.length > 0;

  for (const run of runs) {
    const state = run.state || {};
    const tr = el("tr");
    if (run.id === selected) tr.className = "selected";

    tr.append(el("td", shortID(run.id), "mono"));

    // The AGENT status, not the run's. They are different vocabularies and both
    // are real: workflow_instances.status is ready/running/done/failed for the
    // whole life of a run, so a run that is asleep and a run parked on a person
    // are both `running` there. Only what agent.go publishes distinguishes them,
    // and that is this column. The engine's word is in the detail panel, where
    // the two are shown side by side.
    const status = state.status || run.status || "";
    const pill = el("td");
    pill.append(el("span", status, "pill " + pillFor(status)));
    tr.append(pill);

    tr.append(el("td", run.task || ""));

    // A run that has published no `steps` yet has taken none, which is what the
    // column should read rather than blank.
    tr.append(el("td", state.steps || "0", "mono"));
    tr.append(el("td", spendCell(state), "mono"));

    const actions = el("td");
    const btn = el("button", run.id === selected ? "Hide" : "Detail");
    btn.type = "button";
    btn.className = "secondary";
    btn.addEventListener("click", () => {
      selected = run.id === selected ? null : run.id;
      refresh();
    });
    actions.append(btn);
    tr.append(actions);

    tbody.append(tr);
  }
}

function renderDetail(detail) {
  const box = $("#detail");
  box.replaceChildren();

  if (!detail) {
    box.append(el("p", "No run selected.", "hint"));
    return;
  }

  const state = detail.state || {};

  const head = el("div", null, "detail-head");
  head.append(el("span", shortID(detail.id), "mono"));
  const status = state.status || detail.status || "";
  head.append(el("span", status, "pill " + pillFor(status)));
  // Both words, named, because they are different machines and the page would be
  // misleading if it showed one without saying which it was.
  head.append(el("span", "engine: " + (detail.status || "?"), "detail-engine"));
  box.append(head);

  if (detail.task) box.append(el("p", detail.task, "detail-task"));

  const stateDL = el("dl", null, "pairs");
  for (const [label, value] of stateRows(state)) {
    stateDL.append(el("dt", label), el("dd", value));
  }
  if (!stateDL.childElementCount) {
    stateDL.append(el("dt", "state"), el("dd", "(nothing published yet)"));
  }
  box.append(stateDL);

  if (detail.error) {
    box.append(el("p", "The run failed: " + detail.error, "run-error"));
  }

  const result = parseResult(detail.result);
  if (result) {
    const dl = el("dl", null, "pairs");
    for (const [k, v] of [
      ["result", result.status],
      ["steps", result.steps],
      ["tokens", result.total_tokens],
      ["spent", money(result.spent_usd)],
      ["budget", money(result.budget_usd)],
      ["artifact", result.artifact_key || "(none: this run saved nothing)"],
    ]) {
      dl.append(el("dt", k), el("dd", v));
    }
    box.append(dl);

    if (result.tools_used && result.tools_used.length) {
      box.append(el("p", "Tools used: " + result.tools_used.join(", "), "mono"));
    }
    if (result.output) {
      box.append(el("h3", "Output"));
      box.append(el("pre", result.output, "output"));
    }
  } else if (!detail.error) {
    box.append(el("p",
      "No result yet. It appears when the run finishes; until then the query state above is the live view.",
      "hint"));
  }

  renderApproval(box, detail);
}

// parseResult decodes the workflow's return value.
//
// It arrives as a JSON DOCUMENT ENCODED AS A STRING, not as an object: a WASM
// entry point returns bytes and RunAgent returns `string` (agent.go, and the
// binding rule it cites), so the backend hands on exactly what the engine
// stored. A page that assumed an object would find the whole result behind
// result.output undefined and render an empty panel with no error anywhere --
// which is why this parses rather than reaching for fields.
function parseResult(raw) {
  if (!raw) return null;
  try {
    return JSON.parse(raw);
  } catch (e) {
    return null;
  }
}

// renderApproval offers the decision ONLY while the run is actually waiting, and
// that is not cosmetic. The backend refuses to publish an approval for a run
// that is not in `awaiting_approval`, because the event it publishes is matched
// by (tenant, event_type) and by nothing else -- so a button offered on a parked-
// looking run would either 409 or, worse, wake a different run.
function renderApproval(box, detail) {
  const state = detail.state || {};
  if ((state.status || "") !== "awaiting_approval") return;

  const form = el("form", null, "approval");
  form.append(el("h3", "Waiting for a human"));

  if (state.approval_reason) {
    form.append(el("p", "The agent is asking to: " + state.approval_reason, "detail-task"));
  }

  const label = el("label", "Note");
  const note = el("input");
  note.id = "approve-note";
  note.placeholder = "why (optional; the model is shown it either way)";
  label.append(note);
  form.append(label);

  const buttons = el("div", null, "approval-buttons");
  for (const [text, approved] of [["Approve", true], ["Deny", false]]) {
    const btn = el("button", text);
    // type=button on BOTH, deliberately: a form's implicit submission clicks its
    // first submit button, so Enter in the note field would approve. The note
    // field is the thing a person types in last, which makes that the likeliest
    // accidental approval there is.
    btn.type = "button";
    if (!approved) btn.className = "secondary";
    btn.addEventListener("click", () => decide(detail.id, approved));
    buttons.append(btn);
  }
  form.append(buttons);
  const approvalStatus = el("p", null, "status");
  approvalStatus.id = "approve-status";
  form.append(approvalStatus);

  box.append(form);
}

// ---- requests ----

async function getJSON(url) {
  const res = await fetch(url, { headers: { Accept: "application/json" } });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.status + " " + res.statusText);
  return body;
}

async function postJSON(url, body, extraHeaders) {
  const res = await fetch(url, {
    method: "POST",
    headers: Object.assign({ "Content-Type": "application/json" }, extraHeaders || {}),
    body: JSON.stringify(body),
  });
  const out = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(out.error || res.status + " " + res.statusText);
  return out;
}

async function startAgent(event) {
  event.preventDefault();
  const form = new FormData(event.target);
  const status = $("#start-status");
  status.textContent = "";
  try {
    const body = await postJSON(
      "/api/agents",
      {
        tenant_id: form.get("tenant_id"),
        task: form.get("task"),
        // Empty and 0 are both "use the workflow's default", which is why
        // Number("") -> 0 is not coerced away here: the backend turns 0 into the
        // default it advertises in /api/config, and inventing a second fallback
        // in the page would be a second place for it to be wrong.
        max_steps: Number(form.get("max_steps")) || 0,
        budget_usd: Number(form.get("budget_usd")) || 0,
        artifact_key: form.get("artifact_key"),
      },
      {
        // A fresh key per press, so a double-click returns the ORIGINAL run
        // rather than paying for a second one. This matters more here than in a
        // CRUD page: the thing being duplicated is a paid model call, and the
        // backend reports back whether this call was a replay.
        "Idempotency-Key": crypto.randomUUID(),
      }
    );
    status.textContent =
      "started " + shortID(body.id) + (body.idempotent_replay ? " (idempotent replay)" : "");
    status.className = "status ok";
    selected = body.id;
    refresh();
  } catch (e) {
    status.textContent = e.message;
    status.className = "status bad";
  }
}

async function decide(id, approved) {
  const status = $("#approve-status");
  const note = $("#approve-note");
  try {
    const body = await postJSON("/api/agents/" + encodeURIComponent(id) + "/approve", {
      approved,
      note: note ? note.value : "",
    });
    status.textContent =
      "published " + shortID(body.event_id) + " — the run picks it up on its next " +
      "poll (up to " + pollMs / 1000 + "s)";
    status.className = "status ok";
    refresh();
  } catch (e) {
    status.textContent = e.message;
    status.className = "status bad";
  }
}

// ---- polling ----

async function refresh() {
  try {
    const { runs } = await getJSON("/api/agents");
    renderRuns(runs || []);

    if (!selected) {
      renderDetail(null);
      return;
    }
    try {
      renderDetail(await getJSON("/api/agents/" + encodeURIComponent(selected)));
    } catch (e) {
      // The selected run stopped being readable, which in practice means the
      // list no longer contains it. Drop the selection rather than leaving the
      // panel showing a run that is gone, or retrying it every two seconds.
      selected = null;
      renderDetail(null);
    }
  } catch (e) {
    $("#runs-empty").hidden = false;
    $("#runs-empty").textContent = "Could not read the runs: " + e.message;
  }
}

async function main() {
  $("#start-form").addEventListener("submit", startAgent);
  try {
    config = await getJSON("/api/config");
    renderConfig();
  } catch (e) {
    $("#start-status").textContent = "Could not read /api/config: " + e.message;
    $("#start-status").className = "status bad";
  }
  await refresh();
  setInterval(refresh, pollMs);
}

main();
