<script lang="ts">
  let { active, onNavigate, apiKey = '', onClearKey }: {
    active: string;
    onNavigate: (path: string) => void;
    apiKey?: string;
    onClearKey?: () => void;
  } = $props();

  let showKey = $state(false);
</script>

<aside class="sidebar">
  <h2>Cleat</h2>
  <nav>
    <a href="#dashboard" class:active={active === 'dashboard'} onclick={() => onNavigate('dashboard')}>Dashboard</a>
    <a href="#workflows" class:active={active === 'workflows'} onclick={() => onNavigate('workflows')}>Workflows</a>
    <a href="#schedules" class:active={active === 'schedules'} onclick={() => onNavigate('schedules')}>Schedules</a>
    <a href="#definitions" class:active={active === 'definitions'} onclick={() => onNavigate('definitions')}>Definitions</a>
    <a href="#dead-letters" class:active={active === 'dead-letters'} onclick={() => onNavigate('dead-letters')}>Dead Letters</a>
  </nav>
  <div class="sidebar-footer">
    {#if showKey}
      <div class="key-display">
        <code>{apiKey.slice(0, 20)}&hellip;</code>
        <button onclick={onClearKey} title="Remove API key">&#10005;</button>
        <button onclick={() => showKey = false} title="Hide">&#9664;</button>
      </div>
    {:else}
      <button class="key-toggle" onclick={() => showKey = true} title="Show API key">
        &#128273; Key
      </button>
    {/if}
  </div>
</aside>

<style>
  .sidebar-footer {
    position: absolute;
    bottom: 0;
    left: 0;
    right: 0;
    padding: 0.75rem 1rem;
    border-top: 1px solid rgba(255,255,255,0.1);
    font-size: 0.8rem;
  }
  .key-toggle {
    background: none;
    border: 1px solid rgba(255,255,255,0.2);
    color: #a0a0b8;
    padding: 0.3rem 0.6rem;
    border-radius: 4px;
    cursor: pointer;
    font-size: 0.8rem;
  }
  .key-toggle:hover {
    color: #fff;
    border-color: rgba(255,255,255,0.4);
  }
  .key-display {
    display: flex;
    align-items: center;
    gap: 0.35rem;
  }
  .key-display code {
    font-size: 0.7rem;
    color: #a0a0b8;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    flex: 1;
  }
  .key-display button {
    background: none;
    border: none;
    color: #a0a0b8;
    cursor: pointer;
    font-size: 0.8rem;
    padding: 0.15rem 0.3rem;
  }
  .key-display button:hover {
    color: #fff;
  }
</style>
