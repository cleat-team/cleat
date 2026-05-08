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
   * @returns {WebAssembly.Instance}
   */
  async instantiate(memory, createImports, instantiate, binary) {
    let instance;
    const myImports = {
      env: { memory },
    };
    instance = instantiate(binary, createImports(myImports));
    return instance;
  },
};
