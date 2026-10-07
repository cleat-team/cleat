'use strict';

// The page's whole job is to show what happened to one order, and the half that
// matters is the compensation outcome. Everything here goes through /api/... on
// this origin -- the API key is server-side and this page never holds one, never
// sends an Authorization header, and could not read one if it tried.

const $ = (id) => document.getElementById(id);

// Terminal *workflow* states, from the workflow's own published `status`. A run
// in any of these has stopped changing and polling can stop.
const TERMINAL = new Set(['done', 'failed', 'rejected']);

// The stages the workflow publishes while it is still working, so a reader can
// see where an order is rather than only that it is not finished.
//
// TWO SOURCES, because the workflow publishes two different things. `status`
// carries the states only the app can know -- the approval gate and the
// terminal outcomes. While the saga is running, the phase comes from
// `current_step`, which the saga publishes itself at each boundary (cleat#2627)
// rather than the app hand-writing a status word per step. The keys below mix
// app statuses and saga step names on purpose: to the reader they are one
// thing -- where the order is.
const STAGE_LABEL = {
  validated: 'Validated',
  awaiting_approval: 'Awaiting approval',
  reserve_inventory: 'Reserving inventory',
  charge_psp: 'Charging the card',
  await_payment_confirmation: 'Waiting for the payment webhook',
  dispatch_fulfilment: 'Dispatching',
  notify_customer: 'Notifying the customer',
  done: 'Completed',
  failed: 'Failed',
  rejected: 'Rejected',
};

// States that are a decision rather than a step in progress. A run that failed
// still carries `current_step` = the step that failed, so `current_step` alone
// would report "Charging the card" for an order that has already stopped and
// unwound. These win over it.
const DECIDED = new Set(['done', 'failed', 'rejected', 'awaiting_approval']);

// stageLabel is what to show for a run: the decision or terminal state if there
// is one, otherwise the step the saga is in.
function stageLabel(state) {
  const s = (state && state.status) || '';
  if (state && state.current_step && !DECIDED.has(s)) {
    return STAGE_LABEL[state.current_step] || state.current_step;
  }
  return (
    STAGE_LABEL[s] ||
    s ||
    (state && (STAGE_LABEL[state.current_step] || state.current_step)) ||
    ''
  );
}

const POLL_MS = 900;
const POLL_BUDGET_MS = 90_000; // the webhook wait is a durable step; give it room

// ---- helpers ----------------------------------------------------------

function el(tag, className, text) {
  const n = document.createElement(tag);
  if (className) n.className = className;
  if (text !== undefined) n.textContent = text; // never innerHTML: this is server data
  return n;
}

function splitList(s) {
  return (s || '')
    .split(',')
    .map((x) => x.trim())
    .filter((x) => x !== '');
}

function newOrderID() {
  const n = Math.floor(Math.random() * 1e6).toString().padStart(6, '0');
  return `ord-${n}`;
}

async function api(method, path, body, headers) {
  const opts = { method, headers: Object.assign({}, headers) };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let parsed = null;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    // Leave parsed null; the caller reports the status and the raw text.
  }
  if (!res.ok) {
    const msg = (parsed && (parsed.error || parsed.message)) || text || res.statusText;
    throw new Error(`${res.status}: ${msg}`);
  }
  return parsed;
}

// ---- the compensation outcome ----------------------------------------

// This is the panel the page exists for.
//
// The workflow publishes TWO lists, and keeping them apart is the point:
//
//   compensated    -- compensations that ran AND SUCCEEDED
//   unwind_failed  -- compensations that ran and FAILED
//
// Collapsing them into "the order failed" would hide the only line an operator
// has to act on: a refund that did not go through leaves the customer charged on
// an order that did not complete, which is precisely the state the saga exists to
// avoid. So a page that shows one word for both is a page that cannot be trusted
// to raise it.
// A failed run's query state does not survive to the database, so the trail is
// also written into the run's ERROR by the workflow -- see order.go, and the
// scenario script's comment for the measurement. The shape is fixed by that
// code:
//
//	... not completed (unwound: [reserve_inventory]; could not unwind: []) ...
//
// Parsing an error string is not a good interface, and it is not the intended
// one: the query state above is. This is what makes the page honest until the
// engine publishes state for failed runs too, and it degrades to showing the
// raw error when the marker is absent.
function trailFromError(err) {
  const m = /unwound: \[([^\]]*)\]; could not unwind: \[([^\]]*)\]/.exec(err || '');
  if (!m) return null;
  const parse = (s) => s.split(/\s+/).map((x) => x.trim()).filter(Boolean);
  return { unwound: parse(m[1]), failed: parse(m[2]) };
}

function renderOutcome(state, runStatus, runError) {
  const host = $('outcome');
  host.replaceChildren();

  if (!state || Object.keys(state).length === 0) {
    const trail = trailFromError(runError);
    if (!trail) {
      const div = el('div', 'outcome');
      div.append(el('p', 'sub', runError
        ? 'This run ended before it published any state.'
        : runStatus === 'running'
          ? 'No published state yet — the run has not reached its first step.'
          : 'This run published no state.'));
      if (runError) div.append(el('pre', 'raw-error', runError));
      host.append(div);
      $('state-list').replaceChildren();
      return;
    }
    // Reuse the renderer below by handing it the trail in the shape the state
    // would have carried. The marker keeps the raw-state panel honest: these
    // keys were parsed out of an error message, and listing them as published
    // state would say the run had published something it did not.
    state = {
      status: 'failed',
      compensated: trail.unwound.join(','),
      unwind_failed: trail.failed.join(','),
    };
    state._synthetic = true;
  }

  const status = state.status || '';
  const compensated = splitList(state.compensated);
  const unwindFailed = splitList(state.unwind_failed);
  const failedStep = state.failed_step || '';

  const note = (text) => el('p', 'untouched', text);

  const listUndone = (names) => {
    const ul = el('ul', 'steps');
    for (const n of names) {
      const li = el('li', 'undone');
      li.append(el('span', 'mark', '✓'));
      li.append(el('span', 'name', n));
      li.append(el('span', 'note', 'undone'));
      ul.append(li);
    }
    return ul;
  };

  const listFailed = (names) => {
    const ul = el('ul', 'steps');
    for (const n of names) {
      const li = el('li', 'failed');
      li.append(el('span', 'mark', '✗'));
      li.append(el('span', 'name', n));
      li.append(el('span', 'note', 'compensation ran and FAILED — still outstanding'));
      ul.append(li);
    }
    return ul;
  };

  // Only compensations appear in these lists. Say so, because the alternative
  // reading -- "these are all the steps that ran" -- is wrong in a way that
  // matters: the webhook wait declares no undo, so it contributes nothing to
  // either list even when it completed.
  const compensationNote =
    'Only compensations are listed. A step that declares no undo — the webhook ' +
    'wait, which has nothing to hand back — appears in neither list even when it completed.';

  let box, title, lede, body = [];

  if (unwindFailed.length > 0) {
    // The worst outcome, and the one worth shouting about.
    box = 'outcome fail';
    title = 'Compensated, but not completely';
    lede =
      `The run failed at ${failedStep || 'a step'}, and at least one undo did not ` +
      'succeed. This order is in the state the saga exists to avoid — it needs a ' +
      'person, not a retry.';
    body.push(listUndone(compensated));
    body.push(listFailed(unwindFailed));
  } else if (status === 'failed' && compensated.length > 0) {
    box = 'outcome warn';
    title = 'The saga compensated';
    lede =
      `The run failed at ${failedStep || 'a step'}. Every step that had completed ` +
      'was undone, in reverse order — newest first, below.';
    body.push(listUndone(compensated));
  } else if (status === 'failed') {
    box = 'outcome warn';
    title = 'Failed before anything needed undoing';
    lede = failedStep
      ? `The run failed at ${failedStep}, before any step that had an undo completed.`
      : 'The run failed before any step with an undo completed.';
  } else if (status === 'rejected') {
    box = 'outcome warn';
    title = 'Rejected — nothing was spent';
    lede = state.rejection_reason
      ? `Reason: ${state.rejection_reason}`
      : 'The order was rejected.';
    body.push(note('The approval gate sits before the first spending step, so there is no compensation trail to show.'));
  } else if (status === 'done') {
    box = 'outcome ok';
    title = 'Completed';
    lede = 'Every step ran and nothing needed undoing.';
  } else {
    box = 'outcome';
    title = stageLabel(state) || 'Running';
    lede = 'The run is still working. This panel fills in when it finishes.';
  }

  const div = el('div', box);
  div.append(el('h3', null, title));
  div.append(el('p', null, lede));
  for (const b of body) div.append(b);
  if (compensated.length > 0 || unwindFailed.length > 0) div.append(note(compensationNote));
  host.append(div);

  // Raw published state, unfolded on demand -- so a reader can check the page
  // against the data rather than trusting the rendering. Skipped when the
  // outcome above was parsed out of an error message instead.
  const dl = $('state-list');
  dl.replaceChildren();
  if (state._synthetic === true) {
    dl.append(el('dt', null, '(no published state)'));
    dl.append(el('dd', null, 'this run failed, and the trail above is from its error'));
    return;
  }
  for (const k of Object.keys(state).sort()) {
    dl.append(el('dt', null, k));
    dl.append(el('dd', null, state[k] === '' ? '(empty)' : String(state[k])));
  }
}

// ---- detail view ------------------------------------------------------

let pollTimer = null;
let currentRun = null;

function stopPolling() {
  if (pollTimer !== null) {
    clearTimeout(pollTimer);
    pollTimer = null;
  }
}

function setPill(node, status, state) {
  node.textContent = stageLabel(state) || status || 'unknown';
  node.className = 'pill';
  if (status === 'done') node.classList.add('ok');
  else if (status === 'failed') node.classList.add('fail');
  else if (status === 'rejected' || status === 'awaiting_approval') node.classList.add('warn');
}

async function showRun(id, startedAt) {
  stopPolling();
  currentRun = id;
  $('detail').hidden = false;
  $('detail-id').textContent = id;
  $('detail-status').textContent = '…';
  $('detail-status').className = 'pill';
  $('detail-run-status').textContent = '';
  $('outcome').replaceChildren();
  $('approval-controls').hidden = true;

  const deadline = Date.now() + POLL_BUDGET_MS;

  const tick = async () => {
    let detail;
    try {
      detail = await api('GET', `/api/orders/${encodeURIComponent(id)}`);
    } catch (err) {
      stopPolling();
      $('detail-run-status').textContent = `Could not read the run: ${err.message}`;
      return;
    }
    if (currentRun !== id) return; // a newer selection took over

    const state = detail.state || {};
    setPill($('detail-status'), state.status || detail.status, state);
    $('detail-run-status').textContent = `engine status: ${detail.status}`;

    // Only offer the decision while the workflow is actually waiting on it.
    $('approval-controls').hidden = state.status !== 'awaiting_approval';

    renderOutcome(state, detail.status, detail.error);

    const finished = TERMINAL.has(state.status) ||
      (detail.status && detail.status !== 'running' && detail.status !== 'pending');
    if (finished || Date.now() > deadline) {
      stopPolling();
      if (Date.now() > deadline) {
        $('detail-run-status').textContent = 'Stopped polling after 90s; use Refresh.';
      }
      loadRuns();
      return;
    }
    pollTimer = setTimeout(tick, POLL_MS);
  };

  tick();
}

// ---- run list ---------------------------------------------------------

async function loadRuns() {
  const list = $('run-list');
  let data;
  try {
    data = await api('GET', '/api/orders');
  } catch (err) {
    list.replaceChildren(el('li', 'empty', `Could not list runs: ${err.message}`));
    return;
  }
  const runs = (data && data.runs) || [];
  list.replaceChildren();
  if (runs.length === 0) {
    list.append(el('li', 'empty', 'No runs yet.'));
    return;
  }
  // Newest first. The API returns them oldest-first.
  for (const run of runs.slice().reverse()) {
    const li = el('li');
    const btn = el('button');
    btn.type = 'button';
    btn.append(el('span', 'mono', run.id));
    const when = run.created_at ? new Date(run.created_at).toLocaleTimeString() : '';
    btn.append(el('span', 'when', `${run.status || ''} ${when}`.trim()));
    btn.addEventListener('click', () => showRun(run.id));
    li.append(btn);
    list.append(li);
  }
}

// ---- starting an order ------------------------------------------------

async function submitOrder(event) {
  event.preventDefault();
  const err = $('form-error');
  err.hidden = true;

  const amount = Number($('amount').value);
  if (!Number.isFinite(amount) || amount <= 0) {
    err.textContent = 'Enter an order value above zero.';
    err.hidden = false;
    return;
  }

  const body = {
    order_id: $('order-id').value.trim(),
    email: $('email').value.trim(),
    amount_cents: Math.round(amount),
    simulate_payment_failure: $('sim-payment').checked,
    simulate_fulfilment_failure: $('sim-fulfilment').checked,
    simulate_compensation_failure: $('sim-compensation').checked,
  };

  const submit = $('submit');
  submit.disabled = true;
  try {
    // A fresh key per press. A double-click sends the SAME key twice and the
    // second call returns the ORIGINAL run rather than creating a second order
    // -- which is the property the start path is demonstrating, so the page
    // exercises it rather than working around it.
    const key = (crypto.randomUUID && crypto.randomUUID()) ||
      `key-${Date.now()}-${Math.random()}`;

    const res = await api('POST', '/api/orders', body, { 'Idempotency-Key': key });
    if (res.idempotent_replay) {
      err.textContent = 'That request was a replay of an existing run — showing the original.';
      err.hidden = false;
    }
    $('order-id').value = newOrderID(); // so the next press is a new order
    await loadRuns();
    await showRun(res.id);
  } catch (e) {
    err.textContent = e.message;
    err.hidden = false;
  } finally {
    submit.disabled = false;
  }
}

// ---- approval ---------------------------------------------------------

async function decide(approve) {
  if (!currentRun) return;
  const reason = approve ? '' : window.prompt('Why is this order being rejected?', 'over budget') || '';
  try {
    await api('POST', `/api/orders/${encodeURIComponent(currentRun)}/approve`,
      { approve, reason });
    $('approval-controls').hidden = true;
    showRun(currentRun);
  } catch (e) {
    const err = $('form-error');
    err.textContent = e.message;
    err.hidden = false;
  }
}

// ---- wire up ----------------------------------------------------------

document.addEventListener('DOMContentLoaded', async () => {
  $('order-id').value = newOrderID();
  $('order-form').addEventListener('submit', submitOrder);
  $('refresh').addEventListener('click', loadRuns);
  $('approve').addEventListener('click', () => decide(true));
  $('reject').addEventListener('click', () => decide(false));

  // The threshold is served rather than hardcoded here, so this hint cannot
  // drift from the branch the workflow actually takes.
  try {
    const cfg = await api('GET', '/api/config');
    const t = cfg.approval_threshold_cents;
    $('threshold-hint').textContent =
      `Above $${(t / 100).toFixed(2)} the order waits for an approval signal before ` +
      'anything is charged; below it, it charges straight through.';
    if (cfg.default_amount_cents) $('amount').value = cfg.default_amount_cents / 100;
  } catch {
    $('threshold-hint').textContent = 'Could not read the approval threshold from the backend.';
  }

  await loadRuns();
});
