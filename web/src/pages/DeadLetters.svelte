<script lang="ts">
  import StatusBadge from '../components/StatusBadge.svelte';
  import { listDeadLetters, reprocessDeadLetter, terminateDeadLetter } from '../lib/api';
  import type { WorkflowInstance } from '../lib/types';

  let { onNavigate }: { onNavigate: (path: string) => void } = $props();

  let workflows = $state<WorkflowInstance[]>([]);
  let loading = $state(true);
  let error = $state('');
  let actionMsg = $state('');
  let actionError = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      workflows = await listDeadLetters();
    } catch (e: any) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  $effect(() => { load(); });

  async function doReprocess(id: string) {
    actionMsg = '';
    actionError = '';
    try {
      const result = await reprocessDeadLetter(id);
      actionMsg = `Workflow ${id.slice(0, 12)}... reprocessed as ${result.id.slice(0, 12)}...`;
      await load();
    } catch (e: any) {
      actionError = e.message;
    }
  }

  async function doTerminate(id: string) {
    if (!confirm('Terminate this dead-lettered workflow? This action cannot be undone.')) return;
    actionMsg = '';
    actionError = '';
    try {
      await terminateDeadLetter(id, 'Terminated from Dead Letter Queue UI');
      actionMsg = `Workflow ${id.slice(0, 12)}... terminated.`;
      await load();
    } catch (e: any) {
      actionError = e.message;
    }
  }
</script>

<h2>Dead Letter Queue</h2>

<p style="font-size: 0.85rem; color: var(--color-text-muted); margin-bottom: 1rem;">
  Workflows that have exhausted all retry attempts. You may reprocess them
  (creating a new run with the same input) or terminate them permanently.
</p>

{#if error}<div class="error-banner">{error}</div>{/if}
{#if actionMsg}<div class="card" style="background: #e8f5e9; color: var(--color-success);">{actionMsg}</div>{/if}
{#if actionError}<div class="error-banner">{actionError}</div>{/if}

<div class="card">
  {#if loading}
    <p style="color: var(--color-text-muted)">Loading...</p>
  {:else if workflows.length === 0}
    <p style="color: var(--color-text-muted)">No dead-lettered workflows.</p>
  {:else}
    <table>
      <thead>
        <tr>
          <th>Workflow ID</th>
          <th>Definition</th>
          <th>Error</th>
          <th>Dead-Lettered</th>
          <th>Actions</th>
        </tr>
      </thead>
      <tbody>
        {#each workflows as wf (wf.id)}
          <tr>
            <td style="font-family:monospace; font-size:0.8rem; color:var(--color-primary); cursor:pointer;"
                role="link" tabindex="0"
                onclick={() => onNavigate(`workflows/${wf.id}`)}
                onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onNavigate(`workflows/${wf.id}`); } }}>
              {wf.id.slice(0, 12)}...
            </td>
            <td>{wf.def_name} v{wf.def_version}</td>
            <td style="max-width:300px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--color-danger);"
                title={wf.error}>
              {wf.error?.slice(0, 80) || '-'}
            </td>
            <td>{wf.created_at ? new Date(wf.created_at).toLocaleString() : '-'}</td>
            <td>
              <div style="display:flex; gap:0.25rem;">
                <button class="btn btn-primary btn-sm" onclick={() => doReprocess(wf.id)}>Retry</button>
                <button class="btn btn-danger btn-sm" onclick={() => doTerminate(wf.id)}>Terminate</button>
              </div>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
