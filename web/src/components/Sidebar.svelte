<script lang="ts">
  let { active, onNavigate }: { active: string; onNavigate: (path: string) => void } = $props();
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
</aside>
