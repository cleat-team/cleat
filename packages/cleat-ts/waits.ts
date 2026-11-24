// A wait's millisecond count as the host is given it (cleat#4144).
//
// `waitForSignal` and `awaitPromise` pass a JavaScript number across the ABI as an i64, and three values did the wrong thing on
// the way: a POSITIVE fraction (0.4 ms) truncated to 0, which the host reads as "do not wait"; `Infinity` is not an integer and
// became an arbitrary one; and a count past 2^53 lost precision. The rules, stated once:
//
//   * `Infinity` is the explicit way to wait without a limit. It becomes the largest exact integer, which the host clamps (cleat#4105).
//   * A positive count rounds UP to a whole millisecond, so a positive wait is never 0.
//   * Zero, a negative number and NaN are passed through exactly as before; what they mean is the host's, not this file's.
//
// It lives in its own module so that a test can import it without building a WASM module.

/** The largest count a JavaScript number holds exactly, and so the largest this SDK can state as "for ever". */
export const MAX_WAIT_MS = Number.MAX_SAFE_INTEGER;

export function waitMs(ms: number): number {
  if (ms > MAX_WAIT_MS) return MAX_WAIT_MS; // includes Infinity
  if (ms > 0) return Math.ceil(ms);
  return ms;
}
