// Client-side API key storage.
//
// B5: the dashboard is a static SPA served same-origin by the worker, and the
// worker's supported deployment defaults to --require-auth=true
// (cmd/cleat-worker/config.go:80). There is no session/cookie login endpoint
// on the worker and no server-side template step where a token could be
// injected into index.html (it is served as a plain embedded file, see
// cmd/cleat-worker/main.go's `fs.Sub(webDist, "web/dist")` handler) -- adding
// either would be a backend change outside this stream's file ownership
// (web/, charts/cleat/).
//
// So the token has to come from the browser. The worker already prints a
// bearer-usable key to stdout on first boot ("auto-generated startup key",
// cmd/cleat-worker/main.go) and documents `--generate-api-key` for minting
// more, so an operator always has a real key in hand. This module lets them
// paste it once; it is kept in localStorage (scoped to this origin, never
// sent anywhere but this worker) and attached by api.ts as
// `Authorization: Bearer <token>` on every request.
const STORAGE_KEY = 'cleat_api_token';
const UNAUTHORIZED_EVENT = 'cleat:unauthorized';

/** Returns the stored API token, or '' if none is set. */
export function getToken(): string {
  try {
    return localStorage.getItem(STORAGE_KEY) || '';
  } catch {
    // localStorage can throw (private browsing quota, disabled storage).
    // Treat that the same as "no token" rather than crashing the app.
    return '';
  }
}

/** Stores the API token. Passing '' removes it. */
export function setToken(token: string): void {
  try {
    if (token) {
      localStorage.setItem(STORAGE_KEY, token);
    } else {
      localStorage.removeItem(STORAGE_KEY);
    }
  } catch {
    // Nothing to persist to; the current page's in-flight requests still
    // work off the caller-supplied value, they just won't survive a reload.
  }
}

/** Removes the stored API token. */
export function clearToken(): void {
  setToken('');
}

/**
 * Called by api.ts when a request comes back 401. Clears the (now known to
 * be missing or invalid) token and broadcasts so the UI can prompt for a new
 * one, instead of leaving the user staring at a page of failed fetches with
 * no explanation.
 */
export function notifyUnauthorized(): void {
  clearToken();
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new CustomEvent(UNAUTHORIZED_EVENT));
  }
}

/**
 * Subscribes to unauthorized notifications. Returns an unsubscribe function.
 */
export function onUnauthorized(handler: () => void): () => void {
  if (typeof window === 'undefined') {
    return () => {};
  }
  window.addEventListener(UNAUTHORIZED_EVENT, handler);
  return () => window.removeEventListener(UNAUTHORIZED_EVENT, handler);
}
