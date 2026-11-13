// KNOWN OVER-REPORTS. These are not references to the global, and the checker refuses them anyway, because telling them
// apart from a real call needs a parser (`{ setTimeout(f, 1); }` after a block brace looks identical to the type member below).
// Refusing is the safe direction: the author renames, instead of a real timer call being let through.
export function run(o: { setTimeout(n: number): void }) {   // a method DECLARATION in a type literal
  return o;
}
const performance = 1;                                       // a local that shadows the global name
export { performance };
