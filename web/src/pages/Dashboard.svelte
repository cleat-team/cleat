<script lang="ts">
  import SummaryCard from '../components/SummaryCard.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import { listWorkflows } from '../lib/api';
  import type { WorkflowInstance } from '../lib/types';

  let workflows = $state<WorkflowInstance[]>([]);
  let loading = $state(true);
  let error = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      workflows = await listWorkflows();
    } catch (e: any) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  $effect(() => { load(); });

  let active = $derived(workflows.filter(w => w.status === 'running').length);
  let completed = $derived(workflows.filter(w => w.status === 'completed').length);
  let failed = $derived(workflows.filter(w => w.status === 'failed' || w.status === 'dead_letter').length);
  let recent = $derived(workflows.slice(0, 10));
</script>

<h2>Dashboard</h2>

{#if error}<div class="error-banner">{error}</div>{/if}

<div class="summary-grid">
  <SummaryCard count={active} label="Active" color="#ff9f1c" />
  <SummaryCard count={completed} label="Completed" color="#2ec4b6" />
  <SummaryCard count={failed} label="Failed" color="#e63946" />
</div>

<div class="card">
  <h3>Recent Workflows</h3>
  {#if loading}
    <p style="color: var(--color-text-muted)">Loading...</p>
  {:else}
    <table>
      <thead>
        <tr><th>ID</th><th>Definition</th><th>Status</th><th>Created</th></tr>
      </thead>
      <tbody>
        {#each recent as wf (wf.id)}
          <tr>
            <td><a href="#workflows/{wf.id}" style="color: var(--color-primary)">{wf.id.slice(0, 12)}...</a></td>
            <td>{wf.def_name} v{wf.def_version}</td>
            <td><StatusBadge status={wf.status} /></td>
            <td>{new Date(wf.created_at).toLocaleString()}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
