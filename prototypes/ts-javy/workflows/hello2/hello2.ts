// Phase 0 workflow: no SDK yet, just the two globals the cleat-js plugin installs.
declare function __cleat_result(json: string): void;

export function hello2(): void {
  __cleat_result(JSON.stringify({ hello: "ts-two" }));
}
