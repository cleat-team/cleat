import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import WorkflowList from './WorkflowList.svelte';
import type { WorkflowInstance } from '../lib/types';

const { listWorkflowsDetailed } = vi.hoisted(() => ({ listWorkflowsDetailed: vi.fn() }));

vi.mock('../lib/api', () => ({ listWorkflowsDetailed }));

const run = { id: 'wf-1234567890abcdef', def_name: 'process-order', def_version: 1, status: 'ready', created_at: '2024-01-01T00:00:00Z' } as WorkflowInstance;

const box = () => screen.getByLabelText(/Find a run by a value it published/) as HTMLInputElement;
const lastCall = () => listWorkflowsDetailed.mock.calls[listWorkflowsDetailed.mock.calls.length - 1];

async function search(text: string) {
  await fireEvent.input(box(), { target: { value: text } });
  await fireEvent.click(screen.getByText('Search'));
}

// cleat#3775. The listing is asked for state, and what comes back (or the refusal) is shown.
describe('WorkflowList: find a run by what it published', () => {
  beforeEach(() => {
    vi.resetAllMocks();
    listWorkflowsDetailed.mockResolvedValue({ workflows: [run] });
  });

  it('asks for no state filter until one is searched for', async () => {
    render(WorkflowList, { onNavigate: () => {} });
    await screen.findByText('process-order');
    expect(lastCall()).toEqual([undefined, undefined]);
  });

  it('typing alone does not search; Search does', async () => {
    render(WorkflowList, { onNavigate: () => {} });
    await screen.findByText('process-order');
    const before = listWorkflowsDetailed.mock.calls.length;
    await fireEvent.input(box(), { target: { value: 'order_id=123' } });
    expect(listWorkflowsDetailed.mock.calls.length).toBe(before);
    await fireEvent.click(screen.getByText('Search'));
    await waitFor(() => expect(lastCall()).toEqual([undefined, { order_id: '123' }]));
  });

  it('Enter searches', async () => {
    render(WorkflowList, { onNavigate: () => {} });
    await screen.findByText('process-order');
    await fireEvent.input(box(), { target: { value: 'order_id=123' } });
    await fireEvent.keyDown(box(), { key: 'Enter' });
    await waitFor(() => expect(lastCall()).toEqual([undefined, { order_id: '123' }]));
  });

  it('splits at the first "=", so a value may contain one, and trims around key and value', async () => {
    render(WorkflowList, { onNavigate: () => {} });
    await screen.findByText('process-order');
    await search('  token = a=b  ');
    await waitFor(() => expect(lastCall()).toEqual([undefined, { token: 'a=b' }]));
  });

  it('an empty value is a value', async () => {
    render(WorkflowList, { onNavigate: () => {} });
    await screen.findByText('process-order');
    await search('status=');
    await waitFor(() => expect(lastCall()).toEqual([undefined, { status: '' }]));
  });

  it('text with no key is refused in the page and the server is not asked', async () => {
    render(WorkflowList, { onNavigate: () => {} });
    await screen.findByText('process-order');
    const before = listWorkflowsDetailed.mock.calls.length;
    await search('order123');
    expect(await screen.findByText(/as key=value, for example order_id=123/)).toBeTruthy();
    await search('=123');
    expect(listWorkflowsDetailed.mock.calls.length).toBe(before);
  });

  it('Clear drops the filter, empties the box, and lists again without it', async () => {
    render(WorkflowList, { onNavigate: () => {} });
    await screen.findByText('process-order');
    await search('order_id=123');
    await waitFor(() => expect(lastCall()).toEqual([undefined, { order_id: '123' }]));
    await fireEvent.click(screen.getByText('Clear'));
    await waitFor(() => expect(lastCall()).toEqual([undefined, undefined]));
    expect(box().value).toBe('');
  });

  it('says what was searched for when nothing matches', async () => {
    render(WorkflowList, { onNavigate: () => {} });
    await screen.findByText('process-order');
    listWorkflowsDetailed.mockResolvedValue({ workflows: [] });
    await search('order_id=999');
    expect(await screen.findByText(/No runs found that published order_id=999/)).toBeTruthy();
  });

  it('shows the server message as it comes when the deployment cannot search by state', async () => {
    render(WorkflowList, { onNavigate: () => {} });
    await screen.findByText('process-order');
    listWorkflowsDetailed.mockRejectedValue(new Error('query state is sealed; see --encrypt-sensitive-payloads'));
    await search('order_id=123');
    expect(await screen.findByText(/--encrypt-sensitive-payloads/)).toBeTruthy();
    // and the earlier page of results is not left standing under the refusal
    expect(screen.queryByText('process-order')).toBeNull();
  });

  // cleat#3775, part 2: the worker searched a window of recent runs. The page says what that covered, and
  // says it differently when the window ran out, because only one of the two means "there is no such run".
  describe('when the worker searched a window of recent runs', () => {
    const window = (over: object) => ({ size: 5000, scanned: 5000, exhausted: false, unreadable: 0, ...over });

    it('says it searched everything when the window did not run out', async () => {
      render(WorkflowList, { onNavigate: () => {} });
      await screen.findByText('process-order');
      listWorkflowsDetailed.mockResolvedValue({ workflows: [], window: window({ scanned: 812 }) });
      await search('order_id=999');
      expect(await screen.findByText(/Searched all 812 runs\./)).toBeTruthy();
      expect(screen.queryByText(/Older runs were not searched/)).toBeNull();
    });

    it('says older runs were not searched when the window ran out, and does not call "none" final', async () => {
      render(WorkflowList, { onNavigate: () => {} });
      await screen.findByText('process-order');
      listWorkflowsDetailed.mockResolvedValue({ workflows: [], window: window({ exhausted: true }) });
      await search('order_id=999');
      expect(await screen.findByText(/Searched the most recent 5000 runs\. Older runs were not searched/)).toBeTruthy();
      expect(await screen.findByText(/That is not the same as there being none/)).toBeTruthy();
    });

    it('counts runs it could not read, singular and plural', async () => {
      render(WorkflowList, { onNavigate: () => {} });
      await screen.findByText('process-order');
      listWorkflowsDetailed.mockResolvedValue({ workflows: [run], window: window({ scanned: 10, unreadable: 1 }) });
      await search('order_id=123');
      expect(await screen.findByText(/1 run could not be read and was not matched/)).toBeTruthy();
      listWorkflowsDetailed.mockResolvedValue({ workflows: [run], window: window({ scanned: 10, unreadable: 3 }) });
      await search('order_id=124');
      expect(await screen.findByText(/3 runs could not be read and were not matched/)).toBeTruthy();
    });

    it('shows nothing about a window when the answer was exact, or when no search was made', async () => {
      render(WorkflowList, { onNavigate: () => {} });
      await screen.findByText('process-order');
      expect(screen.queryByRole('status')).toBeNull();
      await search('order_id=123'); // the default mock has no window
      await screen.findByText('process-order');
      expect(screen.queryByRole('status')).toBeNull();
    });

    it('drops the note when the filter is cleared', async () => {
      render(WorkflowList, { onNavigate: () => {} });
      await screen.findByText('process-order');
      listWorkflowsDetailed.mockResolvedValue({ workflows: [run], window: window({ scanned: 3 }) });
      await search('order_id=123');
      await screen.findByText(/Searched all 3 runs/);
      listWorkflowsDetailed.mockResolvedValue({ workflows: [run] });
      await fireEvent.click(screen.getByText('Clear'));
      await waitFor(() => expect(screen.queryByText(/Searched all/)).toBeNull());
    });
  });
});
