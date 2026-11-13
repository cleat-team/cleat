import { workflow } from "../../packages/cleat-ts/index";
import { chunk, groupBy, orderBy, uniqBy, sum, mapValues, merge, cloneDeep, get, set, pick, isEqual, camelCase, kebabCase, template, debounce, random } from "lodash-es";
import { runCase, type Cases } from "../npm_common";

const people = [
  { name: "ada", age: 36, team: "x" },
  { name: "bob", age: 41, team: "y" },
  { name: "cy", age: 36, team: "x" },
  { name: "di", age: 29, team: "y" },
];

export const CASES: Cases = {
  collections: () => ({
    chunk: chunk([1, 2, 3, 4, 5], 2),
    groupBy: mapValues(groupBy(people, "team"), (v) => v.map((p) => p.name)),
    orderBy: orderBy(people, ["age", "name"], ["desc", "asc"]).map((p) => p.name),
    uniq: uniqBy(people, "age").map((p) => p.name),
    sum: sum(people.map((p) => p.age)),
  }),
  objects: () => {
    const base = { a: { b: [1, { c: 2 }] }, k: 1 };
    const copy = cloneDeep(base);
    set(copy, "a.b[1].c", 99);
    return { merged: merge({}, base, { a: { b: [9] }, z: 3 }), baseUntouched: get(base, "a.b[1].c"), copy: get(copy, "a.b[1].c"), pick: pick(base, ["k"]), eq: isEqual(base, cloneDeep(base)) };
  },
  strings: () => [camelCase("Foo Bar-baz_qux"), kebabCase("fooBar BAZ")],
  // _.template compiles its source with the Function constructor.
  template: () => template("hello <%= name %>! <% items.forEach(function (i) { %>[<%- i %>]<% }); %>")({ name: "world", items: ["a<b", "c"] }),
  // Behaves differently by design: debounce needs setTimeout, which this runtime does not define (and tools/tscheck cannot see inside node_modules).
  debounce: () => {
    const f = debounce(() => 1, 10);
    f();
    return "called";
  },
  random: () => {
    const r = random(1, 10);
    return Number.isInteger(r) && r >= 1 && r <= 10;
  },
  control_differs: () => typeof (globalThis as any).process,
};

export const npmLodash = workflow<{ case: string }, unknown>(async (ctx, input) => runCase(ctx, input.case, CASES));
