import type { WorkflowInstance, EventRecord, Schedule, DAGResponse, WorkflowDefInfo } from './types';

const BASE = '';
const KEY_STORAGE = 'cleat_api_key';

export function getAPIKey(): string {
  return localStorage.getItem(KEY_STORAGE) || '';
}

export function setAPIKey(key: string): void {
  localStorage.setItem(KEY_STORAGE, key);
}

export function clearAPIKey(): void {
  localStorage.removeItem(KEY_STORAGE);
}

function authHeaders(): Record<string, string> {
  const key = getAPIKey();
  if (!key) return {};
  return { 'X-Cleat-API-Key': key };
}

async function fetchJSON<T>(url: string, opts?: RequestInit): Promise<T> {
  const res = await fetch(BASE + url, {
    ...opts,
    headers: { ...authHeaders(), ...(opts?.headers || {}) },
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(body.error || res.statusText);
  }
  return res.json();
}

export async function listWorkflows(status?: string): Promise<WorkflowInstance[]> {
  const qs = status ? `?status=${encodeURIComponent(status)}` : '';
  return fetchJSON<WorkflowInstance[]>(`/api/workflows${qs}`);
}

export async function getWorkflow(id: string): Promise<WorkflowInstance> {
  return fetchJSON<WorkflowInstance>(`/api/workflows/${id}`);
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
  return fetchJSON(`/api/workflows/${id}/query?key=${encodeURIComponent(key)}`);
}

export async function listSchedules(): Promise<Schedule[]> {
  return fetchJSON<Schedule[]>('/api/schedules');
}

export async function createSchedule(schedule: { name: string; cron: string; def_name: string; entry_point?: string; input?: string }): Promise<void> {
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
  return fetchJSON<WorkflowInstance[]>('/api/dead-letters');
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
