<script lang="ts">
  import { getToken, setToken } from '../lib/auth';

  let { onClose }: { onClose: () => void } = $props();

  let value = $state(getToken());
  let saved = $state(false);

  function save() {
    setToken(value.trim());
    saved = true;
  }

  function clear() {
    setToken('');
    value = '';
    saved = false;
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
<div class="modal-backdrop" onclick={onClose}>
  <!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
  <div class="modal" onclick={(e) => e.stopPropagation()}>
    <h3>API Key</h3>
    <p style="color: var(--color-text-muted); font-size: 0.85rem;">
      This worker requires an API key on every request (<code>--require-auth</code>,
      the default). Paste the key printed to the worker's startup log, or one
      generated with <code>cleat-worker --generate-api-key &lt;tenant-uuid&gt;</code>.
      It is stored only in this browser (localStorage) and sent as
      <code>Authorization: Bearer &lt;key&gt;</code>.
    </p>
    <div class="form-group">
      <label for="api-key-input">API Key</label>
      <input id="api-key-input" type="password" autocomplete="off" bind:value placeholder="cleat_sk_..." />
    </div>
    {#if saved}
      <p style="color: var(--color-success); font-size: 0.85rem;">Saved.</p>
    {/if}
    <div style="display:flex; gap: 0.5rem; justify-content: flex-end;">
      <button class="btn btn-sm" style="background:#eee;" onclick={clear}>Clear</button>
      <button class="btn btn-sm" style="background:#eee;" onclick={onClose}>Close</button>
      <button class="btn btn-primary btn-sm" onclick={save}>Save</button>
    </div>
  </div>
</div>
