<script lang="ts">
  import Sidebar from './components/Sidebar.svelte';
  import Dashboard from './pages/Dashboard.svelte';
  import WorkflowList from './pages/WorkflowList.svelte';
  import WorkflowDetail from './pages/WorkflowDetail.svelte';
  import ScheduleManagement from './pages/ScheduleManagement.svelte';

  let route = $state(window.location.hash.slice(1) || 'dashboard');
  let routeParams = $state('');

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

<div class="app-layout">
  <Sidebar active={route} onNavigate={navigate} />
  <main class="main-content">
    {#if route === 'dashboard'}
      <Dashboard />
    {:else if route === 'workflows' && !routeParams}
      <WorkflowList onNavigate={navigate} />
    {:else if route === 'workflows'}
      <WorkflowDetail workflowId={routeParams} onNavigate={navigate} />
    {:else if route === 'schedules'}
      <ScheduleManagement />
    {:else}
      <Dashboard />
    {/if}
  </main>
</div>
