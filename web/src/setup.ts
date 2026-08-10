import '@testing-library/jest-dom/vitest';

// Node 22+ defines its own `localStorage` global (a getter that throws
// unless the process was started with --localstorage-file). Vitest's jsdom
// environment does not overwrite an already-present global unless the key
// is in its own hardcoded allowlist, and "localStorage" is not on it -- so
// in a test file, the bare `localStorage` identifier resolves to Node's
// broken one, not jsdom's real, working Storage implementation. Confirmed
// with `typeof localStorage` inside a test: 'undefined' printed alongside
// the Node warning "localStorage is not available because --localstorage-
// file was not provided" (2026-08-09, `npx vitest run` with no other
// change). jsdom's environment setup does stash the underlying JSDOM
// instance at `globalThis.jsdom`, so pull the real localStorage from there
// and install it over Node's, for every test file (lib/auth.ts, the API key
// storage backing the dashboard's Authorization header, needs a working
// one). See vitest.config.ts's environmentOptions.jsdom.url for the related
// fix this alone does not cover (jsdom's storage is unavailable for the
// opaque `about:blank` origin it uses without an explicit url).
const jsdomInstance = (globalThis as unknown as { jsdom?: { window?: { localStorage?: Storage } } }).jsdom;
if (jsdomInstance?.window?.localStorage) {
  Object.defineProperty(globalThis, 'localStorage', {
    value: jsdomInstance.window.localStorage,
    configurable: true,
    writable: true,
  });
}
