<script lang="ts">
  import type { CostBreakdown, WorkflowCost } from '../lib/types';
  import { formatCost, formatTokens } from '../lib/cost';

  let { breakdown, workflowCosts = [] }: { breakdown: CostBreakdown; workflowCosts?: WorkflowCost[] } = $props();

  /** Cost bar: compute width percentage for a given value relative to the max. */
  function barWidth(val: number, max: number): string {
    if (max <= 0) return '0%';
    return Math.max(2, (val / max) * 100) + '%';
  }

  // Find most expensive workflow
  let mostExpensive = $derived<WorkflowCost | null>(
    workflowCosts.length > 0
      ? workflowCosts.reduce((a, b) => (a.totalCost > b.totalCost ? a : b))
      : null
  );

  // Sort model entries by cost descending
  let sortedModels = $derived(
    Object.entries(breakdown.byModel)
      .map(([model, data]) => ({ model, ...data }))
      .sort((a, b) => b.cost - a.cost)
  );

  let maxModelCost = $derived(sortedModels.length > 0 ? sortedModels[0].cost : 0);

  // Sort provider entries by cost descending
  let sortedProviders = $derived(
    Object.entries(breakdown.byProvider)
      .map(([provider, data]) => ({ provider, ...data }))
      .sort((a, b) => b.cost - a.cost)
  );

  let maxProviderCost = $derived(sortedProviders.length > 0 ? sortedProviders[0].cost : 0);
</script>

<div class="card cost-summary">
  <h3>Cost Summary</h3>

  {#if breakdown.llmCalls === 0}
    <p style="color: var(--color-text-muted); font-size: 0.85rem;">No LLM usage found across loaded workflows.</p>
  {:else}
    <!-- Top-level metrics -->
    <div class="cost-metrics">
      <div class="cost-metric">
        <span class="cost-value">{formatCost(breakdown.totalCost)}</span>
        <span class="cost-label">Total Spend</span>
      </div>
      <div class="cost-metric">
        <span class="cost-value">{formatTokens(breakdown.totalTokens.total_tokens)}</span>
        <span class="cost-label">Total Tokens</span>
      </div>
      <div class="cost-metric">
        <span class="cost-value">{breakdown.llmCalls}</span>
        <span class="cost-label">LLM Calls</span>
      </div>
      {#if breakdown.llmCalls > 0}
        <div class="cost-metric">
          <span class="cost-value">{formatCost(breakdown.totalCost / breakdown.llmCalls)}</span>
          <span class="cost-label">Avg Cost / Call</span>
        </div>
      {/if}
    </div>

    <div class="cost-breakdowns">
      <!-- Spend by Model -->
      <div class="cost-column">
        <h4>Spend by Model</h4>
        <div class="cost-bars">
          {#each sortedModels as item}
            <div class="cost-bar-row">
              <span class="cost-bar-label" title="{item.model}">{item.model}</span>
              <div class="cost-bar-track">
                <div class="cost-bar-fill" style="width: {barWidth(item.cost, maxModelCost)}; background: var(--color-primary);"></div>
              </div>
              <span class="cost-bar-value">{formatCost(item.cost)}</span>
            </div>
          {/each}
        </div>
      </div>

      <!-- Spend by Provider -->
      <div class="cost-column">
        <h4>Spend by Provider</h4>
        <div class="cost-bars">
          {#each sortedProviders as item}
            <div class="cost-bar-row">
              <span class="cost-bar-label">{item.provider}</span>
              <div class="cost-bar-track">
                <div class="cost-bar-fill" style="width: {barWidth(item.cost, maxProviderCost)}; background: var(--color-success);"></div>
              </div>
              <span class="cost-bar-value">{formatCost(item.cost)}</span>
            </div>
          {/each}
        </div>
      </div>
    </div>

    <!-- Token breakdown -->
    <div class="cost-token-breakdown">
      <h4>Token Usage</h4>
      <div class="cost-metrics" style="margin-top: 0.5rem;">
        <div class="cost-metric">
          <span class="cost-value">{formatTokens(breakdown.totalTokens.prompt_tokens)}</span>
          <span class="cost-label">Prompt Tokens</span>
        </div>
        <div class="cost-metric">
          <span class="cost-value">{formatTokens(breakdown.totalTokens.completion_tokens)}</span>
          <span class="cost-label">Completion Tokens</span>
        </div>
      </div>
      {#if breakdown.totalTokens.total_tokens > 0}
        <div class="token-bar">
          <div class="token-bar-segment prompt" style="width: {(breakdown.totalTokens.prompt_tokens / breakdown.totalTokens.total_tokens * 100).toFixed(1)}%"></div>
          <div class="token-bar-segment completion" style="width: {(breakdown.totalTokens.completion_tokens / breakdown.totalTokens.total_tokens * 100).toFixed(1)}%"></div>
        </div>
        <div class="token-bar-legend">
          <span><span class="legend-dot prompt"></span> Prompt ({((breakdown.totalTokens.prompt_tokens / breakdown.totalTokens.total_tokens) * 100).toFixed(1)}%)</span>
          <span><span class="legend-dot completion"></span> Completion ({((breakdown.totalTokens.completion_tokens / breakdown.totalTokens.total_tokens) * 100).toFixed(1)}%)</span>
        </div>
      {/if}
    </div>

    <!-- Most expensive workflow -->
    {#if mostExpensive && mostExpensive.totalCost > 0}
      <div class="cost-most-expensive">
        <h4>Most Expensive Workflow</h4>
        <div class="expensive-info">
          <a href="#workflows/{mostExpensive.workflowId}" style="color: var(--color-primary); font-family: monospace; font-size: 0.85rem;">
            {mostExpensive.workflowId.slice(0, 16)}...
          </a>
          <span style="color: var(--color-text-muted); font-size: 0.85rem;">
            {mostExpensive.workflowType} &mdash; {formatCost(mostExpensive.totalCost)} ({mostExpensive.llmCalls} calls)
          </span>
        </div>
      </div>
    {/if}
  {/if}
</div>

<style>
  .cost-summary h4 {
    font-size: 0.8rem;
    margin-bottom: 0.5rem;
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: 0.03em;
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
    font-size: 1.3rem;
    font-weight: 700;
  }

  .cost-label {
    font-size: 0.7rem;
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .cost-breakdowns {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1.5rem;
    margin-bottom: 1rem;
  }

  @media (max-width: 700px) {
    .cost-breakdowns {
      grid-template-columns: 1fr;
    }
  }

  .cost-bars {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }

  .cost-bar-row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }

  .cost-bar-label {
    width: 6rem;
    font-size: 0.75rem;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    flex-shrink: 0;
    color: var(--color-text);
  }

  .cost-bar-track {
    flex: 1;
    height: 10px;
    background: #e9ecef;
    border-radius: 5px;
    overflow: hidden;
  }

  .cost-bar-fill {
    height: 100%;
    border-radius: 5px;
    min-width: 2px;
    transition: width 0.3s ease;
  }

  .cost-bar-value {
    width: 4.5rem;
    text-align: right;
    font-size: 0.75rem;
    font-weight: 600;
    color: var(--color-text);
    flex-shrink: 0;
  }

  .cost-token-breakdown {
    margin-bottom: 1rem;
  }

  .token-bar {
    display: flex;
    height: 12px;
    border-radius: 6px;
    overflow: hidden;
    background: #e9ecef;
    margin-top: 0.5rem;
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
    margin-top: 0.35rem;
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

  .cost-most-expensive {
    border-top: 1px solid var(--color-border);
    padding-top: 0.75rem;
  }

  .expensive-info {
    display: flex;
    gap: 0.75rem;
    align-items: center;
    flex-wrap: wrap;
  }
</style>
