'use strict';

// This page's job is to show a tenant being provisioned, one recorded
// milestone at a time. Unlike examples/order-lifecycle/web/app.js, the API
// key does NOT live server-side: every tenant gets its own, minted at
// signup, and this page holds it in memory (never localStorage) to make the
// one later authenticated call it needs -- see index.html's own comment.

const $ = (id) => document.getElementById(id);

const TERMINAL = new Set(['active', 'failed']);

const STAGE_LABEL = {
  provisioning: 'Provisioning',
  provisioning_workspace: 'Provisioning workspace',
  notifying: 'Sending welcome email',
  active: 'Active',
  failed: 'Failed',
};

const POLL_MS = 700;
const POLL_BUDGET_MS = 20_000;

// Set once, by signup's response. Never sent anywhere except this backend's
// own /api/tenant/lifecycle, and never persisted across a reload.
let tenantKey = null;

function el(tag, className, text) {
  const n = document.createElement(tag);
  if (className) n.className = className;
  if (text !== undefined) n.textContent = text; // never innerHTML: this is server data
  return n;
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

// ---- signup ------------------------------------------------------------

function renderCredential(tenantID, apiKey) {
  $('cred-tenant').textContent = tenantID;
  $('cred-key').textContent = apiKey;
  $('credential').hidden = false;
}

async function signup(evt) {
  evt.preventDefault();
  const form = $('signup-form');
  const errBox = $('form-error');
  errBox.hidden = true;
  $('submit').disabled = true;

  try {
    const body = {
      business_name: form.business_name.value.trim(),
      admin_email: form.admin_email.value.trim(),
      plan: form.plan.value,
      simulate_workspace_failure: $('sim-workspace').checked,
    };
    const res = await api('POST', '/api/signup', body);
    tenantKey = res.api_key;
    renderCredential(res.tenant_id, res.api_key);
    watchProvisioning(res.run_id);
    fetchLifecycle();
  } catch (e) {
    errBox.textContent = e.message;
    errBox.hidden = false;
  } finally {
    $('submit').disabled = false;
  }
}

// ---- provisioning status -------------------------------------------------

function renderOutcome(detail) {
  const box = $('outcome');
  box.replaceChildren();

  const status = (detail.state && detail.state.status) || detail.status;
  if (status === 'active') {
    const wrap = el('div', 'outcome ok');
    wrap.appendChild(el('h3', null, 'Provisioned'));
    wrap.appendChild(el('p', null,
      'Workspace ready, welcome email attempted, every milestone recorded to the audit trail.'));
    box.appendChild(wrap);
  } else if (status === 'failed' || detail.error) {
    const wrap = el('div', 'outcome fail');
    wrap.appendChild(el('h3', null, 'Provisioning failed'));
    const failedStep = detail.state && detail.state.failed_step;
    wrap.appendChild(el('p', null,
      failedStep ? `Failed at: ${failedStep}` : 'See the raw error below.'));
    if (detail.error) {
      const raw = el('pre', 'raw-error', detail.error);
      wrap.appendChild(raw);
    }
    box.appendChild(wrap);
  }

  if (detail.state && detail.state.notify_failed === 'true') {
    const warn = el('div', 'outcome warn');
    warn.appendChild(el('h3', null, 'Welcome email not sent'));
    warn.appendChild(el('p', null,
      'email-notify is not configured on this deployment (needs email_enabled ' +
      'and a provider key). Provisioning still succeeded -- see the README.'));
    box.appendChild(warn);
  }
}

function renderState(state) {
  const dl = $('state-list');
  dl.replaceChildren();
  const keys = Object.keys(state || {}).sort();
  for (const k of keys) {
    dl.appendChild(el('dt', null, k));
    dl.appendChild(el('dd', null, state[k]));
  }
}

async function watchProvisioning(runID) {
  $('detail').hidden = false;
  $('detail-id').textContent = runID;
  const started = Date.now();

  while (Date.now() - started < POLL_BUDGET_MS) {
    let detail;
    try {
      detail = await api('GET', `/api/provisioning/${encodeURIComponent(runID)}`, undefined, {
        Authorization: `Bearer ${tenantKey}`,
      });
    } catch (e) {
      $('detail-run-status').textContent = e.message;
      return;
    }

    const stage = (detail.state && detail.state.status) || detail.status;
    const pill = $('detail-status');
    pill.textContent = STAGE_LABEL[stage] || stage || detail.status;
    pill.className = 'pill' + (stage === 'active' ? ' ok' : stage === 'failed' ? ' fail' : '');
    $('detail-run-status').textContent = `worker status: ${detail.status}`;

    renderOutcome(detail);
    renderState(detail.state);

    if (TERMINAL.has(stage) || detail.status === 'failed' || detail.status === 'done') {
      return;
    }
    await new Promise((r) => setTimeout(r, POLL_MS));
  }
  $('detail-run-status').textContent += ' (gave up polling after 20s)';
}

// ---- tenant lifecycle ----------------------------------------------------

function renderLifecycle(status) {
  const dl = $('lifecycle-list');
  dl.replaceChildren();
  dl.appendChild(el('dt', null, 'has_trial'));
  dl.appendChild(el('dd', null, String(status.has_trial)));
  if (status.has_trial) {
    dl.appendChild(el('dt', null, 'expires_at'));
    dl.appendChild(el('dd', null, status.expires_at));
    dl.appendChild(el('dt', null, 'handled'));
    dl.appendChild(el('dd', null, String(!!status.handled)));
  }
  $('lifecycle').hidden = false;
}

async function fetchLifecycle() {
  if (!tenantKey) return;
  try {
    const status = await api('GET', '/api/tenant/lifecycle', undefined, {
      Authorization: `Bearer ${tenantKey}`,
    });
    renderLifecycle(status);
  } catch (e) {
    $('lifecycle').hidden = false;
    $('lifecycle-list').replaceChildren(el('dt', null, 'error'), el('dd', null, e.message));
  }
}

// ---- wire up --------------------------------------------------------------

$('signup-form').addEventListener('submit', signup);
$('refresh-lifecycle').addEventListener('click', fetchLifecycle);
