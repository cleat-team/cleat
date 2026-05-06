<script lang="ts">
  let { count, label, color = '#4361ee' }: { count: number; label: string; color?: string } = $props();
</script>

<div class="summary-card">
  <div class="count" style="color: {color}">{count}</div>
  <div class="label">{label}</div>
</div>
