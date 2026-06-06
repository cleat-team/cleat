<script lang="ts">
  import Sidebar from './components/Sidebar.svelte';
  import Dashboard from './pages/Dashboard.svelte';
  import WorkflowList from './pages/WorkflowList.svelte';
  import WorkflowDetail from './pages/WorkflowDetail.svelte';
  import ScheduleManagement from './pages/ScheduleManagement.svelte';
  import Definitions from './pages/Definitions.svelte';
  import DeadLetters from './pages/DeadLetters.svelte';
  import WorkflowCompare from './pages/WorkflowCompare.svelte';
  import { getAPIKey, setAPIKey, clearAPIKey } from './lib/api';

  let route = $state(window.location.hash.slice(1) || 'dashboard');
  let routeParams = $state('');
  let apiKey = $state(getAPIKey());
  let keyInput = $state('');

  function handleSetKey() {
    const trimmed = keyInput.trim();
    if (trimmed) {
      setAPIKey(trimmed);
      apiKey = trimmed;
      keyInput = '';
    }
  }

  function handleClearKey() {
    clearAPIKey();
    apiKey = '';
  }

  function navigate(path: string) {
    const [base, ...rest] = path.split('/');
    window.location.hash = path;
    route = base;
    routeParams = rest.join('/');
  }

  window.addEventListener('hashchange', () => {
    const hash = window.location.hash.slice(1);
    const [base, ...rest] = hash.split('/');
    route = base;
    routeParams = rest.join('/');
  });
</script>

{#if !apiKey}
  <div class="auth-screen">
    <div class="auth-card">
      <h1>Cleat</h1>
      <p>Enter your cleat API key to access the dashboard.</p>
      <form onsubmit={(e) => { e.preventDefault(); handleSetKey(); }}>
        <input
          type="password"
          placeholder="cleat_sk_..."
          bind:value={keyInput}
          class="key-input"
          autofocus
        />
        <button type="submit" class="key-btn" disabled={!keyInput.trim()}>Connect</button>
      </form>
      <p class="auth-hint">
        The key is stored in your browser's local storage and sent as <code>X-Cleat-API-Key</code> on every request.
      </p>
    </div>
  </div>
{:else}
  <div class="app-layout">
    <Sidebar active={route} onNavigate={navigate} {apiKey} onClearKey={handleClearKey} />
    <main class="main-content">
      {#if route === 'dashboard'}
        <Dashboard />
      {:else if route === 'workflows' && !routeParams}
        <WorkflowList onNavigate={navigate} />
      {:else if route === 'workflows'}
        <WorkflowDetail workflowId={routeParams} onNavigate={navigate} />
      {:else if route === 'schedules'}
        <ScheduleManagement />
      {:else if route === 'definitions'}
        <Definitions />
      {:else if route === 'dead-letters'}
        <DeadLetters onNavigate={navigate} />
      {:else if route === 'compare'}
        {@const parts = routeParams.split('/')}
        {@const idA = parts[0] || ''}
        {@const idB = parts[1] || ''}
        {#if idA && idB}
          <WorkflowCompare workflowIdA={idA} workflowIdB={idB} onNavigate={navigate} />
        {:else}
          <p style="color: var(--color-text-muted);">Invalid compare route. Use #compare/{id1}/{id2}</p>
        {/if}
      {:else}
        <Dashboard />
      {/if}
    </main>
  </div>
{/if}

<style>
  .auth-screen {
    display: flex;
    justify-content: center;
    align-items: center;
    min-height: 100vh;
    background: var(--color-bg);
  }
  .auth-card {
    background: var(--color-white);
    border-radius: var(--radius);
    box-shadow: 0 2px 12px rgba(0,0,0,0.15);
    padding: 2.5rem;
    width: 100%;
    max-width: 440px;
    text-align: center;
  }
  .auth-card h1 {
    font-size: 1.5rem;
    margin-bottom: 0.5rem;
    color: var(--color-text);
  }
  .auth-card p {
    color: var(--color-text-muted);
    margin-bottom: 1.25rem;
    font-size: 0.9rem;
  }
  .auth-card form {
    display: flex;
    gap: 0.5rem;
  }
  .key-input {
    flex: 1;
    padding: 0.6rem 0.75rem;
    border: 1px solid var(--color-border);
    border-radius: var(--radius);
    font-size: 0.9rem;
    font-family: monospace;
  }
  .key-input:focus {
    outline: none;
    border-color: var(--color-primary);
    box-shadow: 0 0 0 2px rgba(67, 97, 238, 0.2);
  }
  .key-btn {
    padding: 0.6rem 1.25rem;
    background: var(--color-primary);
    color: var(--color-white);
    border: none;
    border-radius: var(--radius);
    font-size: 0.9rem;
    cursor: pointer;
    white-space: nowrap;
  }
  .key-btn:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .key-btn:hover:not(:disabled) {
    filter: brightness(1.1);
  }
  .auth-hint {
    margin-top: 1rem;
    font-size: 0.8rem;
    color: var(--color-text-muted);
  }
  .auth-hint code {
    background: #f0f0f0;
    padding: 0.15rem 0.35rem;
    border-radius: 3px;
    font-size: 0.8rem;
  }
</style>
