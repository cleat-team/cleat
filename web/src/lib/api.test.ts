import { describe, it, expect, beforeEach, vi } from 'vitest';
import {
  listWorkflows,
  getWorkflow,
  startWorkflow,
  signalWorkflow,
  cancelWorkflow,
  getWorkflowHistory,
  getWorkflowDAG,
  getQueryState,
  listSchedules,
  createSchedule,
  deleteSchedule,
  enableSchedule,
  disableSchedule,
} from './api';
import type { WorkflowInstance, Schedule, DAGResponse } from './types';

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------
const mockWorkflowInstance: WorkflowInstance = {
  id: 'wf-abc123',
  def_name: 'test-workflow',
  def_version: 1,
  status: 'running',
  input: '{}',
  result: '',
  error: '',
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T01:00:00Z',
  assigned_to: 'worker-1',
  next_wake_at: '',
  namespace: 'default',
};

const mockSchedule: Schedule = {
  name: 'hourly-job',
  cron_expression: '0 * * * *',
  def_name: 'process-orders',
  entry_point: '',
  input: '',
  enabled: true,
  next_run_at: '2024-01-01T02:00:00Z',
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T00:00:00Z',
  timezone: 'UTC',
};

const mockDAGResponse: DAGResponse = {
  workflow_id: 'wf-abc123',
  dag: {
    name: 'test-dag',
    tasks: [
      { name: 'task-1', parents: [] },
      { name: 'task-2', parents: ['task-1'] },
    ],
  },
};

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
function mockFetchOk(response: any) {
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: true,
    status: 200,
    json: vi.fn().mockResolvedValue(response),
  });
}

function mockFetchError(status: number, body?: any) {
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: false,
    status,
    statusText: body?.error || 'Internal Server Error',
    json: body
      ? vi.fn().mockResolvedValue(body)
      : vi.fn().mockRejectedValue(new Error('parse error')),
  });
}

function mockFetchNetworkError(message = 'Network error') {
  globalThis.fetch = vi.fn().mockRejectedValue(new Error(message));
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------
describe('API Client', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  // -- listWorkflows --------------------------------------------------------

  describe('listWorkflows', () => {
    it('calls /api/workflows without query when no status filter', async () => {
      mockFetchOk([mockWorkflowInstance]);
      const result = await listWorkflows();
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows',
        undefined,
      );
      expect(result).toEqual([mockWorkflowInstance]);
    });

    it('applies status query parameter', async () => {
      mockFetchOk([mockWorkflowInstance]);
      await listWorkflows('running');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows?status=running',
        undefined,
      );
    });

    it('encodes special characters in status filter', async () => {
      mockFetchOk([]);
      await listWorkflows('dead letter');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows?status=dead%20letter',
        undefined,
      );
    });

    it('throws on network error', async () => {
      mockFetchNetworkError('Failed to fetch');
      await expect(listWorkflows()).rejects.toThrow('Failed to fetch');
    });

    it('throws with error message from JSON body on 4xx', async () => {
      mockFetchError(400, { error: 'Bad Request' });
      await expect(listWorkflows()).rejects.toThrow('Bad Request');
    });

    it('falls back to statusText when error body is not JSON', async () => {
      mockFetchError(500);
      await expect(listWorkflows()).rejects.toThrow('Internal Server Error');
    });

    it('throws on 500 with JSON error body', async () => {
      mockFetchError(500, { error: 'Server crashed' });
      await expect(listWorkflows()).rejects.toThrow('Server crashed');
    });
  });

  // -- getWorkflow ----------------------------------------------------------

  describe('getWorkflow', () => {
    it('calls /api/workflows/:id', async () => {
      mockFetchOk(mockWorkflowInstance);
      const result = await getWorkflow('wf-abc123');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf-abc123',
        undefined,
      );
      expect(result).toEqual(mockWorkflowInstance);
    });

    it('handles workflow IDs with special characters', async () => {
      mockFetchOk(mockWorkflowInstance);
      await getWorkflow('wf/123');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf/123',
        undefined,
      );
    });

    it('throws on network error', async () => {
      mockFetchNetworkError();
      await expect(getWorkflow('wf-abc123')).rejects.toThrow('Network error');
    });
  });

  // -- startWorkflow --------------------------------------------------------

  describe('startWorkflow', () => {
    it('sends POST with name, input, and entryPoint', async () => {
      mockFetchOk({ id: 'wf-new' });
      const result = await startWorkflow(
        'my-workflow',
        { key: 'value' },
        'entry',
      );
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/my-workflow/start',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            input: { key: 'value' },
            entry_point: 'entry',
          }),
        },
      );
      expect(result).toEqual({ id: 'wf-new' });
    });

    it('works without optional input and entryPoint', async () => {
      mockFetchOk({ id: 'wf-new' });
      const result = await startWorkflow('my-workflow');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/my-workflow/start',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            input: undefined,
            entry_point: undefined,
          }),
        },
      );
      expect(result).toEqual({ id: 'wf-new' });
    });
  });

  // -- signalWorkflow -------------------------------------------------------

  describe('signalWorkflow', () => {
    it('sends POST with signal name and payload', async () => {
      mockFetchOk(undefined);
      await signalWorkflow('wf-123', 'order_shipped', 'payload-data');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf-123/signal',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            signal_name: 'order_shipped',
            payload: 'payload-data',
          }),
        },
      );
    });

    it('works without payload', async () => {
      mockFetchOk(undefined);
      await signalWorkflow('wf-123', 'my-signal');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf-123/signal',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            signal_name: 'my-signal',
            payload: undefined,
          }),
        },
      );
    });
  });

  // -- cancelWorkflow -------------------------------------------------------

  describe('cancelWorkflow', () => {
    it('sends POST with reason', async () => {
      mockFetchOk(undefined);
      await cancelWorkflow('wf-123', 'no longer needed');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf-123/cancel',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ reason: 'no longer needed' }),
        },
      );
    });

    it('works without reason', async () => {
      mockFetchOk(undefined);
      await cancelWorkflow('wf-123');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf-123/cancel',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ reason: undefined }),
        },
      );
    });
  });

  // -- getWorkflowHistory ---------------------------------------------------

  describe('getWorkflowHistory', () => {
    it('fetches event history for a workflow', async () => {
      const events = [
        { step: 1, type: 'call', service: 'llm' },
        { step: 2, type: 'sleep', duration_ms: 1000 },
      ];
      mockFetchOk(events);
      const result = await getWorkflowHistory('wf-123');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf-123/history',
        undefined,
      );
      expect(result).toEqual(events);
    });
  });

  // -- getWorkflowDAG -------------------------------------------------------

  describe('getWorkflowDAG', () => {
    it('fetches DAG data for a workflow', async () => {
      mockFetchOk(mockDAGResponse);
      const result = await getWorkflowDAG('wf-abc123');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf-abc123/dag',
        undefined,
      );
      expect(result).toEqual(mockDAGResponse);
    });
  });

  // -- getQueryState --------------------------------------------------------

  describe('getQueryState', () => {
    it('calls with key query parameter', async () => {
      mockFetchOk({ key: 'my-key', value: 'my-value' });
      const result = await getQueryState('wf-123', 'my-key');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf-123/query?key=my-key',
        undefined,
      );
      expect(result).toEqual({ key: 'my-key', value: 'my-value' });
    });

    it('encodes special characters in key', async () => {
      mockFetchOk({ key: '', value: '' });
      await getQueryState('wf-123', 'key with spaces');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/workflows/wf-123/query?key=key%20with%20spaces',
        undefined,
      );
    });
  });

  // -- listSchedules --------------------------------------------------------

  describe('listSchedules', () => {
    it('fetches schedules list from /api/schedules', async () => {
      mockFetchOk([mockSchedule]);
      const result = await listSchedules();
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/schedules',
        undefined,
      );
      expect(result).toEqual([mockSchedule]);
    });
  });

  // -- createSchedule -------------------------------------------------------

  describe('createSchedule', () => {
    it('sends POST with schedule data', async () => {
      mockFetchOk(undefined);
      await createSchedule({
        name: 'nightly',
        cron: '0 0 * * *',
        def_name: 'NightlyJob',
      });
      expect(globalThis.fetch).toHaveBeenCalledWith('/api/schedules', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'nightly',
          cron: '0 0 * * *',
          def_name: 'NightlyJob',
        }),
      });
    });

    it('includes optional entry_point and input', async () => {
      mockFetchOk(undefined);
      await createSchedule({
        name: 'custom',
        cron: '*/5 * * * *',
        def_name: 'CustomJob',
        entry_point: 'run',
        input: '{"key":"val"}',
      });
      expect(globalThis.fetch).toHaveBeenCalledWith('/api/schedules', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'custom',
          cron: '*/5 * * * *',
          def_name: 'CustomJob',
          entry_point: 'run',
          input: '{"key":"val"}',
        }),
      });
    });

    it('includes optional timezone', async () => {
      mockFetchOk(undefined);
      await createSchedule({
        name: 'nightly',
        cron: '0 0 * * *',
        def_name: 'NightlyJob',
        timezone: 'America/New_York',
      });
      expect(globalThis.fetch).toHaveBeenCalledWith('/api/schedules', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'nightly',
          cron: '0 0 * * *',
          def_name: 'NightlyJob',
          timezone: 'America/New_York',
        }),
      });
    });

    it('omits timezone when not provided', async () => {
      mockFetchOk(undefined);
      await createSchedule({
        name: 'nightly',
        cron: '0 0 * * *',
        def_name: 'NightlyJob',
      });
      const [, opts] = (globalThis.fetch as any).mock.calls[0];
      expect(JSON.parse(opts.body)).not.toHaveProperty('timezone');
    });

    it('rejects an invalid IANA zone with the server-provided 400 message', async () => {
      mockFetchError(400, { error: 'invalid timezone: "Not/AZone"' });
      await expect(
        createSchedule({
          name: 'bad-tz',
          cron: '0 0 * * *',
          def_name: 'NightlyJob',
          timezone: 'Not/AZone',
        }),
      ).rejects.toThrow('invalid timezone: "Not/AZone"');
    });
  });

  // -- deleteSchedule -------------------------------------------------------

  describe('deleteSchedule', () => {
    it('sends DELETE request', async () => {
      mockFetchOk(undefined);
      await deleteSchedule('nightly');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/schedules/nightly',
        { method: 'DELETE' },
      );
    });
  });

  // -- enableSchedule / disableSchedule -------------------------------------

  describe('enableSchedule', () => {
    it('sends POST to enable endpoint', async () => {
      mockFetchOk(undefined);
      await enableSchedule('nightly');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/schedules/nightly/enable',
        { method: 'POST' },
      );
    });
  });

  describe('disableSchedule', () => {
    it('sends POST to disable endpoint', async () => {
      mockFetchOk(undefined);
      await disableSchedule('nightly');
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/api/schedules/nightly/disable',
        { method: 'POST' },
      );
    });
  });
});
