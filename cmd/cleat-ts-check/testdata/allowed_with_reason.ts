// cleat-ts: allow TS001 -- this workflow probes that timers are absent from the runtime
export function f() { return typeof setTimeout; }
