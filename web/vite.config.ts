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
    // corrected it. ci.yml's `Web Dashboard` job builds into this directory
    // and fails if the result differs from what is committed (cleat#3468), so
    // after changing anything under web/, including web/package-lock.json,
    // run `npm run build` here and commit cmd/cleat-worker/web/dist.
    outDir: '../cmd/cleat-worker/web/dist',
    emptyOutDir: true,
  },
});
