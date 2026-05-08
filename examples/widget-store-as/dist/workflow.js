export async function instantiate(module, imports = {}) {
  const adaptedImports = {
    env: Object.assign(Object.create(globalThis), imports.env || {}, {
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
      durable_call(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen) {
        // assembly/cleat-runtime/import_durable_call(i32, i32, i32, i32, i32, i32, i32, i32) => i64
        return durable_call(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen) || 0n;
      },
      set_query_state(keyPtr, keyLen, valPtr, valLen) {
        // assembly/cleat-runtime/import_set_query_state(i32, i32, i32, i32) => i64
        return set_query_state(keyPtr, keyLen, valPtr, valLen) || 0n;
      },
      durable_await_signals(namesPtr, namesLen, timeoutMs, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen) {
        // assembly/cleat-runtime/import_durable_await_signals(i32, i32, i64, i32, i32, i32, i32) => i64
        return durable_await_signals(namesPtr, namesLen, timeoutMs, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen) || 0n;
      },
      durable_log(msgPtr, msgLen) {
        // assembly/cleat-runtime/import_durable_log(i32, i32) => i64
        return durable_log(msgPtr, msgLen) || 0n;
      },
      durable_child_workflow(namePtr, nameLen, inputPtr, inputLen, runIdPtr, runIdMaxLen) {
        // assembly/cleat-runtime/import_durable_child_workflow(i32, i32, i32, i32, i32, i32) => i64
        return durable_child_workflow(namePtr, nameLen, inputPtr, inputLen, runIdPtr, runIdMaxLen) || 0n;
      },
      durable_sleep(durationMs) {
        // assembly/cleat-runtime/import_durable_sleep(i64) => i64
        return durable_sleep(durationMs) || 0n;
      },
    }),
  };
  const { exports } = await WebAssembly.instantiate(module, adaptedImports);
  const memory = exports.memory || imports.env.memory;
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
  return exports;
}
