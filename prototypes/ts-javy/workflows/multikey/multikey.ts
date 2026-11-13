import { workflow } from "../../packages/cleat-ts/index";

// Q4: a result whose raw JavaScript JSON.stringify is NOT what Go's encoding/json would produce for the same value: keys declared
// out of order, HTML characters, a float, a nested object. The Go twin (gocontrol.GoMultiKey) returns json.Marshal of the same map.
export const multikey = workflow<unknown, Record<string, unknown>>(async () => {
  return { b: 1, a: 2, html: "<x>&", nested: { z: 1.5, y: [true, null] }, n: 100 };
});
