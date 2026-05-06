<script lang="ts">
  import type { EventRecord } from '../lib/types';

  let { events }: { events: EventRecord[] } = $props();

  function itemClass(type: string) {
    if (type === 'sleep') return 'type-sleep';
    if (type === 'await_signals' || type === 'signal_received') return 'type-signal';
    if (type === 'call' && events.some(e => e.err)) return 'type-error';
    return '';
  }

  function eventLabel(ev: EventRecord): string {
    switch (ev.type) {
      case 'call': return ev.service ? `${ev.service}.${ev.op}` : 'call';
      case 'sleep': return `Sleep ${ev.duration_ms}ms`;
      case 'await_signals': return `Await: ${ev.signal_names}`;
      case 'signal_received': return `Signal: ${ev.signal_name}`;
      case 'defer': return `Defer: ${ev.defer_description}`;
      case 'child_workflow': return `Child: ${ev.child_name}`;
      case 'await_child': return `Await child: ${ev.run_id}`;
      case 'continue_as_new': return 'Continue as New';
      default: return ev.type;
    }
  }
</script>

<div class="timeline">
  {#each events as ev (ev.step)}
    <div class="timeline-item {itemClass(ev.type)}">
      <div class="event-type">{ev.type}</div>
      <div class="event-detail">{eventLabel(ev)}</div>
      {#if ev.request}
        <div class="event-meta">Request: {ev.request.slice(0, 100)}{ev.request.length > 100 ? '...' : ''}</div>
      {/if}
      {#if ev.response}
        <div class="event-meta">Response: {ev.response.slice(0, 100)}{ev.response.length > 100 ? '...' : ''}</div>
      {/if}
      {#if ev.err}
        <div class="event-meta" style="color: var(--color-danger)">Error: {ev.err}</div>
      {/if}
    </div>
  {/each}
  {#if events.length === 0}
    <p style="color: var(--color-text-muted); font-size: 0.85rem;">No events recorded.</p>
  {/if}
</div>
