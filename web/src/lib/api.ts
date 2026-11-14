import type { WorkflowInstance, EventRecord, Schedule, DAGResponse, WorkflowDefInfo } from './types';
import { getToken, notifyUnauthorized } from './auth';

const BASE = '';

// B5: every call in this file goes through fetchResponse (fetchJSON is that plus
// `.json()`; listWorkflowsDetailed needs the headers too), so this is the single
// place that needs to attach credentials for the whole dashboard to work
// against a worker running with the supported default, --require-auth=true.
// See lib/auth.ts for where the token comes from.
//
// When there is no stored token, `opts` is passed to fetch() completely
// unmodified (not even an empty headers object is added) so behavior for an
// unauthenticated dev deployment -- and the existing tests that assert the
// exact `undefined` second argument -- is unchanged.
async function fetchResponse(url: string, opts?: RequestInit): Promise<Response> {
  const token = getToken();
  let finalOpts = opts;
  if (token) {
    finalOpts = {
      ...opts,
      headers: {
        ...(opts?.headers as Record<string, string> | undefined),
        Authorization: `Bearer ${token}`,
      },
    };
  }
  const res = await fetch(BASE + url, finalOpts);
  if (!res.ok) {
    if (res.status === 401) {
      notifyUnauthorized();
    }
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(body.error || res.statusText);
  }
  return res;
}

async function fetchJSON<T>(url: string, opts?: RequestInit): Promise<T> {
  const res = await fetchResponse(url, opts);
  return res.json();
}

// The listing's URL. redact=true stays last so the URLs of the calls that existed before `state` are
// byte-identical (see listWorkflows below).
function workflowsURL(status?: string, state?: Record<string, string>): string {
  const params: string[] = [];
  if (status) params.push(`status=${encodeURIComponent(status)}`);
  // cleat#3775: find a run by what it published, `state.<key>=<value>`. The key
  // is encoded as well as the value, because it is part of the parameter NAME.
  for (const [key, value] of Object.entries(state ?? {})) {
    params.push(`state.${encodeURIComponent(key)}=${encodeURIComponent(value)}`);
  }
  params.push('redact=true');
  return `/api/workflows?${params.join('&')}`;
}

export async function listWorkflows(
  status?: string,
  state?: Record<string, string>,
): Promise<WorkflowInstance[]> {
  // redact=true: the dashboard is somewhere people look at what happened, so it
  // asks for the redacted view (cleat#3581). The flag can only add redaction; a
  // programmatic caller that omits it gets the data as stored. A test pins that
  // every data-bearing call below carries it.
  return fetchJSON<WorkflowInstance[]>(workflowsURL(status, state));
}

// What a published-state search covered, when the worker had to search a WINDOW of recent runs because it
// seals query_state (cleat#3775, part 2). Absent when the database answered exactly.
export interface StateSearchWindow {
  size: number; // the window the worker was configured with
  scanned: number; // runs actually examined
  exhausted: boolean; // true: older runs were NOT examined, so "none found" does not mean "none exist"
  unreadable: number; // runs in the window whose published state would not open
}

export interface WorkflowListResult {
  workflows: WorkflowInstance[];
  window?: StateSearchWindow;
}

// listWorkflows plus the headers that say how much a state search covered. A separate function rather than
// a changed return type, so the callers and tests of listWorkflows are untouched.
export async function listWorkflowsDetailed(
  status?: string,
  state?: Record<string, string>,
): Promise<WorkflowListResult> {
  const res = await fetchResponse(workflowsURL(status, state));
  const workflows = (await res.json()) as WorkflowInstance[];
  // `headers` is read defensively: a response without the window headers is an exact answer.
  const size = res.headers?.get?.('X-Query-State-Window');
  if (size == null) return { workflows };
  return {
    workflows,
    window: {
      size: Number(size),
      scanned: Number(res.headers.get('X-Query-State-Scanned') ?? 0),
      exhausted: res.headers.get('X-Query-State-Window-Exhausted') === 'true',
      unreadable: Number(res.headers.get('X-Query-State-Unreadable') ?? 0),
    },
  };
}

export async function getWorkflow(id: string): Promise<WorkflowInstance> {
  return fetchJSON<WorkflowInstance>(`/api/workflows/${id}?redact=true`);
}

export async function startWorkflow(name: string, input?: object, entryPoint?: string): Promise<{ id: string }> {
  return fetchJSON(`/api/workflows/${name}/start`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ input, entry_point: entryPoint }),
  });
}

export async function signalWorkflow(id: string, signalName: string, payload?: string): Promise<void> {
  await fetchJSON(`/api/workflows/${id}/signal`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ signal_name: signalName, payload }),
  });
}

export async function cancelWorkflow(id: string, reason?: string): Promise<void> {
  await fetchJSON(`/api/workflows/${id}/cancel`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ reason }),
  });
}

export async function getWorkflowHistory(id: string): Promise<EventRecord[]> {
  return fetchJSON<EventRecord[]>(`/api/workflows/${id}/history`);
}

export async function getWorkflowDAG(workflowId: string): Promise<DAGResponse> {
  return fetchJSON<DAGResponse>(`/api/workflows/${workflowId}/dag`);
}

export async function getQueryState(id: string, key: string): Promise<{ key: string; value: string }> {
  return fetchJSON(`/api/workflows/${id}/query?key=${encodeURIComponent(key)}&redact=true`);
}

export async function listSchedules(): Promise<Schedule[]> {
  return fetchJSON<Schedule[]>('/api/schedules');
}

export async function createSchedule(schedule: { name: string; cron: string; def_name: string; entry_point?: string; input?: string; timezone?: string }): Promise<void> {
  await fetchJSON('/api/schedules', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(schedule),
  });
}

export async function deleteSchedule(name: string): Promise<void> {
  await fetchJSON(`/api/schedules/${name}`, { method: 'DELETE' });
}

export async function enableSchedule(name: string): Promise<void> {
  await fetchJSON(`/api/schedules/${name}/enable`, { method: 'POST' });
}

export async function disableSchedule(name: string): Promise<void> {
  await fetchJSON(`/api/schedules/${name}/disable`, { method: 'POST' });
}

export async function listDefinitions(): Promise<WorkflowDefInfo[]> {
  return fetchJSON<WorkflowDefInfo[]>('/api/definitions');
}

// ── Dead Letter Queue API ──────────────────────────────────────────

export async function listDeadLetters(): Promise<WorkflowInstance[]> {
  return fetchJSON<WorkflowInstance[]>('/api/dead-letters?redact=true');
}

export async function reprocessDeadLetter(id: string): Promise<{ id: string }> {
  return fetchJSON<{ id: string }>(`/api/dead-letters/${id}/reprocess`, { method: 'POST' });
}

export async function terminateDeadLetter(id: string, reason?: string): Promise<void> {
  await fetchJSON(`/api/dead-letters/${id}/terminate`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ reason: reason || '' }),
  });
}

// ── Batch History API (workflow comparison) ─────────────────────────

export async function getBatchHistories(workflowIds: string[]): Promise<Record<string, EventRecord[]>> {
  return fetchJSON<Record<string, EventRecord[]>>('/api/workflows/batch-history', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ workflow_ids: workflowIds }),
  });
}
