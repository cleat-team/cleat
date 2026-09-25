<script lang="ts">
  let { status }: { status: string } = $props();

  // The eight values of workflow_instances.status. Until 2026-09-25 this
  // switch matched 'completed' and 'dead_letter', neither of which is a status
  // the engine writes -- so a finished run (`done`) and a dead-lettered one
  // (`dead_lettered`) both fell through to the unstyled default, and every
  // terminated/cancelled/terminating run did too. The class names keep their
  // historical spelling (badge-completed, badge-dead_letter) because
  // ScheduleManagement.svelte reuses `badge-completed` for a different meaning
  // (an enabled schedule); only the status strings were wrong.
  function badgeClass(s: string) {
    switch (s) {
      case 'ready': return 'badge-ready';
      case 'running': return 'badge-running';
      case 'done': return 'badge-completed';
      case 'failed': return 'badge-failed';
      case 'dead_lettered': return 'badge-dead_letter';
      case 'terminated': return 'badge-terminated';
      case 'cancelled': return 'badge-cancelled';
      case 'terminating': return 'badge-terminating';
      default: return '';
    }
  }
</script>

<span class="badge {badgeClass(status)}">{status}</span>
