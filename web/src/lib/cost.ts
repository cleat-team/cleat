import type { EventRecord, TokenUsage, CostBreakdown, WorkflowCost, WorkflowInstance, LlmCallInfo } from './types';

// ──────────────────────────────────────────────
// Pricing per 1M tokens (input / output) in USD
// ──────────────────────────────────────────────
const MODEL_PRICING: Record<string, { input: number; output: number }> = {
  'gpt-4o': { input: 2.50, output: 10.00 },
  'gpt-4o-mini': { input: 0.15, output: 0.60 },
  'gpt-4-turbo': { input: 10.00, output: 30.00 },
  'gpt-3.5-turbo': { input: 0.50, output: 1.50 },
  'gpt-4': { input: 30.00, output: 60.00 },
  'claude-sonnet-4-6': { input: 3.00, output: 15.00 },
  'claude-opus-4-7': { input: 15.00, output: 75.00 },
  'claude-haiku-4-5': { input: 0.80, output: 4.00 },
  'claude-3-haiku': { input: 0.25, output: 1.25 },
  'claude-3-sonnet': { input: 3.00, output: 15.00 },
  'claude-3-opus': { input: 15.00, output: 75.00 },
  'claude-2': { input: 8.00, output: 24.00 },
  'claude-instant': { input: 0.80, output: 2.40 },
  'gemini-2.5-flash': { input: 0.15, output: 0.60 },
  'gemini-2.5-pro': { input: 1.25, output: 10.00 },
  'gemini-1.5-flash': { input: 0.075, output: 0.30 },
  'gemini-1.5-pro': { input: 1.25, output: 5.00 },
  'mistral-large': { input: 2.00, output: 6.00 },
  'mistral-small': { input: 1.00, output: 3.00 },
  'mistral-medium': { input: 2.50, output: 7.50 },
  'llama-3.1-70b': { input: 0.59, output: 0.79 },
  'llama-3.1-8b': { input: 0.10, output: 0.40 },
  'llama-3-70b': { input: 0.65, output: 0.85 },
  'llama-2-70b': { input: 0.70, output: 0.90 },
  'deepseek-chat': { input: 0.28, output: 1.10 },
  'deepseek-coder': { input: 0.14, output: 0.42 },
};

// Default pricing for models not in the table (per provider)
const PROVIDER_DEFAULT_PRICING: Record<string, { input: number; output: number }> = {
  'openai': { input: 3.00, output: 12.00 },
  'anthropic': { input: 8.00, output: 24.00 },
  'google': { input: 1.00, output: 4.00 },
  'mistral': { input: 2.00, output: 6.00 },
  'meta': { input: 0.60, output: 0.80 },
  'deepseek': { input: 0.28, output: 1.10 },
  'together': { input: 1.00, output: 3.00 },
  'default': { input: 2.00, output: 6.00 },
};

// ── Provider detection ────────────────────────

function detectProvider(model: string): string {
  const m = model.toLowerCase();
  if (m.startsWith('gpt') || m.startsWith('o1') || m.startsWith('o3') || m.includes('davinci') || m.includes('ada')) return 'openai';
  if (m.startsWith('claude') || m.includes('anthropic')) return 'anthropic';
  if (m.startsWith('gemini') || m.includes('google')) return 'google';
  if (m.startsWith('mistral') || m.includes('mixtral')) return 'mistral';
  if (m.startsWith('llama') || m.includes('llama')) return 'meta';
  if (m.startsWith('deepseek')) return 'deepseek';
  return 'unknown';
}

// ── Cost calculation ──────────────────────────

/**
 * Calculate the dollar cost of an LLM call based on model and token usage.
 * Uses the pricing table if available; falls back to provider defaults.
 */
export function calculateCost(model: string, usage: TokenUsage): number {
  const pricing = MODEL_PRICING[model] ?? PROVIDER_DEFAULT_PRICING[detectProvider(model)] ?? PROVIDER_DEFAULT_PRICING['default'];
  const inputCost = (usage.prompt_tokens / 1_000_000) * pricing.input;
  const outputCost = (usage.completion_tokens / 1_000_000) * pricing.output;
  return inputCost + outputCost;
}

// ── LLM event parsing ─────────────────────────

/**
 * Try to parse an EventRecord as an LLM plugin call.
 * Returns null if the event is not an LLM call or lacks parseable data.
 */
export function tryParseLlmEvent(ev: EventRecord): LlmCallInfo | null {
  if (ev.type !== 'call') return null;
  if (ev.service !== 'llm') return null;
  if (!ev.response) return null;

  const funcName = ev.op || 'chat';
  if (funcName !== 'chat' && funcName !== 'embed') return null;

  let resp: any;
  try {
    resp = JSON.parse(ev.response);
  } catch {
    return null;
  }

  if (funcName === 'embed') {
    const totalTokens = safeNum(resp, 'total_tokens');
    if (!totalTokens) return null;
    const usage: TokenUsage = {
      prompt_tokens: totalTokens,
      completion_tokens: 0,
      total_tokens: totalTokens,
    };
    return {
      step: ev.step,
      model: '',
      provider: detectProvider(resp.model || ''),
      function_name: funcName,
      usage,
      cost: 0,
    };
  }

  // Chat
  const model = resp.model || '';
  const provider = detectProvider(model);
  const promptTokens = safeNum(resp, 'prompt_tokens');
  const completionTokens = safeNum(resp, 'completion_tokens');
  const totalTokens = safeNum(resp, 'total_tokens') || promptTokens + completionTokens;

  if (!promptTokens && !completionTokens && !totalTokens) return null;

  const usage: TokenUsage = {
    prompt_tokens: promptTokens || 0,
    completion_tokens: completionTokens || 0,
    total_tokens: totalTokens || (promptTokens || 0) + (completionTokens || 0),
  };

  // Server may provide cost in micro-dollars (field "cost" as integer micro-dollars).
  // If present and positive, convert to dollars; otherwise calculate from pricing table.
  const serverCostMicro = safeNum(resp, 'cost');
  let cost: number;
  if (serverCostMicro && serverCostMicro > 0) {
    cost = serverCostMicro / 1_000_000;
  } else {
    cost = calculateCost(model, usage);
  }

  return { step: ev.step, model, provider, function_name: funcName, usage, cost };
}

// ── Cost breakdown from events ─────────────────

/**
 * Extract a full cost breakdown from an array of workflow event records.
 */
export function extractCostFromEvents(events: EventRecord[]): CostBreakdown {
  const byModel: Record<string, { tokens: TokenUsage; cost: number; calls: number }> = {};
  const byProvider: Record<string, { tokens: TokenUsage; cost: number; calls: number }> = {};
  let totalCost = 0;
  let totalPromptTokens = 0;
  let totalCompletionTokens = 0;
  let totalTokens = 0;
  let llmCalls = 0;

  for (const ev of events) {
    const info = tryParseLlmEvent(ev);
    if (!info) continue;

    llmCalls++;
    totalCost += info.cost;
    totalPromptTokens += info.usage.prompt_tokens;
    totalCompletionTokens += info.usage.completion_tokens;
    totalTokens += info.usage.total_tokens;

    // Per-model
    const modelKey = info.model || 'unknown';
    if (!byModel[modelKey]) {
      byModel[modelKey] = { tokens: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 }, cost: 0, calls: 0 };
    }
    byModel[modelKey].tokens.prompt_tokens += info.usage.prompt_tokens;
    byModel[modelKey].tokens.completion_tokens += info.usage.completion_tokens;
    byModel[modelKey].tokens.total_tokens += info.usage.total_tokens;
    byModel[modelKey].cost += info.cost;
    byModel[modelKey].calls++;

    // Per-provider
    const provKey = info.provider || 'unknown';
    if (!byProvider[provKey]) {
      byProvider[provKey] = { tokens: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 }, cost: 0, calls: 0 };
    }
    byProvider[provKey].tokens.prompt_tokens += info.usage.prompt_tokens;
    byProvider[provKey].tokens.completion_tokens += info.usage.completion_tokens;
    byProvider[provKey].tokens.total_tokens += info.usage.total_tokens;
    byProvider[provKey].cost += info.cost;
    byProvider[provKey].calls++;
  }

  return {
    byModel,
    byProvider,
    totalCost,
    totalTokens: { prompt_tokens: totalPromptTokens, completion_tokens: totalCompletionTokens, total_tokens: totalTokens },
    llmCalls,
  };
}

// ── Workflow cost aggregation ─────────────────

/**
 * Aggregate cost data across multiple workflows with their event histories.
 */
export function aggregateWorkflowCosts(
  workflows: WorkflowInstance[],
  eventsMap: Record<string, EventRecord[]>
): WorkflowCost[] {
  return workflows.map((wf) => {
    const wfEvents = eventsMap[wf.id] || [];
    const cost = extractCostFromEvents(wfEvents);
    return {
      workflowId: wf.id,
      workflowType: wf.def_name,
      status: wf.status,
      totalCost: cost.totalCost,
      totalTokens: cost.totalTokens,
      llmCalls: cost.llmCalls,
      startedAt: wf.created_at,
    };
  });
}

// ── Helpers ───────────────────────────────────

function safeNum(obj: any, key: string): number {
  const v = obj[key];
  if (typeof v === 'number') return v;
  const n = Number(v);
  return isNaN(n) ? 0 : n;
}

/** Format a dollar amount for display. */
export function formatCost(cost: number): string {
  if (cost < 0.0001) return '$0.00';
  if (cost < 0.01) return `$${cost.toFixed(5)}`;
  if (cost < 1) return `$${cost.toFixed(4)}`;
  if (cost < 100) return `$${cost.toFixed(2)}`;
  return `$${cost.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

/** Format a number of tokens for display (e.g., "1,234"). */
export function formatTokens(n: number): string {
  return n.toLocaleString();
}
