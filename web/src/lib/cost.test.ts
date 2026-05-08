import { describe, it, expect } from 'vitest';
import {
  calculateCost,
  formatCost,
  formatTokens,
  tryParseLlmEvent,
  extractCostFromEvents,
  aggregateWorkflowCosts,
} from './cost';
import type { EventRecord, TokenUsage, WorkflowInstance } from './types';

// ---------------------------------------------------------------------------
// formatCost
// ---------------------------------------------------------------------------
describe('formatCost', () => {
  it('formats zero as $0.00', () => {
    expect(formatCost(0)).toBe('$0.00');
  });

  it('formats negative values as $0.00', () => {
    expect(formatCost(-0.01)).toBe('$0.00');
  });

  it('formats costs below 0.0001 as $0.00', () => {
    expect(formatCost(0.00001)).toBe('$0.00');
    expect(formatCost(0.00009)).toBe('$0.00');
  });

  it('formats very small costs with 5 decimal places', () => {
    expect(formatCost(0.0001)).toBe('$0.00010');
    expect(formatCost(0.001)).toBe('$0.00100');
    expect(formatCost(0.00999)).toBe('$0.00999');
  });

  it('formats costs between 0.01 and 1 with 4 decimal places', () => {
    expect(formatCost(0.01)).toBe('$0.0100');
    expect(formatCost(0.50)).toBe('$0.5000');
    expect(formatCost(0.9999)).toBe('$0.9999');
  });

  it('formats costs between 1 and 100 with 2 decimal places', () => {
    expect(formatCost(1)).toBe('$1.00');
    expect(formatCost(42.5)).toBe('$42.50');
    expect(formatCost(99.99)).toBe('$99.99');
  });

  it('formats costs >= 100 with locale formatting', () => {
    expect(formatCost(100)).toBe('$100.00');
    expect(formatCost(1000)).toBe('$1,000.00');
    expect(formatCost(1234567.89)).toBe('$1,234,567.89');
  });

  it('rounds 0.009999 to $0.01000 via toFixed(5)', () => {
    // 0.009999.toFixed(5) rounds up to '0.01000'
    expect(formatCost(0.009999)).toBe('$0.01000');
  });

  it('handles boundary value 0.01 correctly', () => {
    // Exactly 0.01 — should use 4 decimal places
    expect(formatCost(0.01)).toBe('$0.0100');
  });
});

// ---------------------------------------------------------------------------
// formatTokens
// ---------------------------------------------------------------------------
describe('formatTokens', () => {
  it('formats zero', () => {
    expect(formatTokens(0)).toBe('0');
  });

  it('formats small numbers without commas', () => {
    expect(formatTokens(1)).toBe('1');
    expect(formatTokens(100)).toBe('100');
    expect(formatTokens(999)).toBe('999');
  });

  it('formats numbers >= 1000 with commas', () => {
    expect(formatTokens(1000)).toBe('1,000');
    expect(formatTokens(1000000)).toBe('1,000,000');
    expect(formatTokens(123456789)).toBe('123,456,789');
  });
});

// ---------------------------------------------------------------------------
// calculateCost
// ---------------------------------------------------------------------------
describe('calculateCost', () => {
  function usage(prompt: number, completion: number): TokenUsage {
    return {
      prompt_tokens: prompt,
      completion_tokens: completion,
      total_tokens: prompt + completion,
    };
  }

  it('calculates input-only cost for a known model', () => {
    // gpt-4o: $2.50 / 1M input tokens
    const cost = calculateCost('gpt-4o', usage(1_000_000, 0));
    expect(cost).toBeCloseTo(2.5, 5);
  });

  it('calculates output-only cost for a known model', () => {
    // gpt-4o: $10.00 / 1M output tokens
    const cost = calculateCost('gpt-4o', usage(0, 100_000));
    expect(cost).toBeCloseTo(1.0, 5);
  });

  it('calculates combined input and output cost', () => {
    // gpt-4o: $2.50 input, $10.00 output per 1M tokens
    // 500K input = $1.25, 100K output = $1.00 => total $2.25
    const cost = calculateCost('gpt-4o', usage(500_000, 100_000));
    expect(cost).toBeCloseTo(2.25, 5);
  });

  it('uses provider-default pricing for unknown models with known provider prefix', () => {
    // Starts with "gpt-" => openai default: $3.00 input / $12.00 output
    const cost = calculateCost('gpt-unknown-model', usage(1_000_000, 0));
    expect(cost).toBeCloseTo(3.0, 5);
  });

  it('uses anthropic provider default for claude- prefixed unknown models', () => {
    // anthropic default: $8.00 input / $24.00 output
    const cost = calculateCost('claude-sonnet-5', usage(1_000_000, 500_000));
    // $8.00 + $12.00 = $20.00
    expect(cost).toBeCloseTo(20.0, 5);
  });

  it('uses fallback default pricing for completely unknown providers', () => {
    // default: $2.00 input / $6.00 output
    const cost = calculateCost('completely-unknown-model', usage(1_000_000, 1_000_000));
    expect(cost).toBeCloseTo(8.0, 5);
  });

  it('returns 0 for zero tokens', () => {
    const cost = calculateCost('gpt-4o', usage(0, 0));
    expect(cost).toBe(0);
  });

  it('calculates using specific model pricing for known model name', () => {
    // gpt-4o-mini: $0.15 input / $0.60 output
    const cost = calculateCost('gpt-4o-mini', usage(2_000_000, 1_000_000));
    // $0.30 + $0.60 = $0.90
    expect(cost).toBeCloseTo(0.9, 5);
  });

  it('calculates using gemini pricing', () => {
    // gemini-2.5-flash: $0.15 input / $0.60 output
    const cost = calculateCost('gemini-2.5-flash', usage(1_000_000, 500_000));
    // $0.15 + $0.30 = $0.45
    expect(cost).toBeCloseTo(0.45, 5);
  });
});

// ---------------------------------------------------------------------------
// tryParseLlmEvent
// ---------------------------------------------------------------------------
describe('tryParseLlmEvent', () => {
  it('returns null for non-call events', () => {
    expect(tryParseLlmEvent({ step: 1, type: 'sleep' })).toBeNull();
  });

  it('returns null for non-llm service events', () => {
    expect(
      tryParseLlmEvent({ step: 1, type: 'call', service: 'http' })
    ).toBeNull();
  });

  it('returns null for llm events without response', () => {
    expect(
      tryParseLlmEvent({ step: 1, type: 'call', service: 'llm' })
    ).toBeNull();
  });

  it('returns null for unparseable (non-JSON) response', () => {
    const ev: EventRecord = {
      step: 1,
      type: 'call',
      service: 'llm',
      response: 'not-json',
    };
    expect(tryParseLlmEvent(ev)).toBeNull();
  });

  it('returns null for non-chat/non-embed function names', () => {
    const ev: EventRecord = {
      step: 1,
      type: 'call',
      service: 'llm',
      op: 'translate',
      response: JSON.stringify({ prompt_tokens: 100, completion_tokens: 50 }),
    };
    expect(tryParseLlmEvent(ev)).toBeNull();
  });

  it('parses a valid chat event with all fields', () => {
    const ev: EventRecord = {
      step: 5,
      type: 'call',
      service: 'llm',
      op: 'chat',
      response: JSON.stringify({
        model: 'gpt-4o',
        prompt_tokens: 1000,
        completion_tokens: 500,
        total_tokens: 1500,
      }),
    };
    const result = tryParseLlmEvent(ev);
    expect(result).not.toBeNull();
    expect(result!.step).toBe(5);
    expect(result!.model).toBe('gpt-4o');
    expect(result!.provider).toBe('openai');
    expect(result!.function_name).toBe('chat');
    expect(result!.usage.prompt_tokens).toBe(1000);
    expect(result!.usage.completion_tokens).toBe(500);
    expect(result!.usage.total_tokens).toBe(1500);
    // gpt-4o: $2.50/1M input, $10.00/1M output
    // (1000/1M)*2.50 + (500/1M)*10.00 = 0.0025 + 0.005 = 0.0075
    expect(result!.cost).toBeCloseTo(0.0075, 6);
  });

  it('uses server-provided cost in micro-dollars when present', () => {
    const ev: EventRecord = {
      step: 2,
      type: 'call',
      service: 'llm',
      op: 'chat',
      response: JSON.stringify({
        model: 'gpt-4o',
        prompt_tokens: 100,
        completion_tokens: 50,
        cost: 5000, // $0.005 in micro-dollars
      }),
    };
    const result = tryParseLlmEvent(ev);
    expect(result).not.toBeNull();
    expect(result!.cost).toBeCloseTo(0.005, 6);
  });

  it('defaults model to empty string when absent from response', () => {
    const ev: EventRecord = {
      step: 1,
      type: 'call',
      service: 'llm',
      op: 'chat',
      response: JSON.stringify({ prompt_tokens: 100, completion_tokens: 50 }),
    };
    const result = tryParseLlmEvent(ev);
    expect(result!.model).toBe('');
  });

  it('returns null when no token data is present', () => {
    const ev: EventRecord = {
      step: 1,
      type: 'call',
      service: 'llm',
      op: 'chat',
      response: JSON.stringify({ model: 'gpt-4o' }),
    };
    expect(tryParseLlmEvent(ev)).toBeNull();
  });

  it('parses an embed event', () => {
    const ev: EventRecord = {
      step: 3,
      type: 'call',
      service: 'llm',
      op: 'embed',
      response: JSON.stringify({
        model: 'text-embedding-ada-002',
        total_tokens: 500,
      }),
    };
    const result = tryParseLlmEvent(ev);
    expect(result).not.toBeNull();
    expect(result!.function_name).toBe('embed');
    expect(result!.usage.prompt_tokens).toBe(500);
    expect(result!.usage.completion_tokens).toBe(0);
    expect(result!.usage.total_tokens).toBe(500);
    expect(result!.cost).toBe(0);
  });

  it('returns null for embed event without total_tokens', () => {
    const ev: EventRecord = {
      step: 3,
      type: 'call',
      service: 'llm',
      op: 'embed',
      response: JSON.stringify({ model: 'some-model' }),
    };
    expect(tryParseLlmEvent(ev)).toBeNull();
  });

  it('defaults op to "chat" when absent', () => {
    const ev: EventRecord = {
      step: 1,
      type: 'call',
      service: 'llm',
      response: JSON.stringify({ prompt_tokens: 100, completion_tokens: 50 }),
    };
    const result = tryParseLlmEvent(ev);
    expect(result).not.toBeNull();
    expect(result!.function_name).toBe('chat');
  });

  it('detects provider correctly from model name', () => {
    const ev: EventRecord = {
      step: 1,
      type: 'call',
      service: 'llm',
      op: 'chat',
      response: JSON.stringify({
        model: 'claude-sonnet-4-6',
        prompt_tokens: 1000,
        completion_tokens: 500,
      }),
    };
    const result = tryParseLlmEvent(ev);
    expect(result!.provider).toBe('anthropic');
  });
});

// ---------------------------------------------------------------------------
// extractCostFromEvents
// ---------------------------------------------------------------------------
describe('extractCostFromEvents', () => {
  it('returns empty breakdown for empty events array', () => {
    const result = extractCostFromEvents([]);
    expect(result.llmCalls).toBe(0);
    expect(result.totalCost).toBe(0);
    expect(result.totalTokens.prompt_tokens).toBe(0);
    expect(result.totalTokens.completion_tokens).toBe(0);
    expect(result.totalTokens.total_tokens).toBe(0);
    expect(result.byModel).toEqual({});
    expect(result.byProvider).toEqual({});
  });

  it('aggregates multiple LLM events correctly', () => {
    const events: EventRecord[] = [
      {
        step: 1,
        type: 'call',
        service: 'llm',
        op: 'chat',
        response: JSON.stringify({
          model: 'gpt-4o',
          prompt_tokens: 1_000_000,
          completion_tokens: 0,
        }),
      },
      {
        step: 2,
        type: 'call',
        service: 'llm',
        op: 'chat',
        response: JSON.stringify({
          model: 'gpt-4o-mini',
          prompt_tokens: 1_000_000,
          completion_tokens: 0,
        }),
      },
    ];

    const result = extractCostFromEvents(events);
    expect(result.llmCalls).toBe(2);
    // gpt-4o: $2.50 + gpt-4o-mini: $0.15
    expect(result.totalCost).toBeCloseTo(2.65, 4);
    expect(result.totalTokens.total_tokens).toBe(2_000_000);
    expect(Object.keys(result.byModel)).toHaveLength(2);
    expect(Object.keys(result.byProvider)).toHaveLength(1); // both openai
    expect(result.byProvider['openai'].calls).toBe(2);
    expect(result.byProvider['openai'].cost).toBeCloseTo(2.65, 4);
  });

  it('skips non-LLM events', () => {
    const events: EventRecord[] = [
      { step: 1, type: 'sleep' },
      {
        step: 2,
        type: 'call',
        service: 'llm',
        op: 'chat',
        response: JSON.stringify({ prompt_tokens: 100, completion_tokens: 50 }),
      },
      { step: 3, type: 'call', service: 'http', op: 'get' },
    ];
    const result = extractCostFromEvents(events);
    expect(result.llmCalls).toBe(1);
    expect(result.totalTokens.total_tokens).toBe(150);
  });

  it('groups by model correctly', () => {
    const events: EventRecord[] = [
      {
        step: 1,
        type: 'call',
        service: 'llm',
        op: 'chat',
        response: JSON.stringify({
          model: 'gpt-4o',
          prompt_tokens: 500_000,
          completion_tokens: 0,
        }),
      },
      {
        step: 2,
        type: 'call',
        service: 'llm',
        op: 'chat',
        response: JSON.stringify({
          model: 'gpt-4o',
          prompt_tokens: 500_000,
          completion_tokens: 0,
        }),
      },
    ];
    const result = extractCostFromEvents(events);
    expect(Object.keys(result.byModel)).toEqual(['gpt-4o']);
    expect(result.byModel['gpt-4o'].calls).toBe(2);
    // (500K + 500K) / 1M * $2.50 = $2.50
    expect(result.byModel['gpt-4o'].cost).toBeCloseTo(2.5, 4);
  });

  it('categorizes model-less entries under "unknown" key', () => {
    const events: EventRecord[] = [
      {
        step: 1,
        type: 'call',
        service: 'llm',
        op: 'chat',
        response: JSON.stringify({ prompt_tokens: 100, completion_tokens: 50 }),
      },
    ];
    const result = extractCostFromEvents(events);
    // model is '' so the fallback key is 'unknown'
    expect(result.byModel['unknown']).toBeDefined();
    expect(result.byModel['unknown'].calls).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// aggregateWorkflowCosts
// ---------------------------------------------------------------------------
describe('aggregateWorkflowCosts', () => {
  const sampleWorkflow = (id: string, status = 'completed'): WorkflowInstance => ({
    id,
    def_name: 'test-workflow',
    def_version: 1,
    status,
    input: '',
    result: '',
    error: '',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T01:00:00Z',
    assigned_to: '',
    next_wake_at: '',
    namespace: 'default',
  });

  it('aggregates costs across workflows', () => {
    const workflows = [sampleWorkflow('wf-1')];
    const eventsMap: Record<string, EventRecord[]> = {
      'wf-1': [
        {
          step: 1,
          type: 'call',
          service: 'llm',
          op: 'chat',
          response: JSON.stringify({
            model: 'gpt-4o',
            prompt_tokens: 1_000_000,
            completion_tokens: 0,
          }),
        },
      ],
    };

    const result = aggregateWorkflowCosts(workflows, eventsMap);
    expect(result).toHaveLength(1);
    expect(result[0].workflowId).toBe('wf-1');
    expect(result[0].workflowType).toBe('test-workflow');
    expect(result[0].status).toBe('completed');
    expect(result[0].totalCost).toBeCloseTo(2.5, 5);
    expect(result[0].llmCalls).toBe(1);
    expect(result[0].totalTokens.total_tokens).toBe(1_000_000);
    expect(result[0].startedAt).toBe('2024-01-01T00:00:00Z');
  });

  it('returns zero costs for workflows without events', () => {
    const workflows = [sampleWorkflow('wf-empty', 'running')];
    const result = aggregateWorkflowCosts(workflows, { 'wf-empty': [] });
    expect(result[0].totalCost).toBe(0);
    expect(result[0].llmCalls).toBe(0);
    expect(result[0].totalTokens.total_tokens).toBe(0);
  });

  it('handles missing events map entries gracefully', () => {
    const workflows = [sampleWorkflow('wf-missing')];
    const result = aggregateWorkflowCosts(workflows, {});
    expect(result[0].totalCost).toBe(0);
    expect(result[0].llmCalls).toBe(0);
  });

  it('handles empty workflows array', () => {
    const result = aggregateWorkflowCosts([], {});
    expect(result).toEqual([]);
  });
});
