import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
  plugins: [svelte()],
  base: '/',
  build: {
    // `cmd/cleat-worker`, not `cmd/durable-worker`. The latter does not exist
    // anywhere in the repo -- `npm run build` created it, wrote the bundle
    // into it, and left the real one untouched. `.goreleaser.yml` and the
    // Dockerfile both embed `cmd/cleat-worker/web/dist/**`, so a build of this
    // directory has not been reaching the shipped binary.
    //
    // Reintroduced by the revert in fbaf7506 (2026-06-06) after 14dec5e8 had
    // corrected it, which is the same date the committed bundle was last
    // touched -- see the note in ci.yml's `web` job about that bundle still
    // being stale, which this change does not by itself fix.
    outDir: '../cmd/cleat-worker/web/dist',
    emptyOutDir: true,
  },
});
