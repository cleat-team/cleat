"use strict";

const fs = require("fs");
const path = require("path");

/**
 * AssemblyScript transformer for the cleat durable execution framework.
 *
 * Generates WASM export wrappers for functions decorated with @cleatEntry.
 * The wrappers conform to the cleat ABI:
 *   (argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32) => i64
 *
 * For a user function like:
 *
 *   @cleatEntry
 *   function placeOrder(h: HostCalls, input: PlaceOrderInput): string { ... }
 *
 * The transformer:
 *   1. Renames the original function to __durable_inner_placeOrder
 *   2. Strips the @cleatEntry decorator
 *   3. Generates an export wrapper (with the original name) in the same source
 *
 * Usage in asconfig.json:
 *   { "options": { "transform": ["@cleat/transform"] } }
 *
 * The generated wrapper:
 *   - Reads input JSON from WASM linear memory at (argsPtr, argsLen)
 *   - Deserializes JSON into the user's input type via JSON.parse<T>
 *   - Creates a HostCalls instance from @cleat/sdk
 *   - Invokes the renamed inner function
 *   - Serializes the result to JSON and writes it to (outPtr, maxOutLen)
 *   - Returns a packed i64: low 32 bits = errCode, high 32 bits = actualLen
 *   - On error: writes {"error":"..."} to the output buffer and returns errCode=1
 */
class CleatEntryTransformer {

  // ---------------------------------------------------------------
  // AssemblyScript transformer hook - called after all sources are parsed
  // ---------------------------------------------------------------
  afterParse(parser) {
    const program = parser.program;
    if (!program || !program.sources) return;

    // Phase 1: Find @cleatEntry functions grouped by source file
    const sourceEntries = this._findDurableEntries(program);

    // Phase 1b: Static analysis - build call graphs, compute durable closure,
    //            validate functions, verify HostCalls threading
    for (const source of program.sources) {
      if (!source || !source.statements) continue;

      // Build call graph for all functions in this source
      const callGraph = this._buildCallGraph(source);

      // Find functions that directly call HostCalls methods
      const durableLeaves = this._findDurableLeaves(callGraph);

      if (durableLeaves.size === 0) continue;

      // Compute transitive closure of durable functions
      const durableFunctions = this._computeDurableClosure(callGraph, durableLeaves);

      // Validate all functions in the durable closure for forbidden APIs
      for (const stmt of source.statements) {
        if (!stmt || !stmt.name || !stmt.signature) continue;
        if (durableFunctions.has(stmt.name.text)) {
          this._validateDurableFunction(stmt, source);
        }
      }

      // Verify that functions in the durable closure have access to 'h'
      this._verifyThreading(source, durableFunctions, callGraph);
    }

    if (sourceEntries.length === 0) return;

    // Phase 2: Modify AST - rename functions and strip decorators
    this._renameEntries(sourceEntries);

    // Phase 3: Generate wrapper source code and inject into the source
    this._injectWrappers(parser, sourceEntries);
  }

  // ---------------------------------------------------------------
  // Phase 1: Walk all sources and collect @cleatEntry metadata
  // ---------------------------------------------------------------
  _findDurableEntries(program) {
    const sourceEntries = [];

    for (const source of program.sources) {
      if (!source || !source.statements) continue;

      const entries = [];
      for (const stmt of source.statements) {
        if (!this._isDurableEntryFunc(stmt)) continue;

        // Validate: must have at least one parameter (HostCalls)
        const params = stmt.signature.parameters || [];
        if (params.length === 0) {
          console.error(
            "[@cleat/transform] Warning: @cleatEntry function '" +
            (stmt.name ? stmt.name.text : "unknown") +
            "' has no parameters. The first parameter must be HostCalls."
          );
          continue;
        }

        const info = this._extractEntryInfo(stmt);
        entries.push(info);
      }

      if (entries.length > 0) {
        sourceEntries.push({ source, entries });
      }
    }

    return sourceEntries;
  }

  // ---------------------------------------------------------------
  // Check if a statement is a function declaration with @cleatEntry
  // ---------------------------------------------------------------
  _isDurableEntryFunc(stmt) {
    if (!stmt || typeof stmt !== "object") return false;
    // Must look like a function declaration with decorators
    if (!stmt.name || !stmt.signature || !stmt.decorators) return false;
    if (!Array.isArray(stmt.decorators) || stmt.decorators.length === 0) return false;
    // One of the decorators must be @cleatEntry
    return stmt.decorators.some(
      d => d && d.name && d.name.text === "cleatEntry"
    );
  }

  // ---------------------------------------------------------------
  // Extract metadata from a @cleatEntry function declaration
  // ---------------------------------------------------------------
  _extractEntryInfo(stmt) {
    const funcName = stmt.name.text;
    const innerName = `__durable_inner_${funcName}`;

    // Extract parameters skipping the first one (h: HostCalls)
    const params = stmt.signature.parameters || [];
    const userParams = params.slice(1);

    const paramNames = [];
    const paramTypes = [];
    const callArgs = ["h"];

    for (const p of userParams) {
      const pName = p.name && p.name.text ? p.name.text : "_";
      const pType = p.type && p.type.text ? p.type.text : "string";

      paramNames.push(pName);
      paramTypes.push(pType);
      callArgs.push(pName);
    }

    // Return type string
    const retTypeNode = stmt.signature.returnType;
    const retTypeStr = retTypeNode ? retTypeNode.text : "void";
    const isVoid = retTypeStr === "void";
    const isString = retTypeStr === "string" || retTypeStr === "String";

    return {
      funcName,
      innerName,
      paramNames,
      paramTypes,
      callArgs,
      retTypeStr,
      isVoid,
      isString,
    };
  }

  // ---------------------------------------------------------------
  // Phase 2: Rename @cleatEntry functions and strip decorators
  // ---------------------------------------------------------------
  _renameEntries(sourceEntries) {
    for (const { source, entries } of sourceEntries) {
      for (const stmt of source.statements) {
        if (!this._isDurableEntryFunc(stmt)) continue;

        const entry = entries.find(
          e => stmt.name && stmt.name.text === e.funcName
        );
        if (!entry) continue;

        // Rename the function so the generated wrapper can use the original name
        stmt.name.text = entry.innerName;

        // Remove @cleatEntry decorator
        stmt.decorators = stmt.decorators.filter(
          d => !(d.name && d.name.text === "cleatEntry")
        );
      }
    }
  }

  // ---------------------------------------------------------------
  // Static analysis helpers
  // ---------------------------------------------------------------

  // ---------------------------------------------------------------
  // Validate a function body for forbidden API calls (Math.random,
  // Date.now, console.log, process.*) in the durable closure.
  // ---------------------------------------------------------------
  _validateDurableFunction(stmt, source) {
    const funcName = stmt.name ? stmt.name.text : "unknown";
    const sourceName = source.internalPath || source.name || "unknown";
    const body = stmt.body;
    if (!body || !body.statements) return;

    const self = this;
    self._walkStatements(body.statements, function(callExpr) {
      if (!callExpr || !callExpr.callee) return;
      const callee = callExpr.callee;

      // MemberExpression patterns: Math.random(), Date.now(), console.log(), process.*
      if (callee.object && typeof callee.object === 'object' && callee.property && typeof callee.property === 'object') {
        const objName = callee.object.text || (callee.object.name ? callee.object.name.text : null);
        const propName = callee.property.text || (callee.property.name ? callee.property.name.text : null);

        if (objName === "Math" && (propName === "random" || propName === "seedRandom")) {
          const loc = self._getSourceLocation(callExpr, sourceName);
          console.error("[cleat/transform] E001: Math." + propName + "() in durable function '" + funcName + "' at " + loc + "\n  → Use h.Random() for deterministic randomness.");
          return;
        }

        if (objName === "Date" && propName === "now") {
          const loc = self._getSourceLocation(callExpr, sourceName);
          console.error("[cleat/transform] E002: Date.now() in durable function '" + funcName + "' at " + loc + "\n  → Use h.Now() for deterministic time.");
          return;
        }

        if (objName === "console" && propName === "log") {
          const loc = self._getSourceLocation(callExpr, sourceName);
          console.error("[cleat/transform] E003: console.log() in durable function '" + funcName + "' at " + loc + "\n  → Use h.DurableLog() for durable logging.");
          return;
        }

        if (objName === "process") {
          const loc = self._getSourceLocation(callExpr, sourceName);
          console.error("[cleat/transform] E004: process." + propName + " in durable function '" + funcName + "' at " + loc + "\n  → Process access is not allowed in workflow code.");
          return;
        }
      }
    });
  }

  // ---------------------------------------------------------------
  // Recursively walk AST statements and invoke callback for each
  // CallExpression node. Handles nested calls and control flow.
  // ---------------------------------------------------------------
  _walkStatements(statements, callback) {
    if (!Array.isArray(statements)) return;

    function walkNode(node) {
      if (!node || typeof node !== 'object') return;

      // If this is a CallExpression (has callee), invoke callback
      if (node.callee) {
        callback(node);
      }

      // Recursively walk common AST child properties
      if (node.statements && Array.isArray(node.statements)) {
        for (const s of node.statements) walkNode(s);
      }
      if (node.expression && typeof node.expression === 'object') {
        walkNode(node.expression);
      }
      if (node.args && Array.isArray(node.args)) {
        for (const a of node.args) walkNode(a);
      }
      if (node.init && typeof node.init === 'object') {
        walkNode(node.init);
      }
      if (node.object && typeof node.object === 'object') {
        walkNode(node.object);
      }
      if (node.property && typeof node.property === 'object') {
        walkNode(node.property);
      }
      if (node.callee && typeof node.callee === 'object') {
        walkNode(node.callee);
      }
      // Handle control flow (if/else)
      if (node.condition) walkNode(node.condition);
      if (node.consequent) walkNode(node.consequent);
      if (node.alternate) walkNode(node.alternate);
      // Handle variable declarations
      if (node.declaration) walkNode(node.declaration);
      if (node.value && typeof node.value === 'object') walkNode(node.value);
    }

    for (const stmt of statements) {
      walkNode(stmt);
    }
  }

  // ---------------------------------------------------------------
  // Build a call graph for a source file: map caller -> set of callees
  // and callee -> set of callers (reverse edges).
  // ---------------------------------------------------------------
  _buildCallGraph(source) {
    const callers = {};
    const callees = {};

    for (const stmt of source.statements) {
      if (!stmt || !stmt.name || !stmt.signature) continue;
      const callerName = stmt.name.text;

      if (!stmt.body || !stmt.body.statements) continue;

      const calleeSet = new Set();
      const self = this;

      self._walkStatements(stmt.body.statements, function(callExpr) {
        if (!callExpr || !callExpr.callee) return;
        const callee = callExpr.callee;
        let calleeName = null;

        // MemberExpression: h.durableCall, Math.random, etc.
        if (callee.object && typeof callee.object === 'object' && callee.property && typeof callee.property === 'object') {
          const objName = callee.object.text || (callee.object.name ? callee.object.name.text : null);
          const propName = callee.property.text || (callee.property.name ? callee.property.name.text : null);
          if (objName && propName) {
            calleeName = objName + "." + propName;
          }
        }

        // Simple identifier (direct function call)
        if (!calleeName && callee.text) {
          calleeName = callee.text;
        }

        if (calleeName) {
          calleeSet.add(calleeName);
        }
      });

      callers[callerName] = Array.from(calleeSet);
      for (const cn of calleeSet) {
        if (!callees[cn]) callees[cn] = [];
        callees[cn].push(callerName);
      }
    }

    return { callers, callees };
  }

  // ---------------------------------------------------------------
  // Find functions that directly call HostCalls methods (durable leaves).
  // ---------------------------------------------------------------
  _findDurableLeaves(callGraph) {
    const leaves = new Set();
    for (const [callerName, calleeNames] of Object.entries(callGraph.callers)) {
      for (const calleeName of calleeNames) {
        if (this._isHostCall(calleeName)) {
          leaves.add(callerName);
          break;
        }
      }
    }
    return leaves;
  }

  // ---------------------------------------------------------------
  // Compute the transitive closure of durable functions: starting from
  // durable leaves, traverse callers until fixed point.
  // ---------------------------------------------------------------
  _computeDurableClosure(callGraph, durableLeaves) {
    const durableFuncs = new Set(durableLeaves);

    let changed = true;
    while (changed) {
      changed = false;
      for (const [callerName, calleeNames] of Object.entries(callGraph.callers)) {
        if (durableFuncs.has(callerName)) continue;
        for (const calleeName of calleeNames) {
          // If the callee is already in the durable closure or is a HostCall
          if (durableFuncs.has(calleeName) || this._isHostCall(calleeName)) {
            durableFuncs.add(callerName);
            changed = true;
            break;
          }
        }
      }
    }

    return durableFuncs;
  }

  // ---------------------------------------------------------------
  // Verify that all functions in the durable closure have access to 'h'
  // (HostCalls parameter). Report errors with call chain trace.
  // ---------------------------------------------------------------
  _verifyThreading(source, durableFunctions, callGraph) {
    const sourceName = source.internalPath || source.name || "unknown";

    for (const stmt of source.statements) {
      if (!stmt || !stmt.name || !stmt.signature) continue;
      const funcName = stmt.name.text;
      if (!durableFunctions.has(funcName)) continue;

      // Check if the function has 'h' as first parameter
      const params = stmt.signature.parameters || [];
      const hasH = params.length > 0 && params[0] && params[0].name && params[0].name.text === "h";

      if (!hasH) {
        const loc = this._getSourceLocation(stmt, sourceName);
        const chain = this._traceCallChain(funcName, callGraph);
        console.error(
          "[cleat/transform] E005: Durable function '" + funcName + "' at " + loc + " is missing HostCalls parameter 'h'.\n" +
          "  → Add 'h: HostCalls' as the first parameter.\n" +
          "  → Call chain: " + chain.join(" → ")
        );
      }
    }
  }

  // ---------------------------------------------------------------
  // Trace the call chain from a function back to an entry point.
  // Walks upward through callers in the reverse-edge graph.
  // ---------------------------------------------------------------
  _traceCallChain(funcName, callGraph) {
    const chain = [funcName];
    const visited = new Set();
    let current = funcName;

    while (current && !visited.has(current)) {
      visited.add(current);
      const callers = callGraph.callees[current] || [];
      if (callers.length === 0) break;
      current = callers[0];
      chain.unshift(current);
    }

    return chain;
  }

  // ---------------------------------------------------------------
  // Get formatted source location "(file:line)" from an AST node.
  // Tries node.range.start.line or node.name.range.start.line.
  // ---------------------------------------------------------------
  _getSourceLocation(node, sourceName) {
    const range = node.range || (node.name ? node.name.range : null);
    if (range && range.start && range.start.line !== undefined) {
      return sourceName + ":" + range.start.line;
    }
    return sourceName;
  }

  // ---------------------------------------------------------------
  // Check if a callee name is a known HostCalls method (h.*).
  // ---------------------------------------------------------------
  _isHostCall(calleeName) {
    if (!calleeName || typeof calleeName !== 'string' || !calleeName.startsWith("h.")) return false;
    const method = calleeName.substring(2);
    const knownMethods = [
      "durableCall", "durableSleep", "durableLog", "Now", "Random",
      "UUID", "setEventCallback", "childWorkflow", "getState", "setState"
    ];
    return knownMethods.includes(method);
  }

  // ---------------------------------------------------------------
  // Phase 3: Generate wrapper code and inject it into source files
  // ---------------------------------------------------------------
  _injectWrappers(parser, sourceEntries) {
    for (const { source, entries } of sourceEntries) {
      const needsImport = !this._hasCleatSdkImport(source);
      const needsJsonImport = this._hasMultiParamEntries(entries);
      const wrapperCode = this._generateWrappers(entries);

      try {
        // Insert @cleat/sdk import at the TOP of the source if needed
        if (needsImport) {
          let importLine = 'import { HostCalls, Memory, SUSPEND_SENTINEL, isWorkflowSuspended, resetWorkflowSuspended';
          if (needsJsonImport) {
            importLine += ', JsonParser, JsonVal';
          }
          importLine += ' } from "@cleat/sdk";\n';
          const importSrc = parser.parseFile(
            "~lib/generated/cleat-import.ts",
            importLine,
            false
          );
          if (importSrc && importSrc.statements) {
            // unshift in reverse order so they appear in original order
            for (let i = importSrc.statements.length - 1; i >= 0; i--) {
              const s = importSrc.statements[i];
              if (s) source.statements.unshift(s);
            }
          }
        }

        // Append wrapper functions at the END of the source
        const wrapSrc = parser.parseFile(
          "~lib/generated/cleat-wrappers.ts",
          wrapperCode,
          false
        );
        if (wrapSrc && wrapSrc.statements) {
          for (const s of wrapSrc.statements) {
            if (s) source.statements.push(s);
          }
        }
      } catch (e) {
        console.error("[@cleat/transform] Failed to inject wrappers: " + e.message + ". Falling back to file output.");
        this._writeFallback(wrapperCode);
      }
    }
  }

  // ---------------------------------------------------------------
  // Check if a source already imports from @cleat/sdk
  // ---------------------------------------------------------------
  _hasCleatSdkImport(source) {
    if (!source.statements) return false;

    for (const stmt of source.statements) {
      if (!stmt) continue;

      // Probe various property paths used by different AS versions
      const modName =
        stmt.moduleName ||
        (stmt.internalNamespace && stmt.internalNamespace.text) ||
        (stmt.from && (typeof stmt.from === "string" ? stmt.from : stmt.from.text)) ||
        (stmt.module && (typeof stmt.module === "string" ? stmt.module : stmt.module.text));

      if (modName === "@cleat/sdk") return true;

      if (stmt.namespace && stmt.namespace.text === "@cleat/sdk") return true;
    }

    return false;
  }

  // ---------------------------------------------------------------
  // Fallback: write wrapper code to a file on disk
  // ---------------------------------------------------------------
  _writeFallback(code) {
    try {
      const cwd = process.cwd();
      const outDir = path.join(cwd, "assembly", "generated");
      if (!fs.existsSync(outDir)) {
        fs.mkdirSync(outDir, { recursive: true });
      }
      const outPath = path.join(outDir, "cleat-wrappers.ts");
      fs.writeFileSync(outPath, code, "utf-8");
      console.error(
        "[@cleat/transform] Wrote wrapper file to " + outPath +
        ". Add it to your asconfig.json entries."
      );
    } catch (_e) {
      console.error("[@cleat/transform] Could not write fallback wrapper file.");
    }
  }

  // ---------------------------------------------------------------
  // Generate wrapper export function for a list of entry functions
  // ---------------------------------------------------------------
  _generateWrappers(entries) {
    let code = "";
    code += "// ---- Generated cleat ABI wrappers ----\n";
    code += "// Auto-generated by @cleat/transform. Do not edit.\n";
    code += "// Each wrapper is a WASM export conforming to the cleat ABI.\n";
    code += "// The inner function receives the raw input JSON string and\n";
    code += "// returns a result JSON string. Suspension is detected via\n";
    code += "// the __workflowSuspended flag.\n\n";

    for (const entry of entries) {
      code += this._generateSingleWrapper(entry);
    }

    return code;
  }

  // ---------------------------------------------------------------
  // Generate a single wrapper export function
  // ---------------------------------------------------------------
  _generateSingleWrapper(info) {
    const {
      funcName,
      innerName,
      paramNames,
      paramTypes,
      callArgs,
      retTypeStr,
      isVoid,
      isString,
    } = info;

    let code = "";

    // ABI export signature: (argsPtr, argsLen, outPtr, maxOutLen) => i64
    // argsPtr/argsLen: pointer and length of the input JSON in linear memory
    // outPtr/maxOutLen: output buffer where the result JSON will be written
    // return value: packed i64 (low 32 = errCode, high 32 = actualLen)
    //              OR SUSPEND_SENTINEL if the workflow needs to suspend
    code += `export function ${funcName}(\n`;
    code += `  argsPtr: usize,\n`;
    code += `  argsLen: i32,\n`;
    code += `  outPtr: usize,\n`;
    code += `  maxOutLen: i32\n`;
    code += `): i64 {\n`;

    // ------------------------------------------------------------------
    // Step 1: Read input JSON string from WASM linear memory
    // ------------------------------------------------------------------
    code += `  // ---- Step 1: Read input from linear memory ----\n`;
    code += `  let argsJson: string = "";\n`;
    code += `  if (argsLen > 0) {\n`;
    code += `    argsJson = Memory.readString(argsPtr, argsLen);\n`;
    code += `  }\n\n`;

    // ------------------------------------------------------------------
    // Step 2: Create HostCalls instance (first param is always HostCalls)
    // ------------------------------------------------------------------
    code += `  // ---- Step 2: Create HostCalls instance ----\n`;
    code += `  const h = new HostCalls();\n\n`;

    // ------------------------------------------------------------------
    // Step 3: Reset suspension flag and invoke the workflow function
    // ------------------------------------------------------------------
    code += `  // ---- Step 3: Invoke the workflow function ----\n`;
    code += `  resetWorkflowSuspended();\n\n`;

    // Additional parameters beyond HostCalls are assigned from the raw JSON input
    if (paramNames.length === 0) {
      // No additional params -- just pass the HostCalls
      if (isVoid) {
        code += `  let _result: string = "";\n`;
        code += `  ${innerName}(h);\n`;
      } else {
        code += `  const _result: string = ${innerName}(h);\n`;
      }
    } else if (paramNames.length === 1) {
      // Single additional param -- pass the raw JSON string
      code += `  const ${paramNames[0]}: string = argsJson;\n`;
      if (isVoid) {
        code += `  let _result: string = "";\n`;
        code += `  ${innerName}(h, ${paramNames[0]});\n`;
      } else {
        code += `  const _result: string = ${innerName}(h, ${paramNames[0]});\n`;
      }
    } else {
      // Multiple additional params -- parse JSON and extract each field
      code += `  // ---- Parse argsJson for multi-param entry ----\n`;
      code += `  let _parser = new JsonParser();\n`;
      code += `  let _parsed: JsonVal | null = _parser.parse(argsJson);\n`;
      code += `  if (_parsed === null) {\n`;
      code += `    const _errBody = '{"error":"invalid input JSON for multi-param entry"}';\n`;
      code += `    const _errWritten = Memory.writeString(outPtr, maxOutLen, _errBody);\n`;
      code += `    return Memory.encodeExportResult(1, _errWritten);\n`;
      code += `  }\n\n`;
      code += `  // Extract named params\n`;
      for (let i = 0; i < paramNames.length; i++) {
        code += this._getDeserializeCode(paramNames[i], paramTypes[i]);
      }
      if (isVoid) {
        code += `  let _result: string = "";\n`;
        code += `  ${innerName}(h, ${paramNames.join(", ")});\n`;
      } else {
        code += `  const _result: string = ${innerName}(h, ${paramNames.join(", ")});\n`;
      }
    }
    code += `\n`;

    // ------------------------------------------------------------------
    // Step 4: Check for suspension
    // ------------------------------------------------------------------
    code += `  // ---- Step 4: Check for suspension ----\n`;
    code += `  if (isWorkflowSuspended()) {\n`;
    code += `    return SUSPEND_SENTINEL;\n`;
    code += `  }\n\n`;

    // ------------------------------------------------------------------
    // Step 5: Write result to output buffer and return
    // ------------------------------------------------------------------
    if (isVoid) {
      code += `  // ---- Step 5: Write success response ----\n`;
      code += `  const _out = '{"ok":true}';\n`;
      code += `  const _written = Memory.writeString(outPtr, maxOutLen, _out);\n`;
      code += `  return Memory.encodeExportResult(0, _written);\n`;
    } else {
      // String or object return: write the result directly to output
      code += `  // ---- Step 5: Write result to output buffer ----\n`;
      code += `  const _written = Memory.writeString(outPtr, maxOutLen, _result);\n`;
      code += `  return Memory.encodeExportResult(0, _written);\n`;
    }

    code += `}\n\n`;
    return code;
  }

  // ---------------------------------------------------------------
  // Generate a standard error return block for try/catch
  // ---------------------------------------------------------------
  _makeErrorReturn(errorMessage) {
    return (
      `    const _errBody = '{"error":"${errorMessage}"}';\n` +
      `    const _errWritten = Memory.writeString(outPtr, maxOutLen, _errBody);\n` +
      `    return Memory.encodeExportResult(1, _errWritten);\n`
    );
  }

  // ---------------------------------------------------------------
  // Check if any entry in the list has multiple user params
  // ---------------------------------------------------------------
  _hasMultiParamEntries(entries) {
    return entries.some(e => e.paramNames.length > 1);
  }

  // ---------------------------------------------------------------
  // Generate deserialization code for a single parameter based on type
  // Used by the multi-parameter branch to select the correct JSON getter
  // ---------------------------------------------------------------
  _getDeserializeCode(pname, ptype) {
    // Map AS types to the correct JsonParser getter
    if (ptype === "string" || ptype === "String") {
      return `  let ${pname}: string = _parser.getString(_parsed, "${pname}");\n`;
    } else if (ptype === "i32") {
      return `  let ${pname}: i32 = <i32>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "u32") {
      return `  let ${pname}: u32 = <u32>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "i64") {
      return `  let ${pname}: i64 = <i64>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "u64") {
      return `  let ${pname}: u64 = <u64>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "f64") {
      return `  let ${pname}: f64 = _parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "f32") {
      return `  let ${pname}: f32 = <f32>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "bool" || ptype === "boolean") {
      return `  let ${pname}: bool = _parser.getBool(_parsed, "${pname}");\n`;
    } else {
      // Unknown type — throw a compile-time error from the transformer
      throw new Error(
        "[@cleat/transform] Unsupported type '" + ptype +
        "' for multi-parameter entry function parameter '" + pname +
        "'. Supported types: string, i32, u32, i64, u64, f64, f32, bool"
      );
    }
  }
}

// -----------------------------------------------------------------
// Module exports for the AssemblyScript compiler transformer API.
// The AS compiler instantiates this class and calls afterParse().
// -----------------------------------------------------------------
module.exports = CleatEntryTransformer;
