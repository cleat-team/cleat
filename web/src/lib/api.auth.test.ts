// B5: the dashboard 401s against the worker's supported default
// (--require-auth=true, cmd/cleat-worker/config.go:80) because api.ts sent no
// credentials at all. These tests pin down the fix at the one chokepoint
// every API call goes through (fetchJSON in api.ts): a stored token must be
// sent as `Authorization: Bearer <token>`, a missing token must change
// nothing about the request api.ts already sent, and a 401 response must
// clear a now-known-bad token and tell the UI to ask for a new one.
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { listWorkflows, startWorkflow } from './api';
import { getToken, setToken, onUnauthorized } from './auth';

function mockFetchOk(response: any) {
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: true,
    status: 200,
    json: vi.fn().mockResolvedValue(response),
  });
}

function mockFetchUnauthorized() {
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: false,
    status: 401,
    statusText: 'Unauthorized',
    json: vi.fn().mockResolvedValue({ error: 'authentication required: provide an API key' }),
  });
}

describe('api.ts Authorization header', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('sends no Authorization header (opts unchanged) when no token is stored', async () => {
    mockFetchOk([]);
    await listWorkflows();
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/workflows', undefined);
  });

  it('sends Authorization: Bearer <token> on a GET call when a token is stored', async () => {
    setToken('cleat_sk_test123');
    mockFetchOk([]);
    await listWorkflows();
    const [url, opts] = (globalThis.fetch as any).mock.calls[0];
    expect(url).toBe('/api/workflows');
    expect(opts.headers).toEqual({ Authorization: 'Bearer cleat_sk_test123' });
  });

  it('merges the Authorization header with an existing Content-Type header on a POST call', async () => {
    setToken('cleat_sk_test123');
    mockFetchOk({ id: 'wf-new' });
    await startWorkflow('my-workflow', { key: 'value' });
    const [, opts] = (globalThis.fetch as any).mock.calls[0];
    expect(opts.headers).toEqual({
      'Content-Type': 'application/json',
      Authorization: 'Bearer cleat_sk_test123',
    });
  });

  it('clears the stored token and notifies listeners on a 401 response', async () => {
    setToken('cleat_sk_bad');
    mockFetchUnauthorized();
    const handler = vi.fn();
    const off = onUnauthorized(handler);

    await expect(listWorkflows()).rejects.toThrow('authentication required');

    expect(getToken()).toBe('');
    expect(handler).toHaveBeenCalledTimes(1);
    off();
  });

  it('does not notify unauthorized listeners on a non-401 error', async () => {
    setToken('cleat_sk_test123');
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 500,
      statusText: 'Internal Server Error',
      json: vi.fn().mockResolvedValue({ error: 'boom' }),
    });
    const handler = vi.fn();
    const off = onUnauthorized(handler);

    await expect(listWorkflows()).rejects.toThrow('boom');

    expect(getToken()).toBe('cleat_sk_test123');
    expect(handler).not.toHaveBeenCalled();
    off();
  });
});
