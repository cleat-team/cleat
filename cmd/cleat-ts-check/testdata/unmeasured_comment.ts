export function f() { return 1; }
/* this comment is never closed, so the rest of the file is swallowed
setTimeout(() => {}, 1);
