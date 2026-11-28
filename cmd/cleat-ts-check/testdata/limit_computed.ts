// KNOWN LIMITS. Each of these reaches a timer, and none is seen by the static check; at run time the global is absent
// (a ReferenceError / TypeError mid-run), which is the second layer. If this file ever starts being refused, the limit
// has moved and PHASE2.md's list must change with it.
const g = globalThis;
export function f() {
  (globalThis as any)["set" + "Timeout"](() => {}, 1);   // computed property access
  (g as any).setTimeout(() => {}, 1);                     // an alias of globalThis
  return (0, eval)("typeof setTimeout");                  // indirect eval
}
