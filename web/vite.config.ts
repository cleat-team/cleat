import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
  plugins: [svelte()],
  base: '/',
  build: {
    outDir: '../cmd/durable-worker/web/dist',
    emptyOutDir: true,
  },
});
