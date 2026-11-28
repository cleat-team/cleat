// setTimeout and WeakRef are only named in this comment, in strings, and in template text: none of it is a reference.
/* performance.now() crypto.getRandomValues fetch("x") */
const doc = "call setTimeout or new WeakRef(x) or Intl.DateTimeFormat";
const doc2 = 'process.env and require("fs") and crypto';
const text = `a template that mentions setInterval and performance but only as text`;
const obj = { crypto: 1, performance: 2 };           // object keys are not references
function run(o: any) {
  o.setTimeout(5);                                    // a method call on something else
}
// Date.now, new Date() and Math.random are SHIMMED onto the recorded host values, so they are allowed:
export function f() { return [Date.now(), new Date().getTime(), Math.random(), obj, doc, doc2, text, run]; }
