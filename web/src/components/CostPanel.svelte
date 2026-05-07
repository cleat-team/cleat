<script lang="ts">
  import type { EventRecord, LlmCallInfo } from '../lib/types';
  import { tryParseLlmEvent, formatCost, formatTokens } from '../lib/cost';

  let { events, workflowName = 'Workflow' }: { events: EventRecord[]; workflowName?: string } = $props();

  let llmCalls = $derived<LlmCallInfo[]>(
    events
      .map((ev) => tryParseLlmEvent(ev))
      .filter((info): info is LlmCallInfo => info !== null)
  );

  let totalCost = $derived(llmCalls.reduce((s, c) => s + c.cost, 0));
  let totalTokens = $derived(llmCalls.reduce((s, c) => s + c.usage.total_tokens, 0));
  let totalPrompt = $derived(llmCalls.reduce((s, c) => s + c.usage.prompt_tokens, 0));
  let totalCompletion = $derived(llmCalls.reduce((s, c) => s + c.usage.completion_tokens, 0));

  /** Determine the cost tier label and color. */
  function getCostTier(cost: number): { label: string; color: string } {
    if (cost > 1) return { label: 'HIGH', color: 'var(--color-danger)' };
    if (cost > 0.1) return { label: 'MODERATE', color: 'var(--color-warning)' };
    if (cost > 0) return { label: 'LOW', color: 'var(--color-success)' };
    return { label: 'NONE', color: 'var(--color-text-muted)' };
  }
  let costTier = $derived(getCostTier(totalCost));

  let maxCallCost = $derived(llmCalls.length > 0 ? Math.max(...llmCalls.map((c) => c.cost)) : 0);
</script>

<div class="card cost-panel">
  <h3>LLM Cost <span class="cost-tier-badge" style="background: {costTier.color}; color: #fff;">{costTier.label}</span></h3>

  {#if llmCalls.length === 0}
    <p style="color: var(--color-text-muted); font-size: 0.85rem;">No LLM calls detected in this workflow.</p>
  {:else}
    <!-- Summary metrics -->
    <div class="cost-metrics">
      <div class="cost-metric">
        <span class="cost-value">{formatCost(totalCost)}</span>
        <span class="cost-label">Total Cost</span>
      </div>
      <div class="cost-metric">
        <span class="cost-value">{llmCalls.length}</span>
        <span class="cost-label">LLM Calls</span>
      </div>
      <div class="cost-metric">
        <span class="cost-value">{formatTokens(totalTokens)}</span>
        <span class="cost-label">Total Tokens</span>
      </div>
      {#if llmCalls.length > 0}
        <div class="cost-metric">
          <span class="cost-value">{formatCost(totalCost / llmCalls.length)}</span>
          <span class="cost-label">Avg / Call</span>
        </div>
      {/if}
    </div>

    <!-- Token breakdown bar -->
    <div class="token-section">
      <h4>Token Breakdown</h4>
      <div class="token-bar">
        {#if totalTokens > 0}
          <div class="token-bar-segment prompt" style="width: {((totalPrompt / totalTokens) * 100).toFixed(1)}%"></div>
          <div class="token-bar-segment completion" style="width: {((totalCompletion / totalTokens) * 100).toFixed(1)}%"></div>
        {/if}
      </div>
      <div class="token-bar-legend">
        <span><span class="legend-dot prompt"></span> Prompt: {formatTokens(totalPrompt)} ({totalTokens > 0 ? ((totalPrompt / totalTokens) * 100).toFixed(1) : 0}%)</span>
        <span><span class="legend-dot completion"></span> Completion: {formatTokens(totalCompletion)} ({totalTokens > 0 ? ((totalCompletion / totalTokens) * 100).toFixed(1) : 0}%)</span>
      </div>
    </div>

    <!-- Per-call table -->
    <div class="call-table-wrapper">
      <h4>Per-Call Breakdown</h4>
      <table>
        <thead>
          <tr>
            <th>Step</th>
            <th>Model</th>
            <th>Tokens</th>
            <th>Cost</th>
            <th>Bar</th>
          </tr>
        </thead>
        <tbody>
          {#each llmCalls as call (call.step)}
            <tr>
              <td style="font-family: monospace; font-size: 0.8rem;">#{call.step}</td>
              <td>
                <span title="{call.model}" style="font-size: 0.8rem;">{call.model || '(embed)'}</span>
                <span class="call-provider">{call.provider}</span>
              </td>
              <td style="font-size: 0.8rem;">{formatTokens(call.usage.total_tokens)}</td>
              <td style="font-weight: 600; font-size: 0.8rem;">{formatCost(call.cost)}</td>
              <td>
                <div class="call-bar-track">
                  <div class="call-bar-fill" style="width: {maxCallCost > 0 ? ((call.cost / maxCallCost) * 100).toFixed(1) : 0}%;"></div>
                </div>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</div>

<style>
  .cost-panel h4 {
    font-size: 0.75rem;
    margin-bottom: 0.5rem;
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }

  .cost-tier-badge {
    display: inline-block;
    padding: 0.1rem 0.5rem;
    border-radius: 10px;
    font-size: 0.65rem;
    font-weight: 700;
    vertical-align: middle;
    margin-left: 0.4rem;
  }

  .cost-metrics {
    display: flex;
    gap: 1.5rem;
    flex-wrap: wrap;
    margin-bottom: 1rem;
  }

  .cost-metric {
    display: flex;
    flex-direction: column;
  }

  .cost-value {
    font-size: 1.2rem;
    font-weight: 700;
  }

  .cost-label {
    font-size: 0.7rem;
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .token-section {
    margin-bottom: 1rem;
  }

  .token-bar {
    display: flex;
    height: 10px;
    border-radius: 5px;
    overflow: hidden;
    background: #e9ecef;
  }

  .token-bar-segment.prompt {
    background: var(--color-primary);
  }

  .token-bar-segment.completion {
    background: var(--color-warning);
  }

  .token-bar-legend {
    display: flex;
    gap: 1rem;
    margin-top: 0.3rem;
    font-size: 0.7rem;
    color: var(--color-text-muted);
  }

  .legend-dot {
    display: inline-block;
    width: 8px;
    height: 8px;
    border-radius: 2px;
    margin-right: 3px;
  }

  .legend-dot.prompt {
    background: var(--color-primary);
  }

  .legend-dot.completion {
    background: var(--color-warning);
  }

  .call-table-wrapper {
    overflow-x: auto;
  }

  .call-provider {
    display: inline-block;
    font-size: 0.65rem;
    background: #e9ecef;
    padding: 0.05rem 0.35rem;
    border-radius: 4px;
    margin-left: 0.3rem;
    color: var(--color-text-muted);
  }

  .call-bar-track {
    width: 60px;
    height: 8px;
    background: #e9ecef;
    border-radius: 4px;
    overflow: hidden;
  }

  .call-bar-fill {
    height: 100%;
    background: var(--color-primary);
    border-radius: 4px;
    min-width: 2px;
  }
</style>
