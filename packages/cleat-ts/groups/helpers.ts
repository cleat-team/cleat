// The three functions every group needs from index.ts, handed to it so a group module never imports index.ts back (a cycle
// the bundler would have to untangle, and a reason for a group's PR to edit the core file). Types only: esbuild erases this.

export interface Helpers {
  /** Turn a binding's JSON envelope into a promise: resolves with `ok`, rejects with a CleatError for `err`, NEVER SETTLES for a suspension. */
  settle<T>(raw: string): Promise<T>;
  /** JSON.stringify for anything that leaves the guest: refuses NaN and +-Infinity, as Go's encoding/json does. */
  encode(v: unknown): string;
  /** Parse JSON text if it is JSON, else hand the string back unchanged. */
  parseJSON(s: unknown): unknown;
}
