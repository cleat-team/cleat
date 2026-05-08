<script lang="ts">
  import { listDefinitions } from '../lib/api';
  import type { WorkflowDefInfo, WorkflowMemoryStats } from '../lib/types';

  let definitions = $state<WorkflowDefInfo[]>([]);
  let loading = $state(true);
  let error = $state('');
  let expandedDef = $state<string | null>(null);

  async function load() {
    loading = true;
    error = '';
    try {
      definitions = await listDefinitions();
    } catch (e: any) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  function toggleExpand(name: string) {
    expandedDef = expandedDef === name ? null : name;
  }

  function formatBytes(bytes: number): string {
    if (bytes >= 1_073_741_824) return (bytes / 1_073_741_824).toFixed(1) + ' GiB';
    if (bytes >= 1_048_576) return (bytes / 1_048_576).toFixed(1) + ' MiB';
    if (bytes >= 1024) return (bytes / 1024).toFixed(1) + ' KiB';
    return bytes + ' B';
  }

  function barPercent(value: number, max: number): string {
    if (max === 0) return '0%';
    return Math.min((value / max) * 100, 100).toFixed(1) + '%';
  }

  let allMax = $derived(
    Math.max(...definitions.map(d => d.memory?.max_bytes ?? 0), 1)
  );

  $effect(() => { load(); });
</script>

<div class="definitions-page">
  <h1>Workflow Definitions</h1>

  {#if error}
    <div class="error-banner">{error}</div>
  {/if}

  {#if loading}
    <p>Loading definitions...</p>
  {:else}
    <table class="defs-table">
      <thead>
        <tr>
          <th>Name</th>
          <th>Version</th>
          <th>Active</th>
          <th>Memory (min / avg / max)</th>
          <th>Samples</th>
        </tr>
      </thead>
      <tbody>
        {#each definitions as def (def.name + ':' + def.version)}
          <tr class="def-row" class:deprecated={def.deprecated} onclick={() => toggleExpand(def.name)}>
            <td>
              <span class="def-name">{def.name}</span>
              {#if def.deprecated}<span class="badge deprecated">deprecated</span>{/if}
            </td>
            <td>v{def.version}</td>
            <td>{def.active_instances}</td>
            <td>
              {#if def.memory}
                <div class="memory-bar-container">
                  <div class="memory-bar-labels">
                    <span class="mem-min">{formatBytes(def.memory.min_bytes)}</span>
                    <span class="mem-avg">{formatBytes(def.memory.avg_bytes)}</span>
                    <span class="mem-max">{formatBytes(def.memory.max_bytes)}</span>
                  </div>
                  <div class="memory-bar">
                    <div class="bar-fill" style="width:{barPercent(def.memory.max_bytes, allMax)}">
                      <div class="bar-p50-marker" style="left:{barPercent(def.memory.p50, def.memory.max_bytes)}" title="P50: {formatBytes(def.memory.p50)}"></div>
                      <div class="bar-p90-marker" style="left:{barPercent(def.memory.p90, def.memory.max_bytes)}" title="P90: {formatBytes(def.memory.p90)}"></div>
                    </div>
                  </div>
                </div>
              {:else}
                <span class="no-data">no data</span>
              {/if}
            </td>
            <td>{def.memory?.sample_count ?? 0}</td>
          </tr>
          {#if expandedDef === def.name && def.memory}
            <tr class="expanded-row">
              <td colspan="5">
                <div class="distribution-detail">
                  <h4>{def.name} memory distribution (n={def.memory.sample_count})</h4>
                  <div class="percentile-bars">
                    {#each [
                      { label: 'min', value: def.memory.min_bytes },
                      { label: 'P10', value: def.memory.p10 },
                      { label: 'P25', value: def.memory.p25 },
                      { label: 'P50', value: def.memory.p50 },
                      { label: 'P75', value: def.memory.p75 },
                      { label: 'P90', value: def.memory.p90 },
                      { label: 'P99', value: def.memory.p99 },
                      { label: 'max', value: def.memory.max_bytes }
                    ] as pct}
                      <div class="pct-item">
                        <span class="pct-label">{pct.label}</span>
                        <div class="pct-bar-bg">
                          <div class="pct-bar-fill" style="width:{barPercent(pct.value, def.memory!.max_bytes)}"></div>
                        </div>
                        <span class="pct-value">{formatBytes(pct.value)}</span>
                      </div>
                    {/each}
                  </div>
                </div>
              </td>
            </tr>
          {/if}
        {/each}
      </tbody>
    </table>
  {/if}
</div>

<style>
  .definitions-page {
    padding: 1rem;
  }
  h1 {
    margin-bottom: 1rem;
  }
  .error-banner {
    background: #fee2e2;
    color: #991b1b;
    padding: 0.75rem;
    border-radius: 6px;
    margin-bottom: 1rem;
  }
  .defs-table {
    width: 100%;
    border-collapse: collapse;
  }
  .defs-table th {
    text-align: left;
    padding: 0.5rem 0.75rem;
    border-bottom: 2px solid #e5e7eb;
    font-size: 0.85rem;
    color: #6b7280;
  }
  .defs-table td {
    padding: 0.5rem 0.75rem;
    border-bottom: 1px solid #f3f4f6;
    font-size: 0.9rem;
  }
  .def-row {
    cursor: pointer;
    transition: background 0.15s;
  }
  .def-row:hover {
    background: #f9fafb;
  }
  .def-row.deprecated {
    opacity: 0.6;
  }
  .def-name {
    font-weight: 600;
  }
  .badge.deprecated {
    background: #fef3c7;
    color: #92400e;
    padding: 0.1rem 0.4rem;
    border-radius: 4px;
    font-size: 0.75rem;
    margin-left: 0.5rem;
  }
  .no-data {
    color: #9ca3af;
    font-style: italic;
  }
  .memory-bar-container {
    min-width: 200px;
  }
  .memory-bar-labels {
    display: flex;
    justify-content: space-between;
    font-size: 0.7rem;
    color: #6b7280;
    margin-bottom: 2px;
  }
  .memory-bar {
    height: 10px;
    background: #e5e7eb;
    border-radius: 5px;
    overflow: visible;
    position: relative;
  }
  .bar-fill {
    height: 100%;
    background: linear-gradient(90deg, #60a5fa, #3b82f6, #1d4ed8);
    border-radius: 5px;
    position: relative;
  }
  .bar-p50-marker {
    position: absolute;
    top: -2px;
    width: 2px;
    height: 14px;
    background: #f59e0b;
  }
  .bar-p90-marker {
    position: absolute;
    top: -2px;
    width: 2px;
    height: 14px;
    background: #ef4444;
  }
  .expanded-row td {
    padding: 0;
  }
  .distribution-detail {
    padding: 1rem 2rem;
    background: #f9fafb;
  }
  .distribution-detail h4 {
    margin-bottom: 0.75rem;
  }
  .percentile-bars {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .pct-item {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .pct-label {
    width: 36px;
    text-align: right;
    font-size: 0.8rem;
    color: #6b7280;
  }
  .pct-bar-bg {
    flex: 1;
    height: 8px;
    background: #e5e7eb;
    border-radius: 4px;
    overflow: hidden;
  }
  .pct-bar-fill {
    height: 100%;
    background: #3b82f6;
    border-radius: 4px;
  }
  .pct-value {
    width: 80px;
    font-size: 0.8rem;
    color: #374151;
  }
</style>
