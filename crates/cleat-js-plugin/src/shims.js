// Determinism shims. Evaluated once, at plugin initialisation, after the __cleat_* bindings exist and before
// any user code. Each shim is gated by name so a CONTROL build can remove exactly one (CLEAT_SHIMS_OFF=date,random,...),
// which is how "remove the shim; the test must fail" is run rather than asserted.
(function () {
  "use strict";
  const g = globalThis;
  const off = String(g.__cleat_shims_off || "").split(",");
  const on = (name) => off.indexOf(name) < 0;

  // Date: now and the no-argument constructor read the HOST's recorded clock (cleat_now), not the WASI clock. Dates built
  // from an explicit value or string are pure functions of their arguments and are left alone.
  if (on("date")) {
    const D = g.Date;
    const now = () => __cleat_now();
    const CleatDate = function Date(...args) {
      if (!new.target) return new D(now()).toString();
      if (args.length === 0) return new D(now());
      return new D(...args);
    };
    CleatDate.prototype = D.prototype;
    Object.defineProperty(D.prototype, "constructor", { value: CleatDate, writable: true, configurable: true });
    Object.setPrototypeOf(CleatDate, D);
    CleatDate.now = now;
    CleatDate.UTC = D.UTC;
    CleatDate.parse = D.parse;
    g.Date = CleatDate;
  }

  // Math.random: a uniform double in [0, 1) from the host's recorded random value (an i64, so reinterpret as unsigned and
  // keep the top 53 bits). Anything that is not a decimal integer is a host answer this shim cannot read (the "suspended"
  // envelope is the one the binding can produce), and it THROWS: returning 0 instead is stable across replays, so it would
  // pass every determinism check while giving user code a constant. The "randomstrict" name exists only so a control build can
  // put the old return-0 behaviour back and show the check tells the two apart.
  if (on("random")) {
    Math.random = function random() {
      const s = __cleat_random();
      if (!/^-?[0-9]+$/.test(s)) {
        if (on("randomstrict")) throw new Error("cleat: Math.random: the host did not return a number: " + String(s).slice(0, 60));
        return 0;
      }
      // i64 -> unsigned by offset (add 2^63), exact in BigInt: [0, 2^64). Do NOT use BigInt.asUintN: in this QuickJS it left the
      // value signed and Math.random returned numbers in [-0.5, 0.5) (found by the range check in phase2_controls.py).
      const u = BigInt(s) + 9223372036854775808n;
      return Number(u >> 11n) / 9007199254740992;
    };
  }

  // Globals that expose wall-clock time, GC timing or an event loop outside the recorded host calls. The Javy 9.1.0 build
  // the pinned Javy defines NONE of them (measured when the pin was chosen), so today this deletes nothing; it is here so a newer Javy
  // that adds one cannot silently reintroduce it. A control for these rows can therefore not fail today, and is reported so.
  if (on("lockdown")) {
    for (const k of ["WeakRef", "FinalizationRegistry", "setTimeout", "setInterval", "setImmediate", "clearTimeout",
                     "clearInterval", "performance", "crypto", "queueMicrotaskTimer"]) {
      if (k in g) delete g[k];
    }
  }

  delete g.__cleat_shims_off;
})();
