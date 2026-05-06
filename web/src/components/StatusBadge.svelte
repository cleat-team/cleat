<script lang="ts">
  let { status }: { status: string } = $props();
  function badgeClass(s: string) {
    switch (s) {
      case 'ready': return 'badge-ready';
      case 'running': return 'badge-running';
      case 'completed': return 'badge-completed';
      case 'failed': return 'badge-failed';
      case 'dead_letter': return 'badge-dead_letter';
      default: return '';
    }
  }
</script>

<span class="badge {badgeClass(status)}">{status}</span>
