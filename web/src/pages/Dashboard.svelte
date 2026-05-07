<script lang="ts">
  import SummaryCard from '../components/SummaryCard.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import CostSummary from '../components/CostSummary.svelte';
  import { listWorkflows, getWorkflowHistory } from '../lib/api';
  import type { WorkflowInstance, EventRecord, CostBreakdown, WorkflowCost } from '../lib/types';
  import { extractCostFromEvents, aggregateWorkflowCosts } from '../lib/cost';

  let workflows = $state<WorkflowInstance[]>([]);
  let loading = $state(true);
  let costLoading = $state(false);
  let error = $state('');

  // Cost data
  let costBreakdown = $state<CostBreakdown>({
    byModel: {},
    byProvider: {},
    totalCost: 0,
    totalTokens: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
    llmCalls: 0,
  });
  let workflowCosts = $state<WorkflowCost[]>([]);

  async function load() {
    loading = true;
    error = '';
    try {
      workflows = await listWorkflows();
      // Load cost data for recent workflows
      loadCosts();
    } catch (e: any) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  async function loadCosts() {
    const recent = workflows.slice(0, 10);
    if (recent.length === 0) return;
    costLoading = true;
    try {
      const results = await Promise.allSettled(
        recent.map((wf) => getWorkflowHistory(wf.id))
      );
      const eventsMap: Record<string, EventRecord[]> = {};
      let allEvents: EventRecord[] = [];
      for (let i = 0; i < recent.length; i++) {
        const r = results[i];
        if (r.status === 'fulfilled') {
          eventsMap[recent[i].id] = r.value;
          allEvents = allEvents.concat(r.value);
        } else {
          eventsMap[recent[i].id] = [];
        }
      }
      costBreakdown = extractCostFromEvents(allEvents);
      workflowCosts = aggregateWorkflowCosts(recent, eventsMap);
    } catch {
      // Silently ignore cost loading errors.
    } finally {
      costLoading = false;
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

{#if costLoading}
  <div class="card">
    <h3>Cost Summary</h3>
    <p style="color: var(--color-text-muted); font-size: 0.85rem;">Loading cost data...</p>
  </div>
{:else}
  <CostSummary breakdown={costBreakdown} {workflowCosts} />
{/if}

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
