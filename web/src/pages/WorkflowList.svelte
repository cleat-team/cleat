<script lang="ts">
  import StatusBadge from '../components/StatusBadge.svelte';
  import { listWorkflowsDetailed } from '../lib/api';
  import type { StateSearchWindow } from '../lib/api';
  import type { WorkflowInstance } from '../lib/types';

  let { onNavigate }: { onNavigate: (path: string) => void } = $props();

  let workflows = $state<WorkflowInstance[]>([]);
  let loading = $state(true);
  let error = $state('');
  let statusFilter = $state('');

  // cleat#3775: find a run by what it published. `stateInput` is what is typed;
  // `stateFilter` is what the listing was last asked for, so editing the box does
  // not change the results until Search (or Enter), and the empty-result message
  // can say what was searched for rather than what is half-typed.
  let stateInput = $state('');
  let stateFilter = $state<Record<string, string> | undefined>(undefined);
  let stateHint = $state('');
  // Set when the worker searched a WINDOW of recent runs because it seals query_state (cleat#3775, part 2).
  let searchWindow = $state<StateSearchWindow | undefined>(undefined);

  async function load() {
    loading = true;
    error = '';
    try {
      const result = await listWorkflowsDetailed(statusFilter || undefined, stateFilter);
      workflows = result.workflows;
      searchWindow = result.window;
    } catch (e: any) {
      // A deployment that cannot search by published state answers with a message
      // naming the flag (422); it is shown as it comes, not paraphrased.
      error = e.message;
      workflows = [];
      searchWindow = undefined;
    } finally {
      loading = false;
    }
  }

  $effect(() => { load(); });

  function onFilterChange() { load(); }

  // key=value, split at the FIRST '=', so a value may contain one. Spaces around
  // the key and the value are trimmed: "order_id = 123" is what people type, and
  // searching for the key "order_id " would find nothing and say so only as
  // "No runs". A value with leading or trailing spaces is reachable by the API.
  function parseStateInput(text: string): Record<string, string> | undefined | null {
    const t = text.trim();
    if (!t) return undefined;
    const i = t.indexOf('=');
    const key = i < 0 ? '' : t.slice(0, i).trim();
    if (!key) return null;
    return { [key]: t.slice(i + 1).trim() };
  }

  function searchState() {
    const parsed = parseStateInput(stateInput);
    if (parsed === null) {
      stateHint = 'Enter a published value as key=value, for example order_id=123';
      return;
    }
    stateHint = '';
    stateFilter = parsed;
    load();
  }

  function clearState() {
    stateInput = '';
    stateHint = '';
    stateFilter = undefined;
    load();
  }

  const stateLabel = $derived(
    stateFilter ? Object.entries(stateFilter).map(([k, v]) => `${k}=${v}`).join(', ') : '',
  );

  // What a window search covered, in words a person can act on. "None found" means two different things
  // depending on whether the window ran out, and only one of them says the run does not exist.
  const windowNote = $derived.by(() => {
    const w = searchWindow;
    if (!stateFilter || !w) return '';
    const base = w.exhausted
      ? `Searched the most recent ${w.scanned} runs. Older runs were not searched, so a run that is not listed may exist.`
      : `Searched all ${w.scanned} runs.`;
    return w.unreadable > 0
      ? `${base} ${w.unreadable} run${w.unreadable === 1 ? '' : 's'} could not be read and ${w.unreadable === 1 ? 'was' : 'were'} not matched.`
      : base;
  });
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
  <input
    type="text"
    class="state-filter"
    placeholder="Published value, e.g. order_id=123"
    aria-label="Find a run by a value it published (key=value)"
    value={stateInput}
    oninput={(e) => { stateInput = (e.target as HTMLInputElement).value; }}
    onkeydown={(e) => { if (e.key === 'Enter') searchState(); }}
  />
  <button class="btn btn-primary btn-sm" onclick={searchState}>Search</button>
  <button class="btn btn-sm" onclick={clearState} disabled={!stateInput && !stateFilter}>Clear</button>
  <button class="btn btn-primary btn-sm" onclick={load}>Refresh</button>
</div>
{#if stateHint}<div class="error-banner" role="alert">{stateHint}</div>{/if}
{#if windowNote}<p class="window-note" role="status" style="color: var(--color-text-muted)">{windowNote}</p>{/if}

<div class="card">
  {#if loading}
    <p style="color: var(--color-text-muted)">Loading...</p>
  {:else if workflows.length === 0}
    <p style="color: var(--color-text-muted)">
      {#if stateFilter}No runs found that published {stateLabel}.{#if searchWindow?.exhausted} That is not the same as there being none: see above.{/if}{:else}No workflows found.{/if}
    </p>
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
            <td>{new Date(wf.created_at).toLocaleString()}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
