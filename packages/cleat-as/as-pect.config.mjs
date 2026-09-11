// as-pect test configuration for @cleat/sdk.
// Test entry globs go in `entries` (each file compiled as a separate test).
// The default `include` glob (`assembly/__tests__/**/*.include.ts`) provides
// shared setup files that import @as-pect/assembly for describe/it/expect.
//
// AssemblyScript compiler options go in `as-pect.asconfig.json`, mirroring asconfig.json.

export default {
  // Test entry file glob patterns
  entries: ["assembly/__tests__/**/*.spec.ts"],

  // Do NOT set `include` here so as-pect falls back to the default
  // assembly/__tests__/**/*.include.ts glob, picking up setup.include.ts.

  /**
   * Instantiate the WebAssembly test module.
   * @param {WebAssembly.Memory} memory
   * @param {Function} createImports
   * @param {Function} instantiate
   * @param {Uint8Array} binary
   * @returns {{ module: WebAssembly.Module, instance: WebAssembly.Instance, exports: object }}
   */
  async instantiate(memory, createImports, instantiate, binary) {
    // Capture the module's exported memory after instantiation.
    // This is needed by the JSON host import stubs to read/write WASM memory.
    let wasmMemory = null;

    /**
     * Read a UTF-8 string from WASM linear memory.
     * @param {number} ptr - Byte offset in linear memory.
     * @param {number} len - Number of bytes to read.
     * @returns {string}
     */
    function readWasmString(ptr, len) {
      if (!wasmMemory || len === 0) return "";
      const bytes = new Uint8Array(wasmMemory.buffer, ptr, len);
      return new TextDecoder().decode(bytes);
    }

    /**
     * Write a UTF-8 string into WASM linear memory at a given offset.
     * @param {number} ptr - Byte offset in linear memory.
     * @param {string} str - String to write.
     * @param {number} maxLen - Maximum bytes available at outPtr.
     * @returns {number} Number of bytes written.
     */
    function writeWasmString(ptr, str, maxLen) {
      if (!wasmMemory) return 0;
      const encoded = new TextEncoder().encode(str);
      const written = Math.min(encoded.length, maxLen);
      const out = new Uint8Array(wasmMemory.buffer, ptr, written);
      out.set(encoded.subarray(0, written));
      return written;
    }

    /**
     * Pack (bytesWritten, errCode) into the i64 return value expected by
     * the cleat ABI: upper 32 bits = bytesWritten, lower 8 bits = errCode.
     * @param {number} bytesWritten
     * @param {number} errCode
     * @returns {bigint}
     */
    function encodeResult(bytesWritten, errCode) {
      return BigInt.asIntN(
        64,
        (BigInt(bytesWritten) << 32n) | BigInt(errCode & 0xff),
      );
    }

    /**
     * JS stub for cleat_json_parse.
     * Reads a JSON string from WASM memory, validates via JSON.parse,
     * writes the normalized result back, and returns an ABI-compatible i64.
     */
    function jsonParse(jsonPtr, jsonLen, outPtr, outMaxLen) {
      const jsonStr = readWasmString(jsonPtr, jsonLen);
      if (jsonStr === "") return BigInt(0);
      try {
        const parsed = JSON.parse(jsonStr);
        const normalized = JSON.stringify(parsed);
        const written = writeWasmString(outPtr, normalized, outMaxLen);
        return encodeResult(written, 0);
      } catch (_) {
        return encodeResult(0, 1);
      }
    }

    /**
     * JS stub for cleat_json_stringify.
     * Reads a JSON string from WASM memory, validates via JSON.parse,
     * writes the serialized result back, and returns an ABI-compatible i64.
     */
    function jsonStringify(ptr, len, outPtr, outMaxLen) {
      const valStr = readWasmString(ptr, len);
      if (valStr === "") return BigInt(0);
      try {
        const parsed = JSON.parse(valStr);
        const serialized = JSON.stringify(parsed);
        const written = writeWasmString(outPtr, serialized, outMaxLen);
        return encodeResult(written, 0);
      } catch (_) {
        return encodeResult(0, 1);
      }
    }

    // cleat_now, so a module that pulls in HostCalls can be INSTANTIATED.
    //
    // A WebAssembly import must be satisfied at instantiation whether or not
    // it is ever called, so before this the harness could not load any test
    // that imported HostCalls at all -- the failure was
    // `LinkError: Import #1 "env" "cleat_now": function import requires a
    // callable`, which reads as a broken test rather than a missing stub.
    // That is why no spec here tested a HostCalls method (cleat#1136).
    //
    // Monotonic rather than Date.now(): a deadline loop that reads the clock
    // twice in the same millisecond would otherwise see no time pass.
    let fakeNowMs = 1700000000000n;
    const cleatNow = () => (fakeNowMs += 1n);

    const myImports = {
      env: {
        memory,
        cleat_json_parse: jsonParse,
        cleat_json_stringify: jsonStringify,
        cleat_now: cleatNow,
      },
    };

    // Every OTHER host import gets a stub that THROWS.
    //
    // Not a no-op returning 0. stop-bit.spec.ts's header states the hazard
    // exactly -- "a test built on those would be measuring the stub's return
    // value" -- and a zero-returning stub is indistinguishable from a real
    // answer, so a test that wandered into one would pass while asserting
    // nothing. Throwing makes reaching an unstubbed host call a loud failure
    // naming the import, which is the only way a test can know it stayed on
    // the guest side of the boundary.
    for (const imp of WebAssembly.Module.imports(new WebAssembly.Module(binary))) {
      if (imp.module !== "env" || imp.kind !== "function") continue;
      if (imp.name in myImports.env) continue;
      myImports.env[imp.name] = () => {
        throw new Error(
          `as-pect: host import "${imp.name}" is not stubbed. A test reached the ` +
            `host boundary. Either it should not have, or this import needs a ` +
            `stub here -- do not add one that returns 0, which is a plausible ` +
            `answer and would be measured as one.`,
        );
      };
    }

    // instantiate() is @assemblyscript/loader's instantiate which returns
    // { module, instance, exports }. Capture the module's memory export.
    const result = await instantiate(binary, createImports(myImports));
    wasmMemory = result.exports.memory;
    return result;
  },
};
