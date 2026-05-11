<script lang="ts">
  import StatusBadge from '../components/StatusBadge.svelte';
  import { getWorkflow, getBatchHistories } from '../lib/api';
  import type { WorkflowInstance, EventRecord } from '../lib/types';

  let { workflowIdA, workflowIdB, onNavigate }: {
    workflowIdA: string;
    workflowIdB: string;
    onNavigate: (path: string) => void;
  } = $props();

  let wfA = $state<WorkflowInstance | null>(null);
  let wfB = $state<WorkflowInstance | null>(null);
  let historyA = $state<EventRecord[]>([]);
  let historyB = $state<EventRecord[]>([]);
  let loading = $state(true);
  let error = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      const [wfARes, wfBRes, histories] = await Promise.all([
        getWorkflow(workflowIdA),
        getWorkflow(workflowIdB),
        getBatchHistories([workflowIdA, workflowIdB]),
      ]);
      wfA = wfARes;
      wfB = wfBRes;
      historyA = histories[workflowIdA] || [];
      historyB = histories[workflowIdB] || [];
    } catch (e: any) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  $effect(() => { load(); });

  // Build maps keyed by event step number for proper alignment
  let stepMapA = $derived.by(() => {
    const map = new Map<number, EventRecord>();
    for (const ev of historyA) map.set(ev.step, ev);
    return map;
  });
  let stepMapB = $derived.by(() => {
    const map = new Map<number, EventRecord>();
    for (const ev of historyB) map.set(ev.step, ev);
    return map;
  });
  // Union of all step numbers, sorted
  let allSteps = $derived.by(() => {
    const steps = new Set<number>();
    for (const ev of historyA) steps.add(ev.step);
    for (const ev of historyB) steps.add(ev.step);
    return [...steps].sort((a, b) => a - b);
  });

  // Compare two event records and return a class
  function diffClass(evA: EventRecord | undefined, evB: EventRecord | undefined): string {
    if (!evA && !evB) return '';
    if (!evA || !evB) return 'diff-mismatch';
    if (evA.type !== evB.type) return 'diff-type-changed';
    if (evA.service !== evB.service) return 'diff-changed';
    if (evA.op !== evB.op) return 'diff-changed';
    if (evA.err !== evB.err) return 'diff-changed';
    return '';
  }

  function eventLabel(ev: EventRecord): string {
    switch (ev.type) {
      case 'call': return ev.service ? `${ev.service}.${ev.op}` : 'call';
      case 'sleep': return `Sleep ${ev.duration_ms}ms`;
      case 'await_signals': return `Await: ${ev.signal_names}`;
      case 'signal_received': return `Signal: ${ev.signal_name}`;
      case 'defer': return `Defer: ${ev.defer_description}`;
      case 'child_workflow': return `Child: ${ev.child_name}`;
      case 'await_child': return `Await child: ${ev.run_id}`;
      case 'continue_as_new': return 'Continue as New';
      default: return ev.type;
    }
  }

  function eventDetail(ev: EventRecord | undefined): string {
    if (!ev) return '(no event)';
    const parts: string[] = [];
    parts.push(eventLabel(ev));
    if (ev.request) parts.push(`Req: ${ev.request.slice(0, 60)}`);
    if (ev.response) parts.push(`Res: ${ev.response.slice(0, 60)}`);
    if (ev.err) parts.push(`Err: ${ev.err.slice(0, 60)}`);
    return parts.join(' | ');
  }
</script>

<h2>Workflow Comparison</h2>
<p><a href="#workflows" style="color: var(--color-primary); font-size: 0.85rem;">&larr; Back to list</a></p>

{#if error}<div class="error-banner">{error}</div>{/if}

{#if loading}
  <p style="color: var(--color-text-muted)">Loading...</p>
{:else}
  <!-- Summary cards -->
  <div class="summary-grid">
    <div class="card">
      <h3>Workflow A</h3>
      {#if wfA}
        <table>
          <tbody>
            <tr><td style="font-weight:600;">ID</td><td style="font-family:monospace;font-size:0.75rem;">{wfA.id.slice(0, 16)}...</td></tr>
            <tr><td style="font-weight:600;">Definition</td><td>{wfA.def_name} v{wfA.def_version}</td></tr>
            <tr><td style="font-weight:600;">Status</td><td><StatusBadge status={wfA.status} /></td></tr>
            <tr><td style="font-weight:600;">Created</td><td>{new Date(wfA.created_at).toLocaleString()}</td></tr>
            {#if wfA.error}
              <tr><td style="font-weight:600;color:var(--color-danger);">Error</td><td style="color:var(--color-danger);">{wfA.error}</td></tr>
            {/if}
          </tbody>
        </table>
      {:else}
        <p style="color:var(--color-text-muted);">Not found</p>
      {/if}
    </div>
    <div class="card">
      <h3>Workflow B</h3>
      {#if wfB}
        <table>
          <tbody>
            <tr><td style="font-weight:600;">ID</td><td style="font-family:monospace;font-size:0.75rem;">{wfB.id.slice(0, 16)}...</td></tr>
            <tr><td style="font-weight:600;">Definition</td><td>{wfB.def_name} v{wfB.def_version}</td></tr>
            <tr><td style="font-weight:600;">Status</td><td><StatusBadge status={wfB.status} /></td></tr>
            <tr><td style="font-weight:600;">Created</td><td>{new Date(wfB.created_at).toLocaleString()}</td></tr>
            {#if wfB.error}
              <tr><td style="font-weight:600;color:var(--color-danger);">Error</td><td style="color:var(--color-danger);">{wfB.error}</td></tr>
            {/if}
          </tbody>
        </table>
      {:else}
        <p style="color:var(--color-text-muted);">Not found</p>
      {/if}
    </div>
  </div>

  <!-- Event history comparison side by side -->
  <div class="card">
    <h3>Event History Comparison</h3>
    <p style="font-size:0.8rem;color:var(--color-text-muted);margin-bottom:0.5rem;">
      Steps are aligned by step number. Differences are highlighted in yellow (content changed)
      or red (type changed or one side missing).
    </p>
    <div class="compare-table-wrapper">
      <table>
        <thead>
          <tr>
            <th style="width:40px;">Step</th>
            <th>Workflow A</th>
            <th style="width:40px;">Step</th>
            <th>Workflow B</th>
          </tr>
        </thead>
        <tbody>
          {#each allSteps as step}
            {@const evA = stepMapA.get(step)}
            {@const evB = stepMapB.get(step)}
            {@const cls = diffClass(evA, evB)}
            <tr class={cls}>
              <td style="text-align:center;font-family:monospace;font-size:0.75rem;">{evA?.step ?? '-'}</td>
              <td class="compare-cell" title={eventDetail(evA)}>
                {#if evA}
                  <span class="event-type-tag">{evA.type}</span>
                  <span class="event-label">{eventLabel(evA)}</span>
                  {#if evA.err}
                    <span class="event-err">{evA.err.slice(0, 60)}</span>
                  {/if}
                {:else}
                  <span style="color:var(--color-text-muted);font-style:italic;">No event</span>
                {/if}
              </td>
              <td style="text-align:center;font-family:monospace;font-size:0.75rem;">{evB?.step ?? '-'}</td>
              <td class="compare-cell" title={eventDetail(evB)}>
                {#if evB}
                  <span class="event-type-tag">{evB.type}</span>
                  <span class="event-label">{eventLabel(evB)}</span>
                  {#if evB.err}
                    <span class="event-err">{evB.err.slice(0, 60)}</span>
                  {/if}
                {:else}
                  <span style="color:var(--color-text-muted);font-style:italic;">No event</span>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  </div>
{/if}

<style>
  .compare-table-wrapper {
    max-height: 600px;
    overflow-y: auto;
  }

  .compare-table-wrapper table {
    table-layout: fixed;
  }

  .compare-table-wrapper th {
    position: sticky;
    top: 0;
    background: var(--color-white);
    z-index: 1;
  }

  .compare-cell {
    font-size: 0.8rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 300px;
  }

  .event-type-tag {
    display: inline-block;
    background: #e3f2fd;
    color: #1565c0;
    padding: 0.1rem 0.4rem;
    border-radius: 4px;
    font-size: 0.65rem;
    font-weight: 600;
    margin-right: 0.25rem;
    text-transform: uppercase;
  }

  .event-label {
    font-size: 0.8rem;
  }

  .event-err {
    display: block;
    color: var(--color-danger);
    font-size: 0.7rem;
    margin-top: 0.1rem;
  }

  :global(.diff-type-changed) {
    background: #fff3e0;
  }

  :global(.diff-changed) {
    background: #fffde7;
  }

  :global(.diff-mismatch) {
    background: #ffebee;
  }
</style>
