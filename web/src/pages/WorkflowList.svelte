<script lang="ts">
  import StatusBadge from '../components/StatusBadge.svelte';
  import { listWorkflows } from '../lib/api';
  import type { WorkflowInstance } from '../lib/types';

  let { onNavigate }: { onNavigate: (path: string) => void } = $props();

  let workflows = $state<WorkflowInstance[]>([]);
  let loading = $state(true);
  let error = $state('');
  let statusFilter = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      workflows = await listWorkflows(statusFilter || undefined);
    } catch (e: any) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  $effect(() => { load(); });

  function onFilterChange() { load(); }
</script>

<h2>Workflows</h2>

{#if error}<div class="error-banner">{error}</div>{/if}

<div class="filters">
  <select value={statusFilter} onchange={(e) => { statusFilter = (e.target as HTMLSelectElement).value; onFilterChange(); }}>
    <option value="">All Statuses</option>
    <option value="ready">Ready</option>
    <option value="running">Running</option>
    <option value="completed">Completed</option>
    <option value="failed">Failed</option>
    <option value="dead_letter">Dead Letter</option>
  </select>
  <button class="btn btn-primary btn-sm" onclick={load}>Refresh</button>
</div>

<div class="card">
  {#if loading}
    <p style="color: var(--color-text-muted)">Loading...</p>
  {:else if workflows.length === 0}
    <p style="color: var(--color-text-muted)">No workflows found.</p>
  {:else}
    <table>
      <thead>
        <tr><th>ID</th><th>Definition</th><th>Version</th><th>Status</th><th>Assigned To</th><th>Created</th></tr>
      </thead>
      <tbody>
        {#each workflows as wf (wf.id)}
          <tr style="cursor: pointer" onclick={() => onNavigate(`workflows/${wf.id}`)}>
            <td style="color: var(--color-primary)">{wf.id.slice(0, 12)}...</td>
            <td>{wf.def_name}</td>
            <td>v{wf.def_version}</td>
            <td><StatusBadge status={wf.status} /></td>
            <td>{wf.assigned_to?.slice(0, 8) || '-'}</td>
            <td>{wf.created_at && !wf.created_at.startsWith('0001-01-01') ? new Date(wf.created_at).toLocaleString() : '-'}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
