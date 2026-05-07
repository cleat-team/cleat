<script lang="ts">
  import StatusBadge from '../components/StatusBadge.svelte';
  import EventTimeline from '../components/EventTimeline.svelte';
  import DAGGraph from '../components/DAGGraph.svelte';
  import { getWorkflow, getWorkflowHistory, getWorkflowDAG, signalWorkflow, cancelWorkflow } from '../lib/api';
  import type { WorkflowInstance, EventRecord, DAGSpec } from '../lib/types';

  let { workflowId, onNavigate }: { workflowId: string; onNavigate: (path: string) => void } = $props();

  let wf = $state<WorkflowInstance | null>(null);
  let events = $state<EventRecord[]>([]);
  let dagData = $state<DAGSpec | null>(null);
  let loading = $state(true);
  let error = $state('');

  let showSignal = $state(false);
  let signalName = $state('');
  let signalPayload = $state('');

  let showCancel = $state(false);
  let cancelReason = $state('');

  async function load() {
    loading = true;
    error = '';
    dagData = null;
    try {
      [wf, events] = await Promise.all([getWorkflow(workflowId), getWorkflowHistory(workflowId)]);
      // Fetch DAG data; silently ignore if not available.
      try {
        const dagResp = await getWorkflowDAG(workflowId);
        dagData = dagResp.dag;
      } catch (_) {
        // No DAG spec — this is fine.
      }
    } catch (e: any) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  $effect(() => { load(); });

  async function doSignal() {
    try {
      await signalWorkflow(workflowId, signalName, signalPayload || undefined);
      showSignal = false;
      signalName = '';
      signalPayload = '';
      await load();
    } catch (e: any) {
      error = e.message;
    }
  }

  async function doCancel() {
    try {
      await cancelWorkflow(workflowId, cancelReason || undefined);
      showCancel = false;
      cancelReason = '';
      await load();
    } catch (e: any) {
      error = e.message;
    }
  }
</script>

<h2>Workflow Detail</h2>
<p><a href="#workflows" style="color: var(--color-primary); font-size: 0.85rem;">&larr; Back to list</a></p>

{#if error}<div class="error-banner">{error}</div>{/if}

{#if loading}
  <p style="color: var(--color-text-muted)">Loading...</p>
{:else if wf}
  <div class="card">
    <h3>{wf.def_name} v{wf.def_version} <StatusBadge status={wf.status} /></h3>
    <table style="margin-top: 0.5rem;">
      <tbody>
        <tr><td style="font-weight:600; width:120px;">ID</td><td style="font-family:monospace; font-size:0.8rem;">{wf.id}</td></tr>
        <tr><td style="font-weight:600;">Status</td><td>{wf.status}</td></tr>
        <tr><td style="font-weight:600;">Assigned To</td><td>{wf.assigned_to || '(unassigned)'}</td></tr>
        <tr><td style="font-weight:600;">Created</td><td>{new Date(wf.created_at).toLocaleString()}</td></tr>
        <tr><td style="font-weight:600;">Updated</td><td>{new Date(wf.updated_at).toLocaleString()}</td></tr>
        {#if wf.next_wake_at}
          <tr><td style="font-weight:600;">Next Wake</td><td>{new Date(wf.next_wake_at).toLocaleString()}</td></tr>
        {/if}
        {#if wf.error}
          <tr><td style="font-weight:600; color:var(--color-danger);">Error</td><td style="color:var(--color-danger);">{wf.error}</td></tr>
        {/if}
      </tbody>
    </table>

    <div style="margin-top: 1rem; display: flex; gap: 0.5rem;">
      {#if wf.status === 'running'}
        <button class="btn btn-primary btn-sm" onclick={() => showSignal = true}>Send Signal</button>
        <button class="btn btn-danger btn-sm" onclick={() => showCancel = true}>Cancel</button>
      {/if}
      <button class="btn btn-sm" style="background:#eee;" onclick={load}>Refresh</button>
    </div>
  </div>

  {#if wf.input && wf.input !== '{}' && wf.input !== '""'}
    <div class="card">
      <h3>Input</h3>
      <pre style="font-size:0.8rem; overflow-x:auto; white-space:pre-wrap;">{JSON.stringify(JSON.parse(wf.input), null, 2)}</pre>
    </div>
  {/if}

  {#if wf.result && wf.status === 'completed'}
    <div class="card">
      <h3>Result</h3>
      <pre style="font-size:0.8rem; overflow-x:auto; white-space:pre-wrap;">{wf.result}</pre>
    </div>
  {/if}

  {#if dagData}
    <div class="card">
      <DAGGraph dag={dagData} />
    </div>
  {/if}

  <div class="card">
    <h3>Event Timeline ({events.length} events)</h3>
    <EventTimeline {events} />
  </div>
{:else}
  <p style="color: var(--color-text-muted);">Workflow not found.</p>
{/if}

{#if showSignal}
  <!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
  <div class="modal-backdrop" onclick={() => showSignal = false}>
    <!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
      <div class="modal" onclick={(e) => e.stopPropagation()}>
      <h3>Send Signal</h3>
      <div class="form-group">
        <label for="signal-name-input">Signal Name</label>
        <input id="signal-name-input" type="text" bind:value={signalName} placeholder="e.g. order_shipped" />
      </div>
      <div class="form-group">
        <label for="signal-payload-input">Payload (optional)</label>
        <input id="signal-payload-input" type="text" bind:value={signalPayload} placeholder="JSON payload" />
      </div>
      <div style="display:flex; gap: 0.5rem; justify-content: flex-end;">
        <button class="btn btn-sm" style="background:#eee;" onclick={() => showSignal = false}>Cancel</button>
        <button class="btn btn-primary btn-sm" onclick={doSignal}>Send</button>
      </div>
    </div>
  </div>
{/if}

{#if showCancel}
  <!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
  <div class="modal-backdrop" onclick={() => showCancel = false}>
    <!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
      <div class="modal" onclick={(e) => e.stopPropagation()}>
      <h3>Cancel Workflow</h3>
      <div class="form-group">
        <label for="cancel-reason-input">Reason</label>
        <input id="cancel-reason-input" type="text" bind:value={cancelReason} placeholder="Reason for cancellation" />
      </div>
      <div style="display:flex; gap: 0.5rem; justify-content: flex-end;">
        <button class="btn btn-sm" style="background:#eee;" onclick={() => showCancel = false}>Back</button>
        <button class="btn btn-danger btn-sm" onclick={doCancel}>Cancel Workflow</button>
      </div>
    </div>
  </div>
{/if}
