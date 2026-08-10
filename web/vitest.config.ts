import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import path from 'path';
import { fileURLToPath } from 'url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  plugins: [svelte()],
  resolve: {
    alias: [
      // Force exact imports of 'svelte' (not svelte/compiler, etc.) to the
      // client build so that @testing-library/svelte can use mount() / unmount().
      { find: /^svelte$/, replacement: path.resolve(__dirname, 'node_modules/svelte/src/index-client.js') },
    ],
  },
  test: {
    environment: 'jsdom',
    // jsdom's default document origin is `about:blank`, which the Web
    // Storage spec treats as opaque -- `window.localStorage` is present but
    // every call throws a SecurityError there. lib/auth.ts (the API key
    // storage backing the dashboard's Authorization header, see B5 in
    // WORKSTREAM.md) needs a real, working localStorage in tests, so give
    // jsdom a concrete origin.
    environmentOptions: {
      jsdom: {
        url: 'http://localhost:3000',
      },
    },
    globals: true,
    include: ['src/**/*.test.ts', 'src/**/*.test.svelte'],
    setupFiles: ['src/setup.ts'],
  },
});
