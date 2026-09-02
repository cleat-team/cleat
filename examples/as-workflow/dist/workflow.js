export async function instantiate(module, imports = {}) {
  const adaptedImports = {
    env: Object.setPrototypeOf({
      abort(message, fileName, lineNumber, columnNumber) {
        // ~lib/builtins/abort(~lib/string/String | null?, ~lib/string/String | null?, u32?, u32?) => void
        message = __liftString(message >>> 0);
        fileName = __liftString(fileName >>> 0);
        lineNumber = lineNumber >>> 0;
        columnNumber = columnNumber >>> 0;
        (() => {
          // @external.js
          throw Error(`${message} in ${fileName}:${lineNumber}:${columnNumber}`);
        })();
      },
      cleat_call(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_call(i32, i32, i32, i32, i32, i32, i32, i32) => i64
        return cleat_call(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen) || 0n;
      },
      cleat_poll_cancellation(reasonPtr, reasonMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_poll_cancellation(i32, i32) => i64
        return cleat_poll_cancellation(reasonPtr, reasonMaxLen) || 0n;
      },
      cleat_defer(descPtr, descLen, deferIdPtr, deferIdMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_defer(i32, i32, i32, i32) => i64
        return cleat_defer(descPtr, descLen, deferIdPtr, deferIdMaxLen) || 0n;
      },
      cleat_sleep(durationMs) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_sleep(i64) => i64
        return cleat_sleep(durationMs) || 0n;
      },
    }, Object.assign(Object.create(globalThis), imports.env || {})),
  };
  const { exports } = await WebAssembly.instantiate(module, adaptedImports);
  const memory = exports.memory || imports.env.memory;
  const adaptedExports = Object.setPrototypeOf({
    __durable_inner_place_order(h, input) {
      // assembly/index/__durable_inner_place_order(~lib/@cleat/sdk/assembly/host-calls/HostCalls, ~lib/string/String) => ~lib/string/String
      h = __retain(__lowerInternref(h) || __notnull());
      input = __lowerString(input) || __notnull();
      try {
        return __liftString(exports.__durable_inner_place_order(h, input) >>> 0);
      } finally {
        __release(h);
      }
    },
    __durable_inner_cancel_order(h, input) {
      // assembly/index/__durable_inner_cancel_order(~lib/@cleat/sdk/assembly/host-calls/HostCalls, ~lib/string/String) => ~lib/string/String
      h = __retain(__lowerInternref(h) || __notnull());
      input = __lowerString(input) || __notnull();
      try {
        return __liftString(exports.__durable_inner_cancel_order(h, input) >>> 0);
      } finally {
        __release(h);
      }
    },
    __durable_inner_defer_order(h, input) {
      // assembly/index/__durable_inner_defer_order(~lib/@cleat/sdk/assembly/host-calls/HostCalls, ~lib/string/String) => ~lib/string/String
      h = __retain(__lowerInternref(h) || __notnull());
      input = __lowerString(input) || __notnull();
      try {
        return __liftString(exports.__durable_inner_defer_order(h, input) >>> 0);
      } finally {
        __release(h);
      }
    },
    __durable_inner_defer_suspend(h, input) {
      // assembly/index/__durable_inner_defer_suspend(~lib/@cleat/sdk/assembly/host-calls/HostCalls, ~lib/string/String) => ~lib/string/String
      h = __retain(__lowerInternref(h) || __notnull());
      input = __lowerString(input) || __notnull();
      try {
        return __liftString(exports.__durable_inner_defer_suspend(h, input) >>> 0);
      } finally {
        __release(h);
      }
    },
  }, exports);
  function __liftString(pointer) {
    if (!pointer) return null;
    const
      end = pointer + new Uint32Array(memory.buffer)[pointer - 4 >>> 2] >>> 1,
      memoryU16 = new Uint16Array(memory.buffer);
    let
      start = pointer >>> 1,
      string = "";
    while (end - start > 1024) string += String.fromCharCode(...memoryU16.subarray(start, start += 1024));
    return string + String.fromCharCode(...memoryU16.subarray(start, end));
  }
  function __lowerString(value) {
    if (value == null) return 0;
    const
      length = value.length,
      pointer = exports.__new(length << 1, 2) >>> 0,
      memoryU16 = new Uint16Array(memory.buffer);
    for (let i = 0; i < length; ++i) memoryU16[(pointer >>> 1) + i] = value.charCodeAt(i);
    return pointer;
  }
  class Internref extends Number {}
  function __lowerInternref(value) {
    if (value == null) return 0;
    if (value instanceof Internref) return value.valueOf();
    throw TypeError("internref expected");
  }
  const refcounts = new Map();
  function __retain(pointer) {
    if (pointer) {
      const refcount = refcounts.get(pointer);
      if (refcount) refcounts.set(pointer, refcount + 1);
      else refcounts.set(exports.__pin(pointer), 1);
    }
    return pointer;
  }
  function __release(pointer) {
    if (pointer) {
      const refcount = refcounts.get(pointer);
      if (refcount === 1) exports.__unpin(pointer), refcounts.delete(pointer);
      else if (refcount) refcounts.set(pointer, refcount - 1);
      else throw Error(`invalid refcount '${refcount}' for reference '${pointer}'`);
    }
  }
  function __notnull() {
    throw TypeError("value must not be null");
  }
  return adaptedExports;
}
