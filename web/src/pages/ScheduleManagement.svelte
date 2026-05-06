<script lang="ts">
  import { listSchedules, createSchedule, deleteSchedule, enableSchedule, disableSchedule } from '../lib/api';
  import type { Schedule } from '../lib/types';

  let schedules = $state<Schedule[]>([]);
  let loading = $state(true);
  let error = $state('');

  let showAdd = $state(false);
  let newName = $state('');
  let newCron = $state('');
  let newDefName = $state('');
  let newEntryPoint = $state('');
  let newInput = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      schedules = await listSchedules();
    } catch (e: any) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  $effect(() => { load(); });

  async function doCreate() {
    if (!newName || !newCron || !newDefName) return;
    try {
      await createSchedule({ name: newName, cron: newCron, def_name: newDefName, entry_point: newEntryPoint || undefined, input: newInput || undefined });
      showAdd = false;
      newName = newCron = newDefName = newEntryPoint = newInput = '';
      await load();
    } catch (e: any) {
      error = e.message;
    }
  }

  async function doDelete(name: string) {
    if (!confirm(`Delete schedule "${name}"?`)) return;
    try {
      await deleteSchedule(name);
      await load();
    } catch (e: any) {
      error = e.message;
    }
  }

  async function doToggle(s: Schedule) {
    try {
      if (s.enabled) {
        await disableSchedule(s.name);
      } else {
        await enableSchedule(s.name);
      }
      await load();
    } catch (e: any) {
      error = e.message;
    }
  }
</script>

<h2>Schedules</h2>

{#if error}<div class="error-banner">{error}</div>{/if}

<div style="margin-bottom: 1rem;">
  <button class="btn btn-primary btn-sm" onclick={() => showAdd = true}>+ Add Schedule</button>
  <button class="btn btn-sm" style="background:#eee; margin-left: 0.5rem;" onclick={load}>Refresh</button>
</div>

<div class="card">
  {#if loading}
    <p style="color: var(--color-text-muted)">Loading...</p>
  {:else if schedules.length === 0}
    <p style="color: var(--color-text-muted)">No schedules configured.</p>
  {:else}
    <table>
      <thead>
        <tr><th>Name</th><th>Cron</th><th>Definition</th><th>Entry Point</th><th>Enabled</th><th>Next Run</th><th>Actions</th></tr>
      </thead>
      <tbody>
        {#each schedules as s (s.name)}
          <tr>
            <td style="font-weight:600;">{s.name}</td>
            <td style="font-family:monospace; font-size:0.8rem;">{s.cron_expression}</td>
            <td>{s.def_name}</td>
            <td>{s.entry_point || 'default'}</td>
            <td>
              <span class="badge" class:badge-completed={s.enabled} class:badge-failed={!s.enabled}>
                {s.enabled ? 'Yes' : 'No'}
              </span>
            </td>
            <td style="font-size:0.8rem;">{s.next_run_at ? new Date(s.next_run_at).toLocaleString() : '-'}</td>
            <td>
              <button class="btn btn-sm" style="background:#eee; margin-right:0.25rem;" onclick={() => doToggle(s)}>
                {s.enabled ? 'Disable' : 'Enable'}
              </button>
              <button class="btn btn-danger btn-sm" onclick={() => doDelete(s.name)}>Delete</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

{#if showAdd}
  <div class="modal-backdrop" onclick={() => showAdd = false}>
    <div class="modal" onclick={(e) => e.stopPropagation()}>
      <h3>Add Schedule</h3>
      <div class="form-group">
        <label>Name</label>
        <input type="text" bind:value={newName} placeholder="my-schedule" />
      </div>
      <div class="form-group">
        <label>Cron Expression</label>
        <input type="text" bind:value={newCron} placeholder="*/5 * * * *" />
      </div>
      <div class="form-group">
        <label>Workflow Definition</label>
        <input type="text" bind:value={newDefName} placeholder="PlaceOrder" />
      </div>
      <div class="form-group">
        <label>Entry Point (optional)</label>
        <input type="text" bind:value={newEntryPoint} placeholder="place_order" />
      </div>
      <div class="form-group">
        <label>Input JSON (optional)</label>
        <textarea bind:value={newInput} rows={3} placeholder="Example JSON input"></textarea>
      </div>
      <div style="display:flex; gap: 0.5rem; justify-content: flex-end;">
        <button class="btn btn-sm" style="background:#eee;" onclick={() => showAdd = false}>Cancel</button>
        <button class="btn btn-primary btn-sm" onclick={doCreate}>Create</button>
      </div>
    </div>
  </div>
{/if}
