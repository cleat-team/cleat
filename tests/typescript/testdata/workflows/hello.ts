// The smallest workflow: no SDK, just the one global the cleat-js plugin installs for a result.
declare function __cleat_result(json: string): void;

export function hello(): void {
  __cleat_result(JSON.stringify({ hello: "ts" }));
}
